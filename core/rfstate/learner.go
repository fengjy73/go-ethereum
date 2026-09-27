// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rfstate

import (
	"sync"
	"sync/atomic"
)

// Cold-start global prior. Mean conflict probability is 1/33. Keys with no
// observations use this prior; nothing is injected from a global hot set.
const (
	priorAlpha = 1.0
	priorBeta  = 32.0
)

// Fence is the P0 action at a region entry.
type Fence uint8

const (
	// FencePass reads the latest lower version and registers the reader.
	FencePass Fence = iota
	// FenceWaitFinal parks until the known lower producer has finished.
	FenceWaitFinal
	// FenceWaitPrefix parks until every lower transaction is final.
	// Used for coinbase balance reads.
	FenceWaitPrefix
	// FenceDeferTx delays the start of a transaction until the previous
	// same-sender transaction has finished. Applied by the scheduler, not
	// at a storage read.
	FenceDeferTx
)

// Posterior is a Beta(alpha, beta) conflict model for one key, plus the
// number of in-sample invalidations used to promote the key to FENCED.
type Posterior struct {
	Alpha     float64
	Beta      float64
	Conflicts uint64
	// Single and Multi count how often a transaction wrote this key once
	// versus more than once. Early publication is used only when a single
	// write is the common case, so the value published mid-transaction is
	// the one finish would have published.
	Single uint64
	Multi  uint64
}

// Cold costs are unitless and never satisfy E[wait] < P(conflict)*E[reexec],
// because a conflict probability cannot exceed 1 and the wait prior is 4.
// A fence is chosen only after both sides have a nanosecond estimate.
const (
	priorWait   = 4.0
	priorReexec = 1.0
	costAlpha   = 0.2
	costCap     = 100.0
)

// keyCost is the learned wait, re-execution, and producer duration for one key.
// It lives beside the Beta posterior so a duration sample does not insert a
// conflict posterior (that would let safe reads drift a key that only produced).
type keyCost struct {
	waitV, waitN     float64
	reexecV, reexecN float64
	prodV, prodN     float64
	// fanV is the expected number of other readers a write of this key
	// invalidates. The pass cost multiplies re-execution by 1+fan so a
	// wide block does not PASS a hot key and then cascade.
	fanV, fanN float64
}

// Learner holds per-key Beta posteriors. WAIT_FINAL is chosen only when a
// lower producer exists and the expected wait is strictly cheaper than the
// expected re-execution it avoids. Until both costs are measured in
// nanoseconds the choice is PASS.
type Learner struct {
	mu        sync.Mutex
	post      map[Key]Posterior
	cost      map[Key]keyCost
	anyFenced atomic.Uint32 // 1 once any key has Conflicts > 0
}

// NewLearner returns an empty learner (every key uses the global prior).
func NewLearner() *Learner {
	return &Learner{post: map[Key]Posterior{}, cost: map[Key]keyCost{}}
}

// Clone returns a deep copy. Timed runs restore this snapshot so a block's
// own updates never leak into its prior.
func (l *Learner) Clone() *Learner {
	if l == nil {
		return NewLearner()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := &Learner{
		post: make(map[Key]Posterior, len(l.post)),
		cost: make(map[Key]keyCost, len(l.cost)),
	}
	for k, v := range l.post {
		n.post[k] = v
	}
	for k, v := range l.cost {
		n.cost[k] = v
	}
	n.anyFenced.Store(l.anyFenced.Load())
	return n
}

// Len reports how many keys have a stored posterior.
func (l *Learner) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.post)
}

func (l *Learner) get(k Key) Posterior {
	if p, ok := l.post[k]; ok {
		return p
	}
	return Posterior{Alpha: priorAlpha, Beta: priorBeta}
}

// Fenced reports whether the key has conflict evidence and should open a region.
func (l *Learner) Fenced(k Key) bool {
	if l == nil || l.anyFenced.Load() == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.post[k]
	return ok && p.Conflicts > 0
}

