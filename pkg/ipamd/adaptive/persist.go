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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// modelFormatVersion guards against loading a model written by a build whose
// state or action encoding differs from ours.
const modelFormatVersion = 1

// qEntry is one persisted state/action row.
type qEntry struct {
	DayPart int       `json:"dayPart"`
	Demand  int       `json:"demand"`
	Slack   int       `json:"slack"`
	Deficit int       `json:"deficit"`
	Visits  int       `json:"visits"`
	Q       []float64 `json:"q"`
}

// Model is the on-disk representation of everything the policy has learned.
type Model struct {
	Version int          `json:"version"`
	Actions int          `json:"actions"`
	Epsilon float64      `json:"epsilon"`
	Steps   int          `json:"steps"`
	Q       []qEntry     `json:"q"`
	Tracker TrackerState `json:"tracker"`
}

// Export captures the learned model.
func (p *Policy) Export() Model {
	p.mu.Lock()
	defer p.mu.Unlock()

	m := Model{
		Version: modelFormatVersion,
		Actions: numActions(),
		Epsilon: p.epsilon,
		Steps:   p.steps,
		Q:       make([]qEntry, 0, len(p.q)),
		Tracker: p.trk.Snapshot(),
	}
	for k, row := range p.q {
		q := make([]float64, len(row))
		copy(q, row)
		m.Q = append(m.Q, qEntry{DayPart: k.DayPart, Demand: k.Demand, Slack: k.Slack, Deficit: k.Deficit, Visits: p.visits[k], Q: q})
	}
	return m
}

// Import restores a previously exported model. A model from an incompatible
// build is rejected rather than silently reinterpreted.
func (p *Policy) Import(m Model) error {
	if m.Version != modelFormatVersion {
		return fmt.Errorf("adaptive: model version %d, want %d", m.Version, modelFormatVersion)
	}
	if m.Actions != numActions() {
		return fmt.Errorf("adaptive: model has %d actions, want %d", m.Actions, numActions())
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.q = make(map[stateKey][]float64, len(m.Q))
	p.visits = make(map[stateKey]int, len(m.Q))
	for _, e := range m.Q {
		if len(e.Q) != numActions() {
			return fmt.Errorf("adaptive: q row %v has %d actions, want %d", e, len(e.Q), numActions())
		}
		k := stateKey{DayPart: e.DayPart, Demand: e.Demand, Slack: e.Slack, Deficit: e.Deficit}
		row := make([]float64, len(e.Q))
		copy(row, e.Q)
		p.q[k] = row
		p.visits[k] = e.Visits
	}
	if m.Epsilon > 0 {
		p.epsilon = m.Epsilon
	}
	p.steps = m.Steps
	p.trk.Restore(m.Tracker)
	// A restored policy has no in-flight decision to score.
	p.haveLast = false
	return nil
}

// Save atomically writes the learned model to path.
func (p *Policy) Save(path string) error {
	m := p.Export()
	data, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores the learned model from path. A missing file is not an error:
// the policy simply starts from its conservative cold-start behaviour.
func (p *Policy) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m Model
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	return p.Import(m)
}
