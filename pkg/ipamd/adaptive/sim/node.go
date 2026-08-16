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

// NodeSpec describes the instance the simulation runs on and the latencies of
// the EC2 control plane.
type NodeSpec struct {
	// MaxENI is the number of ENIs the instance type allows.
	MaxENI int
	// MaxIPsPerENI is the usable secondary IPv4 addresses per ENI (the instance
	// type's limit minus the primary IP), which is also the maximum number of
	// /28 prefixes per ENI.
	MaxIPsPerENI int
	// PrefixDelegation selects ENABLE_PREFIX_DELEGATION behaviour.
	PrefixDelegation bool
	// IPsPerPrefix is 16 for an IPv4 /28.
	IPsPerPrefix int

	// IPAllocSeconds is how long AssignPrivateIpAddresses takes to complete and
	// show up in the datastore.
	IPAllocSeconds int64
	// ENIAttachSeconds covers create + attach + IMDS discovery + IP assignment.
	ENIAttachSeconds int64
	// CooldownSeconds mirrors IP_COOLDOWN_PERIOD.
	CooldownSeconds int64
	// ControlSeconds mirrors ipPoolMonitorInterval.
	ControlSeconds int64
	// DecreaseSeconds mirrors decreaseIPPoolInterval.
	DecreaseSeconds int64
	// MinENILifeSeconds mirrors datastore.minENILifeTime: a freshly attached ENI
	// is not a detach candidate for this long.
	MinENILifeSeconds int64
}

// DefaultNodeSpec returns an m5.xlarge-like node: 4 ENIs of 15 IPv4 addresses,
// so 56 pods maximum with secondary IPs.
func DefaultNodeSpec() NodeSpec {
	return NodeSpec{
		MaxENI:            4,
		MaxIPsPerENI:      14,
		PrefixDelegation:  false,
		IPsPerPrefix:      16,
		IPAllocSeconds:    2,
		ENIAttachSeconds:  20,
		CooldownSeconds:   30,
		ControlSeconds:    5,
		DecreaseSeconds:   30,
		MinENILifeSeconds: 60,
	}
}

// WithPrefixDelegation returns a copy of the spec with PD turned on.
func (n NodeSpec) WithPrefixDelegation() NodeSpec {
	n.PrefixDelegation = true
	return n
}

// unitSize is how many IPs one allocation unit carries: 1 for a secondary IP,
// 16 for a /28 prefix.
func (n NodeSpec) unitSize() int {
	if n.PrefixDelegation {
		return n.IPsPerPrefix
	}
	return 1
}

// cidrBlock is one entry in the datastore's per-ENI CIDR map: either a single
// secondary IP (capacity 1) or a /28 prefix (capacity 16).
type cidrBlock struct {
	capacity int
	assigned int
	cooling  int
}

func (c *cidrBlock) free() int { return c.capacity - c.assigned - c.cooling }

// freeable reports whether the whole block can be released back to EC2.
func (c *cidrBlock) freeable() bool { return c.assigned == 0 && c.cooling == 0 }

type simENI struct {
	cidrs     []*cidrBlock
	createdAt int64
	primary   bool
}

// freeIPs is the number of IPs on this ENI that are ready for a pod.
func (e *simENI) freeIPs() int {
	n := 0
	for _, c := range e.cidrs {
		n += c.free()
	}
	return n
}

// totalIPs is the capacity this ENI contributes to the datastore.
func (e *simENI) totalIPs() int {
	n := 0
	for _, c := range e.cidrs {
		n += c.capacity
	}
	return n
}

// removable reports whether the ENI holds nothing a pod or a cooldown needs.
func (e *simENI) removable() bool {
	for _, c := range e.cidrs {
		if !c.freeable() {
			return false
		}
	}
	return true
}

// coolEvent is an IP leaving cooldown. Cooldown is a fixed duration, so these
// are produced in ready-at order and can be drained from a FIFO.
type coolEvent struct {
	at    int64
	block *cidrBlock
}

// pendingOp is an in-flight EC2 call. ipamd's pool manager is a single
// goroutine making synchronous calls, so at most one is outstanding and the
// control loop is blocked until it lands.
type pendingOp struct {
	at     int64
	isENI  bool
	blocks int // units of capacity being added
}

