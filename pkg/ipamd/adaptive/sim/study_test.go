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

// This file holds the study-grade evaluation: repeated trials with confidence
// intervals, a clairvoyant upper bound, an ablation of every design decision,
// and a sweep across instance sizes. TestPolicyComparison in eval_test.go is
// the fast regression check; this is the one whose numbers are worth writing
// down. Run it with:
//
//	go test ./pkg/ipamd/adaptive/sim/ -run 'TestStudy' -v -timeout 30m
//
// It is skipped under -short.

package sim

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/adaptive"
)

// trials is how many independently seeded traces each workload family is
// evaluated on. Every policy sees exactly the same traces.
const trials = 5

// studentT975 is the two-sided 95% critical value of Student's t for
// trials-1 = 4 degrees of freedom. With this few trials the normal
// approximation understates the interval by a third.
const studentT975 = 2.776

// workloadFamily generates one workload family at a given seed.
type workloadFamily struct {
	name string
	gen  func(days int, seed int64) Workload
}

func families() []workloadFamily {
	return []workloadFamily{
		{"steady", SteadyDeployment},
		{"diurnal", DiurnalService},
		{"hourly-batch", HourlyBatch},
		{"bursty-rollout", BurstyRollout},
	}
}

// sample is one policy's score on one trial.
type sample struct {
	delayedPct  float64
	waitSeconds float64
	idleIPs     float64
	ec2Calls    float64
	cost        float64
}

func sampleOf(r Result) sample {
	return sample{
		delayedPct:  r.DelayedPct,
		waitSeconds: float64(r.WaitSeconds),
		idleIPs:     r.MeanIdleIPs,
		ec2Calls:    float64(r.EC2Calls),
		cost:        r.Cost(),
	}
}

// candidate is a named policy under test. Controllers are built per trial
// because a learned policy carries state and a clairvoyant one is tied to its
// trace.
type candidate struct {
	name  string
	build func(train, test Workload) Controller
	// train reports whether the controller should be given the training days
	// before being scored.
	train bool
}

func staticCandidate(name string, c Controller) candidate {
	return candidate{name: name, build: func(_, _ Workload) Controller { return c }}
}

func adaptiveCandidate(name string, mutate func(*adaptive.Config)) candidate {
	return candidate{
		name:  name,
		train: true,
		build: func(_, _ Workload) Controller {
			cfg := adaptive.DefaultConfig()
			cfg.Mode = adaptive.ModeEnforce
			cfg.Seed = 7
			if mutate != nil {
				mutate(&cfg)
			}
			return &AdaptiveController{Policy: adaptive.NewPolicy(cfg), Fallback: StaticENI(1)}
		},
	}
}

// runStudy scores every candidate on every trial of every family and returns
// the samples keyed by family then candidate name.
func runStudy(t *testing.T, spec NodeSpec, candidates []candidate) map[string]map[string][]sample {
	t.Helper()

	out := map[string]map[string][]sample{}
	for _, f := range families() {
		byPolicy := map[string][]sample{}
		for trial := 0; trial < trials; trial++ {
			w := f.gen(totalDays, int64(1000+trial*17))
			train, test := w.Split(trainDays)

			for _, c := range candidates {
				ctrl := c.build(train, test)
				if c.train {
					Run(spec, train, ctrl)
				}
				byPolicy[c.name] = append(byPolicy[c.name], sampleOf(Run(spec, test, ctrl)))
			}
		}
		out[f.name] = byPolicy
	}
	return out
}

// meanCI returns the mean and the half-width of the 95% confidence interval.
func meanCI(xs []float64) (mean, halfWidth float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= n
	if n < 2 {
		return mean, 0
	}
	sumSq := 0.0
	for _, x := range xs {
		d := x - mean
		sumSq += d * d
	}
	stddev := math.Sqrt(sumSq / (n - 1))
	return mean, studentT975 * stddev / math.Sqrt(n)
}

func field(samples []sample, get func(sample) float64) []float64 {
	xs := make([]float64, len(samples))
	for i, s := range samples {
		xs[i] = get(s)
	}
	return xs
}

