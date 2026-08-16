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

// Package adaptive implements a learned replacement for the static warm target
// knobs (WARM_IP_TARGET, MINIMUM_IP_TARGET, WARM_ENI_TARGET, WARM_PREFIX_TARGET).
//
// Instead of asking the operator to pick constants that must hold for every hour
// of every day, the tracker observes how many IPs the node actually has assigned
// over time, learns the node's daily demand profile and how long pods live, and
// feeds that forecast to a reinforcement learning policy that picks the warm
// targets for the next control interval.
package adaptive

import (
	"math"
	"sync"
	"time"
)

const (
	// SlotMinutes is the resolution of the learned daily profile. Hour buckets
	// are too coarse for the workload this policy exists to serve: a CronJob
	// fan-out of 30 pods at the top of the hour averages out to a rate of
	// 0.008 pods/sec over an hour, which forecasts nothing. Ten-minute slots
	// keep the burst visible while still folding several days together.
	SlotMinutes = 10

	// SlotsPerDay is the number of profile buckets in a day.
	SlotsPerDay = 24 * 60 / SlotMinutes

	// maxSampleGap is the longest gap between two observations that we are still
	// willing to attribute demand to. Anything longer (ipamd restart, node
	// suspend, clock jump) only re-baselines the tracker.
	maxSampleGap = 2 * time.Minute

	// seasonalAlpha is the EWMA weight applied to a new day's observation of a
	// slot. 0.3 means roughly the last 3-4 days dominate the profile, so the
	// node adapts to a workload change within a few days without forgetting the
	// shape of a normal day.
	seasonalAlpha = 0.3

	// fastAlpha is the EWMA weight for the short-horizon arrival rate. With a 5s
	// control interval this gives a time constant of roughly one minute.
	fastAlpha = 0.08

	// recentWindow is how many control intervals of raw arrival history to keep.
	//
	// Feeding the largest of these into the forecast was tried as a reactive
	// answer to unscheduled bursts and did not pay: it raised the pool on every
	// workload but only moved pod delay on the one workload it was aimed at,
	// which no forecast can help anyway (see docs/adaptive-ip-target.md). It is
	// kept only as an introspection signal.
	recentWindow = 12
)

// SlotProfile is the learned demand shape of one slot of the day.
type SlotProfile struct {
	// ArrivalRate is the EWMA of pod arrivals per second during this slot.
	ArrivalRate float64 `json:"arrivalRate"`
	// PeakArrivals is the EWMA of the largest number of pods that arrived in a
	// single control interval during this slot. This is what survives a burst:
	// a 30 pod fan-out shows up here even though it barely moves ArrivalRate.
	PeakArrivals float64 `json:"peakArrivals"`
	// PeakRise is the EWMA of the largest upward excursion in assigned IPs
	// within this slot, measured from the level the slot started at.
	//
	// This, not the absolute peak, is what has to be pre-warmed: the IPs pods
	// already hold are not going anywhere, so the only capacity that has to be
	// waiting is the capacity the next few minutes will *add*. Using the
	// absolute level instead makes a node on a slowly drifting workload hold
	// several idle IPs forever, for nothing.
	PeakRise float64 `json:"peakRise"`
	// Days is how many distinct days have contributed to this slot.
	Days int `json:"days"`
}

// TrackerState is the serializable part of the tracker, persisted so that a
// restarted ipamd does not have to re-learn the node's daily profile.
type TrackerState struct {
	Slots         [SlotsPerDay]SlotProfile `json:"slots"`
	ArrivalRate   float64                  `json:"arrivalRate"`
	DepartureRate float64                  `json:"departureRate"`
	// CountMean and CountVar are the mean and variance of pod arrivals per
	// control interval, and IntervalSeconds is how long that interval is. They
	// are kept in counts rather than rates because the burst allowance scales
	// with the square root of the number of intervals, not with their length.
	CountMean       float64 `json:"countMean"`
	CountVar        float64 `json:"countVar"`
	IntervalSeconds float64 `json:"intervalSeconds"`
	MeanPodSeconds  float64 `json:"meanPodSeconds"`
	TotalArrivals   float64 `json:"totalArrivals"`
	ObservedFor     float64 `json:"observedForSeconds"`
}

