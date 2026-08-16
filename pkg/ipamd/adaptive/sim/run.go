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

package sim

import (
	"fmt"
	"sort"
	"time"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/adaptive"
)

// SimStart is the wall clock the simulation starts at. It is a Monday
// midnight UTC, so hour-of-day buckets line up with the workload generators.
var SimStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Knobs are the warm pool settings in force for one control interval. A static
// controller returns the same values forever; the adaptive controller returns
// what its policy decided.
type Knobs struct {
	WarmIPTarget      int
	MinimumIPTarget   int
	WarmENITarget     int
	WarmPrefixTarget  int
	WarmIPTargetsSet  bool
	WarmPrefixTgtsSet bool
}

// Controller decides the knobs for the next control interval.
type Controller interface {
	Name() string
	Decide(now time.Time, st Stats) Knobs
}

// StaticController reproduces today's behaviour: knobs from environment
// variables that never change.
type StaticController struct {
	label string
	knobs Knobs
}

// Static builds a controller for a fixed WARM_IP_TARGET / MINIMUM_IP_TARGET.
func Static(warmIP, minIP int) *StaticController {
	return &StaticController{
		label: fmt.Sprintf("WARM_IP_TARGET=%d,MINIMUM_IP_TARGET=%d", warmIP, minIP),
		knobs: Knobs{WarmIPTarget: warmIP, MinimumIPTarget: minIP, WarmIPTargetsSet: true},
	}
}

// StaticENI builds a controller for the default WARM_ENI_TARGET behaviour.
func StaticENI(warmENI int) *StaticController {
	return &StaticController{
		label: fmt.Sprintf("WARM_ENI_TARGET=%d", warmENI),
		knobs: Knobs{WarmENITarget: warmENI},
	}
}

// StaticPrefix builds a controller for WARM_PREFIX_TARGET with PD enabled.
func StaticPrefix(warmPrefix int) *StaticController {
	return &StaticController{
		label: fmt.Sprintf("WARM_PREFIX_TARGET=%d", warmPrefix),
		knobs: Knobs{WarmPrefixTarget: warmPrefix, WarmPrefixTgtsSet: true},
	}
}

func (s *StaticController) Name() string                  { return s.label }
func (s *StaticController) Decide(time.Time, Stats) Knobs { return s.knobs }

// AdaptiveController drives the pool from the reinforcement learning policy,
// falling back to the static knobs while the policy is still learning.
type AdaptiveController struct {
	Policy *adaptive.Policy
	// Fallback is what runs until the policy reports itself ready. It is the
	// shipped default, WARM_ENI_TARGET=1.
	Fallback Controller
}

// Adaptive wraps a policy as a controller.
func Adaptive(p *adaptive.Policy) *AdaptiveController {
	return &AdaptiveController{Policy: p, Fallback: StaticENI(1)}
}

func (a *AdaptiveController) Name() string { return "ADAPTIVE (RL)" }

func (a *AdaptiveController) Decide(now time.Time, st Stats) Knobs {
	t := a.Policy.Step(now, adaptive.Observation{
		AssignedIPs:  st.AssignedIPs,
		AvailableIPs: st.AvailableIPs,
		TotalIPs:     st.TotalIPs,
		AllocOps:     st.AllocOps,
		Unfulfilled:  st.Unfulfilled,
	})
	if !t.Ready {
		return a.Fallback.Decide(now, st)
	}
	return Knobs{
		WarmIPTarget:     t.WarmIPTarget,
		MinimumIPTarget:  t.MinimumIPTarget,
		WarmIPTargetsSet: true,
	}
}

// Result is the score of one policy on one workload.
type Result struct {
	Policy   string
	Workload string

	Pods           int
	PodsDelayed    int
	WaitSeconds    int64
	MaxWait        int64
	P99Wait        int64
	DelayedPct     float64
	MeanIdleIPs    float64
	MaxTotalIPs    int
	EC2Calls       int
	ENIAttaches    int
	ENIDetaches    int
	SimSeconds     int64
	PeakConcurrent int
}

// DefaultDelayWeight prices one pod-second of startup delay against one
// idle-IP-second. Twenty is a deliberately conservative choice: it says an
// address held for 20 seconds and never used costs the same as a pod waiting
// one second to start. TestCostSensitivity reports the ranking across a range
// of values so no conclusion rests on this number alone.
const DefaultDelayWeight = 20.0

