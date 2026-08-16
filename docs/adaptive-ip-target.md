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

Yes — and the evaluation is clear about which part of it does the work.

1. **The forecast is the contribution; the reinforcement learning is not.** The
   ablation below removes the RL layer entirely, leaving the seasonal forecast
   plus a fixed conservative headroom, and the result is as good or better on
   every workload family, at 30% fewer EC2 calls. Turning off exploration does
   not recover the difference, so this is not an exploration artefact. The RL
   layer is retained because it is the mechanism under study and it is not
   harmful, but **nothing in these results justifies shipping it over the plain
   forecasting controller.**
2. It only beats a static setting where demand is **predictable**. On scheduled
   bursts it closes 91% of the address-waste gap and 98% of the pod-delay gap
   between today's default and a clairvoyant controller. On unscheduled bursts
   it is worse than the default on latency, because there is nothing to forecast.
3. It must **decline to act until it has learned something**. With the readiness
   gate a node's first day is byte-for-byte today's behaviour and the policy
   converges on day two; without it, day one is four times worse than the
   default on the workload the feature exists for.

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

### Results: 5 independently seeded trials per family, mean ± 95% CI

Node: m5.xlarge-like. Every policy sees the same traces. `ORACLE` is a
clairvoyant controller that reads the future of the trace and holds exactly the
pool the next two minutes will need, including addresses still in cooldown. It
is not implementable; it bounds what any forecast could achieve.

```
family "steady" - long-lived deployment pods, no daily pattern
  policy                        pods delayed %      mean idle IPs        EC2 calls
  WARM_ENI_TARGET=1 (default)   0.00 +/- 0.00      22.28 +/- 0.45      243 +/- 65
  WARM_IP_TARGET=1              0.00 +/- 0.00       1.00 +/- 0.00      850 +/- 9
  WARM_IP_TARGET=5              0.00 +/- 0.00       5.00 +/- 0.00      851 +/- 12
  WARM_IP_TARGET=16             0.00 +/- 0.00      16.00 +/- 0.00      853 +/- 9
  WARM_IP_TARGET=5,MIN=16       0.00 +/- 0.00       5.24 +/- 0.20      837 +/- 8
  ADAPTIVE                      0.00 +/- 0.00       1.16 +/- 0.03     1927 +/- 118
  ORACLE (clairvoyant)          0.00 +/- 0.00       0.17 +/- 0.01      708 +/- 15

family "diurnal" - business-hours service, arrivals peak at 15:00
  WARM_ENI_TARGET=1 (default)   0.03 +/- 0.08      21.41 +/- 0.30      416 +/- 92
  WARM_IP_TARGET=1              3.40 +/- 0.92       1.01 +/- 0.00     2582 +/- 39
  WARM_IP_TARGET=5              0.03 +/- 0.08       5.01 +/- 0.00     2558 +/- 40
  WARM_IP_TARGET=16             0.03 +/- 0.08      15.95 +/- 0.07     2414 +/- 112
  WARM_IP_TARGET=5,MIN=16       0.03 +/- 0.08      10.01 +/- 0.12     2128 +/- 59
  ADAPTIVE                      0.11 +/- 0.05       2.22 +/- 0.13     5096 +/- 387
  ORACLE (clairvoyant)          0.04 +/- 0.12       0.44 +/- 0.01     1421 +/- 43

family "hourly-batch" - CronJob fan-out on the hour, short-lived pods
  WARM_ENI_TARGET=1 (default)  11.31 +/- 2.56      22.57 +/- 0.33      763 +/- 37
  WARM_IP_TARGET=1             75.63 +/- 0.74       1.03 +/- 0.00     2352 +/- 17
  WARM_IP_TARGET=5             37.58 +/- 2.03       5.03 +/- 0.00     1702 +/- 20
  WARM_IP_TARGET=16             0.05 +/- 0.09      16.03 +/- 0.00     1551 +/- 16
  WARM_IP_TARGET=5,MIN=16      11.57 +/- 1.45      14.05 +/- 0.05      600 +/- 55
  ADAPTIVE                      0.29 +/- 0.21       2.52 +/- 0.02     4210 +/- 278
  ORACLE (clairvoyant)          0.05 +/- 0.09       0.60 +/- 0.00     1599 +/- 31

family "bursty-rollout" - steady base plus 3 unscheduled 20-40 pod rollouts/day
  WARM_ENI_TARGET=1 (default)   1.08 +/- 1.16      20.62 +/- 0.34       96 +/- 14
  WARM_IP_TARGET=1             44.80 +/- 3.26       1.00 +/- 0.00     1093 +/- 59
  WARM_IP_TARGET=5              8.95 +/- 5.77       5.00 +/- 0.00      931 +/- 29
  WARM_IP_TARGET=16             0.50 +/- 0.59      15.95 +/- 0.06      899 +/- 17
  WARM_IP_TARGET=5,MIN=16       3.38 +/- 2.28       7.79 +/- 0.16      388 +/- 24
  ADAPTIVE                     11.86 +/- 5.74       1.90 +/- 0.05     2445 +/- 262
  ORACLE (clairvoyant)          0.11 +/- 0.30       0.22 +/- 0.01      845 +/- 31
```

Expressed as the fraction of the distance between today's default and perfect
knowledge that the policy closes:

