package controller

import (
	"encoding/json"
	"fmt"
	"strings"
)

// videoState mirrors the state file the video member writes each tick. Only the
// fields RAMP needs to relate the container's in-memory position to the
// Redis-committed one are declared.
type videoState struct {
	Session       string `json:"session"`
	Position      int64  `json:"position"`
	FramesDecoded int64  `json:"frames_decoded"`
	FramesPerTick int64  `json:"frames_per_tick"`
	RedisLinked   bool   `json:"redis_linked"`
	Quiesced      bool   `json:"quiesced"`
	QuiesceEpoch  int64  `json:"quiesce_epoch"`
}

// parseMemberState extracts the in-memory position and instance identity from
// the member's state file.
func parseMemberState(raw string) (videoState, error) {
	var st videoState
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return st, fmt.Errorf("member state file is empty")
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return st, fmt.Errorf("parsing member state %q: %w", truncate(raw, 200), err)
	}
	// The application's own invariant: decoded frames must correspond to the
	// stream position. If it does not hold, the in-memory state is not a sound
	// basis for a recovery point and the epoch should not silently accept it.
	if st.FramesPerTick > 0 && st.FramesDecoded != st.Position*st.FramesPerTick {
		return st, fmt.Errorf("member invariant violated: frames_decoded=%d != position=%d * frames_per_tick=%d",
			st.FramesDecoded, st.Position, st.FramesPerTick)
	}
	return st, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
