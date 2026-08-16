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

package ipamd

import (
	"time"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/adaptive"
	"github.com/aws/amazon-vpc-cni-k8s/utils/prometheusmetrics"
)

// adaptiveSaveInterval is how often the learned model is written to disk. The
// model is worth minutes of learning, not hours, so this trades a little IO for
// a node that comes back from an ipamd restart already knowing its own
// workload.
const adaptiveSaveInterval = 5 * time.Minute

// initAdaptivePolicy wires up the learned warm-target controller when
// ENABLE_ADAPTIVE_IP_TARGET is set. Any failure here leaves the policy nil,
// which means ipamd behaves exactly as it does today.
func (c *IPAMContext) initAdaptivePolicy() {
	if !adaptive.Enabled() {
		return
	}

	// IPv6 nodes get a /80 prefix on the primary ENI, which covers every pod the
	// node can run; there is no pool to size and nothing to learn.
	if c.enableIPv6 {
		log.Info("ENABLE_ADAPTIVE_IP_TARGET is set but the node is in IPv6 mode, where the primary ENI prefix already covers max pods. Ignoring.")
		return
	}

	cfg := adaptive.ConfigFromEnv()
	policy := adaptive.NewPolicy(cfg)

	c.adaptiveStatePath = adaptive.StatePath()
	if err := policy.Load(c.adaptiveStatePath); err != nil {
		// A model we cannot read is not a reason to fail: start from scratch,
		// which is the same conservative place a brand new node starts.
		log.Warnf("Failed to load adaptive IP target model from %s, starting from an empty model: %v", c.adaptiveStatePath, err)
	}

	c.adaptivePolicy = policy
	log.Infof("Adaptive IP target enabled in %q mode; model at %s", cfg.Mode, c.adaptiveStatePath)
}

// adaptiveTargets returns the warm and minimum IP targets the policy wants for
// the primary network card, and whether they should be used at all.
//
// They are used only when the policy is in enforce mode AND has seen enough of
// this node's day to forecast it. Until then the configured static knobs stay
// in charge, so a fresh node - or a node whose model was lost - behaves exactly
// like one without this feature.
func (c *IPAMContext) adaptiveTargets() (warmIPTarget, minimumIPTarget int, active bool) {
	if c.adaptivePolicy == nil || !c.adaptivePolicy.Enforcing() {
		return 0, 0, false
	}
	t := c.adaptivePolicy.Current()
	if !t.Ready {
		return 0, 0, false
	}
	return t.WarmIPTarget, t.MinimumIPTarget, true
}

// stepAdaptivePolicy advances the learned model by one control interval. It is
// called from the pool manager loop before any allocation decision is made, so
// the targets a decision reads are the ones computed from the current pool.
func (c *IPAMContext) stepAdaptivePolicy() {
	if c.adaptivePolicy == nil {
		return
	}

	ds := c.dataStoreAccess.GetDataStore(DefaultNetworkCardIndex)
	if ds == nil {
		return
	}
	stats := ds.GetIPStats(ipV4AddrFamily)

	targets := c.adaptivePolicy.Step(time.Now(), adaptive.Observation{
		AssignedIPs:  stats.AssignedIPs,
		AvailableIPs: stats.AvailableAddresses(),
		TotalIPs:     stats.TotalIPs,
		// ipamd does not count EC2 calls or failed pod assignments per interval
		// today, so the policy infers shortage from demand versus IPs on hand.
		AllocOps:    0,
		Unfulfilled: -1,
	})

	prometheusmetrics.AdaptiveWarmIPTarget.Set(float64(targets.WarmIPTarget))
	prometheusmetrics.AdaptiveMinimumIPTarget.Set(float64(targets.MinimumIPTarget))
	if targets.Ready {
		prometheusmetrics.AdaptiveReady.Set(1)
	} else {
		prometheusmetrics.AdaptiveReady.Set(0)
	}

	c.maybeSaveAdaptiveModel()
}

// maybeSaveAdaptiveModel persists the learned model at a slow cadence so an
// ipamd restart does not throw away the node's daily profile.
func (c *IPAMContext) maybeSaveAdaptiveModel() {
	if c.adaptiveStatePath == "" || time.Since(c.lastAdaptiveSave) < adaptiveSaveInterval {
		return
	}
	c.lastAdaptiveSave = time.Now()
	if err := c.adaptivePolicy.Save(c.adaptiveStatePath); err != nil {
		log.Warnf("Failed to persist adaptive IP target model to %s: %v", c.adaptiveStatePath, err)
	}
}

// AdaptiveDebug exposes what the policy is currently thinking, for the
// introspection endpoint. It returns nil when the feature is off.
func (c *IPAMContext) AdaptiveDebug() map[string]interface{} {
	if c.adaptivePolicy == nil {
		return nil
	}
	return c.adaptivePolicy.Debug()
}
