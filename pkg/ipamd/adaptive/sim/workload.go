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

// Package sim is an offline evaluation harness for IPAM warm-pool policies.
//
// It replays a synthetic pod workload against a model of the ipamd pool manager
// and the EC2 control plane, so that a candidate policy can be scored against
// the existing static knobs without a cluster. The pool model deliberately
// mirrors pkg/ipamd: the same warm/minimum target arithmetic, the same 5s
// control interval, the same 30s IP cooldown, the same one-ENI-per-tick
// allocation, and the same "assign to existing ENIs before attaching a new one"
// ordering.
package sim

import (
	"math"
	"math/rand"
)

// Pod is one scheduled pod: when it asks for an IP and how long it holds it.
type Pod struct {
	Start int64 // seconds from the start of the simulation
	Life  int64 // seconds the pod holds its IP
}

// Workload is a named pod trace.
type Workload struct {
	Name string
	Desc string
	Pods []Pod
	Days int
}

const secondsPerDay = 24 * 3600

// SteadyDeployment models a boring node: a small trickle of long-lived pods.
// This is the case the static knobs already handle well, and the bar a learned
// policy must not regress.
func SteadyDeployment(days int, seed int64) Workload {
	r := rand.New(rand.NewSource(seed))
	var pods []Pod
	// ~1 pod every 10 minutes, living 2-8 hours.
	for t := int64(0); t < int64(days)*secondsPerDay; t += 600 {
		jitter := r.Int63n(600)
		pods = append(pods, Pod{
			Start: t + jitter,
			Life:  int64(2*3600) + r.Int63n(6*3600),
		})
	}
	return Workload{
		Name: "steady",
		Desc: "long-lived deployment pods, ~6/hour, no daily pattern",
		Pods: pods,
		Days: days,
	}
}

// DiurnalService models a user-facing service that scales with business hours:
// arrivals follow a daily curve peaking mid-afternoon, pods live tens of minutes.
func DiurnalService(days int, seed int64) Workload {
	r := rand.New(rand.NewSource(seed))
	var pods []Pod
	for t := int64(0); t < int64(days)*secondsPerDay; t++ {
		hour := float64((t%secondsPerDay)/3600) + float64((t%3600))/3600.0
		// Peak at 15:00, trough at 03:00.
		shape := 0.5 + 0.5*math.Cos((hour-15.0)/24.0*2*math.Pi)
		rate := 0.0005 + 0.016*math.Pow(shape, 3) // pods per second
		if r.Float64() < rate {
			pods = append(pods, Pod{
				Start: t,
				Life:  300 + r.Int63n(3600),
			})
		}
	}
	return Workload{
		Name: "diurnal",
		Desc: "business-hours service, arrivals peak at 15:00, pods live 5-65min",
		Pods: pods,
		Days: days,
	}
}

// HourlyBatch models the case this policy is aimed at: a CronJob fan-out at the
// top of every hour. Demand is near zero for 55 minutes and then a burst of
// short-lived pods arrives. A static WARM_IP_TARGET has to be sized for the
// burst and is therefore idle for most of the hour; a static low target makes
// every burst wait for EC2.
func HourlyBatch(days int, seed int64) Workload {
	r := rand.New(rand.NewSource(seed))
	var pods []Pod
	for day := 0; day < days; day++ {
		for hour := 0; hour < 24; hour++ {
			base := int64(day*secondsPerDay + hour*3600)
			// Bigger fan-out during working hours, small one overnight.
			size := 6
			if hour >= 8 && hour < 20 {
				size = 24
			}
			for i := 0; i < size; i++ {
				pods = append(pods, Pod{
					// The whole batch lands within 20 seconds.
					Start: base + r.Int63n(20),
					Life:  120 + r.Int63n(400),
				})
			}
			// A little background noise between batches.
			for i := 0; i < 3; i++ {
				pods = append(pods, Pod{
					Start: base + 600 + r.Int63n(2400),
					Life:  300 + r.Int63n(1800),
				})
			}
		}
	}
	return Workload{
		Name: "hourly-batch",
		Desc: "CronJob fan-out on the hour (24 pods 08-20h, 6 overnight), short-lived",
		Pods: pods,
		Days: days,
	}
}

// BurstyRollout models deployment rollouts: a steady base load punctuated by
// occasional large replacements that double the pod count for a few minutes.
func BurstyRollout(days int, seed int64) Workload {
	r := rand.New(rand.NewSource(seed))
	var pods []Pod
	for t := int64(0); t < int64(days)*secondsPerDay; t += 900 {
		pods = append(pods, Pod{Start: t + r.Int63n(900), Life: 3600 + r.Int63n(7200)})
	}
	// Three rollouts a day at unpredictable times.
	for day := 0; day < days; day++ {
		for i := 0; i < 3; i++ {
			at := int64(day*secondsPerDay) + r.Int63n(secondsPerDay)
			n := 20 + r.Intn(20)
			for j := 0; j < n; j++ {
				pods = append(pods, Pod{Start: at + r.Int63n(60), Life: 600 + r.Int63n(3600)})
			}
		}
	}
	return Workload{
		Name: "bursty-rollout",
		Desc: "steady base load plus 3 unscheduled 20-40 pod rollouts per day",
		Pods: pods,
		Days: days,
	}
}

// Split returns the pods belonging to the first n days and the rest, so a
// policy can be trained on one stretch of the trace and scored on another.
func (w Workload) Split(trainDays int) (train, test Workload) {
	cut := int64(trainDays) * secondsPerDay
	train = Workload{Name: w.Name + "/train", Desc: w.Desc, Days: trainDays}
	test = Workload{Name: w.Name + "/test", Desc: w.Desc, Days: w.Days - trainDays}
	for _, p := range w.Pods {
		if p.Start < cut {
			train.Pods = append(train.Pods, p)
		} else {
			test.Pods = append(test.Pods, Pod{Start: p.Start - cut, Life: p.Life})
		}
	}
	return train, test
}

// PeakConcurrent returns the highest number of pods alive at once, which is the
// smallest pool that could ever serve the trace without a wait.
func (w Workload) PeakConcurrent() int {
	if len(w.Pods) == 0 {
		return 0
	}
	deltas := map[int64]int{}
	for _, p := range w.Pods {
		deltas[p.Start]++
		deltas[p.Start+p.Life]--
	}
	keys := make([]int64, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	// Sort ascending without pulling in sort for a hot path we only use once.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	cur, peak := 0, 0
	for _, k := range keys {
		cur += deltas[k]
		if cur > peak {
			peak = cur
		}
	}
	return peak
}
