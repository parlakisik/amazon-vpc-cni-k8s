# Turning this into a paper: what holds up, what does not, and how to evaluate it

This is a companion to [`adaptive-ip-target.md`](adaptive-ip-target.md), written
for the question "can this be an academic paper, and how should it be
evaluated?" It is deliberately blunt about what the current results do and do
not support.

## 1. Is there a paper here?

Yes, but **not the paper the feature name suggests.**

"We applied reinforcement learning to CNI warm-pool sizing and it beat the
static knobs" is not supported by this evaluation. The ablation in
`TestStudyAblation` deletes the RL layer and the result is as good or better on
all four workload families, at ~30% fewer EC2 API calls, and disabling
exploration does not recover the difference. A reviewer will run that ablation
in their head immediately; if the paper does not run it, that is the rejection.

Three framings that *are* supported, in decreasing order of how easy they are to
defend:

**(a) Systems paper: "Static warm-pool knobs are the wrong interface."**
The contribution is the problem formulation and the measurement: warm-pool
sizing in a production CNI is an inventory problem with a shared, exhaustible
resource (subnet addresses), a lead time set by the EC2 control plane, and a
per-node observer with no cluster view. The result is that a small seasonal
forecast at the right resolution closes ~91% of the gap between the shipped
default and a clairvoyant controller on scheduled-burst workloads, and that
almost all of the benefit comes from two design choices (10-minute resolution,
pre-warming the *remaining rise* rather than the level). This is a real
contribution and the current harness nearly supports it. Venue: a systems
workshop, or an industry track.

**(b) Negative-result / lessons paper: "RL does not pay for warm-pool sizing."**
The credit-assignment finding is genuinely interesting: the decision that
matters happens ~120 control intervals before its consequence, one-step
Q-learning cannot attribute it, potential-based shaping fixes the learning — and
*even then* the learned policy does not beat the forecast the shaping was
derived from. That is a clean, well-instrumented negative result about a
technique many people are currently reaching for. Venue: a workshop that
explicitly accepts negative results; harder at a main conference.

**(c) ML paper: "A new RL method for resource pool control."**
Not supported. There is no methodological novelty here — tabular Q-learning
with potential-based shaping (Ng, Harada & Russell, 1999) — and the method loses
to its own baseline. Do not attempt this framing.

Be aware of the related work you sit inside: predictive autoscaling of VM and
container pools, warm-pool/cold-start management in serverless (this is very
close to the "keep-alive window" literature), and classical newsvendor and
base-stock inventory control, which is what this problem actually is. A
reviewer familiar with the last of those will ask why a base-stock policy with a
seasonal demand estimate is not the whole answer. On the current evidence, it
mostly is — and saying so first is much stronger than being told.

## 2. What the current evaluation already establishes

Run it:

```
go test ./pkg/ipamd/adaptive/sim/ -run 'TestStudy' -v -timeout 30m
```

| Element | Where | Status |
| --- | --- | --- |
| Simulator fidelity to the real controller | `sim/run.go`, `sim/node.go` | mirrors ipamd's short/over arithmetic, 5s control interval, 30s cooldown, 60s min ENI life, `getDeletableENI` retention, blocking EC2 calls; pinned by `TestSimulatorMirrorsIpamdTargetMath` |
| Repeated trials with CIs | `TestStudyMainResult` | 5 seeds per family, Student's t 95% intervals |
| Train/test separation | `Workload.Split` | learns on 7 days, scored on 3 held out |
| Upper bound | `sim/oracle.go` | clairvoyant controller, cooldown-aware, gives "fraction of achievable gap closed" |
| Ablation | `TestStudyAblation` | one row per design decision, including the RL layer itself |
| Sensitivity to the objective | `TestCostSensitivity` | ranking reported across a 500x range of delay-vs-address prices |
| Generalisation across hardware | `TestStudyInstanceSizes` | 3 instance shapes plus prefix delegation |
| Time-to-benefit | `TestStudyLearningCurve` | day-by-day, single continuous run, never reset |
| Deployability floor | `TestColdStartIsSafe` | day one is identical to the shipped default |

That is a respectable experimental skeleton. It is not yet a publishable
evaluation, for one dominant reason.

## 3. The one thing that will sink it: no real workload

Every number above comes from four synthetic generators that **I wrote to
exhibit the pattern the policy exploits**. That is circular, and a reviewer will
say so in the first paragraph. Nothing else on this list matters as much.

Fix it, in rough order of cost:

1. **Public cluster traces.** The Alibaba cluster traces and the Google
   Borg/cluster traces both contain per-task submission times and durations at
   scale. Group tasks by machine, convert to `sim.Pod{Start, Life}`, replay.
   This costs a day of data wrangling and immediately converts the paper from
   "synthetic study" to "trace-driven study". The `Workload` type already has
   the shape you need — you only need a loader.
2. **Your own cluster.** ipamd already exports `awscni_assigned_ip_addresses`.
   Scrape it per node at 15s for a fortnight across a few node groups and you
   have the exact series the tracker consumes, from real nodes. This is the most
   credible trace you can get for *this* system, and it doubles as the shadow-
   mode validation in step 5.
3. **Characterise before you evaluate.** Publish the properties of the traces —
   autocorrelation of arrivals, spectral power at the 24-hour and 1-hour
   periods, burst size distribution, pod lifetime distribution. Then state up
   front which traces the method should and should not help on. Predicting your
   own failure cases before showing results is far more persuasive than
   explaining them afterwards.
4. **Report per-trace, not just per-family.** With real traces, the distribution
   over nodes is the result. "Helped on 60% of nodes, neutral on 30%, hurt on
   10%, and here is what distinguishes them" is a much stronger claim than a
   mean.

