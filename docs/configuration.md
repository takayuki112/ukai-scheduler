# Configuration

Settings are arguments of the `tierbudget` plugin, all strings. With KAI's Helm chart:

```yaml
scheduler:
  image: {name: ukai-scheduler, tag: <tag>}   # see install.md on the registry
  plugins:
    tierbudget:
      priority: 50
      arguments:
        mode: audit
        userLabel: example.com/user
        budgetedClasses: high
        promotableClasses: mid
        budget: "4"
        allowance: "4"
        promotedPriority: "75"
        excessPriority: "45"
  actions:
    consolidation: {enabled: false}
    ukai-consolidation: {enabled: true, priority: 400}
```

KAI replaces a plugin's arguments as a whole, so list all of them. Changes apply when the
scheduler restarts. An error in any of them disables the plugin, and the log says why.

| Argument | Default | Meaning |
|---|---|---|
| `mode` | `audit` | `audit` only logs decisions; `enforce` applies them |
| `userLabel` | required | Label whose value is the job's user |
| `budgetedClasses` | required | Comma-separated classes that run up to the budget, then wait (P1) |
| `chargedClasses` | none | Classes that use the budget, but never wait for it |
| `promotableClasses` | none | Classes that may be promoted, or become excess (P2) |
| `budget` | required | Units per user |
| `allowance` | required with `promotableClasses` | Units per user that keep their priority |
| `promotedPriority` | required with `promotableClasses` | Usually that of the budgeted class |
| `excessPriority` | required with `promotableClasses` | Below the promotable class, above those it may evict |
| `largeJobs` | `excess` | For a job larger than the allowance: `excess`, or `normal-if-first` to keep its priority if nothing of its user's is ahead of it |
| `gpuTypeLabel` | none | nodeSelector key with the GPU type |
| `gpuWeights` | none | `type=weight`, comma-separated |
| `defaultGPUWeight` | `1` | Weight of other types, and of pods that do not select one |
| `consolidation` | see below | Consolidation passes |
| `protectedPriority` | `promotedPriority` | See below. Required if there are neither promotable classes nor passes |
| `jobMetrics` | `false` | `true` adds a metric for each job |

Everyone has the same budget.

## What counts as a unit

GPUs times the weight of the type that the pod's `nodeSelector` asks for. Give the default
weight the value of your best type, or leaving out the selector becomes a discount:

```yaml
gpuTypeLabel: nvidia.com/gpu.product
gpuWeights: small=1,big=3
defaultGPUWeight: "3"
```

Fractions count as such. Requests by GPU memory count as whole GPUs. Jobs without GPUs are
ignored.

## Consolidation

To make room, KAI's consolidation may move a running job, which restarts it, whatever the
priority of either. `ukai-consolidation` limits that, and replaces it: disable `consolidation`.

By default it may place any job, but does not move those that are budgeted, charged, promoted,
or of `protectedPriority` or higher. To choose, list passes as `classes to place > classes to
move`, separated by `;`:

```yaml
consolidation: "high > high,mid,low; mid,low > mid,low"
```

| Pass | Places | May move |
|---|---|---|
| 1 | `high` | `high`, `mid`, `low` |
| then 2 | `mid`, `low` | `mid`, `low` |

`@promoted` stands for promoted jobs, which do not count as their class. Classes that are not
listed are neither placed nor moved. Each pass has its own search budget. A partly running job
counts as running. In `audit` mode the action behaves like KAI's.

KAI disables its consolidation for spread placement. This action must then be disabled by hand.
