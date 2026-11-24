# Architecture

```mermaid
flowchart TD
    main["cmd/scheduler<br/>registers both, then runs KAI"] --> kai[KAI v0.18.9, unmodified]
    kai -- "each cycle" --> plugin["pkg/tierbudget<br/>plugin: reads jobs, applies results"]
    kai -- "each cycle" --> action["ukai-consolidation<br/>wraps KAI's"]
    plugin --> tiers["pkg/tiers<br/>the rules, a pure function"]
    action --> plugin
    replay["cmd/replay, pkg/replay<br/>one cycle on a snapshot"] --> kai
```

## One cycle

```mermaid
sequenceDiagram
    participant K as KAI
    participant P as tierbudget
    participant T as tiers.Assign
    K->>K: snapshot of the cluster
    K->>P: OnSessionOpen
    P->>T: jobs: user, class, units, running, times
    T-->>P: tier and priority of each
    P->>P: publish metrics and status
    P->>K: in enforce mode: set in-memory priorities, add hooks
    loop allocate, consolidate, reclaim, preempt
        K->>P: may this task be placed?
        P-->>K: no, with a reason, if over budget now
    end
    K->>P: OnSessionClose
```

## Decisions

**Priorities are changed only in memory.** KAI rebuilds its copy each cycle, so a crash, a
restart or an uninstall leaves nothing behind. Whether a job is preemptible is decided before,
from its original class.

**The budget is checked again at each placement**, from live pod statuses, which KAI also
updates for simulated placements and restores when it rolls them back. So several jobs placed in
one cycle cannot exceed it together.

**Held jobs also leave the search for victims early.** Otherwise KAI would look for jobs to evict
on their behalf, and conclude that similar jobs of other users cannot be placed either.

**Users are independent.** No decision about one depends on another's jobs, except through GPUs.

**Failures leave stock KAI.** Nothing is applied until all is computed, and panics are caught.

**The status endpoint is registered once.** KAI's server reads its handlers without a lock, so
adding one while serving could crash the process.

**Pods without a PodGroup still count.** Deleting a Job deletes its PodGroup at once. The plugin
finds such pods on their nodes, and remembers the user, or after a restart reads the pod's label.

## Cost

About 0.8 ms per cycle for 1,300 jobs, on a laptop. KAI's cycle is a second or more.

## Limits

- Same budget for everyone.
- One KAI version at a time: it compiles against KAI's internals.
- A stream of small budgeted jobs can delay a large one of the same user.
- `go install` by version does not work, because KAI needs `replace` directives.
