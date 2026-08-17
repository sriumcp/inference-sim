// vector_scheduler.go implements five schedulers for the two-pool vector
// KVtime campaign. All five use TwoPoolKVStore + VectorMeter.
//
// Schedulers:
//  1. VectorKVTimeScheduler   — vector-kvtime: per-axis buckets, integrate-then-combine
//  2. ScalarKVTimeVectorScheduler — scalar-kvtime-vector: axis-collapsed single bucket over K_P+K_D
//  3. StaticDRFScheduler      — static-drf: instantaneous dominant share, no integral, no bucket
//  4. SDRFOccupancyScheduler  — sdrf-occupancy: integrates instantaneous dominant share (combine-then-integrate)
//  5. FCFSVectorScheduler     — fcfs: no meter, floor; FIFO ordering
//
// All five implement sim.InstanceScheduler (OrderQueue) and additionally
// sim.AdmissionAwareScheduler (AllowAdmission) and sim.PreemptionAwareScheduler
// (ChooseVictims) where applicable.
//
// This file is part of patch 02-vector-tracked.
// It does NOT modify any production BLIS files.
package kvtime

import (
	"sort"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
)

// ─── 1. VectorKVTimeScheduler (vector-kvtime) ────────────────────────────────

// VectorKVTimeScheduler implements per-axis, time-integrated, integrate-then-combine
// scheduling. It uses PerAxisBucketManager and VectorMeter over TwoPoolKVStore.
//
// At each tick:
//  1. Tick VectorMeter to record per-axis A_i^r.
//  2. Reconcile PerAxisBucketManager using cumulative A_i^r.
//  3. Score each waiting request by dominant_balance(tenant) / k_r.
//  4. Sort descending; AllowAdmission vetoes overdrawn tenants.
//  5. ChooseVictims evicts lowest-dominant-score incumbents when needed.
type VectorKVTimeScheduler struct {
	store     *kv.TwoPoolKVStore
	simulator *sim.Simulator
	vmeter    *VectorMeter
	buckets   *PerAxisBucketManager

	reqToTenant map[string]string
	lastTickUs  int64
}

// NewVectorKVTimeScheduler creates a VectorKVTimeScheduler.
// Call SetSimulator before the simulation loop starts.
func NewVectorKVTimeScheduler(store *kv.TwoPoolKVStore, vmeter *VectorMeter, buckets *PerAxisBucketManager) *VectorKVTimeScheduler {
	return &VectorKVTimeScheduler{
		store:       store,
		vmeter:      vmeter,
		buckets:     buckets,
		reqToTenant: make(map[string]string, 128),
	}
}

// SetSimulator wires back to the simulator (needed for RunningBatch access).
func (s *VectorKVTimeScheduler) SetSimulator(sim *sim.Simulator) { s.simulator = sim }

// VectorMeter returns the underlying VectorMeter (for metrics collection).
func (s *VectorKVTimeScheduler) VectorMeter() *VectorMeter { return s.vmeter }

// Buckets returns the PerAxisBucketManager (for diagnostics).
func (s *VectorKVTimeScheduler) Buckets() *PerAxisBucketManager { return s.buckets }

// OrderQueue implements sim.InstanceScheduler.
func (s *VectorKVTimeScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}
	s.buildReqToTenant(requests)
	s.vmeter.Tick(s.store, s.reqToTenant, clock)

	cumP := s.vmeter.TenantKVTimeP()
	cumD := s.vmeter.TenantKVTimeD()
	s.buckets.Reconcile(cumP, cumD, clock, s.lastTickUs)
	s.lastTickUs = clock

	// Score and sort: dominant admission score θ = max(0, B_dom) / k_r
	blockSz := float64(s.store.BlockSize())
	type scored struct {
		req   *sim.Request
		score float64
		idx   int
	}
	ss := make([]scored, len(requests))
	for i, r := range requests {
		// Resident tokens = blocks in whichever pool the request is in.
		residentTokens := s.residentTokens(r.ID, blockSz)
		sc := s.buckets.Score(r.TenantID, residentTokens)
		ss[i] = scored{req: r, score: sc, idx: i}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].req.ArrivalTime < ss[j].req.ArrivalTime
	})
	for i, x := range ss {
		requests[i] = x.req
	}
}

