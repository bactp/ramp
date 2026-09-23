#!/usr/bin/env bash
# The PreparedRecoveryPoint / contract-aware readiness state machine, end to end.
#
#   A  RP1 committed and prepared            -> prepared=RP1, candidate=none, HOT
#   B  RP2 committed, NOT prepared           -> prepared=RP1, candidate=RP2, still HOT
#   C  RP2 prepared                          -> prepared=RP2, candidate=none, HOT
#   D  prepared point outlives the RPO       -> FreshEnough=False, HOT lost
#   E  checkpoint image removed from node    -> RestoreArtifactReady=False, HOT lost
#   F  placement node cordoned               -> TargetPlacementFeasible=False, HOT lost
#   G  RTO contract raised / lowered         -> EstimatedRTOWithinContract flips
#
# B is the one that matters most: a newer RecoveryPoint existing must not spend
# the readiness an older, still-valid one already bought.
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
GROUP="${GROUP:-video-stream-rg}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
NS="${NS:-ramp-demo}"
OUT="${OUT:-$ROOT/evidence/prepared-recovery-point-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }
ts() { date -Is; }
jp() { K get recoverypath "$PATH_NAME" -o jsonpath="$1" 2>/dev/null; }
say(){ echo; echo "########## $* ##########"; }
note(){ echo "$*" | tee -a "$OUT/timings.txt"; }