// Tracker learns a node's pod demand pattern from the IPAM datastore.
//
// It deliberately consumes only the assigned-IP level (plus, when available,
// exact assign/release counts) rather than watching the Kubernetes API: every
// signal it needs is already local to ipamd, so the policy keeps working when
// the API server is unreachable and costs no extra watch.
type Tracker struct {
	mu sync.Mutex

	state TrackerState

	lastSample   time.Time
	lastAssigned int
	lastSlot     int
	slotBase     int
	slotMaxRise  int
	slotMaxBurst int
	slotArrivals float64
	slotSeconds  float64
	started      bool

	// exact event counters, fed by RecordAssign/RecordRelease when the caller
	// can supply them. When unused, the tracker falls back to level deltas.
	pendingAssign  int
	pendingRelease int
	exactEvents    bool

	// recent is a ring of the last recentWindow arrival counts, exposed for
	// introspection and used to report how bursty the node currently is.
	recent    [recentWindow]int
	recentIdx int
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{lastSlot: -1}
}

// SlotOf returns the profile slot a time falls in.
func SlotOf(t time.Time) int {
	return (t.Hour()*60 + t.Minute()) / SlotMinutes
}

// RecordAssign reports that n IPs were handed to pods. Optional: when it is
// never called the tracker derives arrivals from the assigned-IP level instead.
func (t *Tracker) RecordAssign(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingAssign += n
	t.exactEvents = true
}

// RecordRelease reports that n IPs were returned by pods.
func (t *Tracker) RecordRelease(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingRelease += n
	t.exactEvents = true
}

// Observe records the current number of assigned IPs at time now. It returns
// the number of pod arrivals and departures attributed to the interval that
// just closed.
func (t *Tracker) Observe(now time.Time, assigned int) (arrivals, departures int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	pendA, pendR := t.pendingAssign, t.pendingRelease
	t.pendingAssign, t.pendingRelease = 0, 0

	if !t.started {
		t.started = true
		t.lastSample = now
		t.lastAssigned = assigned
		t.slotBase = assigned
		t.lastSlot = SlotOf(now)
		return 0, 0
	}

	dt := now.Sub(t.lastSample)
	if dt <= 0 || dt > maxSampleGap {
		// Re-baseline without attributing the gap to demand.
		t.lastSample = now
		t.lastAssigned = assigned
		t.slotMaxRise = max(t.slotMaxRise, assigned-t.slotBase)
		t.lastSlot = SlotOf(now)
		return 0, 0
	}

	if t.exactEvents {
		arrivals, departures = pendA, pendR
	} else {
		// Level deltas undercount churn when an arrival and a departure land in
		// the same interval. At a 5s control interval that is rare, and the
		// resulting bias only affects the departure rate, never the arrival
		// forecast that drives safety.
		delta := assigned - t.lastAssigned
		arrivals = max(delta, 0)
		departures = max(-delta, 0)
	}

	secs := dt.Seconds()
	t.state.TotalArrivals += float64(arrivals)
	t.state.ObservedFor += secs

	// Short-horizon rates, EWMA in per-second units.
	instArrival := float64(arrivals) / secs
	instDeparture := float64(departures) / secs
	t.state.ArrivalRate = ewma(t.state.ArrivalRate, instArrival, fastAlpha)
	t.state.DepartureRate = ewma(t.state.DepartureRate, instDeparture, fastAlpha)

	// Track the mean and variance of arrivals per control interval so the policy
	// can size a burst buffer.
	t.state.IntervalSeconds = ewma(t.state.IntervalSeconds, secs, fastAlpha)
	t.state.CountMean = ewma(t.state.CountMean, float64(arrivals), fastAlpha)
	dev := float64(arrivals) - t.state.CountMean
	t.state.CountVar = ewma(t.state.CountVar, dev*dev, fastAlpha)

	// Little's law: with L IPs assigned and a departure rate of lambda pods per
	// second, the mean pod lifetime is L/lambda. This tells us how long pods
	// keep running without tracking any single pod.
	if t.state.DepartureRate > 1e-9 && assigned > 0 {
		life := float64(assigned) / t.state.DepartureRate
		t.state.MeanPodSeconds = ewma(t.state.MeanPodSeconds, life, fastAlpha)
	}

	t.recent[t.recentIdx%recentWindow] = arrivals
	t.recentIdx++

	// Close the slot that just ended before accumulating, so that a sample
	// landing on a slot boundary counts towards the slot it is actually in.
	// Getting this backwards credits the first burst of a new slot to the quiet
	// slot before it, and then the new slot looks like it still has the whole
	// climb ahead of it when the pods have already arrived.
	if slot := SlotOf(now); slot != t.lastSlot {
		t.flushSlotLocked()
		t.lastSlot = slot
	}

	t.slotArrivals += float64(arrivals)
	t.slotSeconds += secs
	t.slotMaxRise = max(t.slotMaxRise, assigned-t.slotBase)
	t.slotMaxBurst = max(t.slotMaxBurst, arrivals)

	t.lastSample = now
	t.lastAssigned = assigned
	return arrivals, departures
}

