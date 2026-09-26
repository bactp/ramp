#!/usr/bin/env bash
# Readiness must be STABLE, not just correct at one instant.
#
# The restore-artifact probe re-measures on a timer, because an artifact that
# was confirmed can be deleted and the check has to notice. A first version did
# that by deleting the finished probe and creating a replacement, which left a
# gap where no terminal result existed: the check went False and the path
# dropped out of HOT roughly once a minute, purely because it was re-measuring.
#
# That is the same class of bug as evaluating the latest RecoveryPoint instead
# of the prepared one -- readiness lost for a reason that is not a loss -- so it
# gets its own regression: sample the path for a few minutes and require that a
# prepared, fresh, fully-staged path never leaves HOT.
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
SAMPLES="${SAMPLES:-36}"        # 36 x 5s = 3 minutes, i.e. several probe generations
INTERVAL="${INTERVAL:-5}"
OUT="${OUT:-$ROOT/evidence/readiness-stability-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"

K(){ kubectl --kubeconfig "$MGMT" "$@"; }
jp(){ K get recoverypath "$PATH_NAME" -o jsonpath="$1" 2>/dev/null; }

# Make sure there is something to be stable ABOUT.
#
# "Not HOT" is NOT the same as "not prepared". A path can be fully prepared and
# still not HOT because its prepared point has outlived the RPO -- freshness and
# preparation are separate dimensions, which is exactly what this system is
# built to distinguish. An earlier version of this setup re-prepared whatever
# the latest committed point was; when that point was hours old it re-prepared a
# stale one, waited for a HOT that could never arrive, and then sampled 36
# useless rows. Preparation cannot fix staleness: only a NEW epoch can.
if [ "$(jp '{.status.readiness}')" != "HOT" ]; then
  echo "path is $(jp '{.status.readiness}'); creating and preparing a fresh RecoveryPoint"
  ./20-run-epoch.sh video-stream-rg default > "$OUT/epoch.log" 2>&1 || {
    echo "epoch failed"; tail -20 "$OUT/epoch.log"; exit 1; }
  RP=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/epoch.log" | tail -1)
  [ -n "$RP" ] || { echo "could not determine the epoch just created"; exit 1; }
  echo "prepared point will be: $RP"
  RP="$RP" ./36-prepare-target.sh > "$OUT/prepare.log" 2>&1 || {
    echo "preparation failed"; tail -20 "$OUT/prepare.log"; exit 1; }
  for _ in $(seq 1 60); do [ "$(jp '{.status.readiness}')" = "HOT" ] && break; sleep 3; done
fi

if [ "$(jp '{.status.readiness}')" != "HOT" ]; then
  # Fail on the precondition, with the reason, rather than sampling a state the
  # test cannot say anything useful about.
  echo "FATAL: the path did not reach HOT, so stability cannot be measured."
  echo "unmet: $(jp '{.status.unmetMandatoryChecks}')"
  K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status'].get('checks',[]):
    if c['status'] != 'True': print('  %s: %s' % (c['name'], c['message']))" | tee "$OUT/precondition-failure.txt"
  exit 1
fi

echo "sampling $PATH_NAME every ${INTERVAL}s x $SAMPLES" | tee "$OUT/samples.txt"
printf '%-26s %-6s %-26s %s\n' "at" "level" "restoreArtifactReady" "prepared" >> "$OUT/samples.txt"
NONHOT=0
for _ in $(seq 1 "$SAMPLES"); do
  R=$(jp '{.status.readiness}')
  A=$(K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status'].get('checks',[]):
    if c['name']=='RestoreArtifactReady': print(c['status']+'/'+c['reason']); break
else: print('Absent')")
  printf '%-26s %-6s %-26s %s\n' "$(date -Is)" "$R" "$A" "$(jp '{.status.preparedRecoveryPoint.name}')" >> "$OUT/samples.txt"
  [ "$R" = "HOT" ] || NONHOT=$((NONHOT+1))
  sleep "$INTERVAL"
done

{
  echo
  echo "samples=$SAMPLES  non-HOT samples=$NONHOT"
  echo "verdict=$([ "$NONHOT" -eq 0 ] && echo PASS || echo FAIL)"
} | tee -a "$OUT/samples.txt"
cp "$OUT/samples.txt" "$OUT/result-summary.md"
echo "evidence -> $OUT"
[ "$NONHOT" -eq 0 ]
