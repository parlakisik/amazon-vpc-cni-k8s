// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package adaptive

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// The learning problem
//
//	state   : (part of day, forecast demand, current slack, forecast deficit)
//	action  : (headroom to keep over the arrival forecast,
//	           how much of the forecast peak to pre-warm)
//	reward  : -(shortage cost + idle IP cost + EC2 churn cost)
//
// The transition from one state to the next is dominated by the workload, which
// the policy cannot influence, so this is close to a contextual bandit. We still
// run Q-learning with a small discount so that a decision which leaves the pool
// in a bad place for the next interval is charged for it.
//
// Seasonality lives in the tracker's daily profile rather than in the state
// space: the state carries the *forecast*, not the clock. That keeps the table
// small enough to converge within a day or two of node uptime.

// warmMultipliers are the candidate headroom factors applied to the forecast of
// pod arrivals over the provisioning lead time.
var warmMultipliers = []float64{0.5, 1.0, 1.5, 2.0, 3.0, 4.0}

// prewarmFractions are the candidate fractions of the forecast peak to hold as
// a floor on total IPs (the learned stand-in for MINIMUM_IP_TARGET).
var prewarmFractions = []float64{0.0, 0.5, 1.0}

// defaultActionIdx is the action used before a state has been explored enough
// to trust its Q values: 2x the arrival forecast plus the full forecast peak.
// Cold start therefore behaves like a conservative hand-tuned controller rather
// than like a random agent.
var defaultActionIdx = actionIndex(3, 2) // multiplier 2.0, fraction 1.0

func actionIndex(mult, frac int) int { return mult*len(prewarmFractions) + frac }

func numActions() int { return len(warmMultipliers) * len(prewarmFractions) }

func decodeAction(a int) (mult, frac float64) {
	return warmMultipliers[a/len(prewarmFractions)], prewarmFractions[a%len(prewarmFractions)]
}

// Policy modes.
const (
	// ModeShadow learns and exports what it would do, but leaves the static
	// knobs in charge of allocation.
	ModeShadow = "shadow"
	// ModeEnforce lets the policy drive allocation.
	ModeEnforce = "enforce"
)

// Config controls the adaptive policy. Every field has a safe default; see
// DefaultConfig.
type Config struct {
	// Mode is ModeShadow or ModeEnforce.
	Mode string

	// LeadTime is how long it takes for newly requested capacity to become
	// usable. It is the horizon the warm target has to cover: any pod arriving
	// within LeadTime must be served from IPs we already hold.
	LeadTime time.Duration

	// PrewarmWindow is how far ahead the policy looks in the learned daily
	// profile when deciding the floor on total IPs. It has to be long enough to
	// attach the ENIs a predicted burst needs, which is why it is minutes and
	// not seconds. This window is the whole economic argument for the knob: a
	// static MINIMUM_IP_TARGET holds the burst-sized pool for 24 hours a day,
	// this holds it for PrewarmWindow before each predicted burst.
	PrewarmWindow time.Duration

	// BurstZ is how many standard deviations of arrival noise to add to the
	// live arrival forecast before the headroom multiplier is applied.
	BurstZ float64

	// MinWarmIPTarget is a hard floor on the warm target, so the policy can
	// never starve a node no matter what it learns.
	MinWarmIPTarget int
	// MaxWarmIPTarget is a hard ceiling, so a mislearned burst can never make
	// the node hoard a subnet.
	MaxWarmIPTarget int
	// MaxMinimumIPTarget caps the learned floor on total IPs.
	MaxMinimumIPTarget int

	// MaxTargetDecreasePerStep limits how far the targets may fall each time
	// they are allowed to, and TargetDecayInterval is how often that is. Both
	// stop the pool oscillating between allocate and release and burning EC2
	// API quota. Targets always rise immediately.
	MaxTargetDecreasePerStep int
	TargetDecayFraction      float64
	TargetDecayInterval      time.Duration

	// Reward weights. ShortageCost is per IP request that could not be served
	// from the warm pool, IdleIPCost is per idle IP per control interval,
	// ChurnCost is per EC2 allocate/deallocate call.
	ShortageCost float64
	IdleIPCost   float64
	ChurnCost    float64

	// ShapingWeight scales the potential-based reward shaping that credits an
	// action for closing the gap between the pool and the forecast peak.
	//
	// Without it the agent cannot learn to pre-warm at all: the shortage it
	// causes by not pre-warming lands tens of control intervals later, far
	// outside what one-step Q-learning can attribute. Shaping with a potential
	// function is the standard fix and leaves the optimal policy unchanged
	// (Ng, Harada & Russell, 1999) - it only makes the credit arrive on time.
	// Set to 0 to learn from the raw reward alone.
	ShapingWeight float64

	// Q-learning hyperparameters.
	LearningRate float64
	Discount     float64
	Epsilon      float64
	EpsilonMin   float64
	EpsilonDecay float64

	// MinVisits is how many times a state must be seen before the policy stops
	// falling back to defaultActionIdx and starts acting on what it learned.
	MinVisits int

	// MinProfileDays is how many days a slot of the daily profile must have been
	// observed before the policy will drive the pool during it. Until then its
	// targets are advisory only and the static knobs stay in charge. Zero
	// disables the gate, which is only useful as an ablation.
	MinProfileDays int

	// ProfileAggregation groups this many consecutive profile slots together
	// when forecasting. 1 keeps the native 10-minute resolution; 6 coarsens it
	// to hour buckets. It exists so the cost of the resolution choice can be
	// measured rather than asserted.
	ProfileAggregation int

	// DisableLearning freezes the policy on its conservative default action.
	// What remains is the forecast plus a fixed headroom, which is the ablation
	// that isolates what the reinforcement learning itself contributes.
	DisableLearning bool

	// Seed makes exploration deterministic in tests and simulations.
	Seed int64
}

