# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 17 |
| RecoveryPoint | `video-stream-rg-epoch-17` |
| committed position P | 110 |
| source position at failure Q | 141 |
| ObservedLogicalLoss = Q - P | 31 |
| live replica position at failure | 142 |
| Redis restored initial position | 110 |
| Video restored initial position | ? |
| Video position ~6 s later | ? |
| Redis position after resume | 110 |
| source instance fingerprint | f484ea09-71d3-4893-9a89-16c8d19c2450 |
| restored instance fingerprint | ? |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [ ] Video restored INITIALLY to P
- [ ] restored Video is the SAME process instance as the source
- [ ] both members progress forward from P after recovery
- [ ] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: FAIL
