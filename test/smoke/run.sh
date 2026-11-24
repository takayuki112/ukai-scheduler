#!/usr/bin/env bash
# Builds the scheduler, installs stock KAI with it on a throwaway kind cluster, and
# checks that the plugin loaded and a pod was scheduled.
# Needs go, docker, kind, kubectl and helm. KEEP=1 leaves the cluster running.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
KAI_VERSION=v0.18.9
CLUSTER=${CLUSTER:-ukai-smoke}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.35.0}
NS=kai-scheduler
IMAGE=ghcr.io/kai-scheduler/kai-scheduler/ukai-scheduler:smoke # local only, never pushed
WORK=$PWD/bin/smoke
export KUBECONFIG=$WORK/kubeconfig # never the user's own

fail() { echo "FAIL: $*" >&2; exit 1; }

KAI_MODULE=github.com/kai-scheduler/KAI-scheduler
[ "$(go list -m -f '{{.Version}}' $KAI_MODULE)" = "$KAI_VERSION" ] || fail "go.mod does not pin KAI $KAI_VERSION"
go mod verify

mkdir -p "$WORK/image"
case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "unsupported architecture" ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$WORK/image/scheduler" ./cmd/scheduler
kai_dir=$(go list -m -f '{{.Dir}}' $KAI_MODULE)
install -m 644 "$kai_dir/LICENSE" "$kai_dir/NOTICE" "$WORK/image/"
docker build -t "$IMAGE" -f Dockerfile "$WORK/image"

kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 180s --config - <<Y
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes: [{role: control-plane}, {role: worker}, {role: worker}]
Y
trap '[ -n "${KEEP:-}" ] || kind delete cluster --name "$CLUSTER"' EXIT
kind load docker-image "$IMAGE" --name "$CLUSTER"

helm install kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
  --version "$KAI_VERSION" -n $NS --create-namespace -f test/smoke/values.yaml --wait --timeout 8m

# The operator creates these after the chart is installed.
for d in kai-scheduler-default pod-grouper binder admission; do
  kubectl -n $NS wait --for=create --timeout=180s "deployment/$d"
  kubectl -n $NS rollout status --timeout=300s "deployment/$d"
done
[ "$(kubectl -n $NS get deployment kai-scheduler-default -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$IMAGE" ] ||
  fail "scheduler is not running $IMAGE"

kubectl apply -f test/smoke/pod.yaml
kubectl wait --for=condition=Ready --timeout=180s pod/smoke || {
  kubectl describe pod smoke
  fail "pod was not scheduled"
}

logs=$(kubectl -n $NS logs deployment/kai-scheduler-default --tail=-1)
grep -F 'Plugin tierbudget loaded' <<<"$logs" || fail "plugin did not load"
if grep -F 'Failed to get plugin' <<<"$logs"; then fail "a configured plugin is not registered"; fi
restarts=$(kubectl -n $NS get pods -l app=kai-scheduler-default -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}')
[ "$restarts" = 0 ] || fail "scheduler restarts: '$restarts'"
kubectl get pod smoke -o jsonpath='{.metadata.name}{" scheduled by "}{.spec.schedulerName}{" on "}{.spec.nodeName}{"\n"}'
kubectl get events --field-selector involvedObject.name=smoke
echo PASS
