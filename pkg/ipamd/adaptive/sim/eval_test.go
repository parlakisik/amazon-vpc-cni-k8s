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
	"testing"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/adaptive"
)

const (
	trainDays = 7
	totalDays = 10

	adaptiveName = "ADAPTIVE (RL)"

	// idleTolerance is how many idle IPs of difference count as a tie when
	// checking whether a static policy dominates the learned one.
	idleTolerance = 1.0
)

// workloads builds the evaluation suite. Each is split into a training stretch
// the learned policy may see and a held-out stretch everything is scored on.
func workloads() []Workload {
	return []Workload{
		SteadyDeployment(totalDays, 11),
		DiurnalService(totalDays, 22),
		HourlyBatch(totalDays, 33),
		BurstyRollout(totalDays, 44),
	}
}

// baselines are the static knob settings an operator would realistically pick.
func baselines() []Controller {
	return []Controller{
		StaticENI(1),  // the shipped default
		Static(1, 0),  // aggressive: minimum address footprint
		Static(5, 0),  // the common recommendation
		Static(16, 0), // sized for a burst
		Static(5, 16), // warm target plus a floor
	}
}

func newAdaptive() *adaptive.Policy {
	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeEnforce
	cfg.Seed = 7
	return adaptive.NewPolicy(cfg)
}

// evaluate replays every policy against every workload's held-out days and
// returns the results keyed by workload.
func evaluate(t *testing.T, spec NodeSpec, extraBaselines []Controller) map[string][]Result {
	t.Helper()

	out := map[string][]Result{}
	for _, w := range workloads() {
		train, test := w.Split(trainDays)

		var results []Result
		for _, c := range append(baselines(), extraBaselines...) {
			results = append(results, Run(spec, test, c))
		}

		// The learned policy gets the training stretch first, then is scored on
		// the same held-out stretch as everyone else.
		p := newAdaptive()
		Run(spec, train, Adaptive(p))
		results = append(results, Run(spec, test, Adaptive(p)))

		reportTable(t, w, test, results)
		out[w.Name] = results
	}
	return out
}

// TestPolicyComparison is the evaluation harness, and the answer to "does the
// learned knob actually work". Every policy replays the same held-out pod trace
// through the same pool model and is scored on pod startup delay, idle IPs and
// EC2 calls.
//
// The claim being tested is not "the learned policy beats every static setting
// on every workload" - it cannot, because for any single workload some static
// setting is optimal for it. The claim is that one un-tuned policy is better
// across a mixed fleet than any one static setting, and that it never buys a
// smaller pool by making pods wait.
func TestPolicyComparison(t *testing.T) {
	byWorkload := evaluate(t, DefaultNodeSpec(), nil)

	// 1. Averaged over the suite - which is the situation of an operator who has
	//    to pick one setting for a cluster running all four workloads - the
	//    learned policy must be at least competitive with every static setting.
	//    At the default delay price it is in a near tie with the leanest static
	//    setting; TestCostSensitivity is where it pulls ahead, because that
	//    setting's cost is almost entirely pod delay.
	means := meanCostByPolicy(byWorkload)
	best, bestCost := "", 0.0
	for name, c := range means {
		if best == "" || c < bestCost {
			best, bestCost = name, c
		}
	}
	t.Logf("")
	t.Logf("mean cost across the workload suite (delay weight %.0f):", DefaultDelayWeight)
	for name, c := range means {
		t.Logf("  %-34s %12.0f", name, c)
	}
	if means[adaptiveName] > bestCost*1.05 {
		t.Errorf("adaptive mean cost %.0f is more than 5%% above the best static %q (%.0f)",
			means[adaptiveName], best, bestCost)
	}

	// 2. On no workload may a static setting beat the learned policy on pod
	//    delay AND address footprint at the same time. Being beaten on one while
	//    winning the other is a trade; being beaten on both is a defect.
	for name, results := range byWorkload {
		adp := pick(results, adaptiveName)
		for _, r := range results {
			if r.Policy == adaptiveName {
				continue
			}
			if r.DelayedPct < adp.DelayedPct && r.MeanIdleIPs+idleTolerance < adp.MeanIdleIPs {
				t.Errorf("%s: static %q dominates adaptive on both axes (delayed %.2f%% vs %.2f%%, idle %.1f vs %.1f)",
					name, r.Policy, r.DelayedPct, adp.DelayedPct, r.MeanIdleIPs, adp.MeanIdleIPs)
			}
		}
	}
}

