| test | expectation | phase | failureReason | app quiesced after | app position | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| test1-redis-ack-failure | Redis replica ACK unavailable | Failed | ReplicaAckNotAchieved | False | 65->71 | **PASS** |
| test2-redis-snapshot-failure | Redis epoch snapshot fails | Failed | RedisSnapshotFailed | False | 75->82 | **PASS** |
| test3-video-checkpoint-failure | Video checkpoint fails after the Redis artifact was created | Failed | CheckpointFailed | False | 83->90 | **PASS** |
| test4-position-mismatch | Redis artifact and Video state disagree | Failed | ValidationFailed | False | 91->97 | **PASS** |