// DefaultConfig returns the tuned defaults.
func DefaultConfig() Config {
	return Config{
		Mode: ModeShadow,
		// An ENI attach is the slow path (tens of seconds); secondary IP and
		// prefix allocation take a few seconds. 30s covers both.
		LeadTime: 30 * time.Second,
		// Long enough to attach the ENIs a predicted burst needs (an attach is
		// ~20s and only one EC2 call is in flight at a time), and no longer:
		// every extra second of pre-warm is a second of idle addresses.
		PrewarmWindow:            2 * time.Minute,
		BurstZ:                   1.0,
		MinWarmIPTarget:          1,
		MaxWarmIPTarget:          64,
		MaxMinimumIPTarget:       128,
		MaxTargetDecreasePerStep: 1,
		// Shed a quarter of the target each time it is allowed to fall, so a
		// pool provisioned for a burst drains over a few minutes rather than
		// over an hour, without ever falling in a single step.
		TargetDecayFraction: 0.25,
		// Matches ipamd's own decreaseIPPoolInterval, so the target can never
		// fall faster than the pool is allowed to shrink.
		TargetDecayInterval: 30 * time.Second,
		// A pod that cannot start is far more expensive than an idle IP, but an
		// idle IP is not free: it is a subnet address no other node can use.
		ShortageCost:       20.0,
		IdleIPCost:         0.05,
		ChurnCost:          0.5,
		ShapingWeight:      20.0,
		LearningRate:       0.15,
		Discount:           0.9,
		Epsilon:            0.15,
		EpsilonMin:         0.01,
		EpsilonDecay:       0.9995,
		MinVisits:          3,
		MinProfileDays:     1,
		ProfileAggregation: 1,
		Seed:               1,
	}
}

// Observation is what the policy is told about the pool at each control tick.
type Observation struct {
	// AssignedIPs is the number of IPs currently held by pods.
	AssignedIPs int
	// AvailableIPs is the number of IPs ready to be handed to a new pod.
	AvailableIPs int
	// TotalIPs is AssignedIPs + AvailableIPs + IPs in cooldown.
	TotalIPs int
	// AllocOps is the number of EC2 allocate/deallocate calls made since the
	// previous tick. Used to charge the policy for churn.
	AllocOps int
	// Unfulfilled is the number of IP requests that had to wait since the
	// previous tick, if the caller can measure it directly. When it is negative
	// the policy infers shortage from arrivals versus IPs on hand.
	Unfulfilled int
}

