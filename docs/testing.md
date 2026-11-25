# Testing

| Suite | What it shows | Run |
|---|---|---|
| Unit and property tests | The rules, for random jobs: budgets hold, order does not matter, no thrash | `go test ./...` |
| Against stock KAI | Each case also runs without the plugin, and must differ as stated | same |
| Simulation | Random clusters over many cycles | `SIMULATIONS=100 go test ./pkg/tierbudget` |
| Smoke | It installs with KAI's chart, loads, and places a pod | `KEEP=1 CLUSTER=ukai-dev test/smoke/run.sh` |
| End to end | Real pods with fake GPUs: holding, promotion, excess, gangs, consolidation, metrics | `test/e2e/run.sh` |
| Setups | GPU weights, shares of GPUs, several queues | `test/setups/run.sh` |
| Kubernetes versions | The smoke, end-to-end and setups suites on 1.32, 1.33, 1.34, 1.36 and 1.37; the others use 1.35 | `test/versions.sh v1.34.11 ...` |
| Upgrade | Stock to audit to enforce and back, a broken configuration, a wrong rollback; no running job changes | `test/upgrade/run.sh` |
| Failure | Kills, lost nodes, API outage, failover, stuck pods, changed labels, with the budget sampled throughout | `test/failure/run.sh` |
| Policies | The Kyverno examples, with impersonated Kubernetes usernames, and that the tricks work without them | `test/kyverno/run.sh` |
| Scale | Simulated nodes (KWOK) and a burst of 3,000 jobs; the budget is checked at every pod event | `test/scale/run.sh setup`, then `test/scale/run.sh enforce -jobs 3000 -users 60` |

## Scale

200 simulated nodes of 8 GPUs, 60 users with a budget of 8, 3,000 jobs at once and 30 more a
minute, for 5 minutes, with a real API server. Two rounds each of stock, audit and enforce, on a
busy laptop:

| | Stock | Audit | Enforce |
|---|---|---|---|
| Pod events | 77,085 | 80,078 | 81,292 |
| Budget violations | not enforced | not enforced | **0** |
| Scheduler restarts | 0 | 0 | 0 |
| Mean cycle | 1.8 to 2.5 s | 1.7 to 2.2 s | 1.5 to 1.7 s |
| of which the plugin, opening the session | - | 9 to 16 ms | 9 to 12 ms |
| Scheduler memory, most | 493 MiB | 492 MiB | 485 MiB |

The differences in cycle time are not due to the plugin alone: other jobs run, and the machine was
not idle. The scheduler was near its default limit of 512 MiB in all cases, and KAI's binder
(200 MiB) and pod-grouper (512 MiB) were killed for lack of memory, under stock KAI too. See
[memory limits](install.md#memory-limits).

The cluster tests need go, docker, kind, kubectl, helm and jq. They use their own kubeconfig, and
refuse any context but their kind cluster. Run the smoke test first; it makes the cluster.

## Long operation

```sh
NODES=24 test/scale/run.sh setup
test/scale/run.sh enforce -users 12 -jobs 60 -rate 60 -duration 150m -run-min 20s -run-max 90s -drain 30m -seed 7
```

24 simulated nodes, 12 users with a budget of 8, one job a second for 150 minutes, then 30 minutes
to finish: 9,059 jobs and 548,457 pod events in `enforce`.

| | |
|---|---|
| Budget violations | **0** |
| Scheduler restarts, caught panics | 0, 0 |
| Goroutines | about 173 throughout |
| Jobs that never finished | **73**, all gangs of two pods, see [future work](future.md#gangs-that-lose-a-pod-after-another-has-finished) |

The run counts as failed because of the last row. A 40-minute run of stock KAI left 19 of 2,459 jobs in the
same way. The waiting jobs also make the memory hard to read, as it rose with them.

A second run had jobs of one pod only, two weights of GPU and three queues:

```sh
NODES=24 test/scale/run.sh setup
MIXED=1 test/scale/run.sh enforce -mixed -gangs=false -users 12 -jobs 60 -rate 60 -duration 150m -run-min 20s -run-max 90s -drain 30m -seed 7
```

| | |
|---|---|
| Jobs, pod events | 9,059, 157,606 |
| Budget violations | **0** |
| Scheduler restarts, caught panics | 0, 0 |
| Jobs left at the end | 0 |
| Goroutines, median | 173 throughout |
| Cycle, median and 95th percentile | 13 ms, 29 ms |

9,018 pods were seen to finish. The test deletes some jobs on purpose, which the other 41 are consistent with, but it does not
count them.

The scheduler's memory in MiB, over 15 minutes from the minute shown:

| Minute | 0 | 15 | 30 | 45 | 60 | 75 | 90 | 105 | 120 | 135 |
|---|---|---|---|---|---|---|---|---|---|---|
| Heap, lowest (5th percentile) | 32.0 | 42.7 | 48.8 | 52.7 | 53.4 | 54.9 | 56.1 | 57.5 | 57.3 | 57.3 |
| Heap, median | 43.1 | 52.0 | 58.6 | 63.5 | 64.4 | 66.3 | 67.1 | 68.2 | 68.3 | 68.3 |
| Resident, median | 121.9 | 132.6 | 142.1 | 148.9 | 149.9 | 151.3 | 152.9 | 154.8 | 154.5 | 155.1 |

It rose ever more slowly and was level for the last 45 minutes at a steady load. That shows no leak in two and a half hours;
it does not rule out a slow one over days.

## What is not tested

Real GPUs, GPU sharing with real devices, KAI versions other than v0.18.9, runs longer than
three hours, several KAI shards, MIG and GPUs by resource claims, and gangs other than Jobs.