// TestStudyMainResult is the headline table: every policy on every workload
// family, over independently seeded trials, with confidence intervals, against
// both the static settings an operator would pick and a clairvoyant upper
// bound.
func TestStudyMainResult(t *testing.T) {
	if testing.Short() {
		t.Skip("study-grade evaluation; run without -short")
	}

	spec := DefaultNodeSpec()
	candidates := []candidate{
		staticCandidate("WARM_ENI_TARGET=1 (default)", StaticENI(1)),
		staticCandidate("WARM_IP_TARGET=1", Static(1, 0)),
		staticCandidate("WARM_IP_TARGET=5", Static(5, 0)),
		staticCandidate("WARM_IP_TARGET=16", Static(16, 0)),
		staticCandidate("WARM_IP_TARGET=5,MIN=16", Static(5, 16)),
		adaptiveCandidate("ADAPTIVE (RL)", nil),
		{
			name: "ORACLE (clairvoyant)",
			build: func(_, test Workload) Controller {
				return Oracle(test, adaptive.DefaultConfig().PrewarmWindow)
			},
		},
	}

	byFamily := runStudy(t, spec, candidates)

	t.Logf("")
	t.Logf("=== Main result: %d independently seeded trials per family, mean +/- 95%% CI ===", trials)
	for _, f := range families() {
		t.Logf("")
		t.Logf("workload family %q", f.name)
		t.Logf("  %-30s %18s %18s %18s", "policy", "pods delayed %", "mean idle IPs", "EC2 calls")
		for _, c := range candidates {
			s := byFamily[f.name][c.name]
			dm, dc := meanCI(field(s, func(x sample) float64 { return x.delayedPct }))
			im, ic := meanCI(field(s, func(x sample) float64 { return x.idleIPs }))
			em, ec := meanCI(field(s, func(x sample) float64 { return x.ec2Calls }))
			t.Logf("  %-30s %8.2f +/- %-6.2f %8.2f +/- %-6.2f %8.0f +/- %-6.0f", c.name, dm, dc, im, ic, em, ec)
		}
	}

	reportGapClosed(t, byFamily, candidates)

	// The learned policy must never be worse than the clairvoyant bound on both
	// axes at once, which would mean it is not merely imperfect but incoherent.
	for _, f := range families() {
		adp := byFamily[f.name]["ADAPTIVE (RL)"]
		orc := byFamily[f.name]["ORACLE (clairvoyant)"]
		am, _ := meanCI(field(adp, func(x sample) float64 { return x.idleIPs }))
		om, _ := meanCI(field(orc, func(x sample) float64 { return x.idleIPs }))
		if om > am+idleTolerance {
			t.Errorf("%s: the clairvoyant bound holds MORE idle IPs (%.1f) than the learned policy (%.1f); the bound is not a bound",
				f.name, om, am)
		}
	}
}

// reportGapClosed expresses the learned policy as a fraction of the distance
// between the shipped default and perfect knowledge. It is the number a reader
// should care about: beating an arbitrary baseline says little, closing most of
// the achievable gap says a lot.
func reportGapClosed(t *testing.T, byFamily map[string]map[string][]sample, _ []candidate) {
	t.Helper()
	t.Logf("")
	t.Logf("=== Default -> adaptive -> clairvoyant, and the fraction of the gap closed ===")
	t.Logf("  %-18s %-14s %10s %10s %10s %10s", "family", "metric", "default", "adaptive", "oracle", "gap closed")

	for _, f := range families() {
		def := byFamily[f.name]["WARM_ENI_TARGET=1 (default)"]
		adp := byFamily[f.name]["ADAPTIVE (RL)"]
		orc := byFamily[f.name]["ORACLE (clairvoyant)"]

		row := func(metric string, minGap float64, get func(sample) float64) {
			d, _ := meanCI(field(def, get))
			a, _ := meanCI(field(adp, get))
			o, _ := meanCI(field(orc, get))
			// When the default is already as good as perfect knowledge there is
			// no gap to close, and the ratio is a meaningless division by noise.
			closed := "no gap"
			if math.Abs(d-o) >= minGap {
				closed = fmt.Sprintf("%.0f%%", 100*(d-a)/(d-o))
			}
			t.Logf("  %-18s %-14s %10.2f %10.2f %10.2f %10s", f.name, metric, d, a, o, closed)
		}
		row("idle IPs", 0.5, func(x sample) float64 { return x.idleIPs })
		row("pods delayed %", 1.0, func(x sample) float64 { return x.delayedPct })
	}
}

