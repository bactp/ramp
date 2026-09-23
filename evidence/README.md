# Evidence

`recovery-epoch-*` directories are produced by the RecoveryEpoch correctness
work; they are indexed, with which run is authoritative and why the failed ones
are kept, in
[docs/ramp-scenario1/recovery-epoch-correctness-results.md](../docs/ramp-scenario1/recovery-epoch-correctness-results.md#7-evidence-index).

`prepared-recovery-point-*` and `readiness-stability-*` are produced by the
PreparedRecoveryPoint / contract-aware readiness work; see
[docs/ramp-scenario1/prepared-recovery-point-results.md](../docs/ramp-scenario1/prepared-recovery-point-results.md).

Earlier directories (`scenario1/`, `audit/`, `logs/`) belong to the preceding
Scenario 1 work and are described in `docs/ramp-scenario1/04-results.md`.

Reproduce:

```bash
scripts/ramp-scenario1/95-qgtp-experiment.sh    # the Q > P correctness gate
scripts/ramp-scenario1/96-negative-tests.sh     # four abort paths
scripts/ramp-scenario1/97-regression-tests.sh   # timer catch-up + restart safety
scripts/ramp-scenario1/98-prepared-recovery-point-tests.sh  # candidate/prepared + contract
scripts/ramp-scenario1/99-readiness-stability.sh            # readiness must not flap
```