// flushSlotLocked folds the slot that just ended into the daily profile.
func (t *Tracker) flushSlotLocked() {
	if t.lastSlot < 0 || t.slotSeconds <= 0 {
		t.resetSlotLocked()
		return
	}
	p := &t.state.Slots[t.lastSlot]
	rate := t.slotArrivals / t.slotSeconds
	if p.Days == 0 {
		p.ArrivalRate = rate
		p.PeakArrivals = float64(t.slotMaxBurst)
		p.PeakRise = float64(t.slotMaxRise)
	} else {
		p.ArrivalRate = ewma(p.ArrivalRate, rate, seasonalAlpha)
		p.PeakArrivals = ewma(p.PeakArrivals, float64(t.slotMaxBurst), seasonalAlpha)
		p.PeakRise = ewma(p.PeakRise, float64(t.slotMaxRise), seasonalAlpha)
	}
	p.Days++
	t.resetSlotLocked()
}

func (t *Tracker) resetSlotLocked() {
	t.slotArrivals = 0
	t.slotSeconds = 0
	t.slotMaxBurst = 0
	t.slotMaxRise = 0
	t.slotBase = t.lastAssigned
}

// RecentPeakArrivals returns the largest number of pods that arrived in a
// single control interval over the last recentWindow intervals.
func (t *Tracker) RecentPeakArrivals() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	peak := 0
	for _, c := range t.recent {
		peak = max(peak, c)
	}
	return peak
}

// ArrivalRate returns the recent pod arrival rate in pods per second.
func (t *Tracker) ArrivalRate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.ArrivalRate
}

// MeanPodLifetime returns the estimated mean pod lifetime, derived by Little's
// law from the assigned-IP level and the departure rate. Zero means unknown.
func (t *Tracker) MeanPodLifetime() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.MeanPodSeconds <= 0 {
		return 0
	}
	return time.Duration(t.state.MeanPodSeconds * float64(time.Second))
}

// ForecastArrivals predicts how many pods will start within horizon of now.
//
// It blends what is happening right now (the short-horizon rate plus a burst
// allowance of z standard deviations) with what the daily profile says usually
// happens in the slot our provisioning would land in, taking the larger of the
// two. That is what lets a learned 09:00 ramp pre-warm the pool before the ramp
// is visible in the instantaneous rate.
func (t *Tracker) ForecastArrivals(now time.Time, horizon time.Duration, z float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	h := horizon.Seconds()
	if h <= 0 {
		return 0
	}

	// Arrivals in consecutive control intervals are close to independent, so the
	// spread of the total over n of them grows with sqrt(n), not with n. Scaling
	// the per-interval standard deviation by the whole horizon instead - the
	// obvious-looking mistake - inflates the buffer by an order of magnitude and
	// pins the pool at the instance maximum.
	n := 1.0
	if t.state.IntervalSeconds > 0 {
		n = math.Max(1, h/t.state.IntervalSeconds)
	}
	live := t.state.ArrivalRate*h + z*math.Sqrt(t.state.CountVar)*math.Sqrt(n)

	seasonal := 0.0
	for _, s := range t.slotsInLocked(now, horizon) {
		if s.Days == 0 {
			continue
		}
		seasonal = math.Max(seasonal, s.ArrivalRate*h)
	}

	return math.Max(0, math.Max(live, seasonal))
}

