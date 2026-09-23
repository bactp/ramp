| test | expectation | phase | failureReason | app quiesced after | app position | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| test1-redis-ack-failure | Redis replica ACK unavailable | Failed | ReplicaAckNotAchieved | False | 26394->26401 | **PASS** |
| test2-redis-snapshot-failure | Redis epoch snapshot fails | Failed | RedisSnapshotFailed | False | 26405->26412 | **PASS** |
| test3-video-checkpoint-failure | Video checkpoint fails after the Redis artifact was created | Failed | CheckpointFailed | False | 26412->26419 | **PASS** |
| test4-position-mismatch | Redis artifact and Video state disagree | Failed | ValidationFailed | False | 26419->26425 | **PASS** |
