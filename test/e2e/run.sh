#!/usr/bin/env bash
# Checks tiers with real pods and fake GPUs. Run it against a cluster made by
# test/smoke/run.sh with KEEP=1: it rebuilds the scheduler there first.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=${CLUSTER:-ukai-dev}
NS=kai-scheduler
IMAGE=ghcr.io/kai-scheduler/kai-scheduler/ukai-scheduler:smoke
WORK=$PWD/bin/smoke
export KUBECONFIG=$WORK/kubeconfig # never the user's own
[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }

arch=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$WORK/image/scheduler" ./cmd/scheduler
docker build -q -t "$IMAGE" -f Dockerfile "$WORK/image"
kind load docker-image "$IMAGE" --name "$CLUSTER"
helm upgrade kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
  --version v0.18.9 -n $NS -f test/smoke/values.yaml --wait --timeout 5m >/dev/null
sleep 5 # the operator updates the scheduler's configuration
kubectl -n $NS rollout restart deployment/kai-scheduler-default
kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s

gpus() { # node count
  kubectl patch node "$1" --subresource=status --type=merge \
    -p "{\"status\":{\"capacity\":{\"nvidia.com/gpu\":\"$2\"},\"allocatable\":{\"nvidia.com/gpu\":\"$2\"}}}" >/dev/null
}
gpus "$CLUSTER-worker" 4
gpus "$CLUSTER-worker2" 0
for c in test-top:90 test-high:75 test-mid:50 test-low:25; do
  kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
done

cleanup() { kubectl delete jobs -l ukai-test --wait=true --timeout=90s >/dev/null 2>&1 || true; }
trap cleanup EXIT

submit() { # name owner class gpus
  kubectl create -f - >/dev/null <<Y
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {ukai-test: "1", kai.scheduler/queue: default-queue, example.com/owner: $2}}
spec:
  template:
    metadata: {labels: {ukai-test: "1", kai.scheduler/queue: default-queue}}
    spec:
      schedulerName: kai-scheduler
      priorityClassName: test-$3
      restartPolicy: Never
      terminationGracePeriodSeconds: 1
      automountServiceAccountToken: false
      containers:
        - name: main
          # Must fail when evicted: a Job whose pod exits 0 is complete.
          image: busybox:1.37
          command: [sh, -c, "trap 'exit 1' TERM; while :; do sleep 1; done"]
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {nvidia.com/gpu: $4}}
Y
  sleep 2 # distinct creation times
}
phase() { kubectl get pods -l "job-name=$1" -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{.status.phase}{"\n"}{end}' | grep -E '^(Running|Pending)$' | sort | head -1; }
uid() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].metadata.uid}'; }
expect() { # name phase: reached within 60 s, and kept for 8 s
  for _ in $(seq 30); do [ "$(phase "$1")" = "$2" ] && break; sleep 2; done
  sleep 8
  [ "$(phase "$1")" = "$2" ] || { echo "FAIL: $1 is $(phase "$1"), want $2"; kubectl get pods; kubectl -n $NS logs deployment/kai-scheduler-default | grep tierbudget | tail -20; exit 1; }
  echo "  ok: $1 $2"
}

cleanup
echo "1. A P1 over its owner's budget is held, with a reason, while GPUs are free"
submit a1 alice high 2; expect a1 Running
submit a2 alice high 1; expect a2 Pending
events=$(kubectl get events -o jsonpath='{range .items[*]}{.message}{"\n"}{end}')
grep -q 'owner budget: needs 1 units, using 2 of 2' <<<"$events" || { echo "FAIL: no reason in events"; exit 1; }
echo "  ok: reason is in the events"
echo "2. Another owner is not affected"
submit b1 bob high 2; expect b1 Running
echo "3. The held P1 starts when the budget frees up"
kubectl delete job a1 >/dev/null; expect a2 Running

cleanup
echo "4. A promoted P2 is safe from another owner's P1"
submit p alice mid 2; expect p Running
submit f carol top 2; expect f Running
submit b1 bob high 2; expect b1 Pending; expect p Running
u=$(uid p)
echo "5. Its owner's P1 demotes it and takes its place"
kubectl delete job b1 >/dev/null
submit a1 alice high 2; expect a1 Running; expect p Pending
[ "$(uid p)" != "$u" ] || { echo "FAIL: p was not evicted"; exit 1; }

cleanup
echo "6. Excess P2 gives way to a normal P2, and does not take its place back"
submit x alice mid 3; expect x Running # over budget and allowance: excess
u=$(uid x)
submit x2 carol mid 3; expect x2 Pending # excess does not evict excess
[ "$(uid x)" = "$u" ] || { echo "FAIL: x was restarted"; exit 1; }
kubectl delete job x2 >/dev/null
submit b bob mid 2; expect b Running; expect x Pending
u=$(uid b); sleep 20
[ "$(uid b)" = "$u" ] || { echo "FAIL: b was restarted"; exit 1; }
expect b Running

cleanup
echo "7. P0 evicts P1 and ignores budgets"
submit a1 alice high 2; expect a1 Running
submit t alice top 4; expect t Running; expect a1 Pending

node() { kubectl get pods -l "job-name=$1" --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}'; }
# 2 of 4 GPUs are free on each node, so a job of 4 fits only if one of two moves.
fragment() { # class
  cleanup
  gpus "$CLUSTER-worker" 2; gpus "$CLUSTER-worker2" 0
  submit m1 alice "$1" 2; expect m1 Running
  gpus "$CLUSTER-worker2" 2; gpus "$CLUSTER-worker" 2
  submit m2 bob "$1" 2; expect m2 Running
  gpus "$CLUSTER-worker" 4; gpus "$CLUSTER-worker2" 4
  [ "$(node m1)" != "$(node m2)" ] || { echo "FAIL: not fragmented"; exit 1; }
  u=$(uid m1)$(uid m2)
}
echo "8. Consolidation for a P3 does not move P1"
fragment high
submit w carol low 4; expect w Pending; sleep 10
[ "$(uid m1)$(uid m2)" = "$u" ] || { echo "FAIL: a P1 was moved"; exit 1; }
echo "  ok: both P1 untouched"
echo "9. Consolidation for a P3 moves a P3"
fragment low
submit w carol low 4; expect w Running; expect m1 Running; expect m2 Running
[ "$(uid m1)$(uid m2)" != "$u" ] || { echo "FAIL: nothing was moved"; exit 1; }
echo "  ok: one was moved"

kubectl -n $NS logs deployment/kai-scheduler-default --tail=-1 >"$WORK/scheduler.log"
if grep -E 'Plugin tierbudget.*(panic|is disabled)' "$WORK/scheduler.log"; then echo "FAIL: errors logged"; exit 1; fi
[ "$(kubectl -n $NS get pods -l app=kai-scheduler-default -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}')" = 0 ] || { echo "FAIL: restarts"; exit 1; }
echo PASS
