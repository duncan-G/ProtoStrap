# Scaling examples

Worked examples of the replica scaling rules in the [README](../README.md#scaling-replicas).
Each one ends in the same formula:

```
desired replicas = clamp(min(demand, limit cap), min replicas, max replicas)
```

## Replicas from CPU

A .NET service requests 500m CPU per replica, runs 5 replicas, and targets 70% CPU usage
per replica. ProtoStrap keeps a rolling average of each replica's CPU usage as a share of
its request, and averages that across the replicas:

```
demand = ceil(replicas × average usage / target)
       = ceil(5 × 70% / 70%) = 5            # on target: no change
```

Because every replica requests the same CPU, this is the same as dividing the service's
total CPU use by the work one replica should carry (500m × 70% = 350m):

```
demand = ceil(total CPU used / (request × target))
       = ceil(1750m / 350m) = 5
```

| Average usage | Total CPU used | Demand                 |
| ------------- | -------------- | ---------------------- |
| 70%           | 1750m          | ceil(5 × 70 / 70) = 5  |
| 105%          | 2625m          | ceil(5 × 105 / 70) = 8 |
| 35%           | 875m           | ceil(5 × 35 / 70) = 3  |

Demand then goes through the limit cap and the minimum and maximum replicas, as above.

## Replicas from a queue

A .NET worker drains an SQS queue. One replica handles about 2 jobs per second, and the
queue should be cleared within 60 seconds, so one replica covers 2 × 60 = 120 jobs. The
work is everything not yet finished: messages waiting in the queue plus messages already
being handled. That includes those held by ephemeral replicas on general VMs, because the
count covers every replica of the worker, wherever it runs.

```
work   = waiting + in flight       # SQS ApproximateNumberOfMessagesVisible + …NotVisible
       = 900 + 60 = 960
demand = ceil(work / (jobs per second per replica × drain seconds))
       = ceil(960 / (2 × 60)) = 8
```

The same worker at four moments, with a maximum of 20 replicas:

| Situation                      | Waiting in queue | Being handled (in flight) | Work  | Calculation           | Replicas                          |
| ------------------------------ | ---------------- | ------------------------- | ----- | --------------------- | --------------------------------- |
| Idle                           | 0                | 0                         | 0     | —                     | 0, with no replicas anywhere      |
| Queue empty, last jobs running | 0                | 2                         | 2     | ceil(2 / 120) = 1     | 1, kept until those 2 jobs finish |
| Normal load                    | 900              | 60                        | 960   | ceil(960 / 120) = 8   | 8                                 |
| Spike                          | 2,900            | 100                       | 3,000 | ceil(3000 / 120) = 25 | 20, because the maximum caps it   |

The worker only scales to zero when nothing is waiting or in flight. If the age of the
oldest message keeps growing while demand is met, the jobs-per-second estimate is too
high: measure it again.

## A rate limit

A worker calls a third-party API whose rate limit, 30 calls per minute, is shared by all
its replicas. Each job makes one call, a call takes about 20 seconds, and one replica runs
up to 20 jobs at once, because it mostly waits.

The limit cap comes from how many calls must be in flight to use the whole limit:

```
calls in flight to use the limit = 30 per minute ÷ 60 × 20 seconds = 10
limit cap = ceil(10 / 20 jobs per replica) = 1 replica
```

With 960 jobs of work, the queue alone asks for ceil(960 / (1 job per second × 60)) = 16
replicas. The cap wins:

```
desired replicas = clamp(min(16, 1), 0, 20) = 1
```

The queue drains at the limit's pace, 30 jobs per minute, so 960 jobs take about 32
minutes however many replicas run. Extra replicas would only queue for the same calls.

The cap keeps the replica count sensible, but the worker still enforces the limit itself:
every replica takes a token from one shared limiter (for example a token bucket in Redis)
before each call, and honours `retry-after` on 429 responses. The limiter's wait time
shows whether the limit is what's holding the queue back:

| Queue      | Limiter wait          | Meaning                         | Action                         |
| ---------- | --------------------- | ------------------------------- | ------------------------------ |
| Work waits | About 0, tokens spare | The workers are the bottleneck  | Scale out, up to the limit cap |
| Work waits | Above 0, sustained    | The limit is the bottleneck     | Hold: more replicas won't help |
| Work waits | High for minutes      | Too many replicas for the limit | Remove one replica             |
| Empty      | Any                   | Nothing to do                   | Scale down on demand           |

If calls get faster, the same limit needs fewer calls in flight, so recompute the cap from
the measured call time.

## A monthly budget

A worker's LLM calls have a budget of $300 per calendar month (UTC), and a job costs about
$0.02. A budget is a limit that resets: until it runs out it caps nothing, and once it
does the cap is 0 until the 1st of the next month.

```
budget cap = 0      if month-to-date spend ≥ $300
             none   otherwise
```

| Date (UTC) | Spent this month | Budget cap | Queue demand | Desired replicas          |
| ---------- | ---------------- | ---------- | ------------ | ------------------------- |
| Oct 10     | $120             | none       | 4            | 4                         |
| Oct 24     | $300             | 0          | 6            | 0: in-flight jobs finish  |
| Oct 31     | $300             | 0          | 9            | 0: jobs wait in the queue |
| Nov 1      | $0               | none       | 12           | 12, then any other caps   |

Desired replicas only reach 0 if the worker's minimum is 0. With a minimum of 1, the clamp
keeps one replica running and spending.

Things to get right:

- **Track spend yourself.** Provider billing dashboards lag by hours. Have each worker
  record every call's cost (tokens used × price) in a month-to-date total, for example a
  Redis counter keyed by month, and use that as the budget metric.
- **Enforce it per call, not only by scaling.** Scaling to zero takes time, and calls in
  flight keep spending. Before each call, a worker reserves the call's estimated cost
  against the remaining budget in one atomic step, and doesn't make the call if there
  isn't enough. The job goes back to the queue.
- **Keep waiting jobs from expiring.** SQS deletes a message after its retention period,
  at most 14 days. Budget used up on the 10th means jobs could wait three weeks, so when
  the budget is exhausted, move new jobs out of SQS into a PostgreSQL table, or reject
  them with a clear status, and requeue them on the 1st.
- **Expect a surge on the 1st.** The month's backlog arrives at once, so queue demand
  jumps and any other limits, such as a rate limit, decide how fast it drains.

If running out on the 24th is worse than working more slowly all month, pace the budget
instead: allow at most the remaining budget divided by the days left to be spent each day,
and cap replicas the same way when today's allowance is used up.
