#!/usr/bin/env bash
# Breaks things on a cluster made by test/smoke/run.sh with KEEP=1. Throughout, samples that no owner's P1 pods hold
# more than the budget, and at the end that nothing panicked.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=${CLUSTER:-ukai-dev}
NS=kai-scheduler
WORK=$PWD/bin/smoke
export KUBECONFIG=$WORK/kubeconfig # never the user's own
[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }
BUDGET=2
fail() {
  echo "FAIL: $*" >&2
  kubectl get pods -o wide >&2 || true
  kubectl -n $NS get pods >&2 || true
  kubectl -n $NS logs -l app=kai-scheduler-default --tail=-1 2>/dev/null | grep tierbudget | tail -15 >&2 || true
  exit 1
}
ok() { echo "  ok: $*"; }

gpus() { # node count
  kubectl patch node "$1" --subresource=status --type=merge \
    -p "{\"status\":{\"capacity\":{\"nvidia.com/gpu\":\"$2\"},\"allocatable\":{\"nvidia.com/gpu\":\"$2\"}}}" >/dev/null
}
manifest() { # name owner class gpus [command]
  cat <<Y
---
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {ukai-test: "1", kai.scheduler/queue: default-queue, example.com/owner: $2}}
spec:
  backoffLimit: 0
  podFailurePolicy: {rules: [{action: Ignore, onPodConditions: [{type: DisruptionTarget}]}]}
  template:
    metadata: {labels: {ukai-test: "1", owner: $2, example.com/owner: $2, class: $3, kai.scheduler/queue: default-queue}}
    spec:
      schedulerName: kai-scheduler
      priorityClassName: test-$3
      restartPolicy: Never
      terminationGracePeriodSeconds: 1
      automountServiceAccountToken: false
      containers:
        - name: main
          image: busybox:1.37
          command: [sh, -c, "${5:-trap 'exit 1' TERM; while :; do sleep 1; done}"]
          resources: {requests: {cpu: 10m, memory: 8Mi}, limits: {nvidia.com/gpu: $4}}
Y
}
submit() { manifest "$@" | kubectl create -f - >/dev/null; sleep 2; }
pod() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].metadata.name}'; }
uid() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].metadata.uid}'; }
group() { kubectl get podgroups -o json | jq -r --arg j "$1" '.items[] | select(.metadata.ownerReferences[0].name == $j) | .metadata.name'; }
phase() { kubectl get pods -l "job-name=$1" -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{.status.phase}{"\n"}{end}' | grep -E '^(Running|Pending)$' | sort | head -1; }
expect() { # name phase [seconds]: reached in time, and kept for 8 s
  for _ in $(seq $((${3:-60} / 2))); do [ "$(phase "$1")" = "$2" ] && break; sleep 2; done
  sleep 8
  [ "$(phase "$1")" = "$2" ] || fail "$1 is '$(phase "$1")', want $2"
  ok "$1 $2"
}
scheduler() { kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null; }
# Owners whose bound and unfinished P1 pods hold more than the budget.
violations() {
  jq -c --argjson b $BUDGET '[.items[] | select(.metadata.labels.class == "high" and .spec.nodeName != null
      and (.status.phase == "Pending" or .status.phase == "Running"))]
    | group_by(.metadata.labels.owner)[]
    | {owner: .[0].metadata.labels.owner, units: (map(.spec.containers[0].resources.limits["nvidia.com/gpu"] | tonumber) | add), pods: map(.metadata.name)}
    | select(.units > $b)'
}
samples=$WORK/samples violated=$WORK/violations
: >"$samples"; : >"$violated"
watch_budget() { while :; do if p=$(kubectl get pods -l ukai-test -o json --request-timeout=3s 2>/dev/null); then violations <<<"$p" >>"$violated"; echo >>"$samples"; fi; sleep 0.5; done; }
clean() { kubectl delete jobs -l ukai-test --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl delete pods -l ukai-test --force --grace-period=0 >/dev/null 2>&1 || true; }
finish() {
  set +e
  [ -z "${watcher:-}" ] || kill "$watcher" 2>/dev/null
  docker unpause "$CLUSTER-control-plane" >/dev/null 2>&1
  docker start "$CLUSTER-worker2" >/dev/null 2>&1
  for p in $(kubectl get pods -l ukai-test -o name); do kubectl patch "$p" --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null 2>&1; done
  clean
}
trap finish EXIT
clean
gpus "$CLUSTER-worker" 8; gpus "$CLUSTER-worker2" 0
for c in test-top:90 test-high:75 test-mid:50 test-low:25; do
  kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
done
watch_budget & watcher=$!

echo "1. Fourteen P1 of one owner at once"
for i in $(seq 14); do manifest "b$i" alice high 1; done | kubectl create -f - >/dev/null
for _ in $(seq 30); do [ "$(kubectl get pods -l owner=alice --field-selector=status.phase=Running --no-headers | wc -l | tr -d ' ')" = 2 ] && break; sleep 2; done
sleep 8
[ "$(kubectl get pods -l owner=alice --field-selector=status.phase=Running --no-headers | wc -l | tr -d ' ')" = 2 ] || fail "not two"
ok "two run"