// AllowAdmission implements sim.AdmissionAwareScheduler.
func (s *VectorKVTimeScheduler) AllowAdmission(req *sim.Request, _ int64) bool {
	if req == nil || req.TenantID == "" {
		return true
	}
	return !s.buckets.IsOverdrawn(req.TenantID)
}

// ChooseVictims implements sim.PreemptionAwareScheduler.
func (s *VectorKVTimeScheduler) ChooseVictims(candidate *sim.Request, running []*sim.Request, _ int64) []int {
	if candidate == nil || len(running) == 0 {
		return nil
	}
	blockSz := float64(s.store.BlockSize())
	candTokens := float64(len(candidate.InputTokens))
	if candTokens <= 0 {
		return nil
	}
	candScore := s.buckets.Score(candidate.TenantID, candTokens)
	if candScore <= 0 {
		return nil
	}

	type indexed struct {
		idx     int
		score   float64
		arrival int64
	}
	var victims []indexed
	for i, r := range running {
		if r == nil {
			continue
		}
		residentTokens := s.residentTokens(r.ID, blockSz)
		sc := s.buckets.Score(r.TenantID, residentTokens)
		if sc < candScore {
			victims = append(victims, indexed{i, sc, r.ArrivalTime})
		}
	}
	if len(victims) == 0 {
		return nil
	}
	sort.SliceStable(victims, func(i, j int) bool {
		if victims[i].score != victims[j].score {
			return victims[i].score < victims[j].score
		}
		if victims[i].arrival != victims[j].arrival {
			return victims[i].arrival > victims[j].arrival
		}
		return victims[i].idx > victims[j].idx
	})
	out := make([]int, len(victims))
	for i, v := range victims {
		out[i] = v.idx
	}
	return out
}

func (s *VectorKVTimeScheduler) buildReqToTenant(requests []*sim.Request) {
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
}

func (s *VectorKVTimeScheduler) residentTokens(reqID string, blockSz float64) float64 {
	// Check P-pool first, then D-pool.
	if blocks, ok := s.store.PPoolRequestMap()[reqID]; ok {
		return float64(len(blocks)) * blockSz
	}
	if blocks, ok := s.store.DPoolRequestMap()[reqID]; ok {
		return float64(len(blocks)) * blockSz
	}
	return 0
}

// ─── 2. ScalarKVTimeVectorScheduler (scalar-kvtime-vector) ───────────────────

// ScalarKVTimeVectorScheduler uses the same TwoPoolKVStore but collapses
// the P+D axes into a single scalar bucket over K_P + K_D. This is the
// h-control-negative arm: it demonstrates the cross-subsidy predicted by
// lem:projection(5) when axis information is discarded.
type ScalarKVTimeVectorScheduler struct {
	store     *kv.TwoPoolKVStore
	simulator *sim.Simulator
	vmeter    *VectorMeter  // also ticked for per-axis metrics (read-only)
	buckets   *BucketManager // scalar bucket over K_P + K_D

	reqToTenant map[string]string
	lastTickUs  int64
}

// NewScalarKVTimeVectorScheduler creates a ScalarKVTimeVectorScheduler.
func NewScalarKVTimeVectorScheduler(store *kv.TwoPoolKVStore, vmeter *VectorMeter, buckets *BucketManager) *ScalarKVTimeVectorScheduler {
	return &ScalarKVTimeVectorScheduler{
		store:       store,
		vmeter:      vmeter,
		buckets:     buckets,
		reqToTenant: make(map[string]string, 128),
	}
}

// SetSimulator wires back to the simulator.
func (s *ScalarKVTimeVectorScheduler) SetSimulator(sim *sim.Simulator) { s.simulator = sim }

// VectorMeter returns the underlying VectorMeter.
func (s *ScalarKVTimeVectorScheduler) VectorMeter() *VectorMeter { return s.vmeter }

// ScalarBuckets returns the underlying scalar BucketManager.
func (s *ScalarKVTimeVectorScheduler) ScalarBuckets() *BucketManager { return s.buckets }

