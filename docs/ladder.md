# Ladder (experimental)

A second scheme, next to the budget of [concepts](concepts.md), chosen with `scheme: ladder`.
The budget stays the default. The ladder is a prototype: it has unit tests and tests in KAI
sessions, but has not run on a cluster yet, and its settings may still change. Labels are
whatever you name in `levelLabel`, `protectLabel` and `whenFullLabel`; `example.com/...` below
are examples.

## Why

The budget is one cliff. Up to the budget, a user's work has high priority; past it, P1 waits and
P2 drops. The right budget depends on how busy the cluster is, so one number is either tight for
heavy users on a quiet day, or loose for light users on a busy one.

The ladder replaces the cliff with a slope. The more a user runs, the lower the priority of what
they add, step by step. Nobody is stopped from running: heavy users pay with how easily their
latest jobs are evicted, not with whether they run.

## The ladder

The administrator lists steps, each a priority and a width in units, highest first. The last step
has no width and is the floor.

```
ladder: 75=4, 70=4, 65=4, 60=4, 55=8, 40
```

A user's first 4 units run at 75, the next 4 at 70, and so on. All past 24 units run at 40. Units
are weighted as today, so a GPU of weight 4 takes a whole step and one of weight 1 a quarter.

Keep every step below 100: KAI treats 100 and above as never evicted. Keep the floor above the
classes left alone, for example P3 at 25, so that people still come before agents.

## Which step a job gets

Each cycle, each user's jobs are put in one line, and each job gets the step where its last unit
falls. All of a job counts, so a large job lands lower.

1. **Protected** jobs, see below, at their cost.
2. **Jobs with a level** that may run, running ones first, then pending ones by submission.
3. **Other running jobs**, longest running first.
4. **Other pending jobs**, earliest submitted first.

| Alice, 2 GPUs each | Before | After she submits F with level 75 |
|---|---|---|
| F | | 75 |
| A | 75 | 70 |
| B | 70 | 65 |
| C | 65 | 60 |
| D | 60 | 55 |

Nothing restarts. A new job goes to the end of the line, so submitting more never lowers what
already runs, except a job with a level, which is the point of one.

### Choosing a level

A label, for example `example.com/level: "70"`, asks for a level. The job may run if the user's
protected jobs and jobs with a level of at least 70 fit, with it, in the steps from 70 up. Here
that is 12 units.

If it does not fit, it waits, or with `example.com/when-full: lower` it runs at the highest level
that has room. A running job keeps its level, as a running P1 keeps running over the budget.

A level is only a cap, never a promise of GPUs: as now, a job evicts only lower jobs in its own
KAI queue.

## A share that follows demand

With `ladderWidth: share`, widths are fractions of a fair share instead of units:

```
share = units in the cluster / active users
ladder: 75=0.25, 70=0.25, 65=0.25, 60=0.25, 55=0.5, 40
```

On a cluster of 64 units with 4 active users, the share is 16, and the first 4 units run at 75.
With 16 active users the share is 4, and only the first unit does. Heavy users get much at high
priority when few want GPUs, and less when many do.

**Active** means with a managed job, running or pending, now or within `shareWindow`, by default 1
hour. So the share shrinks as soon as a user comes, and grows back only an hour after they leave.
That keeps steps from moving with every short job. After a restart the window starts empty, so
the share is briefly larger.

## Evictions

**A gap.** With `evictGap: 10`, a job evicts another of the ladder only if its priority is at
least 10 higher, here two steps. Neighbouring steps then never evict each other, so a user who
arrives lowers others by a step without a wave of evictions. Jobs of other classes are evicted as
before.

**Least lost work.** Between jobs of equal priority, the one that started last goes first. Within
a user, the line already puts the newest lowest.

**Grace after start.** KAI's `minruntime` plugin, already on by default, protects jobs for a time
after they start: `defaultPreemptMinRuntime` and `defaultReclaimMinRuntime`, or `preemptMinRuntime`
and `reclaimMinRuntime` on a queue. This needs no change, only configuration.

## Protected jobs

A label, for example `example.com/protect: "true"`, asks that a job is not evicted. KAI's own
`kai.scheduler/preemptibility` stays forbidden, see [security](security.md).

- **It costs more.** A protected job counts its units times `protectCost`, by default 3, at the top
  of the line. So a GPU of weight 4 costs 12, and the user's other jobs drop by three steps.
- **Per user.** The cost must fit in `protectLimit` units, or that fraction of the share.
- **For the cluster.** Protected jobs together may hold at most `protectClusterLimit`, by default
  half of the cluster, earliest started first. Otherwise a cluster full of protected jobs leaves
  light users waiting for days.
- **For a time.** After `protectFor`, by default 24 hours, a job is an ordinary running job again.
  Users then need to save a checkpoint once a day, not be ready for eviction at any moment.
- **Within the queue's quota.** KAI starts a job that cannot be evicted only within its queue's
  deserved quota. On a queue whose deserved quota is 0, protected jobs never start.

A protected job that does not fit waits, or with `example.com/when-full: lower` runs as an
ordinary job. One that runs is never demoted: its user just cannot protect more until it ends.

## How it fits the plugin

- `scheme: ladder`, with `ladderClasses` for the classes it manages. Others are left alone, as
  today. The settings of the budget are then refused.
- A pure function next to `tiers.Assign`, so that it can be tested in the same way.
- Priorities and preemptibility change only in KAI's memory, as now.
- A victim filter for the gap, a victim order for least lost work.
- The status and ukai-top show each job's step and why: "share 4 units, 10 ahead of it".
- **Users are no longer fully independent.** The share counts users, and protection has a limit
  for the cluster. Both are read from the snapshot.

## To decide

- Steps and widths, `evictGap`, `shareWindow`, and the protection settings.
- Whether units in the cluster mean all GPUs or those of one queue. With one node pool, all.
- Whether history should count, as in Slurm's fair share: GPU hours of the past days, not just
  what runs now. It needs stored state, so it is left out of the first version.

## How it will be judged

On kind, with two heavy users who always have work queued and ten light users who now and then
submit a job of one or two GPUs, each run with the same seed:

| Run | Settings |
|---|---|
| Budget | today's default |
| Ladder | steps in units |
| Dynamic ladder | steps as fractions of the share |
| Dynamic ladder with protection | as above, heavy users protect some jobs |

Measured: how long light users wait (median and 95th percentile), GPU hours of heavy users,
evictions and the GPU hours they lost, and idle GPUs.
