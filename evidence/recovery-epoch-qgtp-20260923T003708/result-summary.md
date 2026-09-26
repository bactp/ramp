# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 56 |
| RecoveryPoint | `video-stream-rg-epoch-56` |
| committed position P | 28870 |
| source position at failure Q | 28909 |
| ObservedLogicalLoss = Q - P | 39 |
| live replica position at failure | 28910 |
| Redis restored initial position | 28870 |
| Video restored initial position | 28870 |
| Video position ~6 s later | 28876 |
| Redis position after resume | 28885 |
| source instance fingerprint | 831844db-d532-43d9-93ae-be3f3f95b0d0 |
| restored instance fingerprint | 831844db-d532-43d9-93ae-be3f3f95b0d0 |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [x] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: PASS
