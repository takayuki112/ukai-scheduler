#!/usr/bin/env bash
# Installs, reconfigures and rolls back on a cluster made by test/smoke/run.sh with
# KEEP=1, with jobs running, and checks that none of them restarts.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=${CLUSTER:-ukai-dev}
NS=kai-scheduler
IMAGE=ghcr.io/kai-scheduler/kai-scheduler/ukai-scheduler:smoke
STOCK=ghcr.io/kai-scheduler/kai-scheduler/scheduler:v0.18.9
WORK=$PWD/bin/smoke
export KUBECONFIG=$WORK/kubeconfig # never the user's own
[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }
fail() { echo "FAIL: $*" >&2; kubectl -n $NS get pods; kubectl get pods; kubectl -n $NS logs deployment/kai-scheduler-default --tail=15; exit 1; }
ok() { echo "  ok: $*"; }

arch=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$WORK/image/scheduler" ./cmd/scheduler
docker build -q -t "$IMAGE" -f Dockerfile "$WORK/image" >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1

# helm replaces the whole configuration, so no values is the stock scheduler.
deploy() {
  helm upgrade kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
    --version v0.18.9 -n $NS --reset-values "$@" --wait --timeout 5m >/dev/null 2>&1
}
image() { kubectl -n $NS get deployment kai-scheduler-default -o jsonpath='{.spec.template.spec.containers[0].image}'; }
settled() { # image: the operator has rolled it out
  for _ in $(seq 60); do [ "$(image)" = "$1" ] && break; sleep 2; done
  [ "$(image)" = "$1" ] || fail "image is $(image), want $1"
  sleep 5
  kubectl -n $NS rollout restart deployment/kai-scheduler-default >/dev/null
  kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null || fail "scheduler does not start"
}
conf() { kubectl -n $NS get cm kai-scheduler-default -o jsonpath='{.data.config\.yaml}'; }
configured() { # text yes|no
  for _ in $(seq 30); do
    if grep -q "$1" <<<"$(conf)"; then [ "$2" = yes ] && return; else [ "$2" = no ] && return; fi
    sleep 2
  done
  fail "configuration: $1, want $2"
}
submit() { # name user class gpus
  kubectl create -f - >/dev/null <<Y
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {ukai-test: "1", kai.scheduler/queue: default-queue, example.com/user: $2}}
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
          image: busybox:1.37
          command: [sh, -c, "trap 'exit 1' TERM; while :; do sleep 1; done"]
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {nvidia.com/gpu: $4}}
Y
  sleep 2
}
phase() { kubectl get pods -l "job-name=$1" -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{.status.phase}{"\n"}{end}' | grep -E '^(Running|Pending)$' | sort | head -1; }
expect() {
  for _ in $(seq 30); do [ "$(phase "$1")" = "$2" ] && break; sleep 2; done
  sleep 6
  [ "$(phase "$1")" = "$2" ] || fail "$1 is $(phase "$1"), want $2"
  ok "$1 $2"
}
# Identity and spec of the pods and PodGroups of the named jobs.
record() {
  for j in "$@"; do
    kubectl get pods -l "job-name=$j" -o json | jq -cS '.items[] | [.metadata.name, .metadata.uid, .spec, .status.containerStatuses[0].restartCount]'
    kubectl get podgroups -o json | jq -cS --arg j "$j" '.items[] | select(.metadata.ownerReferences[0].name == $j) | [.metadata.uid, .spec]'
  done
}
unchanged() { # step
  [ "$(record r1 r2 p1)" = "$before" ] || { diff <(echo "$before") <(record r1 r2 p1) | cut -c1-400; fail "$1 changed running jobs"; }
  ok "$1: running pods and PodGroups are untouched"
}
loglines() { kubectl -n $NS logs deployment/kai-scheduler-default --tail=-1 2>/dev/null | grep -c -- "$1" || true; }

cleanup() { kubectl delete jobs -l ukai-test --wait=true --timeout=90s >/dev/null 2>&1 || true; }
finish() { cleanup; deploy -f test/smoke/values.yaml || true; }
trap finish EXIT
cleanup
for n in worker worker2; do
  g=$([ $n = worker ] && echo 8 || echo 0)
  kubectl patch node "$CLUSTER-$n" --subresource=status --type=merge -p "{\"status\":{\"capacity\":{\"nvidia.com/gpu\":\"$g\"},\"allocatable\":{\"nvidia.com/gpu\":\"$g\"}}}" >/dev/null
done
for c in test-top:90 test-high:75 test-mid:50 test-low:25; do
  kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
done
sed 's/mode: enforce/mode: audit/' test/smoke/values.yaml >"$WORK/audit.yaml"
sed 's/budget: "2"/budget: "two"/' test/smoke/values.yaml >"$WORK/broken.yaml"

echo "1. Stock KAI runs more than the budget"
deploy; configured ukai-consolidation no; settled $STOCK
submit r1 alice high 2; submit r2 alice high 1; submit p1 bob mid 3
expect r1 Running; expect r2 Running; expect p1 Running
before=$(record r1 r2 p1)
[ "$(wc -l <<<"$before" | tr -d ' ')" = 6 ] || fail "recorded $before"

echo "2. Install in audit mode"
deploy -f "$WORK/audit.yaml"; configured ukai-consolidation yes; settled $IMAGE
submit a1 alice high 1; expect a1 Running # over budget, but only logged
unchanged audit
[ "$(loglines 'Plugin tierbudget (audit): job .*a1.* is held')" -gt 0 ] || fail "no audit log"
kubectl delete job a1 --wait >/dev/null

echo "3. Enforce: those over budget keep running, new ones wait"
deploy -f test/smoke/values.yaml; configured 'mode: enforce' yes; settled $IMAGE
submit h1 alice high 1; expect h1 Pending
unchanged enforce

echo "4. A scheduler restart decides the same"
kubectl -n $NS delete pod -l app=kai-scheduler-default --wait >/dev/null
kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null
expect h1 Pending; unchanged restart

echo "5. A broken configuration leaves stock behaviour, and the reason in the log"
deploy -f "$WORK/broken.yaml"; configured 'budget: two' yes; settled $IMAGE
[ "$(loglines 'Plugin tierbudget is disabled, invalid configuration: budget')" -gt 0 ] || fail "no reason in the log"
expect h1 Running; unchanged "broken configuration"
kubectl delete job h1 --wait >/dev/null

echo "6. Repairing it enforces again"
deploy -f test/smoke/values.yaml; configured 'budget: "2"' yes; settled $IMAGE
submit h2 alice high 1; expect h2 Pending; unchanged repair

echo "7. Kill switch: back to audit releases held jobs"
deploy -f "$WORK/audit.yaml"; configured 'mode: audit' yes; settled $IMAGE
expect h2 Running; unchanged "kill switch"
kubectl delete job h2 --wait >/dev/null

echo "8. Full rollback to stock"
deploy -f test/smoke/values.yaml; configured 'mode: enforce' yes; settled $IMAGE
submit h3 alice high 1; expect h3 Pending
deploy; configured ukai-consolidation no; settled $STOCK
expect h3 Running; unchanged rollback
[ "$(loglines 'tierbudget')" = 0 ] || fail "stock KAI mentions the plugin"

echo "9. The stock image with our configuration does not start"
kubectl -n $NS scale deployment/kai-operator --replicas=0 >/dev/null
kubectl -n $NS wait --for=delete pod -l app=kai-operator --timeout=60s >/dev/null 2>&1 || true
sleep 3
saved=$(conf)
kubectl -n $NS patch cm kai-scheduler-default --type=merge -p "$(jq -n --arg c "$(sed 's/allocate, consolidation/allocate, ukai-consolidation/' <<<"$saved")" '{data:{"config.yaml":$c}}')" >/dev/null
kubectl -n $NS delete pod -l app=kai-scheduler-default --wait >/dev/null
crashed=no
for _ in $(seq 30); do
  [ "$(kubectl -n $NS get pods -l app=kai-scheduler-default -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')" -gt 0 ] 2>/dev/null && { crashed=yes; break; }
  sleep 2
done
kubectl -n $NS logs -l app=kai-scheduler-default --tail=3 2>/dev/null | tail -3 || true
kubectl -n $NS patch cm kai-scheduler-default --type=merge -p "$(jq -n --arg c "$saved" '{data:{"config.yaml":$c}}')" >/dev/null
kubectl -n $NS scale deployment/kai-operator --replicas=1 >/dev/null
kubectl -n $NS rollout status deployment/kai-operator --timeout=120s >/dev/null
[ $crashed = yes ] || fail "it started, so the README's rollback order is needless"
ok "crash loop, as documented"
kubectl -n $NS delete pod -l app=kai-scheduler-default --wait >/dev/null
kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null || fail "no recovery"
submit l1 alice low 1; expect l1 Running; unchanged "crash loop and recovery"
echo PASS
