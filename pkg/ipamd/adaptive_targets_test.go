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
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/adaptive"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/datastore"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/networkutils"
)

// adaptiveTestContext builds an IPAMContext holding four secondary IPs on the
// primary ENI, two of which are assigned to pods.
func adaptiveTestContext(t *testing.T) *IPAMContext {
	t.Helper()

	c := &IPAMContext{
		warmIPTarget:       1,
		lastDecreaseIPPool: time.Now().Add(-60 * time.Second),
		dataStoreAccess:    testDatastore(),
	}
	c.reconcileCooldownCache.cache = make(map[string]time.Time)

	ds := c.dataStoreAccess.GetDataStore(defaultNetworkCard)
	ds.AddENI(primaryENIid, primaryDevice, true, false, false, networkutils.CalculateRouteTableId(primaryDevice, 0), "")
	for _, ip := range []string{ipaddr01, ipaddr02, ipaddr11, ipaddr12} {
		ds.AddIPv4CidrToStore(primaryENIid, net.IPNet{IP: net.ParseIP(ip), Mask: net.IPv4Mask(255, 255, 255, 255)}, false)
	}
	_, _, _, err := ds.AssignPodIPv4Address(datastore.IPAMKey{ContainerID: "container1"}, datastore.IPAMMetadata{K8SPodName: "pod1"})
	assert.NoError(t, err)
	_, _, _, err = ds.AssignPodIPv4Address(datastore.IPAMKey{ContainerID: "container2"}, datastore.IPAMMetadata{K8SPodName: "pod2"})
	assert.NoError(t, err)

	return c
}

// driveUntilReady feeds the policy a day of a workload with a burst at the top
// of each hour, which is enough for it to report itself ready.
func driveUntilReady(p *adaptive.Policy) {
	now := time.Now().Truncate(24 * time.Hour)
	assigned := 0
	for h := 0; h < 25; h++ {
		for i := 0; i < 720; i++ {
			now = now.Add(5 * time.Second)
			switch i {
			case 0:
				assigned = 12
			case 60:
				assigned = 0
			}
			p.Step(now, adaptive.Observation{
				AssignedIPs:  assigned,
				AvailableIPs: 4,
				TotalIPs:     assigned + 4,
				Unfulfilled:  -1,
			})
		}
	}
}

func TestAdaptivePolicyDisabledLeavesStaticTargetsInCharge(t *testing.T) {
	c := adaptiveTestContext(t)

	_, _, active := c.adaptiveTargets()
	assert.False(t, active, "no policy configured, so nothing may override the static knobs")

	short, over, enabled := c.datastoreTargetState(nil, defaultNetworkCard)
	assert.True(t, enabled)
	assert.Equal(t, 0, short)
	// 4 IPs, 2 assigned, WARM_IP_TARGET=1, so one IP is surplus.
	assert.Equal(t, 1, over)
}

func TestAdaptivePolicyInShadowModeDoesNotDriveThePool(t *testing.T) {
	c := adaptiveTestContext(t)

	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeShadow
	c.adaptivePolicy = adaptive.NewPolicy(cfg)
	driveUntilReady(c.adaptivePolicy)

	assert.True(t, c.adaptivePolicy.Current().Ready, "the policy should have learned the pattern")

	_, _, active := c.adaptiveTargets()
	assert.False(t, active, "shadow mode must observe and report only")

	// Behaviour is byte for byte what it was without the policy.
	short, over, enabled := c.datastoreTargetState(nil, defaultNetworkCard)
	assert.True(t, enabled)
	assert.Equal(t, 0, short)
	assert.Equal(t, 1, over)
}