// OrderQueue implements sim.InstanceScheduler.
func (s *ScalarKVTimeVectorScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}
	s.buildReqToTenant(requests)

	// Tick VectorMeter for per-axis metrics (G2 apparatus).
	s.vmeter.Tick(s.store, s.reqToTenant, clock)

	// Compute scalar KV-time = sum of P + D per tenant.
	cumScalar := s.vmeter.TenantKVTimeScalar()
	s.buckets.Reconcile(cumScalar, clock, s.lastTickUs)
	s.lastTickUs = clock

	// Score using scalar bucket.
	blockSz := float64(s.store.BlockSize())
	type scored struct {
		req   *sim.Request
		score float64
		idx   int
	}
	ss := make([]scored, len(requests))
	for i, r := range requests {
		var residentTokens float64
		if blocks, ok := s.store.PPoolRequestMap()[r.ID]; ok {
			residentTokens = float64(len(blocks)) * blockSz
		} else if blocks, ok := s.store.DPoolRequestMap()[r.ID]; ok {
			residentTokens = float64(len(blocks)) * blockSz
		}
		sc := s.buckets.Score(r.TenantID, residentTokens)
		ss[i] = scored{req: r, score: sc, idx: i}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].req.ArrivalTime < ss[j].req.ArrivalTime
	})
	for i, x := range ss {
		requests[i] = x.req
	}
}

// AllowAdmission implements sim.AdmissionAwareScheduler.
func (s *ScalarKVTimeVectorScheduler) AllowAdmission(req *sim.Request, _ int64) bool {
	if req == nil || req.TenantID == "" {
		return true
	}
	return !s.buckets.IsOverdrawn(req.TenantID)
}

// ChooseVictims implements sim.PreemptionAwareScheduler.
func (s *ScalarKVTimeVectorScheduler) ChooseVictims(candidate *sim.Request, running []*sim.Request, _ int64) []int {
	if candidate == nil || len(running) == 0 {
		return nil
	}
	blockSz := float64(s.store.BlockSize())
	candTokens := float64(len(candidate.InputTokens))
	if candTokens <= 0 {
		return nil
	}
	candScore := s.buckets.Score(candidate.TenantID, candTokens)
	if candScore <= 0 {
		return nil
	}

	type indexed struct {
		idx     int
		score   float64
		arrival int64
	}
	var victims []indexed
	for i, r := range running {
		if r == nil {
			continue
		}
		var residentTokens float64
		if blocks, ok := s.store.PPoolRequestMap()[r.ID]; ok {
			residentTokens = float64(len(blocks)) * blockSz
		} else if blocks, ok := s.store.DPoolRequestMap()[r.ID]; ok {
			residentTokens = float64(len(blocks)) * blockSz
		}
		sc := s.buckets.Score(r.TenantID, residentTokens)
		if sc < candScore {
			victims = append(victims, indexed{i, sc, r.ArrivalTime})
		}
	}
	if len(victims) == 0 {
		return nil
	}
	sort.SliceStable(victims, func(i, j int) bool {
		if victims[i].score != victims[j].score {
			return victims[i].score < victims[j].score
		}
		if victims[i].arrival != victims[j].arrival {
			return victims[i].arrival > victims[j].arrival
		}
		return victims[i].idx > victims[j].idx
	})
	out := make([]int, len(victims))
	for i, v := range victims {
		out[i] = v.idx
	}
	return out
}

func (s *ScalarKVTimeVectorScheduler) buildReqToTenant(requests []*sim.Request) {
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
}

// ─── 3. StaticDRFScheduler (static-drf) ─────────────────────────────────────

// StaticDRFScheduler orders requests by instantaneous dominant share:
//
//	dom_share_i = max_r (k_i^r / C^r)
//
// Requests from the tenant with lower instantaneous dominant share are served
// first. No time integral, no bucket — pure instantaneous DRF.
//
// V2 control: static-drf should TIE vector-kvtime on dominant-share disparity
// when arrivals are near-constant and lengths are narrow (prop:drf-recover).
// On V1 bursty, it is expected to show worse isolation.
type StaticDRFScheduler struct {
	store     *kv.TwoPoolKVStore
	simulator *sim.Simulator
	vmeter    *VectorMeter // ticked for per-axis metrics (apparatus)

	reqToTenant map[string]string
}

