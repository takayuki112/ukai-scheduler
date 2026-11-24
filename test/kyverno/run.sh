#!/usr/bin/env bash
# Tests examples/kyverno, or with POLICIES=examples/kyverno-cel those, with Kyverno on a kind cluster made by
#   KEEP=1 CLUSTER=ukai-kyverno test/smoke/run.sh
# Users are impersonated.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=${CLUSTER:-ukai-kyverno}
export KUBECONFIG=$PWD/bin/kyverno-kubeconfig # never the user's own
kind get kubeconfig --name "$CLUSTER" >"$KUBECONFIG"
[ "$(kubectl config current-context)" = "kind-$CLUSTER" ] || { echo "not on kind-$CLUSTER" >&2; exit 1; }
helm upgrade --install kyverno oci://ghcr.io/kyverno/charts/kyverno --version "${KYVERNO_CHART:-3.7.0}" \
  -n kyverno --create-namespace --set global.image.registry=ghcr.io --set reportsController.enabled=false --set cleanupController.enabled=false --wait --timeout 8m >/dev/null
helm list -n kyverno
for c in test-top:90 test-high:75; do kubectl apply -f - >/dev/null <<Y
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${c%:*}}
value: ${c#*:}
Y
done
for u in alice carol bob@example.com; do
  kubectl create rolebinding "edit-${u%%@*}" --clusterrole=admin --user="$u" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
# As if users had been given too much.
kubectl create clusterrole pg --verb='*' --resource=podgroups.scheduling.run.ai --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create clusterrolebinding pg --clusterrole=pg --user=alice --dry-run=client -o yaml | kubectl apply -f - >/dev/null

n=0
job() { # name [yaml of labels] [yaml of pod metadata] [yaml of the pod spec]
  cat <<Y
apiVersion: batch/v1
kind: Job
metadata: {name: $1, labels: {kai.scheduler/queue: default-queue, ${2:-}}, annotations: {${5:-}}}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {kai.scheduler/queue: default-queue, ${3:-}}}
    spec:
      schedulerName: kai-scheduler
      restartPolicy: Never
      ${4:-}
      containers: [{name: main, image: busybox:1.37, command: [sleep, "300"]}]
Y
}
as() { local u=$1; shift; kubectl --as="$u" --as-group=system:authenticated "$@"; }
allowed() { local what=$1; shift; "$@" >/tmp/ukai-out.$$ 2>&1 || { echo "FAIL: $what was rejected"; cat /tmp/ukai-out.$$; exit 1; }; n=$((n + 1)); echo "  ok: allowed: $what"; }
denied() { # what text command...
  local what=$1 text=$2; shift 2
  if "$@" >/tmp/ukai-out.$$ 2>&1; then echo "FAIL: $what was allowed"; exit 1; fi
  tr -s '\n ' ' ' </tmp/ukai-out.$$ | grep -q "$text" || { echo "FAIL: $what failed for another reason"; cat /tmp/ukai-out.$$; exit 1; }
  n=$((n + 1)); echo "  ok: denied: $what"
}
trap 'rm -f /tmp/ukai-out.$$' EXIT
submit() { local u=$1; shift; job "$@" | as "$u" create -f -; }
label() { kubectl get "$@" -o jsonpath='{.metadata.labels.example\.com/owner}'; }
wait_for() { for _ in $(seq 40); do [ -n "$(kubectl get "$@" -o name 2>/dev/null)" ] && return; sleep 1; done; echo "FAIL: no $*"; exit 1; }
reset() { kubectl delete jobs,cronjobs,deployments,pods --all --force --grace-period=0 >/dev/null 2>&1 || true; }