// ForecastRise returns how much further the assigned-IP level is expected to
// climb within [now, now+window], and whether the profile has enough data to be
// usable.
//
// This is the learned stand-in for MINIMUM_IP_TARGET, and the window is why it
// beats the static knob: a node that takes 30 extra pods at the top of every
// hour only has to hold those addresses for the few minutes before the burst,
// not for all 60.
//
// The climb still to come is not the same as the climb the slot contains. Once
// a burst is half way through, most of its rise has already happened and the
// IPs are already assigned; adding the slot's full rise on top of the current
// level would provision the burst twice. So the current slot contributes only
// the part of its usual rise that has not been observed yet, while slots that
// have not started contribute all of theirs.
func (t *Tracker) ForecastRise(now time.Time, window time.Duration) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	slots := t.slotsInLocked(now, window)
	if len(slots) == 0 {
		return 0, false
	}

	rise, ok := 0.0, false

	// The slot we are in, minus what it has already delivered.
	if cur := slots[0]; cur.Days > 0 {
		ok = true
		remaining := math.Max(cur.PeakRise, cur.PeakArrivals) - float64(t.slotMaxRise)
		rise = math.Max(rise, remaining)
	}

	// Slots that have not begun yet contribute their whole expected rise.
	for _, s := range slots[1:] {
		if s.Days == 0 {
			continue
		}
		ok = true
		rise = math.Max(rise, math.Max(s.PeakRise, s.PeakArrivals))
	}

	return math.Max(0, rise), ok
}

// slotsInLocked returns the profile slots that overlap [now, now+window].
//
// The offset within the current slot matters: a node 30 seconds into a slot
// must not already see the next slot's burst when the window is two minutes,
// or it pre-warms nine minutes too early and holds the burst-sized pool for the
// whole slot.
func (t *Tracker) slotsInLocked(now time.Time, window time.Duration) []SlotProfile {
	slotLen := SlotMinutes * time.Minute
	start := SlotOf(now)
	elapsed := time.Duration(now.Minute()%SlotMinutes)*time.Minute + time.Duration(now.Second())*time.Second

	out := []SlotProfile{t.state.Slots[start]}
	for i := 1; i < SlotsPerDay; i++ {
		// Slot start+i begins this far from now.
		startsIn := time.Duration(i)*slotLen - elapsed
		if startsIn > window {
			break
		}
		out = append(out, t.state.Slots[(start+i)%SlotsPerDay])
	}
	return out
}

// ProfileReady reports whether every slot covering [now, now+window] has been
// observed on at least minDays distinct days.
//
// Until it has, the policy has nothing to forecast from and must not be driving
// the pool: a node whose first hour includes a 30 pod fan-out is far better
// served by the static WARM_ENI_TARGET behaviour, which keeps a whole spare
// ENI, than by a learned controller that has never seen the burst.
func (t *Tracker) ProfileReady(now time.Time, window time.Duration, minDays int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, s := range t.slotsInLocked(now, window) {
		if s.Days < minDays {
			return false
		}
	}
	return true
}

// Snapshot returns a copy of the learned state for persistence or debugging.
func (t *Tracker) Snapshot() TrackerState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Restore loads a previously persisted state.
func (t *Tracker) Restore(s TrackerState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
}

// SlotsProfiled returns how many slots have at least one day of data. The
// policy uses this to know when it may trust the daily profile.
func (t *Tracker) SlotsProfiled() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	filled := 0
	for _, p := range t.state.Slots {
		if p.Days > 0 {
			filled++
		}
	}
	return filled
}

func ewma(old, sample, alpha float64) float64 {
	if old == 0 {
		return sample
	}
	return (1-alpha)*old + alpha*sample
}
