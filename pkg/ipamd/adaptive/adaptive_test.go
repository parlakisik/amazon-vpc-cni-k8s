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
	"path/filepath"
	"testing"
	"time"
)

var testStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

const tick = 5 * time.Second

func TestTrackerDerivesArrivalsFromAssignedLevel(t *testing.T) {
	trk := NewTracker()
	now := testStart

	trk.Observe(now, 0)

	now = now.Add(tick)
	arrivals, departures := trk.Observe(now, 4)
	if arrivals != 4 || departures != 0 {
		t.Fatalf("rising level: arrivals/departures = %d/%d, want 4/0", arrivals, departures)
	}

	now = now.Add(tick)
	arrivals, departures = trk.Observe(now, 1)
	if arrivals != 0 || departures != 3 {
		t.Fatalf("falling level: arrivals/departures = %d/%d, want 0/3", arrivals, departures)
	}
}

func TestTrackerPrefersExactEventsOverLevelDeltas(t *testing.T) {
	trk := NewTracker()
	now := testStart
	trk.Observe(now, 0)

	// Three pods start and three finish inside one interval: the level does not
	// move, but the churn is real and the exact counters must carry it.
	trk.RecordAssign(3)
	trk.RecordRelease(3)

	now = now.Add(tick)
	arrivals, departures := trk.Observe(now, 0)
	if arrivals != 3 || departures != 3 {
		t.Fatalf("arrivals/departures = %d/%d, want 3/3", arrivals, departures)
	}
}

func TestTrackerIgnoresLongGaps(t *testing.T) {
	trk := NewTracker()
	now := testStart
	trk.Observe(now, 5)

	// An ipamd restart or a suspended instance must not be read as a burst of
	// demand that never happened.
	now = now.Add(30 * time.Minute)
	arrivals, departures := trk.Observe(now, 40)
	if arrivals != 0 || departures != 0 {
		t.Fatalf("after a %v gap: arrivals/departures = %d/%d, want 0/0", 30*time.Minute, arrivals, departures)
	}
}

func TestTrackerEstimatesPodLifetimeByLittlesLaw(t *testing.T) {
	trk := NewTracker()
	now := testStart

	// Hold 60 pods steady while one finishes every 5s. Little's law puts the
	// mean lifetime at 60 pods / 0.2 departures per second = 300s.
	trk.Observe(now, 60)
	for i := 0; i < 400; i++ {
		now = now.Add(tick)
		trk.RecordAssign(1)
		trk.RecordRelease(1)
		trk.Observe(now, 60)
	}

	got := trk.MeanPodLifetime()
	if got < 250*time.Second || got > 350*time.Second {
		t.Fatalf("mean pod lifetime = %v, want roughly 300s", got)
	}
}

// feedDailyBurst drives the tracker through days of a workload that is idle
// except for a burst of burstSize pods at the top of each hour.
func feedDailyBurst(trk *Tracker, days, burstSize int) time.Time {
	now := testStart
	trk.Observe(now, 0)

	assigned := 0
	for d := 0; d < days; d++ {
		for h := 0; h < 24; h++ {
			for i := 0; i < 720; i++ { // 720 * 5s = one hour
				now = now.Add(tick)
				switch i {
				case 0:
					assigned = burstSize
				case 60: // the burst's pods finish five minutes in
					assigned = 0
				}
				trk.Observe(now, assigned)
			}
		}
	}
	return now
}

func TestTrackerLearnsAnHourlyBurst(t *testing.T) {
	trk := NewTracker()
	feedDailyBurst(trk, 3, 24)

	// Just before the top of an hour, the profile must predict the climb.
	justBefore := testStart.Add(24 * time.Hour).Add(59*time.Minute + 30*time.Second)
	rise, ok := trk.ForecastRise(justBefore, 2*time.Minute)
	if !ok {
		t.Fatal("profile reported no data after three days")
	}
	if rise < 20 {
		t.Fatalf("predicted rise just before the burst = %.1f, want at least 20", rise)
	}

	// In the quiet middle of an hour it must predict nothing, or the node would
	// hold the burst-sized pool around the clock - exactly the waste that a
	// static MINIMUM_IP_TARGET already causes.
	quiet := testStart.Add(24 * time.Hour).Add(30 * time.Minute)
	rise, _ = trk.ForecastRise(quiet, 2*time.Minute)
	if rise > 5 {
		t.Fatalf("predicted rise in the quiet part of the hour = %.1f, want near zero", rise)
	}
}