// NewStaticDRFScheduler creates a StaticDRFScheduler.
func NewStaticDRFScheduler(store *kv.TwoPoolKVStore, vmeter *VectorMeter) *StaticDRFScheduler {
	return &StaticDRFScheduler{
		store:       store,
		vmeter:      vmeter,
		reqToTenant: make(map[string]string, 128),
	}
}

// SetSimulator wires back to the simulator.
func (s *StaticDRFScheduler) SetSimulator(sim *sim.Simulator) { s.simulator = sim }

// VectorMeter returns the underlying VectorMeter.
func (s *StaticDRFScheduler) VectorMeter() *VectorMeter { return s.vmeter }

// OrderQueue implements sim.InstanceScheduler.
func (s *StaticDRFScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}

	// Tick VectorMeter for apparatus (G2).
	s.vmeter.Tick(s.store, s.reqToTenant, clock)

	// Compute instantaneous dominant share per tenant.
	blockSz := float64(s.store.BlockSize())
	capP := float64(s.store.KPBlocks()) * blockSz
	capD := float64(s.store.KDBlocks()) * blockSz

	// Count current P and D blocks per tenant from running batch.
	pBlocks := make(map[string]float64)
	dBlocks := make(map[string]float64)
	for reqID, bids := range s.store.PPoolRequestMap() {
		tenant := s.reqToTenant[reqID]
		if tenant != "" {
			pBlocks[tenant] += float64(len(bids)) * blockSz
		}
	}
	for reqID, bids := range s.store.DPoolRequestMap() {
		tenant := s.reqToTenant[reqID]
		if tenant != "" {
			dBlocks[tenant] += float64(len(bids)) * blockSz
		}
	}

	tenantDomShare := func(tenant string) float64 {
		shareP := 0.0
		if capP > 0 {
			shareP = pBlocks[tenant] / capP
		}
		shareD := 0.0
		if capD > 0 {
			shareD = dBlocks[tenant] / capD
		}
		if shareP > shareD {
			return shareP
		}
		return shareD
	}

	type scored struct {
		req   *sim.Request
		share float64
		idx   int
	}
	ss := make([]scored, len(requests))
	for i, r := range requests {
		ss[i] = scored{req: r, share: tenantDomShare(r.TenantID), idx: i}
	}
	// Sort ascending by dominant share (lowest share = most entitled = first).
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].share != ss[j].share {
			return ss[i].share < ss[j].share
		}
		return ss[i].req.ArrivalTime < ss[j].req.ArrivalTime
	})
	for i, x := range ss {
		requests[i] = x.req
	}
}

// ─── 4. SDRFOccupancyScheduler (sdrf-occupancy) ──────────────────────────────

// SDRFOccupancyScheduler integrates instantaneous dominant share over time
// (combine-then-integrate, i.e., compute max_r share_i^r first, then integrate
// the combined scalar). No bucket — just a running integral compared across tenants.
//
// This is the V3 comparator: it should match vector-kvtime on long-run dominant
// share but fail to track per-window isolation (integrate-then-combine vs
// combine-then-integrate gap).
type SDRFOccupancyScheduler struct {
	store     *kv.TwoPoolKVStore
	simulator *sim.Simulator
	vmeter    *VectorMeter

	// integratedDomShare[tenant] = ∫ max_r share_i^r dt (token·µs units via dom share × Δt)
	integratedDomShare map[string]float64
	lastTickUs         int64
	reqToTenant        map[string]string
}

// NewSDRFOccupancyScheduler creates an SDRFOccupancyScheduler.
func NewSDRFOccupancyScheduler(store *kv.TwoPoolKVStore, vmeter *VectorMeter) *SDRFOccupancyScheduler {
	return &SDRFOccupancyScheduler{
		store:              store,
		vmeter:             vmeter,
		integratedDomShare: make(map[string]float64),
		reqToTenant:        make(map[string]string, 128),
	}
}

// SetSimulator wires back to the simulator.
func (s *SDRFOccupancyScheduler) SetSimulator(sim *sim.Simulator) { s.simulator = sim }

// VectorMeter returns the underlying VectorMeter.
func (s *SDRFOccupancyScheduler) VectorMeter() *VectorMeter { return s.vmeter }

