# Testing

| Suite | What it shows | Run |
|---|---|---|
| Unit and property tests | The rules, for random jobs: budgets hold, order does not matter, no thrash | `go test ./...` |
| Against stock KAI | Each case also runs without the plugin, and must differ as stated | same |
| Simulation | Random clusters over many cycles | `SIMULATIONS=100 go test ./pkg/tierbudget` |
| Smoke | It installs with KAI's chart, loads, and places a pod | `KEEP=1 CLUSTER=ukai-dev test/smoke/run.sh` |
| End to end | Real pods with fake GPUs: holding, promotion, excess, gangs, consolidation, metrics | `test/e2e/run.sh` |
| Upgrade | Stock to audit to enforce and back, a broken configuration, a wrong rollback; no running job changes | `test/upgrade/run.sh` |
| Failure | Kills, lost nodes, API outage, failover, stuck pods, changed labels, with the budget sampled throughout | `test/failure/run.sh` |
| Policies | The Kyverno examples, with impersonated Kubernetes usernames, and that the tricks work without them | `test/kyverno/run.sh` |

The cluster tests need go, docker, kind, kubectl, helm and jq. They use their own kubeconfig, and
refuse any context but their kind cluster. Run the smoke test first; it makes the cluster.

## What is not tested

Real GPUs, GPU sharing with real devices, clusters of more than a few nodes with a real API
server, KAI versions other than v0.18.9, and long operation.