func TestTrackerDoesNotDoubleCountARiseInProgress(t *testing.T) {
	trk := NewTracker()
	feedDailyBurst(trk, 3, 24)

	// Walk into the next burst and check that once the pods have arrived, the
	// tracker no longer claims the same climb is still ahead of us.
	now := testStart.Add(24 * time.Hour).Add(59*time.Minute + 55*time.Second)
	trk.Observe(now, 0)
	now = now.Add(tick)
	trk.Observe(now, 24)

	rise, _ := trk.ForecastRise(now, 2*time.Minute)
	if rise > 5 {
		t.Fatalf("predicted further rise mid-burst = %.1f, want near zero: the climb already happened", rise)
	}
}

func TestProfileReadyGatesOnObservedDays(t *testing.T) {
	trk := NewTracker()
	if trk.ProfileReady(testStart, 2*time.Minute, 1) {
		t.Fatal("an empty profile reported itself ready")
	}

	feedDailyBurst(trk, 2, 10)
	at := testStart.Add(24 * time.Hour).Add(30 * time.Minute)
	if !trk.ProfileReady(at, 2*time.Minute, 1) {
		t.Fatal("profile still not ready after two full days")
	}
}

func TestPolicyRespectsHardClamps(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	cfg.MinWarmIPTarget = 2
	cfg.MaxWarmIPTarget = 6
	cfg.MaxMinimumIPTarget = 9
	p := NewPolicy(cfg)

	now := testStart
	for i := 0; i < 2000; i++ {
		now = now.Add(tick)
		// A workload far larger than the clamps allow.
		tg := p.Step(now, Observation{
			AssignedIPs:  i % 200,
			AvailableIPs: 0,
			TotalIPs:     i % 200,
			Unfulfilled:  50,
		})
		if tg.WarmIPTarget < cfg.MinWarmIPTarget || tg.WarmIPTarget > cfg.MaxWarmIPTarget {
			t.Fatalf("warm target %d escaped the clamp [%d,%d]", tg.WarmIPTarget, cfg.MinWarmIPTarget, cfg.MaxWarmIPTarget)
		}
		if tg.MinimumIPTarget > cfg.MaxMinimumIPTarget {
			t.Fatalf("minimum target %d above the cap %d", tg.MinimumIPTarget, cfg.MaxMinimumIPTarget)
		}
	}
}

func TestPolicyIsNotReadyUntilItHasLearned(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	p := NewPolicy(cfg)

	tg := p.Step(testStart, Observation{AssignedIPs: 0, AvailableIPs: 0, TotalIPs: 0, Unfulfilled: -1})
	if tg.Ready {
		t.Fatal("a policy that has observed nothing reported itself ready to drive the pool")
	}
}

func TestTargetsFallGraduallyAndRiseAtOnce(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	p := NewPolicy(cfg)

	now := testStart
	p.Step(now, Observation{AssignedIPs: 0, AvailableIPs: 0, TotalIPs: 0, Unfulfilled: -1})

	// A burst pushes the targets up.
	for i := 0; i < 12; i++ {
		now = now.Add(tick)
		p.Step(now, Observation{AssignedIPs: i * 4, AvailableIPs: 2, TotalIPs: i*4 + 2, Unfulfilled: 5})
	}
	peak := p.Current()
	if peak.WarmIPTarget <= cfg.MinWarmIPTarget {
		t.Fatalf("warm target did not rise during a burst: %d", peak.WarmIPTarget)
	}

	// The node goes quiet. The target must come down, but never in one step.
	prev := peak.WarmIPTarget
	for i := 0; i < 200; i++ {
		now = now.Add(tick)
		tg := p.Step(now, Observation{AssignedIPs: 0, AvailableIPs: 40, TotalIPs: 40, Unfulfilled: 0})
		if drop := prev - tg.WarmIPTarget; drop > prev/2+1 {
			t.Fatalf("warm target fell from %d to %d in a single interval", prev, tg.WarmIPTarget)
		}
		prev = tg.WarmIPTarget
	}
	if prev >= peak.WarmIPTarget {
		t.Fatalf("warm target never came back down: still %d after the burst ended (peak %d)", prev, peak.WarmIPTarget)
	}
}

