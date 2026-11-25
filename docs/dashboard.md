# Dashboard

`ukai-top` shows users in a terminal how much of their budget is used, the tier of each of their
jobs, and who else waits. It follows changes as they happen, and only reads, with **your own rights**.

```text
ukai  user alice

Budget [############............] 2 of 4 units
Jobs   1 budget, 1 held, 1 promoted, 1 normal, 1 excess

budget: P1 and charged, within the budget
  team/pg-eval                          2  small        running   40m  high

held: wait for your own budget, even if GPUs are free
  team/pg-next                          4  big          waiting   20m  high
    needs 4 units, using 2 of 4

promoted: P2 at P1 priority
  team/pg-train                         2  small        running    4h  mid

normal: P2 at their own priority
  team/pg-sweep                         2  big          running    1h  mid

excess: P2 below their own priority, evicted first
  team/pg-long                          4  big          waiting    1h  mid

P1 waiting, oldest first
No promise: KAI skips jobs that do not fit, and shares GPUs between queues.
big: you are number 3 of 3
   1 bob        team/pg-fit                           2   30m  q
   2 bob        team/pg-wide                          8   25m  q  held
   3 you        team/pg-next                          4   20m  q  held
small: 1, none of them yours
   1 carol      team/pg-other                         1   10m  q

Tiers are computed here by the scheduler's rules, and may differ from it briefly.
```

## For users

    go build ./cmd/ukai-top
    ./ukai-top -user alice -arguments arguments.yaml -n team

`arguments.yaml` must have the scheduler's [arguments](configuration.md), in the form that
[replay](install.md#1-replay-a-snapshot) takes. If your admins keep them in a ConfigMap, name that
instead, and changes are followed:

    ./ukai-top -user alice -configmap ukai-public/ukai-top -n team

| Flag | Meaning |
|---|---|
| `-user` | Whose jobs, as in the user label |
| `-arguments`, `-configmap` | The settings: a file, or a ConfigMap as namespace/name. One of them is required |
| `-n a,b` | Namespaces of your jobs; default: that of the kubeconfig's context |
| `-all-pods` | Read the pods of all namespaces, if you may: exact units and GPU types of others |
| `-anonymous` | Leave out the names of other users and their jobs |
| `-limit` | How many jobs of others to show in each queue; default 10 |
| `-once` | Print once and exit, as if the output is not a terminal |
| `-kubeconfig`, `-context` | If not the usual ones |

| Key | |
|---|---|
| `j` `k`, arrows | Scroll |
| space, `d` `u`, page keys | Scroll by a page |
| `g` `G`, Home, End | Top, bottom |
| `q`, Ctrl-C | Quit |

From 160 columns on, jobs and queues are side by side. `NO_COLOR` turns colours off.

**What to know**

- Tiers are only in the scheduler's memory, so they are computed here, by the same rules. They may be
  ahead of it or behind by a cycle. If the scheduler says why a job is held, that is what is shown.
- In `audit` mode it shows what would be. Nothing is held, promoted or demoted.
- The queue is that of [submission](concepts.md#when-p1-alone-fills-the-cluster). A number in it is no
  promise: KAI skips jobs that do not fit, and between KAI's queues fair share decides. Held jobs keep
  their place.
- A job without a nodeSelector for the type is under `any type`. `~` marks an estimate from the PodGroup
  alone: its GPU type is not known, so it has the default weight, and requests by GPU memory are missing.
- GPUs requested by resource claims are not counted. A pod that KAI has placed but not bound yet counts
  as waiting, for a moment.
- Jobs without pods, such as a Job that Kubernetes is about to restart, are not shown.
- If a connection is lost, the last state stays, with a warning. One that fails silently is noticed
  only after a while.

## For admins

Nothing is installed. It does not use the scheduler's port, whose `/get-snapshot` may have secrets.
Users need to list and watch:

| What | Where | Without it |
|---|---|---|
| PodGroups (`scheduling.run.ai`) | All namespaces, or at least their own | The queue, and the budget if a user has jobs elsewhere, are incomplete |
| Pods | Their own namespaces | Their jobs are estimates |
| Nodes | Optional; only labels are asked for | Running fractions count as whole GPUs |

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: ukai-top}
rules:
  - apiGroups: [scheduling.run.ai]
    resources: [podgroups]
    verbs: [list, watch]
  - apiGroups: [""]        # optional; allows reading all of a node, not only its labels
    resources: [nodes]
    verbs: [list, watch]
```

To publish the settings, with a namespace and name of your choice:

```yaml
apiVersion: v1
kind: ConfigMap
metadata: {name: ukai-top, namespace: ukai-public}
data:
  arguments.yaml: |
    mode: audit
    userLabel: example.com/user
    budgetedClasses: high
    promotableClasses: mid
    budget: "4"
    allowance: "4"
    promotedPriority: "75"
    excessPriority: "45"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ukai-top, namespace: ukai-public}
rules:
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [ukai-top]
    verbs: [list, watch]
```

Bind both to your users. It is a copy: **change it together with the scheduler's**, or the dashboard is
wrong.

PodGroups of all namespaces show everyone's job names, users and classes: decide whether that is
acceptable. `-anonymous` only leaves them out of the screen. **Do not grant pods of all namespaces for
this:** pod specs have environment variables, which may be secrets. `ukai-top` does not keep them, but
whoever has the right can read them. Any user can also pass another's name to `-user`, and sees no more
than RBAC allows.
