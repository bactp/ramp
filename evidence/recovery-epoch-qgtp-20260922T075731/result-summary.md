# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 18 |
| RecoveryPoint | `video-stream-rg-epoch-18` |
| committed position P | 66 |
| source position at failure Q | 105 |
| ObservedLogicalLoss = Q - P | 39 |
| live replica position at failure | 106 |
| Redis restored initial position | 66 |
| Video restored initial position | 66 |
| Video position ~6 s later | 66 |
| Redis position after resume | 66 |
| source instance fingerprint | bbe2ad5c-27af-4ede-8a27-4856c4dd660b |
| restored instance fingerprint | bbe2ad5c-27af-4ede-8a27-4856c4dd660b |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [ ] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: FAIL
