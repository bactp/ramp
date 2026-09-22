package drivers

import (
	"fmt"
	"time"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/rampredis"
)

// RedisReplication captures a member whose recovery mechanism is Redis-native
// continuity. Nothing is checkpointed here on purpose: the recovery state is a
// live replica, and "capture" means pinning the logical position and the
// replication offset that the epoch is anchored to.
type RedisReplication struct{}

// ReplicationCapture is the recovery state of a redis-replication member.
type ReplicationCapture struct {
	Endpoint          string
	Role              string
	ReplicationOffset int64
	ReplicationID     string
	ConnectedReplicas int64
	LogicalPosition   int64
	CapturedAt        time.Time
}

// Capture reads the primary's replication position and the application logical
// position that the RecoveryGroup declared as its consistency key.
func (d *RedisReplication) Capture(ep *rampv1alpha1.RedisEndpoint, consistencyKeys []string) (*ReplicationCapture, error) {
	if ep == nil {
		return nil, fmt.Errorf("redis-replication member has no sourceEndpoint")
	}
	c := rampredis.New(ep.Host, ep.Port)

	info, err := c.Replication()
	if err != nil {
		return nil, fmt.Errorf("reading replication state from %s: %w", c.Addr(), err)
	}
	if info.Role != "master" {
		return nil, fmt.Errorf("source endpoint %s has role %q, expected master", c.Addr(), info.Role)
	}

	cap := &ReplicationCapture{
		Endpoint:          c.Addr(),
		Role:              info.Role,
		ReplicationOffset: info.MasterReplOffset,
		ReplicationID:     info.Raw["master_replid"],
		ConnectedReplicas: info.ConnectedSlaves,
		CapturedAt:        time.Now(),
		LogicalPosition:   -1,
	}

	// The logical position is what makes this artifact comparable with a
	// container checkpoint. Without it the epoch would only be byte-consistent,
	// not application-consistent.
	for _, k := range consistencyKeys {
		v, present, err := c.GetInt(k)
		if err != nil {
			return nil, fmt.Errorf("reading consistency key %q from %s: %w", k, c.Addr(), err)
		}
		if present {
			cap.LogicalPosition = v
			break
		}
	}
	if cap.LogicalPosition < 0 && len(consistencyKeys) > 0 {
		return nil, fmt.Errorf("none of the consistency keys %v are present on %s", consistencyKeys, c.Addr())
	}
	return cap, nil
}

// StandbyState is the target-side view used by readiness evaluation.
type StandbyState struct {
	Endpoint   string
	Role       string
	LinkStatus string
	// ReplicationID identifies the replication STREAM. Offsets are only
	// comparable within one stream: if the primary is recreated, offsets
	// restart from zero under a new id, and comparing across ids silently
	// produces nonsense.
	ReplicationID    string
	AckedOffset      int64
	LastIOSecondsAgo int64
	LogicalPosition  int64
}

// InspectStandby reads the target standby without changing it. Readiness
// evaluation is strictly an observation: promotion is an actuation step and
// belongs to the recovery execution, not to the readiness controller.
func (d *RedisReplication) InspectStandby(ep *rampv1alpha1.RedisEndpoint, consistencyKeys []string) (*StandbyState, error) {
	if ep == nil {
		return nil, fmt.Errorf("no standbyEndpoint configured")
	}
	c := rampredis.New(ep.Host, ep.Port)
	info, err := c.Replication()
	if err != nil {
		return nil, fmt.Errorf("reading replication state from standby %s: %w", c.Addr(), err)
	}
	st := &StandbyState{
		Endpoint:         c.Addr(),
		Role:             info.Role,
		LinkStatus:       info.MasterLinkStatus,
		ReplicationID:    info.Raw["master_replid"],
		AckedOffset:      info.AckedOffset(),
		LastIOSecondsAgo: info.LastIOSecondsAgo,
		LogicalPosition:  -1,
	}
	for _, k := range consistencyKeys {
		if v, present, err := c.GetInt(k); err == nil && present {
			st.LogicalPosition = v
			break
		}
	}
	return st, nil
}