// Stats is what the pool looks like to the control loop, mirroring
// datastore.DataStoreStats.
type Stats struct {
	TotalIPs     int
	AssignedIPs  int
	AvailableIPs int
	CooldownIPs  int

	TotalCidrs int // total prefixes when PD is on
	FreeCidrs  int // prefixes with nothing assigned and nothing cooling
	ENIs       int

	// AllocOps counts EC2 mutating calls since the previous control tick.
	AllocOps int
	// Unfulfilled counts pods that asked for an IP and had to wait since the
	// previous control tick.
	Unfulfilled int
}

// pool is the simulated ipamd datastore plus the EC2 side of the world.
type pool struct {
	spec NodeSpec

	enis []*simENI

	totalCapacity int
	totalAssigned int
	totalCooling  int

	cooling    []coolEvent
	coolingIdx int

	pending  *pendingOp
	pendENIs int

	// counters reset at each control tick
	allocOps    int
	unfulfilled int

	// lifetime counters
	ec2Calls    int
	eniAttaches int
	eniDetaches int
}

func newPool(spec NodeSpec) *pool {
	// A node boots with its primary ENI attached and no secondary capacity yet.
	return &pool{
		spec: spec,
		enis: []*simENI{{primary: true}},
	}
}

func (p *pool) available() int { return p.totalCapacity - p.totalAssigned - p.totalCooling }

func (p *pool) stats() Stats {
	free, total := 0, 0
	for _, e := range p.enis {
		for _, c := range e.cidrs {
			total++
			if c.freeable() {
				free++
			}
		}
	}
	return Stats{
		TotalIPs:     p.totalCapacity,
		AssignedIPs:  p.totalAssigned,
		AvailableIPs: p.available(),
		CooldownIPs:  p.totalCooling,
		TotalCidrs:   total,
		FreeCidrs:    free,
		ENIs:         len(p.enis),
		AllocOps:     p.allocOps,
		Unfulfilled:  p.unfulfilled,
	}
}

// assign hands one IP to a pod, filling existing CIDRs first the way the
// datastore does. It returns the block the IP came from, or nil when the warm
// pool is empty and the pod has to wait.
func (p *pool) assign() *cidrBlock {
	for _, e := range p.enis {
		for _, c := range e.cidrs {
			if c.free() > 0 {
				c.assigned++
				p.totalAssigned++
				return c
			}
		}
	}
	p.unfulfilled++
	return nil
}

// release returns a pod's IP, which then sits in cooldown.
func (p *pool) release(now int64, c *cidrBlock) {
	c.assigned--
	c.cooling++
	p.totalAssigned--
	p.totalCooling++
	p.cooling = append(p.cooling, coolEvent{at: now + p.spec.CooldownSeconds, block: c})
}

// expireCooldowns moves IPs whose cooldown has elapsed back into the warm pool.
func (p *pool) expireCooldowns(now int64) {
	for p.coolingIdx < len(p.cooling) && p.cooling[p.coolingIdx].at <= now {
		ev := p.cooling[p.coolingIdx]
		ev.block.cooling--
		p.totalCooling--
		p.coolingIdx++
	}
	// Reclaim the drained prefix of the slice occasionally so a long run does
	// not hold every cooldown event ever produced.
	if p.coolingIdx > 4096 {
		p.cooling = append([]coolEvent(nil), p.cooling[p.coolingIdx:]...)
		p.coolingIdx = 0
	}
}

// completePending lands an in-flight EC2 call.
func (p *pool) completePending(now int64) {
	if p.pending == nil || p.pending.at > now {
		return
	}
	op := p.pending
	p.pending = nil

	unit := p.spec.unitSize()
	if op.isENI {
		if len(p.enis) >= p.spec.MaxENI {
			return
		}
		e := &simENI{createdAt: now}
		p.enis = append(p.enis, e)
		p.eniAttaches++
		for i := 0; i < op.blocks; i++ {
			e.cidrs = append(e.cidrs, &cidrBlock{capacity: unit})
			p.totalCapacity += unit
		}
		return
	}

	// Fill the first ENI that still has room, the same ordering the datastore's
	// GetAllocatableENIs walk produces.
	for _, e := range p.enis {
		room := p.spec.MaxIPsPerENI - len(e.cidrs)
		if room <= 0 {
			continue
		}
		n := min(room, op.blocks)
		for i := 0; i < n; i++ {
			e.cidrs = append(e.cidrs, &cidrBlock{capacity: unit})
			p.totalCapacity += unit
		}
		op.blocks -= n
		if op.blocks == 0 {
			return
		}
	}
}