// TestCostSensitivity reports how the ranking moves as the price of a pod-second
// of startup delay changes relative to an idle address. The ranking should not
// hinge on one arbitrary exchange rate, so the table is part of the result
// rather than a hidden constant.
func TestCostSensitivity(t *testing.T) {
	byWorkload := evaluate(t, DefaultNodeSpec(), nil)

	t.Logf("")
	t.Logf("mean cost across the suite at different prices for a pod-second of delay")
	t.Logf("(1 unit = holding one idle IP for one second)")
	t.Logf("  %-34s %12s %12s %12s %12s", "policy", "delay=1", "delay=20", "delay=100", "delay=500")

	weights := []float64{1, 20, 100, 500}
	names := policyNames(byWorkload)
	for _, name := range names {
		costs := make([]float64, len(weights))
		for i, w := range weights {
			sum, n := 0.0, 0
			for _, results := range byWorkload {
				r := pick(results, name)
				sum += r.CostAt(w)
				n++
			}
			costs[i] = sum / float64(n)
		}
		t.Logf("  %-34s %12.0f %12.0f %12.0f %12.0f", name, costs[0], costs[1], costs[2], costs[3])
	}

	// The learned policy is allowed to lose when delay is nearly free: at that
	// price the right answer really is to hold no addresses and let pods wait.
	// Once a pod's startup latency is worth more than a couple of dozen idle
	// address-seconds it must win, and by a widening margin, because unlike the
	// lean static settings almost none of its cost is pod delay.
	for _, w := range []float64{100, 500} {
		best, bestCost := "", 0.0
		for _, name := range names {
			sum, n := 0.0, 0
			for _, results := range byWorkload {
				sum += pick(results, name).CostAt(w)
				n++
			}
			if c := sum / float64(n); best == "" || c < bestCost {
				best, bestCost = name, c
			}
		}
		if best != adaptiveName {
			t.Errorf("at delay weight %.0f the best mean cost is %q, not the adaptive policy", w, best)
		}
	}
}

// TestPrefixDelegationComparison scores the same policies with PD enabled,
// where each allocation unit is a /28 and over-provisioning is 16x coarser.
func TestPrefixDelegationComparison(t *testing.T) {
	spec := DefaultNodeSpec().WithPrefixDelegation()

	for _, w := range workloads() {
		train, test := w.Split(trainDays)

		var results []Result
		for _, c := range []Controller{StaticPrefix(1), Static(1, 0), Static(16, 0), Static(32, 0)} {
			results = append(results, Run(spec, test, c))
		}

		p := newAdaptive()
		Run(spec, train, Adaptive(p))
		results = append(results, Run(spec, test, Adaptive(p)))

		reportTable(t, w, test, results)
	}
}

// TestColdStartIsSafe checks the property that matters most for shipping this.
// Every node starts with an empty model, and nodes are short-lived, so a policy
// that only behaves once it has learned something is a policy that mostly
// misbehaves. Before it has learned anything it must be no worse than today's
// default.
func TestColdStartIsSafe(t *testing.T) {
	spec := DefaultNodeSpec()

	for _, w := range workloads() {
		day1 := Workload{Name: w.Name, Days: 1}
		for _, pod := range w.Pods {
			if pod.Start < secondsPerDay {
				day1.Pods = append(day1.Pods, pod)
			}
		}

		def := Run(spec, day1, StaticENI(1))
		cold := Run(spec, day1, Adaptive(newAdaptive()))

		t.Logf("%-14s cold start: delayed %.2f%% (default %.2f%%), idle IPs %.1f (default %.1f), EC2 calls %d (default %d)",
			w.Name, cold.DelayedPct, def.DelayedPct, cold.MeanIdleIPs, def.MeanIdleIPs, cold.EC2Calls, def.EC2Calls)

		if cold.DelayedPct > def.DelayedPct+1.0 {
			t.Errorf("%s: cold-start policy delays materially more pods than the static default (%.2f%% vs %.2f%%)",
				w.Name, cold.DelayedPct, def.DelayedPct)
		}
	}
}

