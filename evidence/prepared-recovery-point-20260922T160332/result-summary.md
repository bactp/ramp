| Test | Expected | Observed | Pass |
|------|----------|----------|------|
| A | prepared=RP1, candidate=none, HOT | `prepared=video-stream-rg-epoch-37 candidate=none latest=video-stream-rg-epoch-37 readiness=HOT` | **PASS** |
| B | latest=RP2, candidate=RP2, prepared stays RP1, HOT preserved | `prepared=video-stream-rg-epoch-37 candidate=video-stream-rg-epoch-38 latest=video-stream-rg-epoch-38 readiness=HOT` | **PASS** |
| C | prepared=RP2, candidate=none, HOT, artifacts identify RP2 | `prepared=video-stream-rg-epoch-38 candidate= readiness=HOT artifactsPointAtRP2=yes` | **PASS** |
| G1 | RTO 60s > estimated: contract satisfied, HOT | `rto=1m0s estimated=12s within=true readiness=HOT` | **PASS** |
| G2 | RTO 5s < estimated: contract violated, HOT lost | `rto=5s estimated=12s within=false readiness=WARM` | **PASS** |
| E | RestoreArtifactReady=False, HOT lost | `RestoreArtifactReady=False readiness=WARM detected_in=34s` | **PASS** |
| F | TargetPlacementFeasible=False, HOT lost with an explicit reason | `TargetPlacementFeasible=False readiness=COLD` | **PASS** |
| D | FreshEnough True -> False at the RPO deadline, HOT lost | `freshEnough=False age=3m4s rpo=182s readiness=WARM transitionAt=2026-09-22T16:07:31+00:00` | **PASS** |

RP1 = video-stream-rg-epoch-37
RP2 = video-stream-rg-epoch-38
placement node = workload02-md-0-rx5mn-pjkrh
checkpoint image (RP1) = docker.io/ramp/checkpoint-video-session:video-stream-rg-epoch-37
checkpoint image (RP2) = docker.io/ramp/checkpoint-video-session:video-stream-rg-epoch-38
