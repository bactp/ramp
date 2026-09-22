package drivers

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/clusters"
	"github.com/dcn-ssu/ramp/internal/rampredis"
)

// Epoch marker keys. They are written INTO the Redis dataset, which means they
// are also inside the RDB snapshot taken immediately afterwards. That is what
// makes "this RDB belongs to epoch E at position P" a property of the artifact
// itself rather than a claim in a status field.
const (
	KeyEpoch         = "ramp:epoch"
	KeyEpochPosition = "ramp:epoch:position"
)

// EpochPositionKey is the per-epoch marker. Unlike ramp:epoch it is never
// overwritten by a later epoch, so a restored dataset can be attributed to an
// exact epoch even if further epochs ran before the failure.
func EpochPositionKey(epoch int64) string {
	return fmt.Sprintf("ramp:epoch:%d:position", epoch)
}

// BarrierResult is the acknowledged replication barrier for one epoch.
type BarrierResult struct {
	Endpoint      string
	ReplicationID string
	MarkerOffset  int64
	AckedReplicas int64
	Required      int64
	Position      int64
	Epoch         int64
	At            time.Time
}

// Barrier writes the epoch marker for position P onto the primary and then
// requires replicas to ACKNOWLEDGE it.
//
// The marker SETs and the WAIT travel on ONE connection on purpose: Redis scopes
// WAIT to the writes issued on the calling connection, so a WAIT sent on a fresh
// connection acknowledges nothing and would degrade to the same vacuous
// "a replica is attached" test this replaces.
func (d *RedisReplication) Barrier(ep *rampv1alpha1.RedisEndpoint, epoch, position int64,
	requiredAcks int, timeout time.Duration) (*BarrierResult, error) {

	if ep == nil {
		return nil, fmt.Errorf("redis-replication member has no sourceEndpoint")
	}
	c := rampredis.New(ep.Host, ep.Port)

	cmds := [][]string{
		{"SET", KeyEpoch, strconv.FormatInt(epoch, 10)},
		{"SET", KeyEpochPosition, strconv.FormatInt(position, 10)},
		{"SET", EpochPositionKey(epoch), strconv.FormatInt(position, 10)},
		{"WAIT", strconv.Itoa(requiredAcks), strconv.FormatInt(timeout.Milliseconds(), 10)},
	}
	replies, err := c.Pipeline(timeout+rampredis.DialTimeout, cmds)
	if err != nil {
		return nil, fmt.Errorf("epoch barrier on %s: %w", c.Addr(), err)
	}
	acked, convErr := strconv.ParseInt(strings.TrimSpace(replies[len(replies)-1]), 10, 64)
	if convErr != nil {
		return nil, fmt.Errorf("WAIT on %s returned %q, not an integer", c.Addr(), replies[len(replies)-1])
	}

	info, err := c.Replication()
	if err != nil {
		return nil, fmt.Errorf("reading replication state from %s after barrier: %w", c.Addr(), err)
	}
	res := &BarrierResult{
		Endpoint:      c.Addr(),
		ReplicationID: info.Raw["master_replid"],
		MarkerOffset:  info.MasterReplOffset,
		AckedReplicas: acked,
		Required:      int64(requiredAcks),
		Position:      position,
		Epoch:         epoch,
		At:            time.Now(),
	}
	if acked < int64(requiredAcks) {
		return res, fmt.Errorf("replication barrier not acknowledged: WAIT %d returned %d on %s within %s",
			requiredAcks, acked, c.Addr(), timeout)
	}
	return res, nil
}

// SnapshotResult is one epoch-specific Redis RDB.
type SnapshotResult struct {
	Data           []byte
	PositionBefore int64
	PositionAfter  int64
	EpochMarker    int64
	Pod            string
	Started        time.Time
	Completed      time.Time
	Raw            string
}

// snapshotScript removes any stale RDB, takes a synchronous SAVE and reads the
// result back, bracketing it with the application position so the artifact can
// be proved to correspond to exactly one position.
//
// SAVE rather than BGSAVE: the dataset is a handful of keys, the application is
// quiesced, and a synchronous save removes the "did the fork finish, and which
// dataset did it fork from" question entirely. Deleting the file first makes
// freshness unambiguous -- LASTSAVE only has one-second resolution.
const snapshotScript = `
set -e
POSK="$1"; EPOCHK="$2"
echo "POS_BEFORE=$(redis-cli --no-raw GET "$POSK" | tr -d '"\r')"
echo "EPOCH_MARKER=$(redis-cli --no-raw GET "$EPOCHK" | tr -d '"\r')"
rm -f /data/dump.rdb
redis-cli SAVE >/dev/null
[ -s /data/dump.rdb ] || { echo "SAVE produced no /data/dump.rdb" >&2; exit 1; }
echo "POS_AFTER=$(redis-cli --no-raw GET "$POSK" | tr -d '"\r')"
echo "BGSAVE_STATUS=$(redis-cli INFO persistence | tr -d '\r' | awk -F: '/^rdb_last_bgsave_status/{print $2}')"
echo "SIZE=$(wc -c < /data/dump.rdb)"
echo "RDB_BASE64=$(base64 -w0 /data/dump.rdb)"
`

// CaptureSnapshot produces the epoch's immutable Redis artifact.
//
// It runs on the SOURCE PRIMARY rather than on the replica: the primary is the
// instance whose dataset the quiesced application's position was written to, and
// after the WAIT barrier the replica is only guaranteed to be at least that far,
// not exactly that far. Saving on the primary while the writer is quiesced makes
// "this RDB is position P" provable instead of probable.
func (d *RedisReplication) CaptureSnapshot(ctx context.Context, c *clusters.Cluster,
	namespace, pod, container, positionKey string, epoch int64) (*SnapshotResult, error) {

	started := time.Now()
	out, err := clusters.Exec(ctx, c, namespace, pod, container,
		[]string{"sh", "-c", snapshotScript, "ramp-snapshot", positionKey, EpochPositionKey(epoch)})
	if err != nil {
		return nil, fmt.Errorf("redis SAVE in %s/%s: %w", namespace, pod, err)
	}
	res := &SnapshotResult{
		Pod: pod, Started: started, Completed: time.Now(),
		PositionBefore: -1, PositionAfter: -1, EpochMarker: -1,
	}
	var b64 string
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok {
			continue
		}
		switch k {
		case "POS_BEFORE":
			res.PositionBefore, _ = strconv.ParseInt(v, 10, 64)
		case "POS_AFTER":
			res.PositionAfter, _ = strconv.ParseInt(v, 10, 64)
		case "EPOCH_MARKER":
			res.EpochMarker, _ = strconv.ParseInt(v, 10, 64)
		case "RDB_BASE64":
			b64 = v
		case "SIZE", "BGSAVE_STATUS":
			res.Raw += k + "=" + v + " "
		}
	}
	if b64 == "" {
		return nil, fmt.Errorf("redis snapshot produced no RDB payload (output: %s)", truncate(out, 400))
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decoding RDB payload from %s/%s: %w", namespace, pod, err)
	}
	res.Data = data
	return res, nil
}
