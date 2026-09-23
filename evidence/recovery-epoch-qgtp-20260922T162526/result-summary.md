# Q > P recovery correctness experiment

| quantity | value |
| --- | --- |
| epoch E | 48 |
| RecoveryPoint | `video-stream-rg-epoch-48` |
| committed position P | 409 |
| source position at failure Q | 435 |
| ObservedLogicalLoss = Q - P | 26 |
| live replica position at failure | 436 |
| Redis restored initial position | 409 |
| Video restored initial position | 409 |
| Video position ~6 s later | 415 |
| Redis position after resume | 424 |
| source instance fingerprint | 9ecffb68-d3fb-464a-b5f6-56d50050253c |
| restored instance fingerprint | 9ecffb68-d3fb-464a-b5f6-56d50050253c |

- [x] Q > P (the source really advanced past the recovery point)
- [x] live replica held Q, not P, at failure time
- [x] Redis restored INITIALLY to P (measured before Video started)
- [x] Redis did NOT recover to Q
- [x] Video restored INITIALLY to P
- [x] restored Video is the SAME process instance as the source
- [x] both members progress forward from P after recovery
- [x] no catch-up burst: restored Video advanced <= 12 positions in ~6 s

## VERDICT: PASS
