// Package rampredis is a deliberately tiny RESP client. RAMP only needs to ask
// Redis about its own replication state and read a handful of application
// keys; pulling in a full Redis client for that would be more dependency than
// the readiness evaluation is worth.
//
// It talks straight to a routable host:port rather than exec'ing into a pod,
// because in this testbed the management plane can reach workload NodePorts on
// the shared transit network but cannot reach pod or node networks at all.
package rampredis

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// DialTimeout bounds every probe; a readiness check must never block a
// reconcile loop.
const DialTimeout = 5 * time.Second

// Client is a single-shot connection to one Redis endpoint.
type Client struct {
	addr string
	// deadline is the per-command I/O deadline. It is per-client rather than
	// global so that WAIT -- which is supposed to block server-side -- can be
	// given room without loosening every readiness probe.
	deadline time.Duration
}

// New returns a client for host:port.
func New(host string, port int32) *Client {
	return &Client{addr: net.JoinHostPort(host, strconv.Itoa(int(port))), deadline: DialTimeout}
}

// WithDeadline returns a copy of the client using a different I/O deadline.
func (c *Client) WithDeadline(d time.Duration) *Client {
	cp := *c
	cp.deadline = d
	return &cp
}

// Addr is the endpoint this client talks to.
func (c *Client) Addr() string { return c.addr }

func encode(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// Do issues one command and returns the reply as a string. Bulk strings are
// returned without their framing; a nil bulk returns ("", false, nil).
func (c *Client) Do(args ...string) (value string, present bool, err error) {
	conn, err := net.DialTimeout("tcp", c.addr, DialTimeout)
	if err != nil {
		return "", false, fmt.Errorf("dial %s: %w", c.addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.effectiveDeadline()))

	if _, err := conn.Write(encode(args...)); err != nil {
		return "", false, fmt.Errorf("write %s: %w", c.addr, err)
	}

	return readReply(bufio.NewReader(conn), c.addr)
}

// readReply decodes one RESP reply.
func readReply(r *bufio.Reader, addr string) (value string, present bool, err error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", addr, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", false, fmt.Errorf("empty reply from %s", addr)
	}

	switch line[0] {
	case '+': // simple string
		return line[1:], true, nil
	case '-': // error
		return "", false, fmt.Errorf("redis error from %s: %s", addr, line[1:])
	case ':': // integer
		return line[1:], true, nil
	case '$': // bulk string
		n, convErr := strconv.Atoi(line[1:])
		if convErr != nil {
			return "", false, fmt.Errorf("bad bulk header %q from %s", line, addr)
		}
		if n < 0 {
			return "", false, nil // nil bulk: key absent
		}
		buf := make([]byte, n+2) // payload + CRLF
		if _, err := readFull(r, buf); err != nil {
			return "", false, fmt.Errorf("read bulk %s: %w", addr, err)
		}
		return string(buf[:n]), true, nil
	default:
		return "", false, fmt.Errorf("unsupported reply type %q from %s", line[0], addr)
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (c *Client) effectiveDeadline() time.Duration {
	if c.deadline <= 0 {
		return DialTimeout
	}
	return c.deadline
}

// Pipeline issues several commands on ONE connection and returns their replies
// in order. This is what makes a replication barrier meaningful: the epoch
// marker SETs and the WAIT that acknowledges them have to travel on the same
// connection, because WAIT only accounts for writes issued on it.
func (c *Client) Pipeline(deadline time.Duration, cmds [][]string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(deadline))

	var out []byte
	for _, cmd := range cmds {
		out = append(out, encode(cmd...)...)
	}
	if _, err := conn.Write(out); err != nil {
		return nil, fmt.Errorf("write %s: %w", c.addr, err)
	}

	r := bufio.NewReader(conn)
	replies := make([]string, 0, len(cmds))
	for i := range cmds {
		v, _, err := readReply(r, c.addr)
		if err != nil {
			return replies, fmt.Errorf("command %v on %s: %w", cmds[i], c.addr, err)
		}
		replies = append(replies, v)
	}
	return replies, nil
}

// GetInt reads a key expected to hold a base-10 integer.
func (c *Client) GetInt(key string) (int64, bool, error) {
	v, present, err := c.Do("GET", key)
	if err != nil || !present {
		return 0, present, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, true, fmt.Errorf("key %q is not an integer (%q)", key, v)
	}
	return n, true, nil
}

// ReplicationInfo is the parsed subset of INFO replication that RAMP needs.
type ReplicationInfo struct {
	Role             string // "master" | "slave"
	ConnectedSlaves  int64
	MasterLinkStatus string // replica only: "up" | "down"
	MasterReplOffset int64  // primary's stream position
	SlaveReplOffset  int64  // replica's acknowledged position
	MasterHost       string
	MasterPort       string
	LastIOSecondsAgo int64
	Raw              map[string]string
}

// Replication runs INFO replication and parses it.
func (c *Client) Replication() (*ReplicationInfo, error) {
	out, _, err := c.Do("INFO", "replication")
	if err != nil {
		return nil, err
	}
	info := &ReplicationInfo{Raw: map[string]string{}, LastIOSecondsAgo: -1}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		info.Raw[k] = v
		switch k {
		case "role":
			info.Role = v
		case "connected_slaves":
			info.ConnectedSlaves, _ = strconv.ParseInt(v, 10, 64)
		case "master_link_status":
			info.MasterLinkStatus = v
		case "master_repl_offset":
			info.MasterReplOffset, _ = strconv.ParseInt(v, 10, 64)
		case "slave_repl_offset":
			info.SlaveReplOffset, _ = strconv.ParseInt(v, 10, 64)
		case "master_host":
			info.MasterHost = v
		case "master_port":
			info.MasterPort = v
		case "master_last_io_seconds_ago":
			info.LastIOSecondsAgo, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if info.Role == "" {
		return nil, fmt.Errorf("could not parse INFO replication from %s", c.addr)
	}
	return info, nil
}

// AckedOffset is the position the instance has durably acknowledged: the
// replica's own offset when it is a replica, the master's stream offset
// otherwise.
func (i *ReplicationInfo) AckedOffset() int64 {
	if i.Role == "slave" {
		return i.SlaveReplOffset
	}
	return i.MasterReplOffset
}