// Choose picks PASS or WAIT_FINAL. Coinbase and nonce are decided by the caller.
//
// E[pass] = P(conflict) * E[reexec] * (1 + dependents). Dependents are the
// larger of the learned invalidation fanout and the live reader count passed
// to ChooseWith. Wait only when that pass cost exceeds the wait. A missing
// nanosecond estimate on either side is PASS: the unitless prior (wait 4,
// reexec 1) cannot satisfy the inequality, and mixing it with a one-sided
// nanosecond sample would fence every key after a single long replay.
// With no dependents the factor is 1, so a single reader is unchanged.
func (l *Learner) Choose(k Key, hasProducer bool) Fence {
	return l.ChooseWith(k, hasProducer, 0)
}

// ChooseWith is Choose with a live count of other readers already registered
// on k. A wide block passes that count so the first wave of a hot key can
// wait before a fanout sample exists.
func (l *Learner) ChooseWith(k Key, hasProducer bool, live int) Fence {
	if l == nil || !hasProducer {
		return FencePass
	}
	l.mu.Lock()
	p := l.get(k)
	wait, reexec, ok := l.costsLocked(k)
	fan := 0.0
	if c, hit := l.cost[k]; hit && c.fanN >= 1 {
		fan = c.fanV
	}
	l.mu.Unlock()
	if !ok {
		return FencePass
	}
	if live > 0 && float64(live) > fan {
		fan = float64(live)
	}
	pc := p.Alpha / (p.Alpha + p.Beta)
	if wait < pc*reexec*(1+fan) {
		return FenceWaitFinal
	}
	return FencePass
}

// costsLocked reports nanosecond estimates. The wait is the measured park
// time after four samples, otherwise half the producer's attempt, otherwise
// unknown. The re-execution cost is the measured aborted attempt, otherwise
// unknown. Caller holds mu.
func (l *Learner) costsLocked(k Key) (wait, reexec float64, ok bool) {
	c := l.cost[k]
	switch {
	case c.waitN >= 4:
		wait = c.waitV
	case c.prodN >= 1:
		wait = 0.5 * c.prodV
	default:
		return priorWait, priorReexec, false
	}
	if c.reexecN < 1 {
		return priorWait, priorReexec, false
	}
	return wait, c.reexecV, true
}

func observeCost(v, n *float64, sample float64) {
	if sample < 0 {
		return
	}
	if *n <= 0 {
		*v = sample
		*n = 1
		return
	}
	*v = (*v)*(1-costAlpha) + sample*costAlpha
	if *n < costCap {
		*n++
	}
}

func (l *Learner) addCost(k Key, kind int, ns int64) {
	if l == nil || ns <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cost == nil {
		l.cost = map[Key]keyCost{}
	}
	c := l.cost[k]
	switch kind {
	case costWait:
		observeCost(&c.waitV, &c.waitN, float64(ns))
	case costReexec:
		observeCost(&c.reexecV, &c.reexecN, float64(ns))
	case costProd:
		observeCost(&c.prodV, &c.prodN, float64(ns))
	case costFan:
		observeCost(&c.fanV, &c.fanN, float64(ns))
	}
	l.cost[k] = c
}

const (
	costWait = iota
	costReexec
	costProd
	costFan
)

// ObserveWait records how long a reader actually parked on k.
func (l *Learner) ObserveWait(k Key, ns int64) { l.addCost(k, costWait, ns) }

// ObserveReexec records a conflicted attempt's wall time on k.
func (l *Learner) ObserveReexec(k Key, ns int64) { l.addCost(k, costReexec, ns) }

// ObserveProducer records a successful attempt that wrote k.
func (l *Learner) ObserveProducer(k Key, ns int64) { l.addCost(k, costProd, ns) }

// ObserveFanout records how many other readers one write of k invalidated.
func (l *Learner) ObserveFanout(k Key, n int) {
	if n <= 0 {
		return
	}
	l.addCost(k, costFan, int64(n))
}

// ObserveConflict records one push-invalidation against the key.
func (l *Learner) ObserveConflict(k Key) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.get(k)
	p.Alpha++
	p.Conflicts++
	l.post[k] = p
	l.anyFenced.Store(1)
}

// EarlyWrite reports that this key is fenced and its writes are usually final
// on the first store, so publishing that store before transaction end is the
// value later transactions should see.
func (l *Learner) EarlyWrite(k Key) bool {
	if l == nil || l.anyFenced.Load() == 0 {
		return false
	}
	l.mu.Lock()
	p, ok := l.post[k]
	l.mu.Unlock()
	return ok && p.Conflicts > 0 && p.Single > p.Multi
}

