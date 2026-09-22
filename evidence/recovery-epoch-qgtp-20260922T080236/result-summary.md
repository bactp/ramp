# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 19 |
| RecoveryPoint | `video-stream-rg-epoch-19` |
| committed position P | 102 |
| source position at failure Q | 140 |
| ObservedLogicalLoss = Q - P | 38 |
| live replica position at failure | 141 |
| Redis restored initial position | 102 |
| Video restored initial position | 102 |
| Video position ~6 s later | 108 |
| Redis position after resume | 117 |
| source instance fingerprint | c47fd89d-7047-4212-b24a-77d0e486ffed |
| restored instance fingerprint | c47fd89d-7047-4212-b24a-77d0e486ffed |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [x] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: PASS
