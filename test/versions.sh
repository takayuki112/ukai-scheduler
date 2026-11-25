#!/usr/bin/env bash
# Runs the smoke, end-to-end and setups suites on other Kubernetes versions, each on a
# cluster of its own. Arguments are tags of kindest/node.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
export CLUSTER=ukai-k8s WORK=$PWD/bin/k8s
mkdir -p "$WORK"
result=0
for v in "${@:-v1.35.0}"; do
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  for suite in smoke e2e setups; do
    if KEEP=1 NODE_IMAGE=kindest/node:$v "test/$suite/run.sh" >"$WORK/$v-$suite.log" 2>&1; then
      echo "$v $suite: PASS"
    else
      echo "$v $suite: FAIL, see $WORK/$v-$suite.log"
      result=1
      [ "$suite" = smoke ] && break
    fi
  done
done
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
exit $result
