# Observability

All of it works in both modes. In `audit` mode, tiers and priorities are what they would be.

## Metrics

On KAI's `/metrics`:

| Metric | Labels | Meaning |
|---|---|---|
| `ukai_active` | `mode` | 1, or 0 if stock behaviour applies because of an error |
| `ukai_last_session_timestamp_seconds` | | When the last scheduling cycle began |
| `ukai_panics_total` | | Should stay 0 |
| `ukai_owner_budget_units` | | Budget of each user |
| `ukai_jobs`, `ukai_units` | `owner`, `tier`, `state` | `state` is `running` or `pending` |
| `ukai_tier_changes_total` | `from`, `to` | Steady growth without new jobs means tiers flap |
| `ukai_job_priority` | `namespace`, `podgroup`, `owner`, `tier` | With `jobMetrics` |

The label `owner` is the job's user. Tiers are `budget`, `held`, `promoted`, `normal` and `excess`. Useful queries:

```promql
# Use of the budget by user
ukai_units{tier="budget",state="running"}
# Waiting for budget
sum by (owner) (ukai_units{tier="held"})
# Alerts
ukai_active == 0
increase(ukai_panics_total[10m]) > 0
time() - ukai_last_session_timestamp_seconds > 120
```

With several replicas, only the leader has the gauges.

## Status endpoint

Every managed job, with the reason if it is held. On KAI's plugin server, on localhost only:

    kubectl -n kai-scheduler port-forward deployment/kai-scheduler-default 8081
    curl 'localhost:8081/tiers?owner=bob'

```json
{
  "time": "2026-01-01T12:00:00Z",
  "mode": "enforce",
  "budget": 4,
  "jobs": [
    {
      "namespace": "team", "name": "pg-eval2", "owner": "bob", "class": "high",
      "running": false, "units": 2, "tier": "held",
      "priority": 75, "classPriority": 75,
      "reason": "needs 2 units, using 4 of 4"
    }
  ]
}
```

`orphan` is set if pods are left of a job whose PodGroup is gone.

## Events and logs

A held pod has the reason in its events. The log has a line when a job changes tier:

    kubectl -n kai-scheduler logs deployment/kai-scheduler-default | grep tierbudget
