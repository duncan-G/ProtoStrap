# Choosing signals, limits and sizes

The formulas and worked examples are in ProtoStrap's `docs/scaling-examples.md` at the
spec's version. This is how to choose.

## Signals by runtime

| Component                 | Threading               | Demand signals                                                  | Grows by                    |
| ------------------------- | ----------------------- | --------------------------------------------------------------- | --------------------------- |
| .NET (ASP.NET, gRPC)      | Multi-threaded          | `cpu: 70%`; a `metric` on thread pool queue length if it stalls | A bigger `request` first, then replicas |
| Node.js (Express, etc.)   | One thread per process  | `cpu: 70%` and `metric: nodejs_eventloop_lag_seconds, target: 0.05` | Replicas only           |
| Python (FastAPI, uvicorn) | One process, the GIL    | `requests: <what one replica serves>/s` (routed services only) and `cpu: 70%` | Replicas only |
| Worker on a queue         | Any                     | `queue: <name>, rate: <jobs/s one replica does>, drain: 60s`    | Replicas, from zero         |

- **Single-threaded runtimes** use one core however big the request, so give them about one
  core or less (`250m`–`1000m`) and scale out. A big request only wastes room.
- **A Node or Python process that blocks** shows little CPU while it falls behind. That's why
  they get the event-loop-lag metric or a requests signal besides CPU.
- **`rate`** is what one replica sustains, measured, not hoped. If the oldest message keeps
  aging while demand is met, the rate is too high.
- **`drain`** is how quickly a backlog should clear. Shorter means more replicas for a burst.

## Limits

Use a limit when more replicas would add errors, not throughput:

- **A third-party rate limit** shared by every replica: `{ replicas: n }`, with
  n = ceil(calls in flight needed to use the limit ÷ calls one replica keeps in flight).
  The code still takes a token from a shared limiter (a token bucket in Redis) before each
  call, and honours `retry-after`.
- **A budget, or a cap that changes:** export a gauge, a replica cap or −1 for none, and
  name it in `limits: [{ metric: <name> }]`. **Export it from a component that always runs**,
  such as the API, so the cap is known while the limited worker is at zero. Without a value
  for 5 minutes, no cap applies. The worker still checks the budget before each call.
- **Database connections:** `{ replicas: n }` with n = the connections PostgreSQL can give ÷
  each replica's pool size.

## Minimums and maximums

- A service that must stay up in production: `min: 2`, in a pool whose `min` plus `spare` is
  at least 2.
- A worker: `min: 0`, so it costs nothing while the queue is empty. With `min: 1` a budget cap
  of 0 still leaves one replica spending.
- `max` caps cost and blast radius. If a pool's `max` VMs can't hold its components at their
  `max` replicas together, the extra replicas wait as pending at peak.

## Sizing a request

1. Run the component under realistic load (locally, or in dev with the sample's load
   generator) and read its steady CPU and peak memory.
2. **Memory:** peak plus about 25%. It is a hard limit: over it, the replica is killed.
3. **CPU:** for a CPU-scaled service, the steady use of one replica at the load you want it
   to carry, divided by the `cpu:` target. For single-threaded runtimes, at most one core.
4. Check it fits: `request ≤ (VM capacity − reserved)`, and for the data VM, every stateful
   component pinned to it together. `protostrap check` refuses one that doesn't.
5. After it runs, the daily report and the operator UI say when a request is too small (CPU
   pinned at the target, replicas at `max`) or too large (low use, few replicas per VM).
   Those changes are edits to the spec, through a pull request.

## Pools

```
usable per VM = (VM capacity − reserved) × target
VMs           = clamp(ceil(requested ÷ usable) + spare, min, max)
```

For example, general VMs of 2 vCPU (`m7g.large`) with `400m` reserved and a 70% target have
(2000m − 400m) × 70% = 1,120m usable each. If replicas often wait as pending while the totals
fit, the requests are too big for the VM size: use a larger type or smaller requests.