echo "2. The scheduler is killed repeatedly while jobs come and go"
for i in 1 2 3 4 5 6 7 8; do
  kubectl -n $NS delete pod -l app=kai-scheduler-default --force --grace-period=0 >/dev/null 2>&1
  j=$(kubectl get pods -l owner=alice --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.labels.job-name}')
  kubectl delete job "$j" --wait=false >/dev/null
  sleep 6
done
scheduler; sleep 10
[ "$(kubectl get pods -l owner=alice --field-selector=status.phase=Running --no-headers | wc -l | tr -d ' ')" = 2 ] || fail "not two"
ok "two run"
clean

echo "3. Budget frees up when a job ends, fails, or its pod is force-deleted"
submit done1 alice high 2 'sleep 25'; expect done1 Running
submit n1 alice high 2 'sleep 30; exit 1'
[ "$(phase n1)" = Pending ] || fail 'n1 is not held'; expect n1 Running
submit n2 alice high 2; expect n2 Running
submit n3 alice high 2; expect n3 Pending
kubectl delete job n2 --cascade=orphan >/dev/null
kubectl delete pod -l job-name=n2 --force --grace-period=0 >/dev/null 2>&1
expect n3 Running
clean

echo "4. A pod that does not finish terminating still counts"
submit s1 alice high 2; expect s1 Running
p=$(pod s1)
kubectl patch pod "$p" --type=json -p '[{"op":"add","path":"/metadata/finalizers/-","value":"example.com/stuck"}]' >/dev/null
submit s2 alice high 1
kubectl delete job s1 --cascade=orphan >/dev/null; kubectl delete pod "$p" --wait=false >/dev/null
sleep 12
stuck=$(kubectl get pod "$p" -o jsonpath='{.status.phase}')
echo "     its phase is $stuck, s2 is $(phase s2)"
case $stuck in Running) expect s2 Pending ;; *) expect s2 Running ;; esac # once its containers are gone, so is its use
kubectl patch pod "$p" --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null 2>&1 || true
expect s2 Running
clean

echo "   Nor does deleting a Job free budget before its pods are gone"
manifest g1 alice high 2 "trap '' TERM; while :; do sleep 1; done" | sed 's/GracePeriodSeconds: 1$/GracePeriodSeconds: 45/' | kubectl create -f - >/dev/null
expect g1 Running
kubectl delete job g1 --wait=false >/dev/null
submit g2 alice high 1; sleep 8
[ "$(kubectl get pods -l job-name=g1 --no-headers | wc -l | tr -d ' ')" = 1 ] || fail "g1 is gone already"
[ -z "$(group g1)" ] || fail "g1 still has its PodGroup, so this tests nothing"
[ "$(phase g2)" = Pending ] || fail "g2 is $(phase g2)"
ok "g2 Pending"; expect g2 Running 90
clean

echo "5. Labels change while jobs run"
submit l1 alice high 2; expect l1 Running
submit l2 bob high 2; expect l2 Running
u=$(uid l1)$(uid l2)
kubectl label podgroup "$(group l1)" example.com/owner=bob --overwrite >/dev/null # now bob is over
sleep 10
[ "$(uid l1)$(uid l2)" = "$u" ] || fail "restarted"
ok "an owner that is over budget keeps its jobs"
submit l3 bob high 1; expect l3 Pending
kubectl label podgroup "$(group l1)" example.com/owner- >/dev/null # no owner
sleep 10
[ "$(uid l1)$(uid l2)" = "$u" ] || fail "restarted"
echo "     the label is now '$(kubectl get podgroup "$(group l1)" -o jsonpath='{.metadata.labels.example\.com/owner}')'"
kubectl label podgroup "$(group l3)" example.com/owner- >/dev/null
expect l3 Pending
ok "unowned is held"
clean

echo "6. A priority class is deleted while in use"
submit c1 alice high 2; expect c1 Running
u=$(uid c1)
kubectl get priorityclass test-high -o json | jq 'del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp)' >"$WORK/pc.json"
kubectl delete priorityclass test-high >/dev/null; sleep 10
[ "$(uid c1)" = "$u" ] || fail "restarted"
kubectl create -f "$WORK/pc.json" >/dev/null
submit c2 alice high 1; expect c2 Pending
[ "$(uid c1)" = "$u" ] || fail "restarted"
clean

echo "7. The API server is gone for 60 s"
submit o1 alice high 2; submit o2 carol mid 2; expect o1 Running; expect o2 Running
submit o3 alice high 1; expect o3 Pending
u=$(uid o1)$(uid o2)
docker pause "$CLUSTER-control-plane" >/dev/null; sleep 60; docker unpause "$CLUSTER-control-plane" >/dev/null
for _ in $(seq 60); do kubectl get nodes --request-timeout=3s >/dev/null 2>&1 && break; sleep 2; done
kubectl wait --for=condition=Ready node --all --timeout=120s >/dev/null
gpus "$CLUSTER-worker" 8 # the kubelet drops it when it registers again
sleep 15
[ "$(uid o1)$(uid o2)" = "$u" ] || fail "restarted"
scheduler; expect o3 Pending; ok "running jobs are untouched"
kubectl delete job o1 >/dev/null; expect o3 Running 120
clean