// ObserveWriteShape records whether one attempt wrote k once or several times.
func (l *Learner) ObserveWriteShape(k Key, once bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.get(k)
	if once {
		p.Single++
	} else {
		p.Multi++
	}
	l.post[k] = p
}

// ObserveSafe records one PASS read that was still valid when the reader finalized.
func (l *Learner) ObserveSafe(k Key) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.get(k)
	p.Beta++
	l.post[k] = p
}

// ObserveSafeBatch records many safe reads under one lock.
// Keys that have no stored posterior are skipped. A safe read must not
// allocate a posterior for a cold key: only conflict evidence inserts one,
// and only those keys can later choose WAIT_FINAL. Fenced keys already
// in the map, including fast-forward reads that skipped the fence, still
// gain beta.
func (l *Learner) ObserveSafeBatch(keys []Key) {
	if l == nil || len(keys) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		p, ok := l.post[k]
		if !ok {
			continue
		}
		p.Beta++
		l.post[k] = p
	}
}

// Decay fades excess counts by w/(w+1), where w is the prior's own weight
// (priorAlpha+priorBeta). The prior is the stationary point, so a run of
// safe observations cannot drive every key to PASS, and a key that stops
// conflicting falls back toward the prior. One block adds its observations
// after this fade, at full weight.
func (l *Learner) Decay() {
	if l == nil {
		return
	}
	w := priorAlpha + priorBeta
	fade := w / (w + 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, p := range l.post {
		p.Alpha = priorAlpha + (p.Alpha-priorAlpha)*fade
		p.Beta = priorBeta + (p.Beta-priorBeta)*fade
		l.post[k] = p
	}
}

// ApplyDelta adds the observations that turned before into after.
// before is the prior the timed run cloned; after is that run's learner.
func (l *Learner) ApplyDelta(before, after *Learner) {
	if l == nil || after == nil {
		return
	}
	after.mu.Lock()
	type delta struct {
		k       Key
		dA      float64
		dB      float64
		dConf   uint64
		dSingle uint64
		dMulti  uint64
	}
	var rows []delta
	var costs map[Key]keyCost
	if after.cost != nil {
		costs = make(map[Key]keyCost, len(after.cost))
		for k, v := range after.cost {
			costs[k] = v
		}
	}
	for k, got := range after.post {
		base := Posterior{Alpha: priorAlpha, Beta: priorBeta}
		if before != nil {
			before.mu.Lock()
			if p, ok := before.post[k]; ok {
				base = p
			}
			before.mu.Unlock()
		}
		dA := got.Alpha - base.Alpha
		dB := got.Beta - base.Beta
		var dConf, dSingle, dMulti uint64
		if got.Conflicts > base.Conflicts {
			dConf = got.Conflicts - base.Conflicts
		}
		if got.Single > base.Single {
			dSingle = got.Single - base.Single
		}
		if got.Multi > base.Multi {
			dMulti = got.Multi - base.Multi
		}
		if dA == 0 && dB == 0 && dConf == 0 && dSingle == 0 && dMulti == 0 {
			continue
		}
		rows = append(rows, delta{k, dA, dB, dConf, dSingle, dMulti})
	}
	after.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, row := range rows {
		p := l.get(row.k)
		p.Alpha += row.dA
		p.Beta += row.dB
		p.Conflicts += row.dConf
		p.Single += row.dSingle
		p.Multi += row.dMulti
		l.post[row.k] = p
		if p.Conflicts > 0 {
			l.anyFenced.Store(1)
		}
	}
	// Durations are not beta counts. The timed run cloned the prior, so its
	// cost map is that prior plus this block. Decay does not touch it.
	if costs != nil {
		l.cost = costs
	}
}

// Absorb copies every posterior from src (used when a learning pass finishes).
func (l *Learner) Absorb(src *Learner) {
	if l == nil || src == nil {
		return
	}
	src.mu.Lock()
	cp := make(map[Key]Posterior, len(src.post))
	for k, v := range src.post {
		cp[k] = v
	}
	cc := make(map[Key]keyCost, len(src.cost))
	for k, v := range src.cost {
		cc[k] = v
	}
	src.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.post = cp
	l.cost = cc
	l.anyFenced.Store(src.anyFenced.Load())
}