// Cost is a single number for ranking policies, at DefaultDelayWeight.
func (r Result) Cost() float64 { return r.CostAt(DefaultDelayWeight) }

// CostAt scores the run with a given price for a pod-second of startup delay.
//
// It is deliberately NOT the reward the agent optimizes: the agent is scored
// per control interval on IP requests it failed to serve, while this charges
// the pod-seconds of startup delay actually suffered, plus the subnet addresses
// held idle and the EC2 calls made. An agent that games its own reward will not
// game this.
//
// One EC2 mutating call is priced at 10 idle-IP-seconds, standing in for the
// account-wide API rate limit that a busy cluster shares.
func (r Result) CostAt(delayWeight float64) float64 {
	return delayWeight*float64(r.WaitSeconds) +
		r.MeanIdleIPs*float64(r.SimSeconds) +
		10*float64(r.EC2Calls)
}

// Run replays the workload against the controller and returns its score.
func Run(spec NodeSpec, w Workload, ctrl Controller) Result {
	pods := append([]Pod(nil), w.Pods...)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Start < pods[j].Start })

	p := newPool(spec)

	end := int64(w.Days) * secondsPerDay
	if n := len(pods); n > 0 && pods[n-1].Start+pods[n-1].Life > end {
		end = pods[n-1].Start + pods[n-1].Life
	}

	// waiting holds pods that arrived but found no free IP. CNI ADD fails and
	// kubelet retries, so they keep asking every second until served.
	type waiter struct {
		since int64
		life  int64
	}
	var waiting []waiter
	releases := make(map[int64][]*cidrBlock)

	next := 0
	var waitSeconds, maxWait int64
	waits := make([]int64, 0, 1024)
	delayed := 0
	var idleIPSeconds float64
	maxTotal := 0
	lastDecrease := int64(-1 << 62)

	for t := int64(0); t <= end; t++ {
		p.completePending(t)
		p.expireCooldowns(t)

		// Pods whose lifetime ended give their IP back.
		if blocks, ok := releases[t]; ok {
			for _, b := range blocks {
				p.release(t, b)
			}
			delete(releases, t)
		}

		// Pods that were waiting get first claim on the pool.
		if len(waiting) > 0 {
			remaining := waiting[:0]
			for _, wt := range waiting {
				if b := p.assign(); b != nil {
					d := t - wt.since
					waitSeconds += d
					waits = append(waits, d)
					if d > maxWait {
						maxWait = d
					}
					releases[t+wt.life] = append(releases[t+wt.life], b)
				} else {
					remaining = append(remaining, wt)
				}
			}
			waiting = remaining
		}

		// New arrivals.
		for next < len(pods) && pods[next].Start == t {
			pd := pods[next]
			next++
			if b := p.assign(); b != nil {
				releases[t+pd.Life] = append(releases[t+pd.Life], b)
				waits = append(waits, 0)
			} else {
				delayed++
				waiting = append(waiting, waiter{since: t, life: pd.Life})
			}
		}

		idleIPSeconds += float64(p.available())
		if p.totalCapacity > maxTotal {
			maxTotal = p.totalCapacity
		}

		// The pool manager runs every ControlSeconds, but only when it is not
		// already blocked inside a synchronous EC2 call.
		if t%spec.ControlSeconds == 0 && !p.busy() {
			lastDecrease = controlTick(p, spec, ctrl, t, lastDecrease)
		}
	}

	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	var p99 int64
	if len(waits) > 0 {
		p99 = waits[int(float64(len(waits))*0.99)]
	}

	simSeconds := end + 1
	return Result{
		Policy:         ctrl.Name(),
		Workload:       w.Name,
		Pods:           len(pods),
		PodsDelayed:    delayed,
		WaitSeconds:    waitSeconds,
		MaxWait:        maxWait,
		P99Wait:        p99,
		DelayedPct:     100 * float64(delayed) / float64(max(len(pods), 1)),
		MeanIdleIPs:    idleIPSeconds / float64(simSeconds),
		MaxTotalIPs:    maxTotal,
		EC2Calls:       p.ec2Calls,
		ENIAttaches:    p.eniAttaches,
		ENIDetaches:    p.eniDetaches,
		SimSeconds:     simSeconds,
		PeakConcurrent: w.PeakConcurrent(),
	}
}