echo "8. A node is lost"
# Not the node of KAI's own pods: a lost pod-grouper would hide what this tests.
kubectl cordon "$CLUSTER-worker2" >/dev/null
kubectl -n $NS delete pods --all >/dev/null
for d in $(kubectl -n $NS get deployments -o name); do kubectl -n $NS rollout status "$d" --timeout=180s >/dev/null; done
kubectl uncordon "$CLUSTER-worker2" >/dev/null
gpus "$CLUSTER-worker" 4; gpus "$CLUSTER-worker2" 4
kubectl cordon "$CLUSTER-worker" >/dev/null
submit d1 alice high 2; submit d2 bob mid 2; expect d1 Running; expect d2 Running
kubectl uncordon "$CLUSTER-worker" >/dev/null
[ "$(kubectl get pod "$(pod d1)" -o jsonpath='{.spec.nodeName}')" = "$CLUSTER-worker2" ] || fail "d1 is elsewhere"
submit d3 alice high 1; expect d3 Pending
docker stop -t 1 "$CLUSTER-worker2" >/dev/null
kubectl wait --for=condition=Ready=Unknown node "$CLUSTER-worker2" --timeout=120s >/dev/null
submit d4 carol high 2; expect d4 Running 90
ok "others are still placed"
kubectl delete pod -l 'job-name in (d1,d2)' --force --wait=false --grace-period=0 >/dev/null 2>&1 # as an admin would
# The Job's finalizer keeps them until they are known to have ended, so they still count.
sleep 8; [ "$(phase d3)" = Pending ] || fail "d3 is $(phase d3)"; ok "its pods still count"
for p in $(kubectl get pods -l 'job-name in (d1,d2)' --field-selector spec.nodeName="$CLUSTER-worker2" -o name); do
  kubectl patch "$p" --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null
done
sleep 25
echo "     without them: d1 '$(phase d1)', d3 '$(phase d3)'"
docker start "$CLUSTER-worker2" >/dev/null
kubectl wait --for=condition=Ready node "$CLUSTER-worker2" --timeout=180s >/dev/null
gpus "$CLUSTER-worker2" 4
submit d5 dave high 2; expect d5 Running 120
clean

echo "9. The leader fails over"
image=$(kubectl -n $NS get deployment kai-scheduler-default -o jsonpath='{.spec.template.spec.containers[0].image}')
kubectl -n $NS patch deployment kai-scheduler-default --type=json -p '[{"op":"test","path":"/spec/template/spec/containers/0/image","value":"'"$image"'"}]' >/dev/null
kubectl -n $NS scale deployment/kai-operator --replicas=0 >/dev/null # or it reverts this
kubectl -n $NS wait --for=delete pod -l app=kai-operator --timeout=60s >/dev/null 2>&1 || true
kubectl -n $NS patch deployment kai-scheduler-default --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--leader-elect=true"}]' >/dev/null
kubectl -n $NS scale deployment/kai-scheduler-default --replicas=2 >/dev/null
scheduler
gpus "$CLUSTER-worker" 8; gpus "$CLUSTER-worker2" 0
submit f1 alice high 2; expect f1 Running 90
submit f2 alice high 1; expect f2 Pending
submit p1 bob mid 2; expect p1 Running
u=$(uid f1)$(uid p1)
holder() { kubectl -n $NS get leases -o json | jq -r '[.items[] | select(.metadata.name | test("kai-scheduler")) | select(.spec.holderIdentity | test("kai-scheduler-default"))][0].spec.holderIdentity | split("_")[0]'; }
old=$(holder)
kubectl -n $NS get pod "$old" >/dev/null || fail "no leader: $old"
kubectl -n $NS delete pod "$old" --force --grace-period=0 >/dev/null 2>&1
for _ in $(seq 30); do h=$(holder); [ "$h" != "$old" ] && [ "$h" != null ] && break; sleep 2; done
[ "$(holder)" != "$old" ] || fail "no failover"
ok "$old -> $(holder)"
expect f2 Pending; submit f3 carol high 1; expect f3 Running
[ "$(uid f1)$(uid p1)" = "$u" ] || fail "restarted"
ok "running jobs are untouched"
kubectl -n $NS scale deployment/kai-operator --replicas=1 >/dev/null
kubectl -n $NS rollout status deployment/kai-operator --timeout=120s >/dev/null
for _ in $(seq 60); do kubectl -n $NS get deployment kai-scheduler-default -o json | grep -q leader-elect || break; sleep 2; done
scheduler
clean

n=$(wc -l <"$samples" | tr -d ' ')
[ "$n" -gt 200 ] || fail "only $n samples"
kubectl -n $NS logs -l app=kai-scheduler-default --tail=-1 | grep -E 'Plugin tierbudget.*(panic|is disabled)' && fail "errors logged"
[ ! -s "$violated" ] || { sort "$violated" | uniq -c; fail "over budget"; }
ok "never over budget in $n samples"
echo PASS
