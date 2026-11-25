#!/usr/bin/env bash
# Checks budgets with GPU weights, shared GPUs and several queues. Run it against a
# cluster made by test/smoke/run.sh with KEEP=1: it rebuilds the scheduler there first.
# GPUs are fake, so this supplies the device that KAI's reservation pods would read.
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
deploy() { # further helm flags
  helm upgrade kai-scheduler "$chart"/*.tgz -n $NS -f test/smoke/values.yaml "$@" --wait --timeout 5m >/dev/null
  sleep 5 # the operator updates the scheduler's configuration
  kubectl -n $NS rollout restart deployment/kai-scheduler-default >/dev/null
  kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null
}
A=scheduler.plugins.tierbudget.arguments
deploy --set-string "$A.gpuTypeLabel=gpu-type" --set-string "$A.gpuWeights=small=1\,big=2" --set-string "$A.defaultGPUWeight=2"

gpus() { # node count
  kubectl patch node "$1" --subresource=status --type=merge \
    -p "{\"status\":{\"capacity\":{\"nvidia.com/gpu\":\"$2\"},\"allocatable\":{\"nvidia.com/gpu\":\"$2\"}}}" >/dev/null
}
gpus "$CLUSTER-worker" 4; gpus "$CLUSTER-worker2" 4
kubectl label node "$CLUSTER-worker" gpu-type=small nvidia.com/gpu.memory=10000 --overwrite >/dev/null
kubectl label node "$CLUSTER-worker2" gpu-type=big nvidia.com/gpu.memory=20000 --overwrite >/dev/null
for c in test-top:90 test-high:75 test-mid:50 test-low:25; do
  kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
done
kubectl apply -f - >/dev/null <<Y
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: nvidia}
handler: runc
---
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: test-a}
spec: {parentQueue: default-parent-queue, resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: 4, limit: -1, overQuotaWeight: 1}}}
---
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: test-b}
spec: {parentQueue: default-parent-queue, resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: 4, limit: -1, overQuotaWeight: 1}}}
---
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: test-limited}
spec: {parentQueue: default-parent-queue, resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: 0, limit: 1, overQuotaWeight: 1}}}
Y

cleanup() { kubectl delete jobs -l ukai-test --wait=true --timeout=90s >/dev/null 2>&1 || true; }
pids=()
finish() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  cleanup
  kubectl delete queues.scheduling.run.ai test-a test-b test-limited >/dev/null 2>&1 || true
  kubectl label node "$CLUSTER-worker" "$CLUSTER-worker2" gpu-type- nvidia.com/gpu.memory- >/dev/null 2>&1 || true
}
trap finish EXIT

# A reservation pod reports the GPU it got. Here there is none to report.
while :; do
  kubectl -n kai-resource-reservation get pods -o jsonpath='{range .items[?(@.spec.nodeName)]}{.metadata.name} {.metadata.annotations.run\.ai/reserve_for_gpu_index}{"\n"}{end}' 2>/dev/null |
    while read -r pod index; do
      [ -n "$index" ] || kubectl -n kai-resource-reservation annotate pod "$pod" "run.ai/reserve_for_gpu_index=GPU-$pod" >/dev/null 2>&1 || true
    done
  sleep 0.5
done & pids+=($!)
(while :; do kubectl -n $NS port-forward deployment/kai-scheduler-default 18080:8080 18081:8081 & pf=$!; trap "kill $pf; exit" TERM; wait $pf || true; sleep 1; done) >"$WORK/port-forward.log" 2>&1 & pids+=($!)
for _ in $(seq 30); do curl -sf localhost:18081/tiers >/dev/null && break; sleep 1; done

submit() { # name user class: GPUS, TYPE, QUEUE and ANNOTATIONS may be set
  local limits="" selector="" queue=${QUEUE:-default-queue}
  if [ "${GPUS:-1}" != 0 ]; then limits="nvidia.com/gpu: ${GPUS:-1}"; fi
  if [ -n "${TYPE:-}" ]; then selector="gpu-type: $TYPE"; fi
  kubectl create -f - >/dev/null <<Y
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {ukai-test: "1", kai.scheduler/queue: $queue, example.com/owner: $2}}
spec:
  template:
    metadata: {labels: {ukai-test: "1", kai.scheduler/queue: $queue}, annotations: {${ANNOTATIONS:-}}}
    spec:
      schedulerName: kai-scheduler
      priorityClassName: test-$3
      restartPolicy: Never
      terminationGracePeriodSeconds: 1
      automountServiceAccountToken: false
      nodeSelector: {$selector}
      containers:
        - name: main
          image: busybox:1.37
          command: [sh, -c, "trap 'exit 1' TERM; while :; do sleep 1; done"]
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {$limits}}
Y
  sleep 2 # distinct creation times
}
share() { # fraction, then submit's
  local f=$1; shift
  GPUS=0 ANNOTATIONS="gpu-fraction: \"$f\"" submit "$@"
}
die() { echo "FAIL: $*"; kubectl get pods -A | grep -vE 'kube-system|local-path'; kubectl -n $NS logs deployment/kai-scheduler-default | grep tierbudget | tail -20; exit 1; }
phase() { kubectl get pods -l "job-name=$1" -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{.status.phase}{"\n"}{end}' | grep -E '^(Running|Pending)$' | sort | head -1; }
uid() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].metadata.uid}'; }
node() { kubectl get pods -l "job-name=$1" -o jsonpath='{.items[0].spec.nodeName}'; }
expect() { # name phase: reached within 60 s, and kept for 8 s
  for _ in $(seq 30); do [ "$(phase "$1")" = "$2" ] && break; sleep 2; done
  sleep 8
  [ "$(phase "$1")" = "$2" ] || die "$1 is $(phase "$1"), want $2"
  echo "  ok: $1 $2"
}
held() { # name reason
  expect "$1" Pending
  local got
  got=$(curl -sSf --retry 8 --retry-delay 2 --retry-all-errors localhost:18081/tiers | jq -r --arg n "$1" '[.. | objects | select((.name? // "") | contains($n + "-")) | .reason // ""] | join(" ")')
  [[ $got == *"$2"* ]] || die "$1 is held because '$got', want '$2'"
  echo "  ok: $1: $got"
}
metric() { # line
  for _ in $(seq 10); do curl -sSf --retry 8 --retry-delay 2 --retry-all-errors localhost:18080/metrics | grep -qxF -- "$1" && { echo "  ok: $1"; return; }; sleep 2; done
  curl -sSf --retry 8 --retry-delay 2 --retry-all-errors localhost:18080/metrics | grep '^ukai_units' || true
  die "no metric $1"
}

cleanup
echo "1. A big GPU uses two units"
TYPE=big submit a1 alice high; expect a1 Running
TYPE=small submit a2 alice high; held a2 'needs 1 units, using 2 of 2'
echo "2. Small ones use one"
TYPE=small GPUS=2 submit b1 bob high; expect b1 Running
TYPE=small submit b2 bob high; held b2 'needs 1 units, using 2 of 2'
echo "3. Naming no type costs as the best one, wherever it runs"
submit c1 carol high; expect c1 Running
submit c2 carol high; held c2 'needs 2 units, using 2 of 2'
echo "4. One too large for the budget says so"
TYPE=big GPUS=2 submit d1 dave high; held d1 'needs 4 units, budget is 2'
cleanup
echo "5. P2 are promoted by weight: a big one fills the budget"
TYPE=big submit e1 erin mid; expect e1 Running
TYPE=small submit e2 erin mid; expect e2 Running
metric 'ukai_units{owner="erin",state="running",tier="promoted"} 2'

cleanup
echo "6. Shares of a GPU count as such, and share it"
for i in 1 2 3 4; do TYPE=small share 0.5 "f$i" frank high; expect "f$i" Running; done
TYPE=small share 0.5 f5 frank high; held f5 'needs 0.5 units, using 2 of 2'
[ "$(kubectl -n kai-resource-reservation get pods --no-headers | wc -l | tr -d ' ')" = 2 ] || die "4 halves should be on 2 GPUs"
echo "7. The held share starts when another ends"
kubectl delete job f1 >/dev/null; expect f5 Running
cleanup
echo "8. Shares and whole GPUs of a user add up, with weights"
TYPE=small submit g1 grace high; expect g1 Running
TYPE=big share 0.5 g2 grace high; expect g2 Running # 0.5 of 2 units
TYPE=small share 0.1 g3 grace high; held g3 'needs 0.1 units, using 2 of 2'
cleanup
echo "9. A request by memory counts as a whole GPU"
TYPE=small GPUS=0 ANNOTATIONS='gpu-memory: "2000"' submit h1 heidi high; expect h1 Running
TYPE=small GPUS=0 ANNOTATIONS='gpu-memory: "2000"' submit h2 heidi high; expect h2 Running
TYPE=small GPUS=0 ANNOTATIONS='gpu-memory: "2000"' submit h3 heidi high; held h3 'needs 1 units, using 2 of 2'
cleanup
echo "    and a small fraction next to it does not make it less"
for i in 1 2; do TYPE=small GPUS=0 ANNOTATIONS='gpu-fraction: "0.01", nvidia.com/container.main.gpu-memory.request: 2000Mi' submit "k$i" ken high; expect "k$i" Running; done
TYPE=small GPUS=0 ANNOTATIONS='gpu-fraction: "0.01", nvidia.com/container.main.gpu-memory.request: 2000Mi' submit k3 ken high; held k3 'needs 1 units, using 2 of 2'
cleanup
echo "10. Shares of several GPUs"
TYPE=small GPUS=0 ANNOTATIONS='gpu-fraction: "0.75", gpu-fraction-num-devices: "2"' submit i1 ivan high; expect i1 Running # 1.5 units
TYPE=small share 0.6 i2 ivan high; held i2 'needs 0.6 units, using 1.5 of 2'
TYPE=small share 0.5 i3 ivan high; expect i3 Running
cleanup
echo "11. Shares are promoted, and P0 evicts them"
TYPE=small share 0.5 j1 judy mid; expect j1 Running
metric 'ukai_units{owner="judy",state="running",tier="promoted"} 0.5'
TYPE=small GPUS=4 submit j2 kim top; expect j2 Running; expect j1 Pending
kubectl delete job j2 >/dev/null; expect j1 Running
[ "$(phase j1)" = Running ] || die "j1 did not come back"

cleanup
deploy # no weights
echo "12. The budget is across queues"
QUEUE=test-a GPUS=2 submit l1 liam high; expect l1 Running
QUEUE=test-b submit l2 liam high; held l2 'needs 1 units, using 2 of 2'
QUEUE=test-b GPUS=2 submit m1 mona high; expect m1 Running
echo "13. A queue's limit applies too"
QUEUE=test-limited GPUS=2 submit n1 nina high; expect n1 Pending
QUEUE=test-limited GPUS=1 submit n2 nina high; expect n2 Running
echo "14. Promotion counts P2 of all queues"
QUEUE=test-a submit o1 olaf mid; QUEUE=test-b submit o2 olaf mid; QUEUE=test-b submit o3 olaf mid
expect o1 Running; expect o2 Running
metric 'ukai_units{owner="olaf",state="running",tier="promoted"} 2'
cleanup
echo "15. A held job reclaims nothing for its queue"
for i in 1 2 3; do QUEUE=test-a GPUS=2 submit "p$i" "pat$i" high; expect "p$i" Running; done # test-a is over its quota of 4
QUEUE=test-b GPUS=2 submit q1 quin high; expect q1 Running # the cluster is full
u=$(uid p1)$(uid p2)$(uid p3)
QUEUE=test-b GPUS=2 submit q2 quin high; held q2 'needs 2 units, using 2 of 2'
sleep 20
[ "$(uid p1)$(uid p2)$(uid p3)" = "$u" ] || die "a held job evicted another queue's P1"
echo "  ok: test-a is untouched"
echo "16. One within its budget does, whatever the tier: KAI's fairness between queues"
QUEUE=test-b GPUS=2 submit r1 rose high; expect r1 Running
[ "$(uid p1)$(uid p2)$(uid p3)" != "$u" ] || die "nothing was reclaimed"
echo "  ok: one of test-a gave way"
kubectl delete job q2 >/dev/null

kubectl -n $NS logs deployment/kai-scheduler-default --tail=-1 >"$WORK/scheduler.log"
if grep -E 'Plugin tierbudget.*(panic|is disabled)' "$WORK/scheduler.log"; then die "errors logged"; fi
[ "$(kubectl -n $NS get pods -l app=kai-scheduler-default -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}')" = 0 ] || die restarts
echo PASS