for k in clusterpolicies validatingpolicies.policies.kyverno.io mutatingpolicies.policies.kyverno.io; do kubectl delete $k --all >/dev/null 2>&1 || true; done
reset
echo "Without the policies, all of it works, so the tests test something"
sleep 5
allowed "a long grace period" submit alice w1 "" "" "terminationGracePeriodSeconds: 99999"
allowed "another's owner label" submit alice w2 "example.com/owner: bob"
allowed "the exempt class" submit alice w3 "" "" "priorityClassName: test-top"
allowed "the exempt class by label" submit alice w4 "priorityClassName: test-top" "" "priorityClassName: test-high"
wait_for podgroups; sleep 5
[ "$(kubectl get podgroups -o json | jq -r '.items[] | select(.metadata.ownerReferences[0].name == "w4").spec.priorityClassName')" = test-top ] || { echo "FAIL: KAI ignores the label, so kai-overrides.yaml is needless"; exit 1; }
echo "  ok: and KAI honours that label over the pod spec"
pg=$(kubectl get podgroups -o json | jq -r '.items[] | select(.metadata.ownerReferences[0].name == "w2").metadata.name')
[ "$(label podgroup "$pg")" = bob ] || { echo "FAIL: PodGroup owner"; exit 1; }
allowed "relabelling a PodGroup" as alice label podgroup "$pg" example.com/owner=carol --overwrite
reset

kubectl apply -f "${POLICIES:-examples/kyverno}" >/dev/null
if [ -n "$(kubectl get clusterpolicies -o name)" ]; then kubectl wait --for=condition=Ready clusterpolicy --all --timeout=90s >/dev/null; fi
sleep 15

echo "Grace period"
denied "an hour" "at most 300" submit alice g1 "" "" "terminationGracePeriodSeconds: 3600"
denied "301" "at most 300" submit alice g1 "" "" "terminationGracePeriodSeconds: 301"
allowed "300" submit alice g2 "" "" "terminationGracePeriodSeconds: 300"
allowed "the default" submit alice g3
denied "an hour on a bare pod" "at most 300" as alice run g4 --image=busybox:1.37 --overrides='{"spec":{"terminationGracePeriodSeconds":3600}}'

echo "Owner label"
allowed "another's owner label is overwritten" submit alice o1 "example.com/owner: bob" "example.com/owner: bob"
wait_for pods -l job-name=o1
[ "$(label job o1)/$(kubectl get pods -l job-name=o1 -o jsonpath='{.items[0].metadata.labels.example\.com/owner}')" = alice/alice ] || { echo "FAIL: o1 is $(label job o1)"; exit 1; }
for _ in $(seq 30); do pg=$(kubectl get podgroups -o json | jq -r '.items[] | select(.metadata.ownerReferences[0].name == "o1").metadata.name'); [ -n "$pg" ] && break; sleep 1; done
[ "$(label podgroup "$pg")" = alice ] || { echo "FAIL: PodGroup is $(label podgroup "$pg")"; exit 1; }
echo "  ok: on the Job, its pod and its PodGroup"
allowed "a user name with @" submit bob@example.com o2
[ "$(label job o2)" = bob-at-example.com ] || { echo "FAIL: o2 is $(label job o2)"; exit 1; }
allowed "a bare pod" as alice run o3 --image=busybox:1.37 --labels=example.com/owner=bob
[ "$(label pod o3)" = alice ] || { echo "FAIL: o3"; exit 1; }
allowed "a CronJob" as alice create cronjob o4 --image=busybox:1.37 --schedule='* * * * *' -- sleep 100
[ "$(kubectl get cronjob o4 -o jsonpath='{.metadata.labels.example\.com/owner}{.spec.jobTemplate.metadata.labels.example\.com/owner}{.spec.jobTemplate.spec.template.metadata.labels.example\.com/owner}')" = alicealicealice ] || { echo "FAIL: o4"; exit 1; }
allowed "a Deployment" as alice create deployment o5 --image=busybox:1.37 -- sleep 100
[ "$(label deployment o5)$(kubectl get deployment o5 -o jsonpath='{.spec.template.metadata.labels.example\.com/owner}')" = alicealice ] || { echo "FAIL: o5"; exit 1; }
kubectl rollout status deployment/o5 --timeout=90s >/dev/null && echo "  ok: which runs"
denied "relabelling a Job" "cannot be changed" as alice label job o1 example.com/owner=bob --overwrite
denied "removing the label" "cannot be changed" as alice label job o1 example.com/owner-
denied "relabelling a pod" "cannot be changed" as alice label pod o3 example.com/owner=bob --overwrite
denied "relabelling a Deployment's template" "cannot be changed" as alice patch deployment o5 --type=merge -p '{"spec":{"template":{"metadata":{"labels":{"example.com/owner":"bob"}}}}}'
denied "relabelling a CronJob's template" "cannot be changed" as alice patch cronjob o4 --type=merge -p '{"spec":{"jobTemplate":{"metadata":{"labels":{"example.com/owner":"bob"}}}}}'
allowed "another label" as alice label job o1 note=x
allowed "scaling" as alice scale deployment o5 --replicas=2

