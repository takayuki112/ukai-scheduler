# Security

The plugin takes a job's user from the label that `userLabel` names, here called the *user
label*. It does not know the *Kubernetes username* of who submitted the job, and trusts this label
and the priority class. Without an admission policy, users can evade it:

| Way around | Example |
|---|---|
| Another's name in the user label | `example.com/user: somebody-else` |
| A class without a budget | `priorityClassName: p0` |
| **A label that overrides the class** | KAI takes a `priorityClassName` *label*, on the pod or any of its parent objects (`ownerReferences`), over the pod spec. A policy on the spec alone does not see it |
| Not being evicted | label `kai.scheduler/preemptibility`, annotations `kai.scheduler/preemption-delay` and `kai.scheduler/staleness-grace-period` |
| Another's PodGroup | annotation `pod-group-name` |
| Editing PodGroups | the user and class are read there |
| Changing a pod that runs | Its charge is read from the pod, as KAI does: those who may edit the annotations for GPU fractions or memory of their running pods may change it. Not tested |
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

With 1.17, `user.yaml` of the second set does not work. After installing, **try to evade
the rules with an ordinary Kubernetes account**: a policy that does nothing looks like one that works.

| Policy | Does |
|---|---|
| [user.yaml](../examples/kyverno/user.yaml) | Sets the user label to the Kubernetes username of who submits, and keeps it |
| [podgroups.yaml](../examples/kyverno/podgroups.yaml) | Lets only KAI write PodGroups |
| [exempt-class.yaml](../examples/kyverno/exempt-class.yaml) | Limits the exempt class to listed users, by the user label |
| [kai-overrides.yaml](../examples/kyverno/kai-overrides.yaml) | Forbids the labels and annotations above |
| [grace-period.yaml](../examples/kyverno/grace-period.yaml) | Caps the grace period |

`test/kyverno/run.sh` tests both with impersonated Kubernetes usernames. They cover Pods and the built-in
workloads: **add every custom workload type that users may submit**, as they set labels there.
They do not cover those who can act as a service account, other schedulers, or requests for a
long grace period when deleting a pod. That last one keeps GPUs, but does not free budget.

Scaled to zero, Kyverno removes its webhooks, and no policy applies, whatever `failurePolicy` says (seen
with 1.17 and 1.20). Run several replicas, and let few people scale or uninstall it.

Start with `failureAction: Audit`, or `validationActions: [Audit]` in the second set.

## What the scheduler exposes

`/metrics` has users' names (from the user label) and usage, without authentication, on the port that KAI already
uses. `jobMetrics` adds job names. `/tiers` is on localhost only.

The plugin needs no permissions beyond KAI's, and makes no API calls itself.