// Targets is what the policy decides for the next control interval. They are
// consumed exactly where WARM_IP_TARGET and MINIMUM_IP_TARGET are consumed
// today, so nothing downstream in ipamd has to know the policy exists.
type Targets struct {
	WarmIPTarget    int
	MinimumIPTarget int

	// Ready is false while the policy is still learning the slots it is about
	// to be responsible for. The caller must fall back to the static knobs for
	// as long as it is false, even in enforce mode.
	Ready bool
}

// stateKey identifies a discretized situation.
type stateKey struct {
	DayPart int
	Demand  int
	Slack   int
	Deficit int
}

func (s stateKey) String() string {
	return fmt.Sprintf("p%d/d%d/s%d/f%d", s.DayPart, s.Demand, s.Slack, s.Deficit)
}

// Policy is the reinforcement learning controller. It is safe for concurrent
// use: ipamd's pool manager writes to it while the introspection endpoint reads.
type Policy struct {
	mu  sync.Mutex
	cfg Config
	trk *Tracker
	rng *rand.Rand

	q      map[stateKey][]float64
	visits map[stateKey]int

	epsilon float64

	// carried between ticks so we can score the previous decision
	haveLast   bool
	lastState  stateKey
	lastAction int
	lastAvail  int
	lastPhi    float64
	lastDecay  time.Time

	// most recent decision, for metrics and introspection
	current      Targets
	currentState stateKey
	lastReward   float64
	steps        int
	explored     bool
}

// NewPolicy creates a policy with the given configuration.
func NewPolicy(cfg Config) *Policy {
	def := DefaultConfig()
	if cfg.LeadTime <= 0 {
		cfg.LeadTime = def.LeadTime
	}
	if cfg.PrewarmWindow <= 0 {
		cfg.PrewarmWindow = def.PrewarmWindow
	}
	if cfg.MaxTargetDecreasePerStep <= 0 {
		cfg.MaxTargetDecreasePerStep = def.MaxTargetDecreasePerStep
	}
	if cfg.TargetDecayInterval <= 0 {
		cfg.TargetDecayInterval = def.TargetDecayInterval
	}
	if cfg.TargetDecayFraction <= 0 {
		cfg.TargetDecayFraction = def.TargetDecayFraction
	}
	if cfg.ProfileAggregation <= 0 {
		cfg.ProfileAggregation = def.ProfileAggregation
	}
	if cfg.MaxWarmIPTarget < cfg.MinWarmIPTarget {
		cfg.MaxWarmIPTarget = cfg.MinWarmIPTarget
	}
	trk := NewTracker()
	trk.SetAggregation(cfg.ProfileAggregation)
	return &Policy{
		cfg:     cfg,
		trk:     trk,
		rng:     rand.New(rand.NewSource(cfg.Seed)),
		q:       make(map[stateKey][]float64),
		visits:  make(map[stateKey]int),
		epsilon: cfg.Epsilon,
		current: Targets{WarmIPTarget: cfg.MinWarmIPTarget},
	}
}

// Tracker exposes the workload tracker so callers can feed exact pod events.
func (p *Policy) Tracker() *Tracker { return p.trk }

// Config returns the policy configuration.
func (p *Policy) Config() Config { return p.cfg }

// Enforcing reports whether the policy should drive allocation decisions.
func (p *Policy) Enforcing() bool { return p.cfg.Mode == ModeEnforce }

