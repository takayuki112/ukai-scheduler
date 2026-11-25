# User guide

For users: those who submit jobs. Your cluster's class names and numbers may differ: ask your admins.

## Which class?

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="images/which-dark.svg">
  <img src="images/which-light.svg" alt="Need it soon, and is it small? P1, which counts against your budget. If not, would losing progress hurt? P2, promoted while your budget is unused. If not, P3, whatever is free">
</picture>

Do not use P1 for everything: beyond the budget it waits even when GPUs are idle, where a P2
would run.

## Why is my job waiting?

    kubectl describe pod <pod>

| Message | Meaning | What to do |
|---|---|---|
| `user budget: needs 2 units, using 4 of 4` | Your budget is in use | Wait, stop a P1, or resubmit as P2 |
| `user budget: needs 8 units, budget is 4` | It can never fit | Resubmit as P2, or smaller |
| `user budget: no user: label ... is not set` | The label that says whose job it is, is missing | Set it, or ask your admins |
| anything else | KAI's own reason, such as no free GPUs | |

The [dashboard](dashboard.md) shows this live, with your budget and the queue.

## Your job was evicted

Evicted pods get SIGTERM, then a grace period (30 seconds unless the job sets another), then
SIGKILL. So:

1. **Save a checkpoint on SIGTERM**, and at intervals.
2. **Resume from the last checkpoint** on start.
3. **Exit with an error on SIGTERM, not with 0.** A Job whose program exits 0 counts as
   complete, and does not run again.

```python
import os, signal, sys
import torch

CHECKPOINT = "/shared/my-run/last.pt"  # must outlive the pod
stop = False

def on_term(signum, frame):
    global stop
    stop = True  # only note it: saving here could interrupt a step or another save

def save(step):
    torch.save({"model": model.state_dict(), "optimizer": optimizer.state_dict(), "step": step},
               CHECKPOINT + ".tmp")
    os.replace(CHECKPOINT + ".tmp", CHECKPOINT)  # a half-written file never replaces a good one

signal.signal(signal.SIGTERM, on_term)
start = 0
if os.path.exists(CHECKPOINT):
    c = torch.load(CHECKPOINT, map_location="cpu")
    model.load_state_dict(c["model"])
    optimizer.load_state_dict(c["optimizer"])
    start = c["step"]

for step in range(start, total):
    train_step(step)
    if stop:
        save(step + 1)
        sys.exit(143)  # not 0: there is more to do
    if (step + 1) % 500 == 0:
        save(step + 1)  # crashes of a node give no warning
```

Also save whatever else your run needs to continue, such as the learning rate schedule and the place
in the data.

In the Job:

```yaml
spec:
  backoffLimit: 20
  podFailurePolicy:  # evictions do not use up retries, if your admins have set this up
    rules:
      - action: Ignore
        onPodConditions: [{type: DisruptionTarget, status: "True"}]
  template:
    spec:
      restartPolicy: Never
      terminationGracePeriodSeconds: 120  # the longest step and a save, with room to spare
```

Your cluster may limit the grace period. The job keeps its GPUs until it has ended, so a long one makes
others wait. Ask your admins whether they [set up evictions and retries](install.md#evictions-and-retries).

Kubernetes waits before it replaces a Job's evicted pod, longer each time, up to 10 minutes. Until
then there is no pod to see.

**What often goes wrong**

| Problem | What to do |
|---|---|
| A shell script starts Python, and does not pass SIGTERM on | Start it with `exec python train.py`, or without a shell |
| The program has no handler, and runs as the container's first process | The kernel ignores SIGTERM for it, so it runs until SIGKILL. Add the handler |
| The checkpoint was on the pod's own disk | Use storage that outlives the pod |
| The step is longer than the grace period | Check the flag more often, or raise the grace period |
| Several processes, as with `torchrun`: some stop and others wait for them | Agree on it, as below |
| Workers of a `DataLoader` inherit the handler | In `worker_init_fn`: `signal.signal(signal.SIGTERM, signal.SIG_DFL)` |
| The framework saves by itself | Check its documentation, and that it exits with an error |

With several processes, all must stop at the same step, and one of them saves:

```python
flag = torch.tensor(int(stop), device=device)
torch.distributed.all_reduce(flag, op=torch.distributed.ReduceOp.MAX)
if flag.item():
    if torch.distributed.get_rank() == 0:
        save(step + 1)
    torch.distributed.barrier()  # nobody leaves before it is saved
    sys.exit(143)
```

Each process must get SIGTERM or hear of it this way. With sharded models (FSDP, DeepSpeed), all of
them take part in saving: use the framework's own way.

**Try it.** While the job runs, `kubectl delete pod <pod>`. It should save, come back, and go on from the
right step. This is not an eviction, so it may use up a retry.

## Example

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: train
  labels:
    kai.scheduler/queue: team
    example.com/user: alice        # may be set for you
spec:
  backoffLimit: 20
  template:
    metadata:
      labels:
        kai.scheduler/queue: team
        example.com/user: alice    # also here
    spec:
      schedulerName: kai-scheduler
      priorityClassName: mid        # your P2
      restartPolicy: Never
      terminationGracePeriodSeconds: 120
      containers:
        - name: main
          image: my/image
          resources:
            limits: {nvidia.com/gpu: 4}
```

## My job says P2. Is it promoted?

The class shown never changes, as promotion is only in the scheduler. Your admins may offer a
dashboard from the [metrics](observability.md).