echo "PodGroups"
denied "relabelling a PodGroup" "managed by KAI" as alice label podgroup "$pg" example.com/owner=bob --overwrite
denied "deleting a PodGroup" "managed by KAI" as alice delete podgroup "$pg"
denied "creating a PodGroup" "managed by KAI" bash -c "printf 'apiVersion: scheduling.run.ai/v2alpha2\nkind: PodGroup\nmetadata: {name: x}\nspec: {minMember: 1, queue: default-queue}\n' | kubectl --as=alice create -f -"
allowed "deleting the Job" as alice delete job o1
for _ in $(seq 60); do kubectl get podgroup "$pg" >/dev/null 2>&1 || break; sleep 1; done
if kubectl get podgroup "$pg" >/dev/null 2>&1; then echo "FAIL: its PodGroup is left"; exit 1; fi
echo "  ok: its PodGroup is collected"

echo "Priority classes and KAI's overrides"
allowed "an ordinary class" submit alice c1 "" "" "priorityClassName: test-high"
allowed "the exempt class, submitted by one who is listed" submit carol c2 "" "" "priorityClassName: test-top"
wait_for pods -l job-name=c2; echo "  ok: whose pod is created"
submit alice c3 "" "" "priorityClassName: test-top" >/dev/null # the Job is accepted, its pods are not
sleep 8
[ -z "$(kubectl get pods -l job-name=c3 -o name)" ] || { echo "FAIL: c3 has a pod"; exit 1; }
[[ $(kubectl get events --field-selector involvedObject.name=c3 -o jsonpath='{.items[*].message}') == *"priority class is restricted"* ]] || { echo "FAIL: no reason"; exit 1; }
n=$((n + 1)); echo "  ok: denied: the exempt class, submitted by one who is not listed"
denied "the class by label" "not allowed" submit alice c4 "priorityClassName: test-top"
denied "the class by label on a bare pod" "not allowed" as alice run c5 --image=busybox:1.37 --labels=priorityClassName=test-top
denied "not being preemptible" "not allowed" submit alice c6 "kai.scheduler/preemptibility: non-preemptible"
denied "delaying preemption" "not allowed" submit alice c7 "" "" "" "kai.scheduler/preemption-delay: 100h"
denied "choosing a PodGroup" "not allowed" as alice run c8 --image=busybox:1.37 --annotations=pod-group-name=x
submit alice c9 "" "priorityClassName: test-top" >/dev/null
sleep 8
[ -z "$(kubectl get pods -l job-name=c9 -o name)" ] || { echo "FAIL: c9 has a pod"; exit 1; }
n=$((n + 1)); echo "  ok: denied: the class by label on the template"

echo "KAI still works"
allowed "a job" submit alice k1 "" "" "priorityClassName: test-high"
kubectl wait --for=condition=Ready --timeout=90s pod -l job-name=k1 >/dev/null
allowed "labelling a pod that KAI has annotated" as alice label pod -l job-name=k1 note=x
reset
echo "PASS ($n checks)"
