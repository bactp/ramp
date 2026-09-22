# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 16 |
| RecoveryPoint | `video-stream-rg-epoch-16` |
| committed position P | 7314 |
| source position at failure Q | 7345 |
| ObservedLogicalLoss = Q - P | 31 |
| live replica position at failure | 7346 |
| Redis restored initial position | 7314 |
| Video restored initial position | ? |
| Video position ~6 s later | ? |
| Redis position after resume | 7314 |
| source instance fingerprint | 46b0a9aa-ad64-44a9-9137-7294feeec064 |
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