// TestStudyAblation removes one design decision at a time. Each row is a claim
// in docs/adaptive-ip-target.md with a number attached to it.
func TestStudyAblation(t *testing.T) {
	if testing.Short() {
		t.Skip("study-grade evaluation; run without -short")
	}

	spec := DefaultNodeSpec()
	candidates := []candidate{
		adaptiveCandidate("full policy", nil),
		adaptiveCandidate("-RL (forecast + fixed headroom)", func(c *adaptive.Config) {
			c.DisableLearning = true
		}),
		// If the learned policy only loses to the forecast because it is still
		// exploring at evaluation time, turning exploration off should recover
		// the difference. If it does not, the learning itself is what is not
		// paying for itself.
		adaptiveCandidate("RL without exploration", func(c *adaptive.Config) {
			c.Epsilon = 0
			c.EpsilonMin = 0
		}),
		adaptiveCandidate("-reward shaping", func(c *adaptive.Config) {
			c.ShapingWeight = 0
		}),
		adaptiveCandidate("-seasonal pre-warm", func(c *adaptive.Config) {
			c.MaxMinimumIPTarget = 0
		}),
		adaptiveCandidate("-readiness gate", func(c *adaptive.Config) {
			c.MinProfileDays = 0
		}),
		adaptiveCandidate("hourly profile (10min -> 60min)", func(c *adaptive.Config) {
			c.ProfileAggregation = 6
		}),
		adaptiveCandidate("-target decay damping", func(c *adaptive.Config) {
			c.TargetDecayInterval = time.Nanosecond
			c.TargetDecayFraction = 1.0
		}),
	}

	byFamily := runStudy(t, spec, candidates)

	t.Logf("")
	t.Logf("=== Ablation: %d trials per family, mean +/- 95%% CI ===", trials)
	for _, f := range families() {
		t.Logf("")
		t.Logf("workload family %q", f.name)
		t.Logf("  %-34s %18s %18s %18s", "variant", "pods delayed %", "mean idle IPs", "EC2 calls")
		for _, c := range candidates {
			s := byFamily[f.name][c.name]
			dm, dc := meanCI(field(s, func(x sample) float64 { return x.delayedPct }))
			im, ic := meanCI(field(s, func(x sample) float64 { return x.idleIPs }))
			em, ec := meanCI(field(s, func(x sample) float64 { return x.ec2Calls }))
			t.Logf("  %-34s %8.2f +/- %-6.2f %8.2f +/- %-6.2f %8.0f +/- %-6.0f", c.name, dm, dc, im, ic, em, ec)
		}
	}

	// The two ablations the design rests on must each cost something visible on
	// the workload they exist for, or the design decision is unjustified.
	batch := byFamily["hourly-batch"]
	full, _ := meanCI(field(batch["full policy"], func(x sample) float64 { return x.delayedPct }))
	noPrewarm, _ := meanCI(field(batch["-seasonal pre-warm"], func(x sample) float64 { return x.delayedPct }))
	if noPrewarm <= full {
		t.Errorf("removing the seasonal pre-warm did not hurt the scheduled-burst workload (%.2f%% vs %.2f%%): the feature is not earning its place",
			noPrewarm, full)
	}

	fullIdle, _ := meanCI(field(batch["full policy"], func(x sample) float64 { return x.idleIPs }))
	hourlyIdle, _ := meanCI(field(batch["hourly profile (10min -> 60min)"], func(x sample) float64 { return x.idleIPs }))
	if hourlyIdle <= fullIdle {
		t.Errorf("coarsening the profile to hour buckets did not cost idle addresses (%.2f vs %.2f): the 10-minute resolution is unjustified",
			hourlyIdle, fullIdle)
	}

	noShaping, _ := meanCI(field(batch["-reward shaping"], func(x sample) float64 { return x.delayedPct }))
	if noShaping <= full {
		t.Errorf("removing reward shaping did not hurt the scheduled-burst workload (%.2f%% vs %.2f%%): the credit assignment problem it solves is not real",
			noShaping, full)
	}

	// The reinforcement learning layer is NOT asserted to help. The ablation
	// measures whether it does, and as of writing it does not: see
	// docs/adaptive-ip-target.md. Asserting it here would only encode a wish.
}