// controlTick is the simulated updateIPPoolIfRequired.
func controlTick(p *pool, spec NodeSpec, ctrl Controller, t, lastDecrease int64) int64 {
	st := p.stats()
	p.allocOps, p.unfulfilled = 0, 0

	k := ctrl.Decide(SimStart.Add(time.Duration(t)*time.Second), st)

	short, over, warmIPTargetsSet := targetState(st, k, spec)

	switch {
	case warmIPTargetsSet && short > 0:
		increase(p, spec, t, short)
	case !warmIPTargetsSet && poolTooLow(st, k, spec):
		increase(p, spec, t, unitsNeeded(st, k, spec))
	case tooHigh(st, k, spec, over, warmIPTargetsSet) && t-lastDecrease >= spec.DecreaseSeconds:
		if over > 0 {
			p.unassignCidrs(over)
		} else if k.WarmPrefixTgtsSet {
			p.unassignCidrs(st.FreeCidrs - k.WarmPrefixTarget)
		}
		lastDecrease = t
	}

	if shouldRemoveExtraENIs(st, k, spec) {
		p.freeENI(t, k)
	}
	return lastDecrease
}

// targetState mirrors IPAMContext.datastoreTargetState.
func targetState(st Stats, k Knobs, spec NodeSpec) (short, over int, enabled bool) {
	if !k.WarmIPTargetsSet {
		return 0, 0, false
	}

	available := st.AvailableIPs
	short = max(k.WarmIPTarget-available, 0)
	short = max(short, k.MinimumIPTarget-st.TotalIPs)
	over = max(available-k.WarmIPTarget, 0)
	over = max(min(over, st.TotalIPs-k.MinimumIPTarget), 0)

	if spec.PrefixDelegation {
		perPrefix := spec.IPsPerPrefix
		shortPrefix := divCeil(short, perPrefix)
		neededForWarm := divCeil(st.AssignedIPs+k.WarmIPTarget, perPrefix)
		neededForMin := divCeil(k.MinimumIPTarget, perPrefix)
		overPrefix := max(min(st.FreeCidrs, st.TotalCidrs-neededForWarm), 0)
		overPrefix = max(min(overPrefix, st.TotalCidrs-neededForMin), 0)
		return shortPrefix, overPrefix, true
	}
	return short, over, true
}

// poolTooLow mirrors the WARM_ENI_TARGET / WARM_PREFIX_TARGET branch of
// isDatastorePoolTooLow.
func poolTooLow(st Stats, k Knobs, spec NodeSpec) bool {
	warmTarget, unit := k.WarmENITarget, spec.MaxIPsPerENI
	if spec.PrefixDelegation {
		warmTarget, unit = k.WarmPrefixTarget, spec.IPsPerPrefix
	}
	return st.AvailableIPs < unit*warmTarget || (warmTarget == 0 && st.AvailableIPs == 0)
}

// unitsNeeded mirrors getPrefixesNeeded / GetENIResourcesToAllocate when no
// warm IP target is set: fill the ENI with secondary IPs, or take one prefix.
func unitsNeeded(st Stats, k Knobs, spec NodeSpec) int {
	if spec.PrefixDelegation {
		return max(1, k.WarmPrefixTarget-st.FreeCidrs)
	}
	return spec.MaxIPsPerENI
}

// tooHigh mirrors isDatastorePoolTooHigh.
func tooHigh(st Stats, k Knobs, spec NodeSpec, over int, warmIPTargetsSet bool) bool {
	if warmIPTargetsSet {
		return over > 0
	}
	if k.WarmPrefixTgtsSet && spec.PrefixDelegation {
		return st.FreeCidrs > k.WarmPrefixTarget
	}
	return false
}

// increase mirrors increaseDatastorePool: top up existing ENIs first, and only
// attach a new ENI when none of them has room.
func increase(p *pool, spec NodeSpec, now int64, units int) {
	if units <= 0 {
		return
	}
	if p.startAssignCidrs(now, units) {
		return
	}
	p.startAllocENI(now, units)
}

// shouldRemoveExtraENIs mirrors the function of the same name in ipamd.
func shouldRemoveExtraENIs(st Stats, k Knobs, spec NodeSpec) bool {
	if k.WarmIPTargetsSet {
		return true
	}
	warmTarget := k.WarmENITarget + 1
	if spec.PrefixDelegation {
		warmTarget = k.WarmPrefixTarget + 1
	}
	return st.AvailableIPs >= warmTarget*spec.MaxIPsPerENI
}