func TestPolicySurvivesTheClockStepingBackwards(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	p := NewPolicy(cfg)

	now := testStart
	for i := 0; i < 40; i++ {
		now = now.Add(tick)
		p.Step(now, Observation{AssignedIPs: i, AvailableIPs: 1, TotalIPs: i + 1, Unfulfilled: 3})
	}
	raised := p.Current().WarmIPTarget

	// An NTP correction drags the clock back an hour. The decay timer must not
	// end up parked in the future, freezing the targets where they stand.
	now = now.Add(-time.Hour)
	for i := 0; i < 200; i++ {
		now = now.Add(tick)
		p.Step(now, Observation{AssignedIPs: 0, AvailableIPs: 40, TotalIPs: 40, Unfulfilled: 0})
	}
	if got := p.Current().WarmIPTarget; got >= raised {
		t.Fatalf("warm target stuck at %d after the clock stepped backwards (was %d)", got, raised)
	}
}

func TestModelRoundTripsThroughDisk(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	p := NewPolicy(cfg)
	feedPolicyDailyBurst(p, 2, 20)

	path := filepath.Join(t.TempDir(), "model.json")
	if err := p.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored := NewPolicy(cfg)
	if err := restored.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	before, after := p.Export(), restored.Export()
	if len(before.Q) != len(after.Q) {
		t.Fatalf("restored %d q rows, saved %d", len(after.Q), len(before.Q))
	}
	if before.Tracker.Slots != after.Tracker.Slots {
		t.Fatal("restored daily profile differs from the saved one")
	}

	// The whole point of persisting is that a restarted node does not go back to
	// square one and re-learn its own burst.
	at := testStart.Add(24 * time.Hour).Add(59*time.Minute + 30*time.Second)
	rise, ok := restored.Tracker().ForecastRise(at, cfg.PrewarmWindow)
	if !ok || rise < 15 {
		t.Fatalf("restored policy forecasts a rise of %.1f (ok=%v), want the burst it learned before the restart", rise, ok)
	}
}

func TestLoadOfAMissingModelIsNotAnError(t *testing.T) {
	p := NewPolicy(DefaultConfig())
	if err := p.Load(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("loading a model that was never written: %v", err)
	}
}

func TestImportRejectsAnIncompatibleModel(t *testing.T) {
	p := NewPolicy(DefaultConfig())
	if err := p.Import(Model{Version: modelFormatVersion + 1, Actions: numActions()}); err == nil {
		t.Fatal("a model from a different format version was accepted")
	}
	if err := p.Import(Model{Version: modelFormatVersion, Actions: numActions() + 1}); err == nil {
		t.Fatal("a model with a different action space was accepted")
	}
}

func feedPolicyDailyBurst(p *Policy, days, burstSize int) {
	now := testStart
	assigned := 0
	for d := 0; d < days; d++ {
		for h := 0; h < 24; h++ {
			for i := 0; i < 720; i++ {
				now = now.Add(tick)
				switch i {
				case 0:
					assigned = burstSize
				case 60:
					assigned = 0
				}
				p.Step(now, Observation{
					AssignedIPs:  assigned,
					AvailableIPs: 4,
					TotalIPs:     assigned + 4,
					Unfulfilled:  -1,
				})
			}
		}
	}
}
