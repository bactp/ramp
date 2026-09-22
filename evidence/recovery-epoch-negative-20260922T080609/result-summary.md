| test | expectation | phase | failureReason | app quiesced after | app position | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| test1-redis-ack-failure | Redis replica ACK unavailable | Failed | ReplicaAckNotAchieved | False | 11->17 | **PASS** |
| test2-redis-snapshot-failure | Redis epoch snapshot fails | Failed | RedisSnapshotFailed | False | 21->29 | **PASS** |
| test3-video-checkpoint-failure | Video checkpoint fails after the Redis artifact was created | Failed | RedisSnapshotFailed | False | 29->35 | **FAIL** |
| test4-position-mismatch | Redis artifact and Video state disagree | Failed | RedisSnapshotFailed | False | 35->41 | **FAIL** |
