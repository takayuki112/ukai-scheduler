#!/usr/bin/env bash
# Runs the load test on a cluster of simulated nodes, made here as ukai-scale.
#   test/scale/run.sh setup          the cluster: NODES (200) nodes of 8 GPUs
#                                    (load KWOK's image first if the cluster cannot pull it)
#   test/scale/run.sh stock|audit|enforce [flags of test/load]
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
export CLUSTER=ukai-scale WORK=${WORK:-$PWD/bin/scale}
export KUBECONFIG=$WORK/kubeconfig # never the user's own
NS=kai-scheduler KWOK=v0.7.0
BUDGET=${BUDGET:-8}
mode=${1:?}; shift

nodes() { # count
  for i in $(seq "$1"); do
    cat <<Y
---
apiVersion: v1
kind: Node
metadata:
  name: kwok-$i
  annotations: {kwok.x-k8s.io/node: fake}
  labels: {type: kwok, kubernetes.io/hostname: kwok-$i, kubernetes.io/os: linux}
spec: {taints: [{key: kwok.x-k8s.io/node, value: fake, effect: NoSchedule}]}
status:
  allocatable: &r {cpu: "128", memory: 1Ti, pods: "110", nvidia.com/gpu: "8"}
  capacity: *r
Y
  done
}
if [ "$mode" = setup ]; then
  # Simulated pods come and go faster than real ones; the rest of the cluster must keep up.
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || KEEP=1 test/smoke/run.sh
  for c in kube-controller-manager kube-scheduler; do # Jobs and deletions at 20/s would hide the load
    docker exec "$CLUSTER-control-plane" sh -c "grep -q api-qps /etc/kubernetes/manifests/$c.yaml || sed -i 's/^    - $c\$/&\n    - --kube-api-qps=200\n    - --kube-api-burst=400/' /etc/kubernetes/manifests/$c.yaml"
  done
  kubectl -n kube-system set resources daemonset kindnet --limits=memory=300Mi >/dev/null # 50Mi is too little for so many nodes
  # kindnet stops if it must route to a simulated node.
  docker exec "$CLUSTER-control-plane" sed -i 's/allocate-node-cidrs=true/allocate-node-cidrs=false/' /etc/kubernetes/manifests/kube-controller-manager.yaml
  sleep 20
  for _ in $(seq 60); do kubectl get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done
  kubectl -n kube-system wait --for=condition=Ready pod -l component=kube-controller-manager --timeout=120s >/dev/null
  for f in kwok stage-fast; do
    kubectl apply --server-side -f https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK/$f.yaml
  done
  kubectl -n kube-system rollout status deployment/kwok-controller --timeout=180s
  kubectl apply --server-side --force-conflicts -f test/scale/stages.yaml
  kubectl delete node -l type=kwok --wait=true >/dev/null
  nodes "${NODES:-200}" | kubectl create -f - >/dev/null
  for c in test-top:90 test-high:75 test-mid:50 test-low:25; do
    kubectl apply -f - <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
  done
  kubectl wait --for=condition=Ready node -l type=kwok --timeout=180s >/dev/null
  exit
fi

[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }
# So that an eviction does not count against a Job's backoffLimit, and can be counted here.
args=(--set-string scheduler.args.update-pod-eviction-condition=true --set-string scheduler.plugins.tierbudget.arguments.budget="$BUDGET" --set-string scheduler.plugins.tierbudget.arguments.allowance="$BUDGET")
check=$BUDGET
# MIXED=1, with the flag -mixed: two GPU types of different weight, and three queues.
if [ -n "${MIXED:-}" ]; then
  T=scheduler.plugins.tierbudget.arguments
  args+=(--set-string "$T.gpuTypeLabel=gpu-type" --set-string "$T.gpuWeights=small=1\,big=2" --set-string "$T.defaultGPUWeight=2")
  i=0
  for n in $(kubectl get nodes -l type=kwok -o name); do
    i=$((i + 1)) t=small
    if [ $((i % 2)) = 0 ]; then t=big; fi
    kubectl label "$n" gpu-type="$t" --overwrite >/dev/null
  done
  for q in load-a:100 load-b:40 load-c:0; do
    kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: ${q%:*}}
spec: {parentQueue: default-parent-queue, resources: {cpu: {quota: -1, limit: -1, overQuotaWeight: 1}, memory: {quota: -1, limit: -1, overQuotaWeight: 1}, gpu: {quota: ${q#*:}, limit: -1, overQuotaWeight: 1}}}
Y
  done
fi
case $mode in
  enforce) ;;
  audit) args+=(--set-string scheduler.plugins.tierbudget.arguments.mode=audit); check=0 ;;
  stock) # KAI's own actions and plugins
    args+=(--set scheduler.plugins.tierbudget.enabled=false --set scheduler.actions.consolidation.enabled=true --set scheduler.actions.ukai-consolidation.enabled=false)
    check=0 ;;
  *) echo "unknown: $mode" >&2; exit 1 ;;
esac
helm upgrade kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
  --version v0.18.9 -n $NS -f test/smoke/values.yaml "${args[@]}" --wait --timeout 5m >/dev/null
sleep 5
kubectl -n $NS rollout restart deployment/kai-scheduler-default >/dev/null
kubectl -n $NS rollout status deployment/kai-scheduler-default --timeout=180s >/dev/null
kubectl -n $NS get cm kai-scheduler-default -o jsonpath='{.data.config\.yaml}' | grep -E '^actions|mode|  budget'  || true
kubectl delete jobs -l ukai-load --wait=false >/dev/null
for _ in $(seq 300); do [ -z "$(kubectl get pods -l ukai-load -o name | head -1)" ] && break; sleep 3; done
go run ./test/load -kubeconfig "$KUBECONFIG" -context "kind-$CLUSTER" -budget "$check" -out "$WORK/$mode.csv" "$@"
