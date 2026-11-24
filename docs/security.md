# Security

The plugin trusts the owner label and the priority class. Without an admission policy, people
can evade it:

| Way around | Example |
|---|---|
| Another's owner label | `example.com/owner: somebody-else` |
| A class without a budget | `priorityClassName: p0` |
| **A label that overrides the class** | KAI takes a `priorityClassName` *label*, on the pod or any of its owners, over the pod spec. A policy on the spec alone does not see it |
| Not being evicted | label `kai.scheduler/preemptibility`, annotations `kai.scheduler/preemption-delay` and `kai.scheduler/staleness-grace-period` |
| Another's PodGroup | annotation `pod-group-name` |
| Editing PodGroups | the owner and class are read there |
| Staying after eviction | a long `terminationGracePeriodSeconds` |
| Another scheduler | `schedulerName: default-scheduler` |

Several are independent of this project, and worth closing with stock KAI too.

## Kyverno examples

Two sets that do the same, with placeholder names to adapt. **Kyverno 1.20 no longer enforces
`ClusterPolicy`**, and says nothing at run time, so use the right one:

| Your Kyverno | Use | Tested with |
|---|---|---|
| before 1.20 | [examples/kyverno](../examples/kyverno) | 1.17 |
| 1.20 and later | [examples/kyverno-cel](../examples/kyverno-cel) | 1.20 |

With 1.17, the owner policy of the second set does not work. After installing, **try to evade
the rules as an ordinary user**: a policy that does nothing looks like one that works.

| Policy | Does |
|---|---|
| [owner.yaml](../examples/kyverno/owner.yaml) | Sets the owner to who submits, and keeps it |
| [podgroups.yaml](../examples/kyverno/podgroups.yaml) | Lets only KAI write PodGroups |
| [exempt-class.yaml](../examples/kyverno/exempt-class.yaml) | Limits the exempt class to listed owners |
| [kai-overrides.yaml](../examples/kyverno/kai-overrides.yaml) | Forbids the labels and annotations above |
| [grace-period.yaml](../examples/kyverno/grace-period.yaml) | Caps the grace period |

`test/kyverno/run.sh` tests both with impersonated users. They cover Pods and the built-in
workloads: **add every custom workload type that people may submit**, as they set labels there.
They do not cover users who can act as a service account, other schedulers, or requests for a
long grace period when deleting a pod. That last one keeps GPUs, but does not free budget.

Start with `failureAction: Audit`, or `validationActions: [Audit]` in the second set.

## What the scheduler exposes

`/metrics` has owners' names and usage, without authentication, on the port that KAI already
uses. `jobMetrics` adds job names. `/tiers` is on localhost only.

The plugin needs no permissions beyond KAI's, and makes no API calls itself.
