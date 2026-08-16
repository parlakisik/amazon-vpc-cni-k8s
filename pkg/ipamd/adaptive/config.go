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
	"os"
	"strconv"
	"time"
)

const (
	// EnvEnable turns the adaptive controller on. Default false, so an upgrade
	// never changes the behaviour of an existing node.
	EnvEnable = "ENABLE_ADAPTIVE_IP_TARGET"

	// EnvMode selects "shadow" (learn and export metrics only, static knobs
	// still drive allocation) or "enforce" (the policy drives allocation).
	// Default "shadow".
	EnvMode = "ADAPTIVE_IP_TARGET_MODE"

	// EnvMinWarm and EnvMaxWarm clamp the warm IP target the policy may ask for.
	EnvMinWarm = "ADAPTIVE_MIN_WARM_IP_TARGET"
	EnvMaxWarm = "ADAPTIVE_MAX_WARM_IP_TARGET"

	// EnvMaxMinimum clamps the learned floor on total IPs.
	EnvMaxMinimum = "ADAPTIVE_MAX_MINIMUM_IP_TARGET"

	// EnvLeadTime overrides the provisioning lead time the policy forecasts over.
	EnvLeadTime = "ADAPTIVE_LEAD_TIME_SECONDS"

	// EnvStatePath is where the learned model is persisted across restarts.
	EnvStatePath     = "ADAPTIVE_IP_TARGET_STATE_PATH"
	DefaultStatePath = "/var/run/aws-node/adaptive-ip-target.json"
)

// Enabled reports whether the adaptive controller was requested.
func Enabled() bool {
	v, err := strconv.ParseBool(os.Getenv(EnvEnable))
	return err == nil && v
}

// ConfigFromEnv builds a policy config from the environment, falling back to
// DefaultConfig for anything unset or invalid.
func ConfigFromEnv() Config {
	cfg := DefaultConfig()

	if mode := os.Getenv(EnvMode); mode == ModeEnforce || mode == ModeShadow {
		cfg.Mode = mode
	}
	if v, ok := envInt(EnvMinWarm); ok && v >= 0 {
		cfg.MinWarmIPTarget = v
	}
	if v, ok := envInt(EnvMaxWarm); ok && v > 0 {
		cfg.MaxWarmIPTarget = v
	}
	if v, ok := envInt(EnvMaxMinimum); ok && v >= 0 {
		cfg.MaxMinimumIPTarget = v
	}
	if v, ok := envInt(EnvLeadTime); ok && v > 0 {
		cfg.LeadTime = time.Duration(v) * time.Second
	}
	if cfg.MaxWarmIPTarget < cfg.MinWarmIPTarget {
		cfg.MaxWarmIPTarget = cfg.MinWarmIPTarget
	}
	return cfg
}

// StatePath returns where the learned model should be persisted.
func StatePath() string {
	if p := os.Getenv(EnvStatePath); p != "" {
		return p
	}
	return DefaultStatePath
}

func envInt(key string) (int, bool) {
	s, found := os.LookupEnv(key)
	if !found {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}
