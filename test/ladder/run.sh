#!/usr/bin/env bash
# Checks the ladder scheme with real pods and fake GPUs. Run it against a cluster made
# by test/smoke/run.sh with KEEP=1: it rebuilds the scheduler there first, and puts the
# budget back at the end.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=${CLUSTER:-ukai-dev}
NS=kai-scheduler
IMAGE=ghcr.io/kai-scheduler/kai-scheduler/ukai-scheduler:smoke
WORK=${WORK:-$PWD/bin/smoke}
export KUBECONFIG=$WORK/kubeconfig # never the user's own
[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }

arch=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$WORK/image/scheduler" ./cmd/scheduler
docker build -q -t "$IMAGE" -f Dockerfile "$WORK/image"
kind load docker-image "$IMAGE" --name "$CLUSTER"

chart=$(mktemp -d)
helm pull oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler --version v0.18.9 -d "$chart"
deploy() { # values
  helm upgrade kai-scheduler "$chart"/*.tgz -n $NS -f "$1" --wait --timeout 5m >/dev/null
  sleep 5 # the operator updates the scheduler's configuration
  kubectl -n $NS rollout restart deployment/kai-scheduler-default >/dev/null
  kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null
}
deploy test/ladder/values.yaml

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
# KAI starts a job that may not be evicted only within its queue's deserved quota, and
# that of every parent. The default queues have none.
kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: test-parent}
spec: {resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: -1, limit: -1, overQuotaWeight: 1}}}
---
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: test-protect}
spec: {parentQueue: test-parent, resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: 4, limit: -1, overQuotaWeight: 1}}}
Y

cleanup() { kubectl delete jobs -l ukai-test --wait=true --timeout=90s >/dev/null 2>&1 || true; }
pids=()
finish() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  cleanup
  kubectl delete queues.scheduling.run.ai test-protect test-parent >/dev/null 2>&1 || true
  deploy test/smoke/values.yaml # the budget again, for the other tests
}
trap finish EXIT

(while :; do kubectl -n $NS port-forward deployment/kai-scheduler-default 18080:8080 18081:8081 & pf=$!; trap "kill $pf; exit" TERM; wait $pf || true; sleep 1; done) >"$WORK/port-forward.log" 2>&1 & pids+=($!)
for _ in $(seq 30); do curl -sf localhost:18081/tiers >/dev/null && break; sleep 1; done

submit() { # name user class: GPUS, QUEUE and LABELS (as "k: v, k: v") may be set
  kubectl create -f - >/dev/null <<Y
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {ukai-test: "1", kai.scheduler/queue: ${QUEUE:-default-queue}, example.com/user: $2, ${LABELS:-}}}
spec:
  template:
    metadata: {labels: {ukai-test: "1", kai.scheduler/queue: ${QUEUE:-default-queue}}}
    spec:
      schedulerName: kai-scheduler
      priorityClassName: test-$3
      restartPolicy: Never
      terminationGracePeriodSeconds: 1
      automountServiceAccountToken: false
      containers:
        - name: main
          image: busybox:1.37
          command: [sh, -c, "trap 'exit 1' TERM; while :; do sleep 1; done"]
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {nvidia.com/gpu: ${GPUS:-1}}}
Y
  sleep 2 # distinct creation times
}
die() { echo "FAIL: $*"; kubectl get pods -A | grep -vE 'kube-system|local-path'; kubectl -n $NS logs deployment/kai-scheduler-default | grep tierbudget | tail -20; exit 1; }
phase() { kubectl get pods -l "job-name=$1" -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{.status.phase}{"\n"}{end}' | grep -E '^(Running|Pending)$' | sort | head -1; }
uid() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].metadata.uid}'; }
expect() { # name phase: reached within 60 s, and kept for 8 s
  for _ in $(seq 30); do [ "$(phase "$1")" = "$2" ] && break; sleep 2; done
  sleep 8
  [ "$(phase "$1")" = "$2" ] || die "$1 is $(phase "$1"), want $2"
  echo "  ok: $1 $2"
}
tiers() { curl -sSf --retry 8 --retry-delay 2 --retry-all-errors localhost:18081/tiers; }
job() { # name field
  tiers | jq -r --arg n "$1" --arg f "$2" '.jobs[] | select(.name | test("^(pg-)?" + $n + "-")) | .[$f] // ""'
}
priority() { # name want
  local got
  for _ in $(seq 10); do got=$(job "$1" priority); [ "$got" = "$2" ] && break; sleep 2; done
  [ "$got" = "$2" ] || die "$1 has priority '$got', want $2"
  echo "  ok: $1 at $2"
}

cleanup
echo "1. Priority falls step by step"
for a in a1 a2 a3 a4; do submit $a alice mid; done
for a in a1 a2 a3 a4; do expect $a Running; done
priority a1 75; priority a2 75; priority a3 70; priority a4 40
[ "$(tiers | jq -r .scheme)" = ladder ] || die "scheme is not ladder"

echo "2. Another user's first job evicts the floor, across the gap"
u4=$(uid a4); u3=$(uid a3)
submit b1 bob mid; expect b1 Running; expect a4 Pending
[ "$(uid a4)" != "$u4" ] || die "a4 was not evicted"
priority b1 75

echo "3. ... but not a job one step below, within the gap"
submit b2 bob mid; expect b2 Pending; expect a3 Running
[ "$(uid a3)" = "$u3" ] || die "a3 was evicted"
kubectl delete job b2 >/dev/null

echo "4. A chosen level goes first and pushes its user's other jobs down"
LABELS="example.com/level: \"75\"" submit l1 alice mid; expect l1 Running; expect a3 Pending
priority l1 75; priority a1 75; priority a2 70; priority a3 40
[ "$(uid a3)" != "$u3" ] || die "a3 was not evicted"

echo "5. A level that is full waits, with a reason"
GPUS=2 LABELS="example.com/level: \"75\"" submit l2 alice mid; expect l2 Pending
reason=$(job l2 reason)
[ "$reason" = "level 75: needs 2 units, using 1 of 2" ] || die "l2: '$reason'"
echo "  ok: $reason"

cleanup
echo "6. A protected job on a queue without deserved quota does not start: KAI's rule"
LABELS="example.com/protect: \"true\"" submit p0 alice mid; expect p0 Pending
[ "$(job p0 protected)" = true ] || die "p0 is not protected"
events=$(kubectl get events -o jsonpath='{range .items[*]}{.message}{"\n"}{end}')
grep -qi 'non-preemptible' <<<"$events" || die "no reason in events"
echo "  ok: $(grep -i 'non-preemptible' <<<"$events" | tail -1 | cut -c1-120)"
kubectl delete job p0 >/dev/null

echo "7. On a queue with quota, it starts and is not evicted, even for a higher class"
QUEUE=test-protect LABELS="example.com/protect: \"true\"" submit p1 alice mid; expect p1 Running
up=$(uid p1)
QUEUE=test-protect GPUS=4 submit t carol top; expect t Pending; expect p1 Running
[ "$(uid p1)" = "$up" ] || die "p1 was evicted"
[ "$(job p1 protected)" = true ] || die "p1 is not shown as protected"
echo "8. Without protection, the same job is evicted"
cleanup
QUEUE=test-protect submit p2 alice mid; expect p2 Running
QUEUE=test-protect GPUS=4 submit t carol top; expect t Running; expect p2 Pending

kubectl -n $NS logs deployment/kai-scheduler-default --tail=-1 >"$WORK/scheduler.log"
if grep -E 'Plugin tierbudget.*(panic|is disabled)' "$WORK/scheduler.log"; then die "errors logged"; fi
[ "$(kubectl -n $NS get pods -l app=kai-scheduler-default -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}')" = 0 ] || die "restarts"
echo PASS
