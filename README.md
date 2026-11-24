# ukai-scheduler

A build of [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) that gives
each person a GPU budget at high priority within a shared queue. It is an independent
project and is not affiliated with KAI Scheduler or NVIDIA.

It is not a fork: it imports KAI v0.18.9 and adds one plugin, `tierbudget`, and one
action, `ukai-consolidation`. The rest of a KAI installation stays as it is.

Status: not yet used in production.

## What it does

Jobs are sorted by their priority class. "Owner" is the value of a label on the job.

| Class | Behaviour |
|---|---|
| Budgeted | Runs up to the owner's budget. Beyond that it stays pending, with the reason in its events. Whatever fits starts, oldest first. |
| Charged | Counts against the budget but is never held, for example an interactive workspace. |
| Promotable | While it runs, it may use budget that is free, and is then promoted. Otherwise the first units up to the allowance keep their priority and the rest are excess. |
| Any other | Untouched. |

- Promotion goes to the longest running jobs, and the allowance to the earliest
  submitted. Tiers are recalculated every scheduling cycle and nothing is stored.
- Waiting budgeted jobs that fit the budget take precedence over promotion. One that
  does not fit leaves promotion alone.
- Only KAI's in-memory priority changes. Pods and PodGroups keep theirs, and a
  priority change does not restart anything by itself.
- Budgets are in units: GPUs times a weight by GPU type. A running job is charged in
  full. Requests by GPU memory count as whole GPUs. Jobs without GPUs are ignored.
- A budgeted job without an owner is held, and a promotable one is excess.
- If the configuration is invalid or the plugin fails, KAI's stock behaviour applies.

## Configuration

Set these as arguments of the plugin, all as strings. See
[test/smoke/values.yaml](test/smoke/values.yaml) for Helm values.

| Argument | Default | Meaning |
|---|---|---|
| `mode` | `audit` | `audit` only logs decisions; `enforce` applies them |
| `ownerLabel` | required | Label that names the owner |
| `budgetedClasses` | required | Comma-separated priority classes |
| `chargedClasses` | none | |
| `promotableClasses` | none | |
| `budget` | required | Units per owner |
| `allowance` | required with `promotableClasses` | Units per owner that keep their priority |
| `promotedPriority`, `excessPriority` | required with `promotableClasses` | |
| `largeJobs` | `excess` | For a job larger than the allowance: `excess`, or `normal-if-first` to keep its priority if nothing of its owner's is ahead of it |
| `gpuTypeLabel` | none | nodeSelector key with the GPU type |
| `gpuWeights` | none | `type=weight`, comma-separated |
| `defaultGPUWeight` | `1` | Weight of other types, and of pods that do not select one |
| `consolidation` | see below | Consolidation passes |
| `jobMetrics` | `false` | `true` adds a metric for each job |
| `protectedPriority` | `promotedPriority` | See below. Required if there are neither promotable classes nor passes |

Arguments replace the defaults of an installation as a whole, so list them all.

### Consolidation

KAI's consolidation may restart a running job of any priority to place any other.
Disable its `consolidation` action and enable `ukai-consolidation`. In `audit` mode it
behaves like KAI's.

By default it may place any job, but does not move running jobs that are budgeted,
charged, promoted, or of `protectedPriority` or higher. To choose, list passes as
`classes to place > classes to move`, separated by `;`:

    consolidation: "high > high,mid,low; mid,low > mid,low"

`@promoted` stands for promoted jobs, which do not count as their class. Classes that
are not listed are neither placed nor moved. Each pass has its own search budget. A
partly running job counts as running.

KAI disables its consolidation for spread placement. This action must then be disabled
by hand.

## Observing

In both modes; in `audit` mode, tiers and priorities are what they would be.

KAI's `/metrics` also has:

| Metric | Labels | Meaning |
|---|---|---|
| `ukai_active` | `mode` | 1, or 0 if stock behaviour applies because of an error |
| `ukai_last_session_timestamp_seconds` | | When the last scheduling cycle began |
| `ukai_panics_total` | | Should stay 0 |
| `ukai_owner_budget_units` | | Budget of each owner |
| `ukai_jobs`, `ukai_units` | `owner`, `tier`, `state` | `state` is `running` or `pending` |
| `ukai_tier_changes_total` | `from`, `to` | Steady growth without new jobs means tiers flap |
| `ukai_job_priority` | `namespace`, `podgroup`, `owner`, `tier` | With `jobMetrics` |

Tiers are `budget`, `held`, `promoted`, `normal` and `excess`. Use of the budget is
`ukai_units{tier="budget",state="running"}`.

KAI's plugin server lists every job, with the reason if it is held. It listens on
localhost only:

    kubectl -n kai-scheduler port-forward deployment/kai-scheduler-default 8081
    curl 'localhost:8081/tiers?owner=alice'

## Rolling back

To stop enforcing, set `mode: audit` and restart the scheduler: held jobs start, and
running ones are not touched. To return to stock KAI, remove the plugin, the action and
the image together, as `helm upgrade` without these values does. The stock image does
not start while its configuration names `ukai-consolidation`.

## Limits

- Users who can set the owner label or the priority class can evade the rules. Guard
  both with an admission policy.
- An evicted Job whose program exits 0 on SIGTERM counts as complete and does not run
  again.
- A stream of small budgeted jobs can delay a large one of the same owner.

## Building

    CGO_ENABLED=0 go build ./cmd/scheduler

`go install` by version does not work, because KAI needs `replace` directives. KAI's
chart has one registry for all images, so either mirror them next to this one, or set
the scheduler's image in the KAI `Config` resource.

## Testing

    go test ./...                      # SIMULATIONS=100 for a longer simulation
    KEEP=1 CLUSTER=ukai-dev test/smoke/run.sh   # kind cluster
    test/e2e/run.sh                    # on that cluster, with fake GPUs
    test/upgrade/run.sh                # installing and rolling back, there too

## License

[Apache-2.0](LICENSE). KAI Scheduler is also licensed under Apache-2.0. Images include its LICENSE and NOTICE.
