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

import "sync"

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
}

// Learner holds per-key Beta posteriors. P0 decides greedily by comparing
// expected PASS cost with expected WAIT_FINAL cost. Replay and wait costs
// start equal, so the rule reduces to "wait when the posterior mean exceeds
// one half and a lower producer is already known". Costs stay equal in P0
// (Thompson sampling and measured costs are P1).
type Learner struct {
	mu   sync.Mutex
	post map[Key]Posterior
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
	if l == nil {
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
}