// Step advances the policy by one control interval: it folds the observation
// into the workload model, scores the previous decision, and returns the targets
// to use until the next call.
func (p *Policy) Step(now time.Time, obs Observation) Targets {
	arrivals, _ := p.trk.Observe(now, obs.AssignedIPs)

	forecast := p.trk.ForecastArrivals(now, p.cfg.LeadTime, p.cfg.BurstZ)
	rise, haveRise := p.trk.ForecastRise(now, p.cfg.PrewarmWindow)
	ready := p.trk.ProfileReady(now, p.cfg.PrewarmWindow, p.cfg.MinProfileDays)

	// The pool the next few minutes are predicted to need: what pods hold now
	// plus the climb the daily profile expects.
	needed := float64(obs.AssignedIPs) + rise

	p.mu.Lock()
	defer p.mu.Unlock()

	next := p.buildState(obs, now, forecast, needed)

	// The potential of a situation is minus the shortage we would suffer if the
	// forecast peak landed right now. Closing that gap is therefore rewarded at
	// the moment the pool grows, not tens of intervals later when the burst
	// finally arrives and it is too late to act on.
	phi := -p.cfg.ShapingWeight * math.Max(0, needed-float64(obs.TotalIPs))

	// Score the action taken at the previous tick against what happened since.
	if p.haveLast {
		r := p.reward(obs, arrivals)
		p.lastReward = r
		p.learn(p.lastState, p.lastAction, r+p.cfg.Discount*phi-p.lastPhi, next)
	}
	p.lastPhi = phi

	action := p.chooseAction(next)
	targets := p.targetsFor(now, action, obs, forecast, rise, haveRise)
	targets.Ready = ready

	p.haveLast = true
	p.lastState = next
	p.lastAction = action
	p.lastAvail = obs.AvailableIPs
	p.current = targets
	p.currentState = next
	p.steps++
	if p.epsilon > p.cfg.EpsilonMin {
		p.epsilon *= p.cfg.EpsilonDecay
	}

	return targets
}

// reward charges the previous decision for the outcome we just observed.
func (p *Policy) reward(obs Observation, arrivals int) float64 {
	shortage := float64(obs.Unfulfilled)
	if obs.Unfulfilled < 0 {
		// Without a direct measurement, a shortage is demand that exceeded the
		// IPs that were on hand when the interval began.
		shortage = math.Max(0, float64(arrivals-p.lastAvail))
	}

	return -(p.cfg.ShortageCost*shortage +
		p.cfg.IdleIPCost*float64(obs.AvailableIPs) +
		p.cfg.ChurnCost*float64(obs.AllocOps))
}

func (p *Policy) learn(s stateKey, a int, r float64, next stateKey) {
	q := p.qRow(s)
	best := 0.0
	if p.visits[next] > 0 {
		nq := p.qRow(next)
		best = nq[0]
		for _, v := range nq[1:] {
			best = math.Max(best, v)
		}
	}
	target := r + p.cfg.Discount*best
	q[a] += p.cfg.LearningRate * (target - q[a])
}

func (p *Policy) qRow(s stateKey) []float64 {
	row, ok := p.q[s]
	if !ok {
		row = make([]float64, numActions())
		p.q[s] = row
	}
	return row
}

func (p *Policy) chooseAction(s stateKey) int {
	p.visits[s]++
	p.explored = false

	if p.cfg.DisableLearning {
		return defaultActionIdx
	}

	// Cold start: act like a conservative static controller until this state has
	// been seen enough times for its Q values to mean anything.
	if p.visits[s] <= p.cfg.MinVisits {
		return defaultActionIdx
	}

	if p.rng.Float64() < p.epsilon {
		p.explored = true
		return p.rng.Intn(numActions())
	}

	row := p.qRow(s)
	best, bestIdx := row[0], 0
	for i, v := range row[1:] {
		if v > best {
			best, bestIdx = v, i+1
		}
	}
	return bestIdx
}

// buildState discretizes the situation into a table key.
func (p *Policy) buildState(obs Observation, now time.Time, forecast, needed float64) stateKey {
	return stateKey{
		DayPart: now.Hour() / 6,
		Demand:  bucketDemand(forecast),
		Slack:   bucketSlack(float64(obs.AvailableIPs), forecast),
		Deficit: bucketDeficit(needed - float64(obs.TotalIPs)),
	}
}

// bucketDemand maps forecast pod arrivals over the lead time into 5 levels.
func bucketDemand(forecast float64) int {
	switch {
	case forecast < 0.5:
		return 0
	case forecast < 2:
		return 1
	case forecast < 5:
		return 2
	case forecast < 12:
		return 3
	default:
		return 4
	}
}