func TestAdaptivePolicyNotReadyLeavesStaticTargetsInCharge(t *testing.T) {
	c := adaptiveTestContext(t)

	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeEnforce
	c.adaptivePolicy = adaptive.NewPolicy(cfg)

	// One observation is nowhere near a day of history.
	c.adaptivePolicy.Step(time.Now(), adaptive.Observation{AssignedIPs: 2, AvailableIPs: 2, TotalIPs: 4, Unfulfilled: -1})

	_, _, active := c.adaptiveTargets()
	assert.False(t, active, "a policy that has learned nothing must not drive the pool")

	short, over, enabled := c.datastoreTargetState(nil, defaultNetworkCard)
	assert.True(t, enabled)
	assert.Equal(t, 0, short)
	assert.Equal(t, 1, over)
}

func TestAdaptivePolicyInEnforceModeSuppliesTheTargets(t *testing.T) {
	c := adaptiveTestContext(t)

	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeEnforce
	// Pin the policy to a known answer so the test asserts on the wiring rather
	// than on what the agent happened to learn.
	cfg.MinWarmIPTarget = 3
	cfg.MaxWarmIPTarget = 3
	// Clamp the learned floor to zero as well, so only the warm target is in
	// play and the expected arithmetic is unambiguous.
	cfg.MaxMinimumIPTarget = 0
	c.adaptivePolicy = adaptive.NewPolicy(cfg)
	driveUntilReady(c.adaptivePolicy)

	warm, _, active := c.adaptiveTargets()
	assert.True(t, active, "an enforcing, ready policy must supply the targets")
	assert.Equal(t, 3, warm)

	assert.True(t, c.warmIPTargetsDefined(), "warm IP target arithmetic must be enabled for the adaptive targets")

	// With 4 IPs, 2 assigned and a warm target of 3, we are one IP short of the
	// policy's target rather than one IP over the static WARM_IP_TARGET=1.
	short, over, enabled := c.datastoreTargetState(nil, defaultNetworkCard)
	assert.True(t, enabled)
	assert.Equal(t, 1, short)
	assert.Equal(t, 0, over)
}

func TestAdaptivePolicyIsIgnoredOnSecondaryNetworkCards(t *testing.T) {
	c := adaptiveTestContext(t)

	// Add a second, empty network card alongside the primary.
	secondary := datastore.NewDataStore(log, datastore.NewTestCheckpoint(datastore.CheckpointData{Version: datastore.CheckpointFormatVersion}), false, defaultNetworkCard+1)
	c.dataStoreAccess.DataStores = append(c.dataStoreAccess.DataStores, secondary)

	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeEnforce
	cfg.MinWarmIPTarget = 30
	cfg.MaxWarmIPTarget = 30
	cfg.MaxMinimumIPTarget = 0
	c.adaptivePolicy = adaptive.NewPolicy(cfg)
	driveUntilReady(c.adaptivePolicy)

	// Network cards above the primary keep their fixed WARM_IP_TARGET=1 /
	// MINIMUM_IP_TARGET=1 behaviour; multi-NIC attachments are not the workload
	// this policy models. An empty secondary card is therefore short exactly one
	// IP, not the 30 the learned target would have asked for.
	short, _, enabled := c.datastoreTargetState(nil, defaultNetworkCard+1)
	assert.True(t, enabled)
	assert.Equal(t, DefaultWarmIPTarget, short, "secondary cards must not see the learned target")
}

func TestStepAdaptivePolicyIsANoOpWhenDisabled(t *testing.T) {
	c := adaptiveTestContext(t)
	// Must not panic or touch anything when the feature is off.
	c.stepAdaptivePolicy()
	assert.Nil(t, c.AdaptiveDebug())
}

func TestStepAdaptivePolicyFeedsTheDatastoreStats(t *testing.T) {
	c := adaptiveTestContext(t)

	cfg := adaptive.DefaultConfig()
	cfg.Mode = adaptive.ModeEnforce
	c.adaptivePolicy = adaptive.NewPolicy(cfg)

	c.stepAdaptivePolicy()

	debug := c.AdaptiveDebug()
	assert.NotNil(t, debug)
	assert.Equal(t, adaptive.ModeEnforce, debug["mode"])
	assert.Equal(t, false, debug["ready"], "a single step cannot make the policy ready")
}