// IntegratedDomShare returns accumulated ∫ max_r share_i^r dt per tenant.
func (s *SDRFOccupancyScheduler) IntegratedDomShare() map[string]float64 {
	out := make(map[string]float64, len(s.integratedDomShare))
	for k, v := range s.integratedDomShare {
		out[k] = v
	}
	return out
}

// OrderQueue implements sim.InstanceScheduler.
func (s *SDRFOccupancyScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}

	// Tick VectorMeter for apparatus.
	s.vmeter.Tick(s.store, s.reqToTenant, clock)

	// Compute instantaneous dominant share and integrate.
	blockSz := float64(s.store.BlockSize())
	capP := float64(s.store.KPBlocks()) * blockSz
	capD := float64(s.store.KDBlocks()) * blockSz

	pBlocks := make(map[string]float64)
	dBlocks := make(map[string]float64)
	for reqID, bids := range s.store.PPoolRequestMap() {
		tenant := s.reqToTenant[reqID]
		if tenant != "" {
			pBlocks[tenant] += float64(len(bids)) * blockSz
		}
	}
	for reqID, bids := range s.store.DPoolRequestMap() {
		tenant := s.reqToTenant[reqID]
		if tenant != "" {
			dBlocks[tenant] += float64(len(bids)) * blockSz
		}
	}

	// Collect all tenants.
	allTenants := make(map[string]struct{})
	for t := range pBlocks {
		allTenants[t] = struct{}{}
	}
	for t := range dBlocks {
		allTenants[t] = struct{}{}
	}
	for _, r := range requests {
		if r.TenantID != "" {
			allTenants[r.TenantID] = struct{}{}
		}
	}

	if s.lastTickUs > 0 && clock > s.lastTickUs {
		deltaUs := float64(clock - s.lastTickUs)
		for tenant := range allTenants {
			shareP := 0.0
			if capP > 0 {
				shareP = pBlocks[tenant] / capP
			}
			shareD := 0.0
			if capD > 0 {
				shareD = dBlocks[tenant] / capD
			}
			dom := shareP
			if shareD > dom {
				dom = shareD
			}
			s.integratedDomShare[tenant] += dom * deltaUs
		}
	}
	s.lastTickUs = clock

	// Sort ascending by integrated dominant share (SDRF: lowest integrated = most entitled).
	type scored struct {
		req   *sim.Request
		score float64
		idx   int
	}
	ss := make([]scored, len(requests))
	for i, r := range requests {
		ss[i] = scored{req: r, score: s.integratedDomShare[r.TenantID], idx: i}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score < ss[j].score
		}
		return ss[i].req.ArrivalTime < ss[j].req.ArrivalTime
	})
	for i, x := range ss {
		requests[i] = x.req
	}
}

// ─── 5. FCFSVectorScheduler (fcfs) ───────────────────────────────────────────

// FCFSVectorScheduler provides FIFO ordering with the VectorMeter ticked for
// apparatus purposes (G2 conservation check). No entitlement metering.
type FCFSVectorScheduler struct {
	store     *kv.TwoPoolKVStore
	simulator *sim.Simulator
	vmeter    *VectorMeter

	reqToTenant map[string]string
}

// NewFCFSVectorScheduler creates an FCFSVectorScheduler.
func NewFCFSVectorScheduler(store *kv.TwoPoolKVStore, vmeter *VectorMeter) *FCFSVectorScheduler {
	return &FCFSVectorScheduler{
		store:       store,
		vmeter:      vmeter,
		reqToTenant: make(map[string]string, 128),
	}
}

// SetSimulator wires back to the simulator.
func (s *FCFSVectorScheduler) SetSimulator(sim *sim.Simulator) { s.simulator = sim }

// VectorMeter returns the underlying VectorMeter.
func (s *FCFSVectorScheduler) VectorMeter() *VectorMeter { return s.vmeter }

// OrderQueue implements sim.InstanceScheduler — FIFO by arrival time.
func (s *FCFSVectorScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
	// Tick VectorMeter for apparatus.
	s.vmeter.Tick(s.store, s.reqToTenant, clock)

	// FIFO: sort ascending by arrival time.
	sort.SliceStable(requests, func(i, j int) bool {
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}
