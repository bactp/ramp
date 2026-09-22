package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/dcn-ssu/ramp/internal/clusters"
)

// The application control path.
//
// The demo workload's quiesce state is represented by a control FILE inside the
// container rather than by a flag in the process. That choice is deliberate and
// load-bearing:
//
//   - RAMP can drive it through the apiserver exec subresource, which is the
//     only door into the workload pod network in this testbed (the HTTP
//     /quiesce and /resume endpoints the application also serves exist for
//     humans and for the experiment scripts).
//   - It survives the checkpoint. A CRI checkpoint carries the container's
//     writable layer, so a process checkpointed while quiesced comes back
//     RESTORED AND STILL QUIESCED, frozen at exactly the epoch position. That
//     is deliberate: the restored member cannot write anything into Redis
//     before the recovery has verified that both members are at P, which is the
//     only ordering in which "Redis was restored to P" is falsifiable. The
//     recovery releases it explicitly as its last step
//     (scripts/ramp-scenario1/50-recover.sh).
const (
	controlPath = "/tmp/ramp-video-control"
	statePath   = "/tmp/ramp-video-state.json"
)

// quiesceMember asks the application to stop advancing its logical position and
// waits until it reports that it has. It does NOT stop the process: the
// container must keep running to be checkpointed, and the quiesce must be
// reversible.
func quiesceMember(ctx context.Context, c *clusters.Cluster, namespace, pod, container string,
	epoch int64, timeout time.Duration) (videoState, error) {

	cmd := fmt.Sprintf(`printf '{"quiesce": true, "epoch": %d}' > %s && cat %s`, epoch, controlPath, statePath)
	if _, err := clusters.Exec(ctx, c, namespace, pod, container, []string{"sh", "-c", cmd}); err != nil {
		return videoState{}, fmt.Errorf("writing quiesce control file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var last videoState
	for {
		st, err := readMemberState(ctx, c, namespace, pod, container)
		if err == nil {
			last = st
			if st.Quiesced {
				return st, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("application did not report quiesced within %s (last state: position=%d quiesced=%t)",
				timeout, last.Position, last.Quiesced)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// verifyQuiesced samples the logical position twice, separated by interval, and
// requires both samples to be identical AND the application to still report
// quiesced. One sample proves nothing: the application ticks at 1 Hz, so a
// single read is indistinguishable from a read taken between two ticks.
func verifyQuiesced(ctx context.Context, c *clusters.Cluster, namespace, pod, container string,
	interval time.Duration) (int64, []int64, videoState, error) {

	var samples []int64
	var st videoState
	for i := 0; i < 2; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return -1, samples, st, ctx.Err()
			case <-time.After(interval):
			}
		}
		s, err := readMemberState(ctx, c, namespace, pod, container)
		if err != nil {
			return -1, samples, st, fmt.Errorf("sampling quiesced position: %w", err)
		}
		if !s.Quiesced {
			return -1, samples, s, fmt.Errorf("application reports quiesced=false at position %d", s.Position)
		}
		st = s
		samples = append(samples, s.Position)
	}
	if samples[0] != samples[1] {
		return -1, samples, st, fmt.Errorf("application still advancing while quiesced: %d then %d", samples[0], samples[1])
	}
	return samples[0], samples, st, nil
}

// resumeMember removes the control file. It is called from a defer, so it takes
// its own context: the epoch's context may already be cancelled, and leaving the
// source application paused because an epoch failed is a worse outcome than any
// failure the epoch could report.
func resumeMember(c *clusters.Cluster, namespace, pod, container string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := clusters.Exec(ctx, c, namespace, pod, container,
		[]string{"sh", "-c", fmt.Sprintf("rm -f %s", controlPath)})
	return err
}
