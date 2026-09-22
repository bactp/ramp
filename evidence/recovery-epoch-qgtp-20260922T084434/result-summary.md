# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 29 |
| RecoveryPoint | `video-stream-rg-epoch-29` |
| committed position P | 2112 |
| source position at failure Q | 2151 |
| ObservedLogicalLoss = Q - P | 39 |
| live replica position at failure | 2152 |
| Redis restored initial position | 2112 |
| Video restored initial position | 2112 |
| Video position ~6 s later | 2118 |
| Redis position after resume | 2127 |
| source instance fingerprint | 5ad2f48c-d435-483c-bd13-44ee39afa759 |
| restored instance fingerprint | 5ad2f48c-d435-483c-bd13-44ee39afa759 |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [x] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: PASS