// TestStudyInstanceSizes checks that the result is not an artefact of one
// instance type. A small node has almost no room to hold a warm pool; a large
// one has room to waste a great deal of a subnet.
func TestStudyInstanceSizes(t *testing.T) {
	if testing.Short() {
		t.Skip("study-grade evaluation; run without -short")
	}

	specs := []struct {
		name string
		spec NodeSpec
	}{
		{"m5.large-like (3 ENI x 9 IP)", NodeSpec{
			MaxENI: 3, MaxIPsPerENI: 9, IPsPerPrefix: 16,
			IPAllocSeconds: 2, ENIAttachSeconds: 20, CooldownSeconds: 30,
			ControlSeconds: 5, DecreaseSeconds: 30, MinENILifeSeconds: 60,
		}},
		{"m5.xlarge-like (4 ENI x 14 IP)", DefaultNodeSpec()},
		{"m5.4xlarge-like (8 ENI x 29 IP)", NodeSpec{
			MaxENI: 8, MaxIPsPerENI: 29, IPsPerPrefix: 16,
			IPAllocSeconds: 2, ENIAttachSeconds: 20, CooldownSeconds: 30,
			ControlSeconds: 5, DecreaseSeconds: 30, MinENILifeSeconds: 60,
		}},
		{"m5.xlarge-like, prefix delegation", DefaultNodeSpec().WithPrefixDelegation()},
	}

	candidates := []candidate{
		staticCandidate("WARM_ENI_TARGET=1 (default)", StaticENI(1)),
		staticCandidate("WARM_IP_TARGET=5", Static(5, 0)),
		staticCandidate("WARM_IP_TARGET=16", Static(16, 0)),
		adaptiveCandidate("ADAPTIVE (RL)", nil),
	}

	t.Logf("")
	t.Logf("=== Instance size sweep, scheduled-burst workload, %d trials, mean +/- 95%% CI ===", trials)
	for _, s := range specs {
		byFamily := runStudy(t, s.spec, candidates)
		t.Logf("")
		t.Logf("%s", s.name)
		t.Logf("  %-30s %18s %18s", "policy", "pods delayed %", "mean idle IPs")
		for _, c := range candidates {
			samples := byFamily["hourly-batch"][c.name]
			dm, dc := meanCI(field(samples, func(x sample) float64 { return x.delayedPct }))
			im, ic := meanCI(field(samples, func(x sample) float64 { return x.idleIPs }))
			t.Logf("  %-30s %8.2f +/- %-6.2f %8.2f +/- %-6.2f", c.name, dm, dc, im, ic)
		}
	}
}

// TestStudyLearningCurve reports how long a node has to run before the policy
// is worth having, which is the question that decides whether this is
// deployable at all: nodes in an autoscaled cluster are often short-lived.
func TestStudyLearningCurve(t *testing.T) {
	if testing.Short() {
		t.Skip("study-grade evaluation; run without -short")
	}

	spec := DefaultNodeSpec()

	t.Logf("")
	t.Logf("=== Learning curve: score on day N of a single continuous run, %d trials ===", trials)
	t.Logf("  %-18s %6s %18s %18s", "family", "day", "pods delayed %", "mean idle IPs")

	const days = 8
	for _, f := range families() {
		perDay := make([][]sample, days)
		for trial := 0; trial < trials; trial++ {
			w := f.gen(days, int64(2000+trial*23))

			cfg := adaptive.DefaultConfig()
			cfg.Mode = adaptive.ModeEnforce
			cfg.Seed = 7
			policy := adaptive.NewPolicy(cfg)
			ctrl := &AdaptiveController{Policy: policy, Fallback: StaticENI(1)}

			// One continuous run, scored a day at a time. The policy is never
			// reset, so day N reflects N-1 days of learning - the situation a
			// real node is actually in.
			for d := 0; d < days; d++ {
				day := dayOf(w, d)
				perDay[d] = append(perDay[d], sampleOf(Run(spec, day, ctrl)))
			}
		}

		for d := 0; d < days; d++ {
			dm, dc := meanCI(field(perDay[d], func(x sample) float64 { return x.delayedPct }))
			im, ic := meanCI(field(perDay[d], func(x sample) float64 { return x.idleIPs }))
			label := ""
			if d == 0 {
				label = f.name
			}
			t.Logf("  %-18s %6d %8.2f +/- %-6.2f %8.2f +/- %-6.2f", label, d+1, dm, dc, im, ic)
		}
	}
}

// dayOf extracts day d of a workload as a one-day trace starting at zero.
func dayOf(w Workload, d int) Workload {
	lo := int64(d) * secondsPerDay
	hi := lo + secondsPerDay
	out := Workload{Name: fmt.Sprintf("%s/day%d", w.Name, d+1), Desc: w.Desc, Days: 1}
	for _, p := range w.Pods {
		if p.Start >= lo && p.Start < hi {
			out.Pods = append(out.Pods, Pod{Start: p.Start - lo, Life: p.Life})
		}
	}
	sort.Slice(out.Pods, func(i, j int) bool { return out.Pods[i].Start < out.Pods[j].Start })
	return out
}
