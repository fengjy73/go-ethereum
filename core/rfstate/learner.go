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

// Learner holds per-key Beta posteriors. P0 decides greedily by comparing
// expected PASS cost with expected WAIT_FINAL cost. Replay and wait costs
// start equal, so the rule reduces to "wait when the posterior mean exceeds
// one half and a lower producer is already known". Costs stay equal in P0
// (Thompson sampling and measured costs are P1).
type Learner struct {
	mu        sync.Mutex
	post      map[Key]Posterior
	anyFenced atomic.Uint32 // 1 once any key has Conflicts > 0
}

// NewLearner returns an empty learner (every key uses the global prior).
func NewLearner() *Learner {
	return &Learner{post: map[Key]Posterior{}}
}

// Clone returns a deep copy. Timed runs restore this snapshot so a block's
// own updates never leak into its prior.
func (l *Learner) Clone() *Learner {
	if l == nil {
		return NewLearner()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := &Learner{post: make(map[Key]Posterior, len(l.post))}
	for k, v := range l.post {
		n.post[k] = v
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
func (l *Learner) Choose(k Key, hasProducer bool) Fence {
	if l == nil || !hasProducer {
		return FencePass
	}
	l.mu.Lock()
	p := l.get(k)
	l.mu.Unlock()
	// E[C_pass] = P_c * replayCost, E[C_wait] = (1-P_c) * waitCost,
	// replayCost = waitCost = 1 in P0.
	pc := p.Alpha / (p.Alpha + p.Beta)
	if pc > 0.5 {
		return FenceWaitFinal
	}
	return FencePass
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
	src.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.post = cp
	l.anyFenced.Store(src.anyFenced.Load())
}