READINESS(){ jp '{.status.readiness}'; }
PREPARED(){  jp '{.status.preparedRecoveryPoint.name}'; }
CANDIDATE(){ jp '{.status.candidateRecoveryPoint.name}'; }
LATEST(){    jp '{.status.latestRecoveryPoint.name}'; }
CHECK(){     K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status'].get('checks',[]):
    if c['name']=='$1': print(c['status']); break
else: print('Absent')"; }
REASON(){    K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status'].get('checks',[]):
    if c['name']=='$1': print(c['reason']); break
else: print('Absent')"; }

snap(){ # snap <file>
  K get recoverypath "$PATH_NAME" -o yaml > "$OUT/$1"
}

# wait_until <seconds> <shell-condition>
wait_until(){ local n="$1"; shift; local i=0
  while [ "$i" -lt "$n" ]; do eval "$@" && return 0; sleep 2; i=$((i+2)); done; return 1; }

RESULTS=()
record(){ RESULTS+=("$1|$2|$3|$4"); }

restore_contract(){ K patch recoverygroup "$GROUP" --type=merge \
  -p '{"spec":{"recoveryContract":{"rpo":"30m","rto":"1m0s"}}}' >/dev/null; }

trap 'K2 uncordon '"$(jp '{.status.targetPlacement.node}')"' >/dev/null 2>&1 || true; restore_contract' EXIT

note "T_test_start=$(ts)"
restore_contract
K get recoverygroup "$GROUP" -o yaml > "$OUT/recoverygroup.yaml"

NODE=$(jp '{.status.targetPlacement.node}')
[ -n "$NODE" ] || { echo "the path has not selected a placement node yet"; exit 1; }
note "placement_node=$NODE"

# ===================================================================== A ====
say "A. first prepared RecoveryPoint"
./20-run-epoch.sh "$GROUP" default > "$OUT/a-epoch.log" 2>&1 || { echo "epoch failed"; tail -20 "$OUT/a-epoch.log"; exit 1; }
RP1=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/a-epoch.log" | tail -1)
note "RP1=$RP1"
K get recoverypoint "$RP1" -o yaml > "$OUT/rp1.yaml"
RP="$RP1" TARGET_NODE="$NODE" ./36-prepare-target.sh > "$OUT/a-prepare.log" 2>&1 || {
  echo "preparation of $RP1 failed"; tail -20 "$OUT/a-prepare.log"; exit 1; }
CKPT_IMAGE_1=$(awk -F= '/^CKPT_IMAGE=/{print $2}' "$OUT/a-prepare.log" | tail -1)
note "RP1_checkpoint_image=$CKPT_IMAGE_1"

wait_until 180 '[ "$(PREPARED)" = "'"$RP1"'" ] && [ "$(READINESS)" = "HOT" ]'
snap path-rp1-hot.yaml
A_CAND=$(CANDIDATE)
A_OBS="prepared=$(PREPARED) candidate=${A_CAND:-none} latest=$(LATEST) readiness=$(READINESS)"
echo "$A_OBS"
A_PASS=$([ "$(PREPARED)" = "$RP1" ] && [ -z "$(CANDIDATE)" ] && [ "$(READINESS)" = "HOT" ] && echo PASS || echo FAIL)
record "A" "prepared=RP1, candidate=none, HOT" "$A_OBS" "$A_PASS"
note "T_A_done=$(ts)"

# ===================================================================== B ====
say "B. a newer RecoveryPoint appears and is NOT prepared"
./20-run-epoch.sh "$GROUP" default > "$OUT/b-epoch.log" 2>&1 || { echo "epoch failed"; tail -20 "$OUT/b-epoch.log"; exit 1; }
RP2=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/b-epoch.log" | tail -1)
note "RP2=$RP2"
K get recoverypoint "$RP2" -o yaml > "$OUT/rp2.yaml"

# Give the controller several full reconciles to notice RP2 and to be tempted
# by it. The defect being tested is a change that should NOT happen.
wait_until 120 '[ "$(CANDIDATE)" = "'"$RP2"'" ]'
sleep 30
snap path-rp2-candidate.yaml
B_OBS="prepared=$(PREPARED) candidate=$(CANDIDATE) latest=$(LATEST) readiness=$(READINESS)"
echo "$B_OBS"
B_PASS=$([ "$(PREPARED)" = "$RP1" ] && [ "$(CANDIDATE)" = "$RP2" ] && [ "$(LATEST)" = "$RP2" ] && [ "$(READINESS)" = "HOT" ] && echo PASS || echo FAIL)
record "B" "latest=RP2, candidate=RP2, prepared stays RP1, HOT preserved" "$B_OBS" "$B_PASS"
note "T_B_done=$(ts)"
K get recoverypath "$PATH_NAME" -o jsonpath='{.status.candidateRecoveryPoint}' | python3 -m json.tool > "$OUT/b-candidate.json" 2>/dev/null || true

# ===================================================================== C ====
say "C. promote the candidate"
RP="$RP2" TARGET_NODE="$NODE" ./36-prepare-target.sh > "$OUT/c-prepare.log" 2>&1 || {
  echo "preparation of $RP2 failed"; tail -20 "$OUT/c-prepare.log"; exit 1; }
CKPT_IMAGE_2=$(awk -F= '/^CKPT_IMAGE=/{print $2}' "$OUT/c-prepare.log" | tail -1)
note "RP2_checkpoint_image=$CKPT_IMAGE_2"
wait_until 240 '[ "$(PREPARED)" = "'"$RP2"'" ]'
wait_until 120 '[ "$(READINESS)" = "HOT" ]'
snap path-rp2-prepared.yaml
K get recoverypath "$PATH_NAME" -o jsonpath='{.status.preparedRecoveryPoint}' | python3 -m json.tool > "$OUT/c-prepared-artifacts.json" 2>/dev/null || true
K get recoverypath "$PATH_NAME" -o jsonpath='{.status.targetPlacement}' | python3 -m json.tool > "$OUT/placement.json" 2>/dev/null || true
# every prepared artifact must now point at RP2, not RP1
ART_OK=$(python3 - "$OUT/c-prepared-artifacts.json" "$RP2" "$RP1" <<'PY'
import json,sys
try: d=json.load(open(sys.argv[1]))
except Exception: print("no"); raise SystemExit
rp2, rp1 = sys.argv[2], sys.argv[3]
arts = d.get("artifacts", [])
bad = [a for a in arts if rp1 in (a.get("ref") or "")]
img = [a for a in arts if a.get("kind") == "checkpointImage"]
print("yes" if arts and not bad and img and rp2 in img[0]["ref"] else "no")
PY
)
C_OBS="prepared=$(PREPARED) candidate=$(CANDIDATE) readiness=$(READINESS) artifactsPointAtRP2=$ART_OK"
echo "$C_OBS"
C_PASS=$([ "$(PREPARED)" = "$RP2" ] && [ -z "$(CANDIDATE)" ] && [ "$(READINESS)" = "HOT" ] && [ "$ART_OK" = "yes" ] && echo PASS || echo FAIL)
record "C" "prepared=RP2, candidate=none, HOT, artifacts identify RP2" "$C_OBS" "$C_PASS"
note "T_C_done=$(ts)"

# ===================================================================== G ====
# RTO before the destructive tests, while the path is cleanly HOT.
say "G. RTO contract"
EST=$(jp '{.status.contract.estimatedActivationLatency}')
note "estimated_activation_latency=$EST"
G1_OBS="rto=1m0s estimated=$EST within=$(jp '{.status.contract.rtoWithinContract}') readiness=$(READINESS)"
G1_PASS=$([ "$(jp '{.status.contract.rtoWithinContract}')" = "true" ] && [ "$(READINESS)" = "HOT" ] && echo PASS || echo FAIL)
record "G1" "RTO 60s > estimated: contract satisfied, HOT" "$G1_OBS" "$G1_PASS"

K patch recoverygroup "$GROUP" --type=merge -p '{"spec":{"recoveryContract":{"rto":"5s"}}}' >/dev/null
wait_until 90 '[ "$(CHECK EstimatedRTOWithinContract)" = "False" ]'
snap path-rto-exceeded.yaml
G2_OBS="rto=5s estimated=$(jp '{.status.contract.estimatedActivationLatency}') within=$(jp '{.status.contract.rtoWithinContract}') readiness=$(READINESS)"
echo "$G2_OBS"
G2_PASS=$([ "$(jp '{.status.contract.rtoWithinContract}')" = "false" ] && [ "$(READINESS)" != "HOT" ] && echo PASS || echo FAIL)
record "G2" "RTO 5s < estimated: contract violated, HOT lost" "$G2_OBS" "$G2_PASS"
{
  echo "estimated activation latency = $EST"
  echo "--- breakdown (status.contract.activationSteps) ---"
  K get recoverypath "$PATH_NAME" -o jsonpath='{.status.contract.activationSteps}' | python3 -m json.tool
  echo "--- RTO = 60s ---"; echo "$G1_OBS"
  echo "--- RTO =  5s ---"; echo "$G2_OBS"
} > "$OUT/rto-contract-test.txt" 2>&1
restore_contract
wait_until 90 '[ "$(READINESS)" = "HOT" ]'
note "T_G_done=$(ts)"

# ===================================================================== E ====
say "E. the prepared checkpoint image is removed from the placement node"
T_E0=$(date +%s)
K2 delete job ramp-imgrm -n "$NS" --ignore-not-found >/dev/null 2>&1
cat <<YAML | K2 apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: ramp-imgrm, namespace: ${NS}}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      nodeName: ${NODE}
      volumes: [{name: host, hostPath: {path: /, type: Directory}}]
      containers:
        - name: rm
          image: busybox:1.36
          securityContext: {privileged: true}
          volumeMounts: [{name: host, mountPath: /host}]
          command: ["/bin/sh","-c"]
          args: ["chroot /host ctr -n k8s.io images rm ${CKPT_IMAGE_2} && chroot /host ctr -n k8s.io images ls -q | grep -c checkpoint-video || true"]
YAML
K2 wait --for=condition=complete job/ramp-imgrm -n "$NS" --timeout=120s >/dev/null 2>&1
K2 logs job/ramp-imgrm -n "$NS" > "$OUT/e-image-removed.log" 2>&1 || true
wait_until 180 '[ "$(CHECK RestoreArtifactReady)" = "False" ]'
T_E1=$(date +%s)
# Wait for the probe to SETTLE on the definitive answer. "False because the
# probe has not finished" is the correct conservative verdict but it is not
# evidence that the missing image was detected; exit code 11 is.
wait_until 180 '[ "$(REASON RestoreArtifactReady)" = "CheckpointImageMissingOnNode" ]'
T_E2=$(date +%s)
snap path-restore-artifact-missing.yaml
E_OBS="RestoreArtifactReady=$(CHECK RestoreArtifactReady) reason=$(REASON RestoreArtifactReady) readiness=$(READINESS) hotLostIn=$((T_E1-T_E0))s definitiveIn=$((T_E2-T_E0))s"
echo "$E_OBS"
K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status']['checks']:
    if c['name']=='RestoreArtifactReady': print(c['reason']+': '+c['message'])" | tee "$OUT/e-reason.txt"
E_PASS=$([ "$(CHECK RestoreArtifactReady)" = "False" ] \
        && [ "$(REASON RestoreArtifactReady)" = "CheckpointImageMissingOnNode" ] \
        && [ "$(READINESS)" != "HOT" ] && echo PASS || echo FAIL)
record "E" "RestoreArtifactReady=False (image missing on the placement node), HOT lost" "$E_OBS" "$E_PASS"
note "T_E_hot_lost_seconds=$((T_E1-T_E0)) T_E_definitive_seconds=$((T_E2-T_E0))"

echo "--- rebuilding the image so the path recovers ---"
RP="$RP2" TARGET_NODE="$NODE" ./35-build-checkpoint-image.sh > "$OUT/e-rebuild.log" 2>&1 || true
wait_until 240 '[ "$(READINESS)" = "HOT" ]'
note "T_E_done=$(ts) readiness_after_rebuild=$(READINESS)"

# ===================================================================== F ====
say "F. the placement node becomes unavailable"
K2 cordon "$NODE" >/dev/null
wait_until 120 '[ "$(CHECK TargetPlacementFeasible)" = "False" ]'
snap path-placement-unavailable.yaml
F_OBS="TargetPlacementFeasible=$(CHECK TargetPlacementFeasible) readiness=$(READINESS)"
echo "$F_OBS"
K get recoverypath "$PATH_NAME" -o json | python3 -c "
import json,sys
for c in json.load(sys.stdin)['status']['checks']:
    if c['name']=='TargetPlacementFeasible': print(c['reason']+': '+c['message'])" | tee "$OUT/f-reason.txt"
F_PASS=$([ "$(CHECK TargetPlacementFeasible)" = "False" ] && [ "$(READINESS)" != "HOT" ] && echo PASS || echo FAIL)
record "F" "TargetPlacementFeasible=False, HOT lost with an explicit reason" "$F_OBS" "$F_PASS"
K2 uncordon "$NODE" >/dev/null
wait_until 240 '[ "$(READINESS)" = "HOT" ]'
note "T_F_done=$(ts) readiness_after_uncordon=$(READINESS)"

# ===================================================================== D ====
# Last, because it is the only test that needs the prepared point to age past a
# deadline and therefore ends with the path deliberately not HOT.
say "D. the prepared RecoveryPoint outlives the RPO"
AGE_NOW=$(K get recoverypoint "$RP2" -o json | python3 -c "
import json,sys,datetime
t=json.load(sys.stdin)['status']['timings']['commitTime']
t=datetime.datetime.strptime(t,'%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=datetime.timezone.utc)
print(int((datetime.datetime.now(datetime.timezone.utc)-t).total_seconds()))")
RPO_S=$((AGE_NOW + 20))
note "D_rpo_seconds=$RPO_S D_age_at_start=$AGE_NOW"
K patch recoverygroup "$GROUP" --type=merge -p "{\"spec\":{\"recoveryContract\":{\"rpo\":\"${RPO_S}s\"}}}" >/dev/null
{
  echo "prepared RecoveryPoint : $RP2"
  echo "RPO set to             : ${RPO_S}s"
  echo
  printf '%-22s %-10s %-12s %-9s %s\n' "observed at" "age" "freshEnough" "readiness" "remaining"
} > "$OUT/rpo-contract-test.txt"
FRESH_SEEN_TRUE=no; T_STALE=""
for _ in $(seq 1 45); do
  A=$(jp '{.status.contract.recoveryPointAge}'); F=$(CHECK RecoveryPointFreshEnough)
  R=$(READINESS); REM=$(jp '{.status.contract.freshnessRemaining}')
  printf '%-22s %-10s %-12s %-9s %s\n' "$(date -Is)" "$A" "$F" "$R" "$REM" >> "$OUT/rpo-contract-test.txt"
  [ "$F" = "True" ] && FRESH_SEEN_TRUE=yes
  if [ "$F" = "False" ] && [ "$FRESH_SEEN_TRUE" = "yes" ]; then T_STALE=$(date -Is); break; fi
  sleep 2
done
snap path-rpo-expired.yaml
D_OBS="freshEnough=$(CHECK RecoveryPointFreshEnough) age=$(jp '{.status.contract.recoveryPointAge}') rpo=${RPO_S}s readiness=$(READINESS) transitionAt=${T_STALE:-<not observed>}"
echo "$D_OBS"
D_PASS=$([ "$FRESH_SEEN_TRUE" = "yes" ] && [ "$(CHECK RecoveryPointFreshEnough)" = "False" ] && [ "$(READINESS)" != "HOT" ] && echo PASS || echo FAIL)
record "D" "FreshEnough True -> False at the RPO deadline, HOT lost" "$D_OBS" "$D_PASS"
note "T_D_stale_transition=${T_STALE:-none}"
{ echo; echo "transition observed at: ${T_STALE:-<not observed>}"; echo "verdict: $D_PASS"; } >> "$OUT/rpo-contract-test.txt"
restore_contract
wait_until 120 '[ "$(READINESS)" = "HOT" ]'
note "T_D_done=$(ts) readiness_after_contract_restored=$(READINESS)"

# ================================================================= report ===
say "summary"
{
  echo "| Test | Expected | Observed | Pass |"
  echo "|------|----------|----------|------|"
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r id exp obs v <<<"$r"
    echo "| $id | $exp | \`$obs\` | **$v** |"
  done
  echo
  echo "RP1 = $RP1"
  echo "RP2 = $RP2"
  echo "placement node = $NODE"
  echo "checkpoint image (RP1) = $CKPT_IMAGE_1"
  echo "checkpoint image (RP2) = $CKPT_IMAGE_2"
} | tee "$OUT/result-summary.md"

tail -600 "$ROOT/evidence/logs/ramp-manager.log" > "$OUT/controller.log" || true
snap path-final.yaml
echo; echo "evidence -> $OUT"
for r in "${RESULTS[@]}"; do case "$r" in *\|FAIL) exit 1 ;; esac; done
exit 0
