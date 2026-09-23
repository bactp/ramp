# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 45 |
| RecoveryPoint | `video-stream-rg-epoch-45` |
| committed position P | 26430 |
| source position at failure Q | 26457 |
| ObservedLogicalLoss = Q - P | 27 |
| live replica position at failure | 26458 |
| Redis restored initial position | 26430 |
| Video restored initial position | 26430 |
| Video position ~6 s later | 26436 |
| Redis position after resume | 26445 |
| source instance fingerprint | 2fb624e4-3392-4da7-8d57-caf4c4951412 |
| restored instance fingerprint | 2fb624e4-3392-4da7-8d57-caf4c4951412 |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [x] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: PASS