// bucketSlack maps how well the current pool covers the forecast into 4 levels.
func bucketSlack(available, forecast float64) int {
	if forecast < 0.5 {
		// With no expected demand, what matters is only whether we hold
		// anything at all.
		if available == 0 {
			return 0
		}
		return 3
	}
	switch ratio := available / forecast; {
	case ratio < 0.5:
		return 0
	case ratio < 1:
		return 1
	case ratio < 2:
		return 2
	default:
		return 3
	}
}

// bucketDeficit maps how far the pool is below what the daily profile predicts
// the pre-warm window will need, into 4 levels. This is the feature that tells
// the agent a scheduled burst is coming while the node is still idle.
func bucketDeficit(deficit float64) int {
	switch {
	case deficit <= 0:
		return 0
	case deficit < 4:
		return 1
	case deficit < 16:
		return 2
	default:
		return 3
	}
}

// targetsFor turns an action into concrete warm and minimum IP targets.
func (p *Policy) targetsFor(now time.Time, action int, obs Observation, forecast, rise float64, haveRise bool) Targets {
	mult, frac := decodeAction(action)

	warm := int(math.Ceil(mult * forecast))
	// Hard safety clamps. These are the guarantee that a mislearned policy
	// degrades into a static one rather than into an outage.
	warm = clamp(warm, p.cfg.MinWarmIPTarget, p.cfg.MaxWarmIPTarget)

	// The floor on total IPs: what pods hold now, plus the share of the
	// predicted climb this action chooses to pre-warm. Never below what is
	// already assigned, since releasing those would only churn IPs in use.
	minimum := obs.AssignedIPs
	if haveRise && frac > 0 {
		minimum += int(math.Ceil(frac * rise))
	}
	minimum = clamp(minimum, 0, p.cfg.MaxMinimumIPTarget)

	// A clock that steps backwards (NTP correction, a resumed instance) would
	// otherwise park the decay timer in the future and freeze the targets at
	// whatever they happened to be, which is exactly the failure mode this
	// controller must not have.
	if now.Before(p.lastDecay) {
		p.lastDecay = now
	}

	// Rise immediately, fall slowly, and only fall on a timer. Without this the
	// targets chatter from one control interval to the next and ipamd spends
	// its EC2 quota allocating and releasing the same addresses.
	if now.Sub(p.lastDecay) < p.cfg.TargetDecayInterval {
		warm = max(warm, p.current.WarmIPTarget)
		minimum = max(minimum, p.current.MinimumIPTarget)
	} else {
		p.lastDecay = now
		warm = max(warm, p.current.WarmIPTarget-p.decayStep(p.current.WarmIPTarget))
		minimum = max(minimum, p.current.MinimumIPTarget-p.decayStep(p.current.MinimumIPTarget))
	}

	return Targets{WarmIPTarget: warm, MinimumIPTarget: minimum}
}

// Current returns the targets from the most recent Step.
func (p *Policy) Current() Targets {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// Debug returns a human readable summary for the introspection endpoint.
func (p *Policy) Debug() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	mult, frac := decodeAction(p.lastAction)
	return map[string]interface{}{
		"mode":              p.cfg.Mode,
		"ready":             p.current.Ready,
		"state":             p.currentState.String(),
		"warmIPTarget":      p.current.WarmIPTarget,
		"minimumIPTarget":   p.current.MinimumIPTarget,
		"headroomFactor":    mult,
		"prewarmFraction":   frac,
		"lastReward":        p.lastReward,
		"epsilon":           p.epsilon,
		"steps":             p.steps,
		"statesExplored":    len(p.q),
		"meanPodLifetime":   p.trk.MeanPodLifetime().String(),
		"slotsProfiled":     fmt.Sprintf("%d/%d", p.trk.SlotsProfiled(), SlotsPerDay),
		"exploringThisStep": p.explored,
	}
}

// decayStep is how far a target may fall this time round: a fixed fraction of
// where it stands, but never less than MaxTargetDecreasePerStep.
func (p *Policy) decayStep(current int) int {
	return max(p.cfg.MaxTargetDecreasePerStep, int(math.Ceil(p.cfg.TargetDecayFraction*float64(current))))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
