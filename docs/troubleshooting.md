# Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| Nothing is held | `mode` is `audit`, the default, or the configuration is invalid | `ukai_active`, its `mode`, and the log |
| A change has no effect | The scheduler reads its configuration at the start | Restart it |
| `Plugin tierbudget is disabled, invalid configuration: ...` | As it says; stock behaviour applies | Arguments are strings, and replaced as a whole |
| Scheduler crash-loops: `failed to find Action ukai-consolidation` | Stock image with this project's configuration | [Rolling back](install.md#rolling-back) |
| `Failed to get plugin tierbudget` | The same, for the plugin | |
| A user's jobs do not count | The user label is missing, or names somebody else | `/tiers` |
| Budget in use, but no such job | Its pods still terminate after it was deleted | `"orphan": true` in `/tiers` |
| The budget of a lost node's jobs stays in use | Kubernetes keeps their pods until it knows that they have ended | Delete the node, or taint it `node.kubernetes.io/out-of-service` |
| A job has another class than its pod spec | A `priorityClassName` label overrides it | [Security](security.md) |
| A P1 moved though it should not | KAI's own `consolidation` is enabled as well; the log warns | [Consolidation](configuration.md#consolidation) |
| An evicted job does not come back | Its program exited 0 on SIGTERM | [User guide](user-guide.md#your-job-was-evicted) |
| A large P1 never starts | Smaller ones of the same user keep taking its place | The user has to pause them |
| Pods have no PodGroup, nothing is scheduled | KAI's pod-grouper is down | Not this project |

## Reporting a problem

Include the KAI version, the plugin's arguments, the log lines with `tierbudget`, and `/tiers`.
**Do not attach a snapshot** in public: it may contain secrets.
