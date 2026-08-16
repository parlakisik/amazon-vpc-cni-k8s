# Adaptive IP target: a learned replacement for the warm pool knobs

`WARM_IP_TARGET`, `MINIMUM_IP_TARGET`, `WARM_ENI_TARGET` and `WARM_PREFIX_TARGET`
ask an operator for a constant that has to be right for every hour of every day.
Most nodes do not have a constant demand. A node running a CronJob fan-out needs
30 addresses at the top of the hour and two for the other 55 minutes; a static
setting must either hold 30 all hour and waste the subnet, or hold two and make
every batch wait for EC2.

`ENABLE_ADAPTIVE_IP_TARGET` replaces those constants with a controller that
learns the node's own daily demand pattern and sets the targets for the next
control interval itself.

**Status: experimental, off by default, and shadow mode by default when on.**

## Short answer to "is this possible?"

Yes, with three qualifications that the evaluation below establishes:

1. It has to be **forecasting first and reinforcement learning second**. The
   daily pattern is learned by a plain seasonal model; RL only chooses how much
   headroom to keep on top of that forecast. Pure model-free RL on this control
   loop does not work — see [What did not work](#what-did-not-work).
2. It only beats a static setting where demand is **predictable**. On scheduled
   bursts it is dramatically better; on unscheduled ones it is no better than a
   small static pool, because there is nothing to forecast.
3. It must **decline to act until it has learned something**, and fall back to
   the static knobs until then. Without that gate it is materially worse than
   today's default on a node's first day, which is most of the lifetime of many
   nodes.

## How it works

### The interception point

Everything ipamd does to size the pool funnels through two integers. The warm
and minimum IP targets flow into `datastoreTargetState`, which computes how many
addresses the node is `short` or `over`; every allocation, release, ENI attach
and ENI detach downstream reads only those. Prefix delegation, multi-NIC, custom
networking, security groups for pods and the reconciler are all downstream of it.

So the policy does not replace any allocation logic. It supplies those two
integers, per control interval, on the primary network card only:

```
tracker  ->  forecast  ->  RL policy  ->  (warmIPTarget, minimumIPTarget)  ->  existing ipamd
```

That is the entire blast radius. If the policy is disabled, not ready, or in
shadow mode, ipamd reads the environment variables exactly as it does today.

### What the tracker learns

The tracker (`pkg/ipamd/adaptive/tracker.go`) watches one number: how many IPs
the datastore currently has assigned. It needs no Kubernetes watch, so it keeps
working when the API server is unreachable.

From that series it maintains:

- **A daily profile at 10-minute resolution** (144 slots). Hour buckets are too
  coarse for the workload this exists to serve: 30 pods arriving at the top of
  the hour average out to 0.008 pods/second over an hour, which forecasts
  nothing. Each slot holds an EWMA over days of the arrival rate, the largest
  single-interval arrival burst, and the largest **rise** in assigned IPs.
- **A short-horizon arrival rate and variance**, in counts per control interval.
- **Mean pod lifetime**, by Little's law: with `L` IPs assigned and `λ`
  departures per second, pods live `L/λ` seconds on average. No per-pod
  bookkeeping required.

The rise, not the absolute level, is what has to be pre-warmed. Addresses that
pods already hold are not going anywhere; the only capacity that must be waiting
is the capacity the next few minutes will *add*. And the current slot only
contributes the part of its usual rise that has **not happened yet**, otherwise a
burst in progress gets provisioned twice.

### What the policy decides

State (`pkg/ipamd/adaptive/policy.go`), all discretized:

| dimension | meaning |
| --- | --- |
| day part | quarter of the day, for time-of-day risk appetite |
| demand | forecast pod arrivals over the provisioning lead time |
| slack | how well the current free pool covers that forecast |
| deficit | how far total IPs sit below what the pre-warm window predicts |

Seasonality lives in the tracker's forecast, not in the state space, which keeps
the Q table small enough to converge within a day or two of node uptime.

Action: a pair — how much headroom to keep over the arrival forecast
(0.5x–4x), and how much of the predicted rise to pre-warm (0, half, all). 18
actions total.

Reward: `-(shortage + idle IPs + EC2 churn)`, weighted so an IP request that
cannot be served immediately costs far more than an idle address, but an idle
address is not free — it is a subnet address no other node can use.

### The pre-warm window is the whole economic argument

A static `MINIMUM_IP_TARGET` sized for a burst holds those addresses 24 hours a
day. The policy holds them for `ADAPTIVE_PREWARM_WINDOW` (default two minutes,
long enough to attach the ENIs a predicted burst needs) before each predicted
burst, and lets them go afterwards. On the hourly-batch workload that is the
difference between 16.0 and 2.5 mean idle addresses at the same pod latency.

### Safety

- **Off by default.** `ENABLE_ADAPTIVE_IP_TARGET` is unset, and when set it
  defaults to `shadow` mode: the policy learns and exports metrics but the static
  knobs still drive allocation.
- **Readiness gate.** Even in `enforce` mode the policy reports `ready: false`
  until every 10-minute slot it is about to be responsible for has been observed
  on at least one day. Until then ipamd uses the configured static knobs.
- **Hard clamps.** `ADAPTIVE_MIN_WARM_IP_TARGET` / `ADAPTIVE_MAX_WARM_IP_TARGET`
  / `ADAPTIVE_MAX_MINIMUM_IP_TARGET` bound whatever the policy learns, so a
  mislearned pattern degrades into a static setting rather than into an outage
  or a drained subnet.
- **Asymmetric response.** Targets rise immediately and fall by a quarter at a
  time, no more than once per 30s (ipamd's own `decreaseIPPoolInterval`), so the
  pool cannot oscillate between allocate and release and burn EC2 API quota.
- **Never on secondary network cards.** Multi-NIC attachments keep their fixed
  `WARM_IP_TARGET=1` / `MINIMUM_IP_TARGET=1` behaviour.
- **Not in IPv6 mode**, where the primary ENI's prefix already covers max pods.
- **Model persistence.** The learned model is written to
  `/var/run/aws-node/adaptive-ip-target.json` every five minutes so an ipamd
  restart does not throw away the node's daily profile. A missing or
  incompatible model is not an error; the node just starts cold.

## Evaluation

Reproduce with:

```
go test ./pkg/ipamd/adaptive/sim/ -v
```

### The harness

`pkg/ipamd/adaptive/sim` replays a synthetic pod trace against a model of the
ipamd pool manager and the EC2 control plane. It deliberately mirrors
`pkg/ipamd`: the same short/over arithmetic, the same 5s control interval, the
same 30s IP cooldown, the same one-ENI-per-tick allocation, the same
"assign to existing ENIs before attaching a new one" ordering, the same
`getDeletableENI` retention rules and 60s minimum ENI lifetime, and a control
loop that blocks while a synchronous EC2 call is outstanding.
`TestSimulatorMirrorsIpamdTargetMath` pins the arithmetic against the cases in
`docs/eni-and-ip-target.md` so the harness cannot silently drift.

Node: m5.xlarge-like — 4 ENIs x 14 secondary IPs. Ten days of trace per
workload; the policy trains on the first seven and **every policy is scored on
the same held-out final three**.

Metrics. `delayed%` is the share of pods that found no free IP and had to wait;
`waitSec` is the total pod-seconds of startup delay; `idleIPs` is the mean number
of addresses held and unused; `cost` combines them. The cost function is
deliberately **not** the agent's reward — the agent is scored per control
interval on requests it failed to serve, this charges the delay pods actually
suffered — so an agent that games its reward does not game the score.

### Results, held-out days

```
workload "steady" - long-lived deployment pods, ~6/hour, no daily pattern
  policy                                delayed%   waitSec   idleIPs   ec2calls
  WARM_ENI_TARGET=1                         0.00         0      21.4        289
  WARM_IP_TARGET=1                          0.00         0       1.0        856
  WARM_IP_TARGET=5                          0.00         0       5.0        856
  WARM_IP_TARGET=16                         0.00         0      16.0        859
  WARM_IP_TARGET=5,MINIMUM_IP_TARGET=16     0.00         0       5.6        836
  ADAPTIVE (RL)                             0.00         0       1.2       2037

workload "diurnal" - business-hours service, arrivals peak at 15:00
  WARM_ENI_TARGET=1                         0.00         0      21.8        441
  WARM_IP_TARGET=1                          3.25       126       1.0       2442
  WARM_IP_TARGET=5                          0.00         0       5.0       2423
  WARM_IP_TARGET=16                         0.00         0      16.0       2388
  WARM_IP_TARGET=5,MINIMUM_IP_TARGET=16     0.00         0      10.3       2007
  ADAPTIVE (RL)                             0.07         2       2.4       5583

workload "hourly-batch" - CronJob fan-out on the hour, short-lived pods
  WARM_ENI_TARGET=1                        15.51      1426      22.1        804
  WARM_IP_TARGET=1                         74.69     42484       1.0       2331
  WARM_IP_TARGET=5                         37.35      1696       5.0       1717
  WARM_IP_TARGET=16                         0.08         2      16.0       1556
  WARM_IP_TARGET=5,MINIMUM_IP_TARGET=16    13.73       461      14.1        623
  ADAPTIVE (RL)                             0.08         2       2.5       3884

workload "bursty-rollout" - steady base plus 3 unscheduled 20-40 pod rollouts/day
  WARM_ENI_TARGET=1                         2.30        36      20.5        104
  WARM_IP_TARGET=1                         46.99     14960       1.0       1138
  WARM_IP_TARGET=5                          8.87       425       5.0        962
  WARM_IP_TARGET=16                         0.00         0      16.0        921
  WARM_IP_TARGET=5,MINIMUM_IP_TARGET=16     4.96       151       7.8        405
  ADAPTIVE (RL)                            14.72       455       2.2       2600
```

**The headline is hourly-batch.** The learned policy matches the best static
setting's pod latency exactly — 0.08% delayed, 2 pod-seconds of delay across
1296 pods — while holding **2.5 idle addresses instead of 16.0**. To get that
latency from a static knob you must hold the burst-sized pool around the clock.

**Steady and diurnal are ties or small wins.** On a flat workload nothing beats
a tiny fixed pool, and the policy converges to within 0.2 addresses of it. On
diurnal it lands on the efficient frontier between `WARM_IP_TARGET=1` (leaner,
3.25% of pods delayed) and `WARM_IP_TARGET=5` (no delay, twice the addresses).

**Bursty-rollout is the honest loss.** Unscheduled rollouts have no seasonal
signature, so there is nothing to forecast and the policy behaves like a lean
static pool: 14.72% delayed at 2.2 idle addresses, between `WARM_IP_TARGET=1`
(46.99% at 1.0) and `WARM_IP_TARGET=5` (8.87% at 5.0). Only a large static pool
avoids the delay, at 16 addresses per node. If your bursts are unscheduled, this
feature will not help you.

### Cost sensitivity

Ranking a policy needs a price for a pod-second of startup delay in units of
idle-address-seconds, and that price is a judgement call. So it is reported
across a range rather than buried in a constant:

```
  policy                                  delay=1     delay=20    delay=100    delay=500
  WARM_ENI_TARGET=1                       5739435      5746380      5775620      5921820
  WARM_IP_TARGET=1                         301418       574875      1726275      7483275
  WARM_IP_TARGET=5                        1356078      1366153      1408573      1620673
  WARM_IP_TARGET=16                       4296719      4296728      4296768      4296968
  WARM_IP_TARGET=5,MINIMUM_IP_TARGET=16   2512417      2515324      2527564      2588764
  ADAPTIVE (RL)                            584294       586474       595654       641554
```

The number that matters is not any single column, it is the **slope**. Across a
500x change in the price of pod latency the learned policy's cost moves 10%,
while the leanest static setting's moves 25x. Almost none of the learned
policy's cost is pod delay — it stays lean *and* rarely makes a pod wait, which
is the property a fleet-wide default needs. The crossover is around delay=20:
below it the right answer really is to hold nothing and let pods wait, and the
policy is honestly beaten there.

### Cold start

Day one on a fresh node, with no model:

```
  steady          delayed 0.00% (default 0.00%),  idle 20.9 (default 22.3)
  diurnal         delayed 0.00% (default 0.00%),  idle 22.3 (default 22.3)
  hourly-batch    delayed 14.12% (default 14.12%), idle 22.3 (default 22.6)
  bursty-rollout  delayed 0.53% (default 0.53%),  idle 18.6 (default 20.7)
```

Identical to the shipped default, because the readiness gate keeps the static
knobs in charge until the profile fills in. Before the gate existed, the
cold-start policy delayed 54% of pods on hourly-batch against the default's 14%.

## What did not work

Recorded because the failures are the interesting part, and because each one is
a trap for anyone extending this.

- **Plain Q-learning cannot learn to pre-warm.** The cost of not pre-warming
  lands ~120 control intervals after the decision that caused it. One-step
  Q-learning attributes it to whichever state happens to be current when the
  burst hits, which is far too late to act on, so the agent learned never to
  pre-warm. Potential-based reward shaping (Ng, Harada & Russell, 1999) on the
  gap between the pool and the forecast fixes it and provably leaves the optimal
  policy unchanged. This is why the design is forecasting-first: model-free RL
  alone is the wrong tool for a control loop with this much delay.
- **Hour-of-day buckets destroy the signal.** Averaging a 30-pod fan-out over an
  hour yields a forecast of 0.2 pods. Ten-minute slots keep it.
- **Absolute peak level as the pre-warm floor is wrong twice.** It double-counts
  a burst in progress, and on a slowly drifting workload it makes the node hold
  several idle addresses forever. The expected *remaining rise* is the right
  quantity.
- **Scaling the burst buffer by the horizon instead of its square root.**
  Arrivals in consecutive intervals are near-independent, so the spread of their
  sum grows as `sqrt(n)`. Using `n` inflated the buffer by an order of magnitude
  and pinned the pool at the instance maximum — where `short` stayed positive
  forever and the decrease path was never reached at all.
- **A reactive "recent maximum arrivals" term.** Tried as an answer to
  unscheduled bursts. It raised the pool on every workload and barely moved pod
  delay on the one workload it targeted, because a 40-pod rollout outruns a 20s
  ENI attach no matter how the target is set. Reverted; kept only as an
  introspection signal.

## Configuration

| variable | default | meaning |
| --- | --- | --- |
| `ENABLE_ADAPTIVE_IP_TARGET` | `false` | master switch |
| `ADAPTIVE_IP_TARGET_MODE` | `shadow` | `shadow` learns and reports only; `enforce` drives the pool |
| `ADAPTIVE_MIN_WARM_IP_TARGET` | `1` | hard floor on the learned warm target |
| `ADAPTIVE_MAX_WARM_IP_TARGET` | `64` | hard ceiling on the learned warm target |
| `ADAPTIVE_MAX_MINIMUM_IP_TARGET` | `128` | hard ceiling on the learned floor for total IPs |
| `ADAPTIVE_LEAD_TIME_SECONDS` | `30` | how long new capacity takes to become usable |
| `ADAPTIVE_IP_TARGET_STATE_PATH` | `/var/run/aws-node/adaptive-ip-target.json` | where the learned model is persisted |

When enabled, the policy overrides `WARM_IP_TARGET` and `MINIMUM_IP_TARGET` on
the primary network card once it is ready. `WARM_ENI_TARGET` and
`WARM_PREFIX_TARGET` are what it falls back to until then.

## Observability

Metrics:

- `awscni_adaptive_warm_ip_target` — the warm target the policy chose
- `awscni_adaptive_minimum_ip_target` — the floor on total IPs it chose
- `awscni_adaptive_ready` — 1 when it is driving the pool, 0 while the static
  targets still are

Introspection: `curl http://127.0.0.1:61679/v1/adaptive-ip-target` returns the
current state key, the chosen targets, the headroom and pre-warm factors behind
them, the last reward, the exploration rate, how many slots of the daily profile
have been learned, and the estimated mean pod lifetime. It is the first thing to
look at when the pool is not the size you expected.

## Suggested rollout

1. Turn it on in `shadow` mode on a representative node group. Compare
   `awscni_adaptive_warm_ip_target` against the pool you actually run.
2. After a few days, check `awscni_adaptive_ready` is steady at 1 and the
   targets track the shape of your workload rather than jittering.
3. Move a canary group to `enforce` with `ADAPTIVE_MAX_WARM_IP_TARGET` set
   conservatively, and watch pod scheduling latency and
   `awscni_no_available_ip_addresses`.
4. Widen only if the canary holds. If your bursts are unscheduled, the
   evaluation above says this will not help — stay on a static target.

## Known limitations

- Per-node only. Nothing is shared across a node group, so every node relearns
  the same pattern, and a node that lives less than a day never becomes ready.
  Seeding the model from a node group would be the obvious next step.
- Arrivals are derived from the assigned-IP level, so a pod starting and another
  finishing inside one 5s interval cancel out. `Tracker.RecordAssign` /
  `RecordRelease` accept exact counts if the CNI RPC path is ever wired to feed
  them; the bias otherwise affects only the departure rate, never the arrival
  forecast that drives safety.
- Evaluated only in simulation, against synthetic traces. The simulator mirrors
  ipamd's arithmetic and the EC2 latencies, but it is not a cluster. Nothing here
  substitutes for a shadow-mode soak on real nodes.
- The reward weights encode one opinion about what an idle address is worth.
  They are `Config` fields, not constants, but they are not tunable from the
  environment yet.