## 4. What else to add before submitting

**Stronger baselines.** The static knobs are the deployed baseline and belong in
the paper, but they are weak. Add at least:

- A **base-stock / newsvendor policy** with the same seasonal demand estimate:
  order up to the `q`-th quantile of lead-time demand. This is the textbook
  answer and is the baseline that decides whether the paper has a contribution.
- A **simple time-series forecaster** — seasonal naive (yesterday, same slot),
  Holt-Winters, or a small ARIMA — feeding the same base-stock rule. This
  separates "seasonal forecasting helps" from "our particular estimator helps".
- **Reactive-only** control (no seasonality at all): a PID or
  target-tracking controller on free addresses. Cheap, and shows the value of
  the seasonal term specifically.

The `Controller` interface in `sim/run.go` is two methods; each of these is well
under a hundred lines.

**A defensible objective.** The cost function currently prices a pod-second of
delay at 20 idle-address-seconds, and the sensitivity analysis shows the ranking
flips below ~20. That is honest but weak. Better: report a **Pareto frontier** —
sweep each policy's tuning parameter and plot delay against address footprint,
one curve per policy. A dominating curve is an argument no choice of weights can
undermine. This is the single highest-value change to the presentation, and the
harness already produces both axes.

**Statistical care.** Five seeds is thin. Twenty is cheap here (a full study run
is well under a minute per configuration). Report paired comparisons on the same
traces (the harness already gives every policy identical traces) and a
non-parametric test — Wilcoxon signed-rank across traces — rather than
overlapping CIs, which reviewers correctly distrust.

**Simulator validation.** The simulator's fidelity is currently argued from code
review. Argue it from data: run one workload on a real single-node cluster with
each static setting, and show the simulator predicts the observed idle-address
and pod-latency curves within some stated error. Without this, every result is
one reviewer question away from "how do you know your simulator is right?"

**Overhead.** State the CPU, memory and disk cost of the controller on a node,
and the added EC2 API call rate. The last one is not free: the policy makes
40-100% more mutating calls than the static settings, and EC2 API quota is
account-wide and shared. This is a real deployment cost and hiding it is worse
than reporting it.

**Failure and safety analysis.** Report what happens under model corruption,
clock steps (the harness has a regression test for this), ipamd restart, node
drain, and subnet exhaustion. For a system paper, the safety envelope — hard
clamps, readiness gate, static fallback — is a contribution in itself, because
it is what makes a learned controller deployable in a data path at all.

## 5. Proposed evaluation plan

Ordered so the highest-risk question is answered first.

**Phase 1 — kill the circularity (highest priority).**
Load one public trace family plus one from your own cluster. Re-run
`TestStudyMainResult` and `TestStudyAblation` unchanged. If the scheduled-burst
result does not survive real traces, stop and rethink — everything else is
downstream of this.

**Phase 2 — make the baselines honest.**
Add base-stock, a classical forecaster, and reactive control. Re-run the
ablation. The paper's contribution is precisely whatever survives this.

**Phase 3 — resolve the RL question.**
Either (i) report the negative result as a finding, which the ablation already
supports, or (ii) give the RL a fair chance to win — a fleet-shared model rather
than per-node, a contextual bandit with a proper confidence-bound exploration
rule instead of ε-greedy, or a learned residual on top of the base-stock policy
rather than a from-scratch controller. Do not spend more than a bounded effort
on (ii); the honest version of the paper does not need it.

**Phase 4 — Pareto frontiers, not scalar costs.**
Sweep tuning parameters, plot delay against address footprint, one curve per
policy family. This becomes the paper's main figure.

**Phase 5 — real-cluster validation.**
Shadow mode on a real node group for two weeks: compare the targets the policy
would have chosen against the pool actually held, and validate the simulator
against the observed behaviour. Then a canary in enforce mode with pod
scheduling latency and `awscni_no_available_ip_addresses` as the guard metrics.
Even a small deployment turns this from a simulation paper into a systems paper.

## 6. Threats to validity, to state explicitly

State these yourself. Every one of them is something a reviewer will otherwise
raise.

- **Synthetic traces written by the same author as the method.** The dominant
  threat until Phase 1 is done.
- **Simulator, not a cluster.** Fidelity is argued from code correspondence.
  Unmodelled: EC2 API throttling and retries, IMDS lag, node-level CPU
  contention, kubelet's actual CNI ADD retry backoff (modelled as a 1s retry),
  concurrent CNI ADDs, and reconciler interactions.
- **Single-node scope.** Subnet exhaustion is a cluster-level effect; a per-node
  policy optimising its own footprint is not the same as optimising the
  cluster's. The idle-address metric is a proxy for the harm, not the harm.
- **Objective weights are a judgement call.** Mitigated by the sensitivity
  analysis; properly fixed by Pareto frontiers.
- **The pod-delay model.** A pod that cannot get an address retries every
  second; real kubelet backoff is coarser, so wall-clock delays in production
  are likely *worse* than reported here, not better.
- **Learning requires a day.** On clusters where nodes live hours, the policy
  never activates. The learning-curve experiment quantifies this; it is a
  limitation of the per-node design, not a tuning problem.
- **Reward and evaluation share a designer.** Mitigated by scoring on
  pod-seconds of delay, which the agent never observes, rather than on the
  agent's own reward.

## 7. If you only do three things

1. Replay a real trace. Everything else is secondary.
2. Add a base-stock baseline with a seasonal forecast, and report the RL
   ablation honestly whichever way it comes out.
3. Replace the scalar cost with a Pareto frontier.

With those, framing (a) — a systems paper about the interface being wrong, with
a measured, safety-gated controller and an honest account of where learning does
and does not help — is a defensible submission.