// busy reports whether an EC2 call is in flight, which blocks the control loop.
func (p *pool) busy() bool { return p.pending != nil }

// startAssignCidrs issues one AssignPrivateIpAddresses / AssignIpv6Prefixes
// call for up to want units, mirroring tryAssignIPs and tryAssignPrefixes:
// only ENIs with room are considered, and one call is made per tick.
func (p *pool) startAssignCidrs(now int64, want int) bool {
	if want <= 0 || p.busy() {
		return false
	}
	room := 0
	for _, e := range p.enis {
		room += p.spec.MaxIPsPerENI - len(e.cidrs)
	}
	if room <= 0 {
		return false
	}
	n := min(want, room)
	p.pending = &pendingOp{at: now + p.spec.IPAllocSeconds, blocks: n}
	p.allocOps++
	p.ec2Calls++
	return true
}

// startAllocENI attaches one new ENI carrying want units of capacity.
func (p *pool) startAllocENI(now int64, want int) bool {
	if p.busy() || len(p.enis) >= p.spec.MaxENI {
		return false
	}
	n := clamp(want, 1, p.spec.MaxIPsPerENI)
	p.pending = &pendingOp{at: now + p.spec.ENIAttachSeconds, isENI: true, blocks: n}
	p.allocOps++
	// CreateNetworkInterface + AttachNetworkInterface + AssignPrivateIpAddresses.
	p.ec2Calls += 3
	return true
}

// unassignCidrs releases up to want free CIDRs back to EC2. Unassign is
// effective immediately, matching DeallocCidrs.
func (p *pool) unassignCidrs(want int) int {
	if want <= 0 {
		return 0
	}
	unit := p.spec.unitSize()
	freed := 0
	for _, e := range p.enis {
		if freed >= want {
			break
		}
		kept := e.cidrs[:0]
		touched := false
		for _, c := range e.cidrs {
			if freed < want && c.freeable() {
				p.totalCapacity -= c.capacity
				freed += c.capacity / unit
				touched = true
				continue
			}
			kept = append(kept, c)
		}
		e.cidrs = kept
		if touched {
			p.allocOps++
			p.ec2Calls++
		}
	}
	return freed
}

// freeENI detaches one ENI, mirroring tryFreeENI plus datastore.getDeletableENI:
// at most one per control tick, never the primary, never one that is too young,
// holds pods, has IPs in cooldown, or whose capacity the warm and minimum
// targets still need.
func (p *pool) freeENI(now int64, k Knobs) bool {
	for i, e := range p.enis {
		if e.primary || e.createdAt+p.spec.MinENILifeSeconds > now || !e.removable() {
			continue
		}

		// isRequiredForWarmIPTarget / isRequiredForMinimumIPTarget: would the
		// remaining ENIs still satisfy the targets without this one?
		if k.WarmIPTarget != 0 && p.freeIPsExcluding(e) < k.WarmIPTarget {
			continue
		}
		if k.MinimumIPTarget != 0 && p.totalIPsExcluding(e) < k.MinimumIPTarget {
			continue
		}
		if p.spec.PrefixDelegation && k.WarmPrefixTarget != 0 &&
			p.freeCidrsExcluding(e) < k.WarmPrefixTarget {
			continue
		}

		p.totalCapacity -= e.totalIPs()
		p.enis = append(p.enis[:i], p.enis[i+1:]...)
		p.eniDetaches++
		p.allocOps++
		// DetachNetworkInterface + DeleteNetworkInterface.
		p.ec2Calls += 2
		return true
	}
	return false
}

func (p *pool) freeIPsExcluding(skip *simENI) int {
	n := 0
	for _, e := range p.enis {
		if e != skip {
			n += e.freeIPs()
		}
	}
	return n
}

func (p *pool) totalIPsExcluding(skip *simENI) int {
	n := 0
	for _, e := range p.enis {
		if e != skip {
			n += e.totalIPs()
		}
	}
	return n
}

func (p *pool) freeCidrsExcluding(skip *simENI) int {
	n := 0
	for _, e := range p.enis {
		if e == skip {
			continue
		}
		for _, c := range e.cidrs {
			if c.freeable() {
				n++
			}
		}
	}
	return n
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func divCeil(x, y int) int {
	if y == 0 {
		return 0
	}
	return (x + y - 1) / y
}
