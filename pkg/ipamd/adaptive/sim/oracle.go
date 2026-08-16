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
	"time"
)

// cooldownSeconds mirrors the datastore's IP_COOLDOWN_PERIOD default. The
// oracle has to respect it: it is a property of the system being controlled,
// not of the policy controlling it.
const cooldownSeconds = 30

// OracleController is a clairvoyant baseline: it reads the future of the trace
// and holds exactly the pool the next Horizon seconds will need, no more.
//
// It is not implementable - it knows demand that has not happened yet - and
// that is the point. It bounds what any forecasting policy could achieve on a
// given trace, so the learned policy can be reported as a fraction of the
// achievable gap between the static settings and perfect knowledge rather than
// only against baselines that happen to have been chosen.
//
// "The pool the next Horizon seconds will need" includes addresses still in
// cooldown, not just pods alive: an oracle that ignored the cooldown would make
// pods wait and would bound nothing.
//
// The horizon is finite rather than infinite because provisioning is not
// instant: a policy that learns a burst is coming still has to attach ENIs
// before it lands. Giving the oracle the same window the learned policy plans
// over keeps the comparison about forecast quality rather than about physics.
type OracleController struct {
	horizon int64
	// windowMax[t] is the highest number of pods concurrently holding an IP at
	// any second in [t, t+horizon].
	windowMax []int
}

// Oracle builds a clairvoyant controller for a specific trace.
func Oracle(w Workload, horizon time.Duration) *OracleController {
	h := int64(horizon.Seconds())

	end := int64(w.Days) * secondsPerDay
	for _, p := range w.Pods {
		if e := p.Start + p.Life; e > end {
			end = e
		}
	}

	// Concurrency at each second, from a difference array, plus the number of
	// addresses released in each second.
	delta := make([]int, end+2)
	released := make([]int, end+2)
	for _, p := range w.Pods {
		delta[p.Start]++
		if e := p.Start + p.Life; e < int64(len(delta)) {
			delta[e]--
			released[e]++
		}
	}

	// An address a pod hands back is not immediately reusable: it sits in
	// cooldown. A pool sized only for concurrency would therefore still make
	// pods wait, and would not be an upper bound on anything. What the node
	// actually has to hold at second s is the pods live then plus everything
	// released in the cooldown window behind it.
	cooling := 0
	need := make([]int, end+1)
	cur := 0
	for t := int64(0); t <= end; t++ {
		cur += delta[t]
		cooling += released[t]
		if out := t - cooldownSeconds; out >= 0 {
			cooling -= released[out]
		}
		need[t] = cur + cooling
	}
	concurrent := need

	// Sliding window maximum over [t, t+h], scanning right to left with a
	// monotonic deque so the whole trace costs one pass.
	windowMax := make([]int, end+1)
	deque := make([]int64, 0, 64) // indices, values non-increasing
	for t := end; t >= 0; t-- {
		for len(deque) > 0 && concurrent[deque[len(deque)-1]] <= concurrent[t] {
			deque = deque[:len(deque)-1]
		}
		deque = append(deque, t)
		// Drop indices that have fallen out of the window ahead of t.
		for len(deque) > 0 && deque[0] > t+h {
			deque = deque[1:]
		}
		windowMax[t] = concurrent[deque[0]]
	}

	return &OracleController{horizon: h, windowMax: windowMax}
}

func (o *OracleController) Name() string {
	return fmt.Sprintf("ORACLE (clairvoyant, %ds)", o.horizon)
}

func (o *OracleController) Decide(now time.Time, st Stats) Knobs {
	t := int64(now.Sub(SimStart).Seconds())
	need := 0
	if t >= 0 && t < int64(len(o.windowMax)) {
		need = o.windowMax[t]
	}
	// Hold exactly what the window needs and not one address more: the warm
	// target is zero because the minimum target already covers every arrival
	// the oracle can see coming.
	return Knobs{MinimumIPTarget: need, WarmIPTargetsSet: true}
}