// TestSimulatorMirrorsIpamdTargetMath pins the warm/minimum target arithmetic
// against the cases documented in docs/eni-and-ip-target.md, so the harness
// cannot silently drift away from what pkg/ipamd actually does.
func TestSimulatorMirrorsIpamdTargetMath(t *testing.T) {
	spec := DefaultNodeSpec()

	cases := []struct {
		name              string
		st                Stats
		k                 Knobs
		wantShort         int
		wantOver          int
		wantWarmIPTargets bool
	}{
		{
			name:              "no warm ip target falls through to eni target mode",
			st:                Stats{TotalIPs: 14, AssignedIPs: 5, AvailableIPs: 9},
			k:                 Knobs{WarmENITarget: 1},
			wantWarmIPTargets: false,
		},
		{
			name:              "short by the gap to the warm target",
			st:                Stats{TotalIPs: 10, AssignedIPs: 9, AvailableIPs: 1},
			k:                 Knobs{WarmIPTarget: 5, WarmIPTargetsSet: true},
			wantShort:         4,
			wantWarmIPTargets: true,
		},
		{
			name:              "minimum ip target raises short beyond the warm target",
			st:                Stats{TotalIPs: 10, AssignedIPs: 9, AvailableIPs: 1},
			k:                 Knobs{WarmIPTarget: 5, MinimumIPTarget: 20, WarmIPTargetsSet: true},
			wantShort:         10,
			wantWarmIPTargets: true,
		},
		{
			name:              "over is capped so total never drops below the minimum",
			st:                Stats{TotalIPs: 20, AssignedIPs: 1, AvailableIPs: 19},
			k:                 Knobs{WarmIPTarget: 5, MinimumIPTarget: 16, WarmIPTargetsSet: true},
			wantOver:          4,
			wantWarmIPTargets: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			short, over, enabled := targetState(tc.st, tc.k, spec)
			if enabled != tc.wantWarmIPTargets {
				t.Fatalf("enabled = %v, want %v", enabled, tc.wantWarmIPTargets)
			}
			if short != tc.wantShort || over != tc.wantOver {
				t.Fatalf("short/over = %d/%d, want %d/%d", short, over, tc.wantShort, tc.wantOver)
			}
		})
	}
}

// TestPoolNeverExceedsInstanceLimits guards the pool model itself: no policy,
// however badly it behaves, may conjure capacity the instance type cannot hold.
func TestPoolNeverExceedsInstanceLimits(t *testing.T) {
	spec := DefaultNodeSpec()
	maxIPs := spec.MaxENI * spec.MaxIPsPerENI

	for _, w := range workloads() {
		for _, c := range append(baselines(), Adaptive(newAdaptive())) {
			r := Run(spec, w, c)
			if r.MaxTotalIPs > maxIPs {
				t.Errorf("%s/%s: pool reached %d IPs, above the instance limit of %d",
					w.Name, c.Name(), r.MaxTotalIPs, maxIPs)
			}
		}
	}
}

func pick(results []Result, policy string) Result {
	for _, r := range results {
		if r.Policy == policy {
			return r
		}
	}
	return Result{Policy: policy}
}

func policyNames(byWorkload map[string][]Result) []string {
	for _, results := range byWorkload {
		names := make([]string, 0, len(results))
		for _, r := range results {
			names = append(names, r.Policy)
		}
		return names
	}
	return nil
}

func meanCostByPolicy(byWorkload map[string][]Result) map[string]float64 {
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, results := range byWorkload {
		for _, r := range results {
			sums[r.Policy] += r.Cost()
			counts[r.Policy]++
		}
	}
	means := make(map[string]float64, len(sums))
	for name, s := range sums {
		means[name] = s / float64(counts[name])
	}
	return means
}

func reportTable(t *testing.T, w Workload, test Workload, results []Result) {
	t.Helper()
	t.Logf("")
	t.Logf("workload %q - %s", w.Name, w.Desc)
	t.Logf("  %d pods over %d held-out days, peak %d concurrent",
		len(test.Pods), test.Days, test.PeakConcurrent())
	t.Logf("  %-34s %9s %9s %9s %9s %9s %10s",
		"policy", "delayed%", "waitSec", "p99wait", "idleIPs", "ec2calls", "cost")
	for _, r := range results {
		t.Logf("  %-34s %9.2f %9d %9d %9.1f %9d %10.0f",
			r.Policy, r.DelayedPct, r.WaitSeconds, r.P99Wait, r.MeanIdleIPs, r.EC2Calls, r.Cost())
	}
}