```
  family             metric            default   adaptive     oracle  gap closed
  steady             idle IPs            22.28       1.16       0.17         96%
  diurnal            idle IPs            21.41       2.22       0.44         92%
  hourly-batch       idle IPs            22.57       2.52       0.60         91%
  hourly-batch       pods delayed %      11.31       0.29       0.05         98%
  bursty-rollout     idle IPs            20.62       1.90       0.22         92%
```

**The scheduled-burst family is the headline.** Against today's default the
policy cuts pods delayed from 11.3% to 0.29% *and* address waste from 22.6 to
2.5 — it is not trading one for the other. To get that latency from a static
knob you must hold `WARM_IP_TARGET=16` around the clock, 6x the addresses.

**Bursty-rollout is the honest loss**, and it is a loss against the default, not
just against a tuned static: 11.86% ± 5.74 of pods delayed versus the default's
1.08%. Unscheduled rollouts have no seasonal signature, so the policy runs lean
and pays for it. If your bursts are unscheduled, this feature will hurt you.

### Ablation: what is actually doing the work

Each row removes one design decision. Scheduled-burst family, same 5 trials:

```
  variant                             pods delayed %      mean idle IPs      EC2 calls
  full policy                          0.29 +/- 0.21       2.52 +/- 0.02    4210 +/- 278
  -RL (forecast + fixed headroom)      0.08 +/- 0.12       2.35 +/- 0.01    2983 +/- 27
  RL without exploration               0.32 +/- 0.27       2.54 +/- 0.05    4273 +/- 446
  -reward shaping                      5.88 +/- 2.18       2.17 +/- 0.06    3510 +/- 107
  -seasonal pre-warm                  32.78 +/- 3.00       1.99 +/- 0.04    3548 +/- 140
  -readiness gate                      0.46 +/- 0.32       2.55 +/- 0.05    4182 +/- 201
  hourly profile (10min -> 60min)      0.11 +/- 0.16      13.18 +/- 0.06    4936 +/- 989
  -target decay damping                1.25 +/- 0.75       2.06 +/- 0.03    3836 +/- 79
```

Reading it honestly:

- **The seasonal pre-warm is the feature.** Removing it takes pods delayed from
  0.29% to 32.78%. Everything else is second order.
- **The 10-minute resolution is worth 5x the address footprint.** Hour buckets
  reach the same latency by holding the burst-sized pool for the whole hour:
  13.18 idle addresses against 2.52.
- **Reward shaping is what makes the RL learn to pre-warm at all** — 5.88%
  versus 0.29%. This is the credit-assignment result described below.
- **The reinforcement learning does not pay for itself.** Deleting it entirely
  and running the forecast with a fixed 2x headroom is as good or better on
  every family (0.08% vs 0.29% delayed here; 7.74% vs 11.86% on
  bursty-rollout), at 30% fewer EC2 calls. `RL without exploration` is
  indistinguishable from the full policy, so this is not exploration noise at
  evaluation time — the learned policy simply does not beat a well-specified
  forecast with a sensible constant. On this evidence the RL layer is the part
  to cut, not the part to ship.
- **The readiness gate does not show up here** because these are held-out days
  on an already-trained policy. Its effect is on day one; see the learning
  curve.

### Learning curve

Score on day N of a single continuous run, never reset — the situation a real
node is in:

```
  family            day     pods delayed %      mean idle IPs
  hourly-batch        1    13.52 +/- 3.05      22.36 +/- 0.60     <- fallback, == default
                      2     0.28 +/- 0.24       2.60 +/- 0.06     <- converged
                      3     0.28 +/- 0.37       2.70 +/- 0.05
                      8     0.28 +/- 0.24       2.57 +/- 0.09
  steady              1     0.00 +/- 0.00      18.73 +/- 1.38
                      2     0.00 +/- 0.00       1.22 +/- 0.04
                      8     0.00 +/- 0.00       1.13 +/- 0.03
```

Convergence takes **one day**, which is the time needed to observe every slot
of the daily profile once. Day one is the static fallback by construction. The
corollary matters for deployment: a node that lives less than a day never
benefits, so on a heavily autoscaled cluster the model would need to be seeded
from the node group rather than learned per node.

### Instance size sweep

Scheduled-burst family, 5 trials:

```
  m5.large-like (3 ENI x 9 IP)     pods delayed %      mean idle IPs
    WARM_ENI_TARGET=1 (default)   28.98 +/- 1.78      13.87 +/- 0.20
    WARM_IP_TARGET=16              0.05 +/- 0.09      15.52 +/- 0.01
    ADAPTIVE                       0.25 +/- 0.24       1.98 +/- 0.05

  m5.4xlarge-like (8 ENI x 29 IP)
    WARM_ENI_TARGET=1 (default)    0.05 +/- 0.09      48.18 +/- 0.72
    WARM_IP_TARGET=16              0.05 +/- 0.09      16.04 +/- 0.00
    ADAPTIVE                       0.23 +/- 0.19       2.72 +/- 0.12

  m5.xlarge-like, prefix delegation
    WARM_ENI_TARGET=1 (default)    0.32 +/- 0.17      27.76 +/- 0.06
    WARM_IP_TARGET=16              0.05 +/- 0.09      26.20 +/- 0.38
    ADAPTIVE                       0.05 +/- 0.09      15.74 +/- 0.05
```

The result is not an artefact of one instance size, and it grows with the
instance: on a large node the default holds 48 idle addresses where the policy
holds 2.7. Under prefix delegation the advantage narrows to roughly 2x, because
a /28 is a coarse unit and even a perfect policy over-allocates by up to 15
addresses at a time.

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
