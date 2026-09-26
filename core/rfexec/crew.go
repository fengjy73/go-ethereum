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

package rfexec

import (
	"encoding/binary"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
	"golang.org/x/sys/unix"
)

const (
	// ucbBeta is the minimisation bonus. lcb = T * (1 - beta/sqrt(samples+1)).
	// Unmeasured C keep the full bonus, so a cold first block cannot be the
	// only C that ever receives a sample.
	ucbBeta = 0.35
	// selShrink pulls a selector estimate toward the gas-limit prior.
	// One observation does not replace the limit.
	selShrink = 4.0
	// hotShrink and hotMinSeen keep a contract from looking fully serial
	// after a handful of conflicts. hotRateMin is conflicts/(seen+shrink).
	hotShrink  = 8.0
	hotMinSeen = 4.0
	hotRateMin = 0.25
	// rateGasFull is enough executed gas to count as one rate sample.
	// Shorter segments still move the EMA and add a fractional sample.
	rateGasFull  = 200_000
	rateGasBlend = 500_000
	// baseGasMin is the gas a C=1 segment needs before it may move the
	// nanoseconds-per-gas baseline. Smaller C=1 segments still teach the
	// fixed overhead.
	baseGasMin = 500_000
)

// Segment is one stretch of a block executed at a single active worker
// count. Tail segments, and body segments whose frontier was narrower
// than C, do not update that C's rate or inflation.
type Segment struct {
	C      int
	WallNs uint64
	CPUNs  uint64
	Gas    uint64
	Execs  uint64
	Rolls  uint64
	Width  int
	Tail   bool
}

type selStat struct {
	gas float64
	n   float64
}

type hotStat struct {
	conflicts float64
	seen      float64
}

// CostPrior is the cross-block worker-count model. It is not the per-key
// Beta learner. -prior reset clears the Beta learner and does not clear
// this model.
//
//	T(C) = fixedNs*infl(C) + nTx*txFixed*infl(C) + max(CP, Work/C)*(1+r(C))*base*infl(C)
//
// CP is the longest same-sender chain, extended by cross-sender RAW on a
// hot contract. Work is selector gas shrunk toward the gas limit, else the
// gas limit times util. base is nanoseconds per gas at C=1, from process
// CPU. infl(C) is CPU-per-gas at C divided by base, not a ratio of wall
// rates. Until base is known, T is in gas-equivalents and model_pred_ns is 0.
type CostPrior struct {
	Chosen  int
	meanGas float64
	infl    map[int]float64
	reexec  map[int]float64
	samples map[int]float64
	sel     map[selKey]selStat
	// rate is wall nanoseconds per actual gas at that C. It records what
	// ran. The score uses base and infl, not this rate, so a parallelism
	// mistake is not baked into the slowdown.
	rate    map[int]float64
	util    float64
	base    float64
	txFixed float64
	fixedNs float64
	// refC is the worker count base was seeded from. A later C=1 sample
	// replaces it and rescales infl.
	refC int
	hot  map[common.Address]hotStat
}

type selKey struct {
	to  common.Address
	sel uint32
}

// NewCostPrior is a cold model. Inflation is 1 until a segment measures
// it. The first block therefore follows the structural argmin.
func NewCostPrior() *CostPrior {
	return &CostPrior{
		infl:    map[int]float64{},
		reexec:  map[int]float64{},
		samples: map[int]float64{},
		sel:     map[selKey]selStat{},
		rate:    map[int]float64{},
		hot:     map[common.Address]hotStat{},
	}
}

func (c *CostPrior) Clone() *CostPrior {
	if c == nil {
		return NewCostPrior()
	}
	out := NewCostPrior()
	out.Chosen = c.Chosen
	out.meanGas = c.meanGas
	out.util = c.util
	out.base = c.base
	out.txFixed = c.txFixed
	out.fixedNs = c.fixedNs
	out.refC = c.refC
	for k, v := range c.infl {
		out.infl[k] = v
	}
	for k, v := range c.reexec {
		out.reexec[k] = v
	}
	for k, v := range c.samples {
		out.samples[k] = v
	}
	for k, v := range c.sel {
		out.sel[k] = v
	}
	for k, v := range c.rate {
		out.rate[k] = v
	}
	for k, v := range c.hot {
		out.hot[k] = v
	}
	return out
}

func (c *CostPrior) inflation(n int) float64 {
	if c == nil || n < 1 {
		return 1
	}
	if c.refC == n {
		return 1
	}
	if v, ok := c.infl[n]; ok && v > 0 {
		return v
	}
	// No nearest-neighbor copy. A short or contended sample at C=2
	// used to clamp infl to 8 and then paint C=4 with the same number,
	// so later parallel blocks never left 1.
	return 1
}

func (c *CostPrior) reexecRate(n int) float64 {
	if c == nil {
		return 0
	}
	if v, ok := c.reexec[n]; ok && v > 0 {
		return v
	}
	return 0
}

// conflictProb is the shrunk cross-sender RAW rate for a contract.
// Below hotMinSeen observations it is zero, so a cold contract stays parallel.
func (c *CostPrior) conflictProb(addr common.Address) float64 {
	if c == nil {
		return 0
	}
	st := c.hot[addr]
	if st.seen < hotMinSeen {
		return 0
	}
	p := st.conflicts / (st.seen + hotShrink)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	if p < hotRateMin {
		return 0
	}
	return p
}

func (c *CostPrior) noteContract(addr common.Address, conflict bool) {
	if c == nil {
		return
	}
	if c.hot == nil {
		c.hot = map[common.Address]hotStat{}
	}
	st := c.hot[addr]
	st.seen++
	if conflict {
		st.conflicts++
	}
	c.hot[addr] = st
}

// ObserveSegments updates rate, inflation, and the C=1 overhead from each
// body segment. A tail segment is ignored. A body segment whose frontier
// width was below C does not move rate(C); it only records a fractional
// sample so the same probe is not opened every block.
func (c *CostPrior) ObserveSegments(segs []Segment) {
	if c == nil {
		return
	}
	for _, seg := range segs {
		c.observeSegment(seg)
	}
}

func (c *CostPrior) observeSegment(seg Segment) {
	if seg.C < 1 || seg.Gas == 0 || seg.WallNs == 0 || seg.Tail {
		return
	}
	if seg.Width > 0 && seg.Width < seg.C {
		c.samples[seg.C] += 0.25
		return
	}
	sample := float64(seg.WallNs) / float64(seg.Gas)
	alpha := float64(seg.Gas) / (float64(seg.Gas) + rateGasBlend)
	if prev := c.rate[seg.C]; prev > 0 {
		c.rate[seg.C] = prev*(1-alpha) + sample*alpha
	} else {
		c.rate[seg.C] = sample
	}
	if seg.Gas >= rateGasFull {
		c.samples[seg.C] += 1
	} else {
		c.samples[seg.C] += 0.25
	}
	if seg.Execs > 0 {
		r := float64(seg.Rolls) / float64(seg.Execs)
		c.reexec[seg.C] = ema(c.reexec[seg.C], r, 0.25)
		c.meanGas = ema(c.meanGas, float64(seg.Gas)/float64(seg.Execs), 0.25)
	}
	c.learnScale(seg)
	c.Chosen = seg.C
}

func (c *CostPrior) learnScale(seg Segment) {
	if seg.CPUNs == 0 || seg.Gas == 0 {
		return
	}
	per := float64(seg.CPUNs) / float64(seg.Gas)
	// Short segments still update rate above. They must not seed the
	// baseline or the per-C inflation: a drain's CPU/gas is overhead.
	if seg.Gas < baseGasMin {
		return
	}
	if seg.Execs > 0 && seg.Rolls > seg.Execs/4 {
		return
	}
	if c.base <= 0 {
		c.base = per
		c.refC = seg.C
	}
	if seg.C == 1 {
		if per > 0 {
			if c.refC != 1 && c.base > 0 {
				scale := c.base / per
				for k, v := range c.infl {
					c.infl[k] = v * scale
				}
			}
			if c.refC != 1 {
				c.base = per
			} else if per < c.base {
				c.base = c.base*0.5 + per*0.5
			} else {
				c.base = c.base*0.9 + per*0.1
			}
			c.refC = 1
		}
		if c.refC == 1 && c.base > 0 {
			c.learnOverhead(seg)
		}
		return
	}
	if c.base <= 0 || seg.C == c.refC {
		return
	}
	inf := per / c.base
	if inf < 0.25 {
		inf = 0.25
	}
	if inf > 8 {
		inf = 8
	}
	if prev := c.infl[seg.C]; prev > 0 {
		c.infl[seg.C] = prev*0.75 + inf*0.25
	} else {
		c.infl[seg.C] = inf
	}
}

func (c *CostPrior) learnOverhead(seg Segment) {
	extra := float64(seg.WallNs) - c.base*float64(seg.Gas)
	if extra < 0 {
		extra = 0
	}
	n := float64(seg.Execs)
	if n < 1 {
		n = 1
	}
	fixedGuess := extra * (8 / (n + 8))
	perTx := (extra - fixedGuess) / n
	c.fixedNs = ema(c.fixedNs, fixedGuess, 0.25)
	c.txFixed = ema(c.txFixed, perTx, 0.25)
}

// NoteRemainder folds wall time outside the accounted segments (pre-state,
// post-state, withdrawals) into the fixed per-block cost.
func (c *CostPrior) NoteRemainder(ns uint64) {
	if c == nil || ns == 0 {
		return
	}
	c.fixedNs = ema(c.fixedNs, float64(ns), 0.25)
}

// NoteUtil records actual gas divided by the sum of gas limits.
func (c *CostPrior) NoteUtil(gas, limitSum float64) {
	if c == nil || gas <= 0 || limitSum <= 0 {
		return
	}
	u := gas / limitSum
	if u < 0.02 {
		u = 0.02
	}
	if u > 1 {
		u = 1
	}
	c.util = ema(c.util, u, 0.25)
}

// ObserveBlock records one non-tail segment whose CPU time is the wall.
// Tests and a single-C block use it. Production auto runs call
// ObserveSegments with the per-segment CPU sample.
func (c *CostPrior) ObserveBlock(chosen int, wallNs, gas, execs, rolls uint64, predGas, limitSum float64) {
	if c == nil || chosen < 1 || wallNs == 0 || gas == 0 {
		return
	}
	_ = predGas
	c.ObserveSegments([]Segment{{
		C:      chosen,
		WallNs: wallNs,
		CPUNs:  wallNs,
		Gas:    gas,
		Execs:  execs,
		Rolls:  rolls,
		Width:  chosen,
	}})
	c.NoteUtil(float64(gas), limitSum)
}

func ema(prev, sample, alpha float64) float64 {
	if prev <= 0 || alpha >= 1 {
		return sample
	}
	if alpha < 0 {
		alpha = 0
	}
	return prev*(1-alpha) + sample*alpha
}

// crew plans the active worker count for one block.
//
// The opening choice minimises a lower confidence bound over every integer
// C in range, so an unmeasured rung stays eligible. That is the one
// exploration for the block. Checkpoints re-pick by the posterior mean
// after the opening segment has been folded in, and abandon a bad probe.
// The frontier width is an EWMA. The integer cap moves only when the
// average is a full worker away from the cap. Tail drain may lower the
// active count once two or fewer transactions remain. It does not change
// Best, and its segment is not a rate sample.
type crew struct {
	cost       *CostPrior
	limit      int
	n          int
	txs        []*types.Transaction
	prev       []int
	senders    []common.Address
	weight     []float64
	active     int
	bestC      int
	predG      map[int]float64
	bodyG      float64
	startG     float64
	frontier   int
	widthE     float64
	widthC     int
	liveWidth  int
	finals     int
	limitSum   float64
	startScore map[int]float64
	startNS    bool
	nextAt     int
	checks     []int
	tail       bool
	probing    bool
	rolls      uint64
	execs      uint64
	trace      []int

	clock    func() (time.Time, int64)
	open     *openSeg
	segments []Segment
	writers  map[rfstate.Key]int
}

type openSeg struct {
	c        int
	start    time.Time
	cpu0     int64
	gas      uint64
	execs    uint64
	rolls0   uint64
	widthMin int
	tail     bool
}

func newCrew(cost *CostPrior, limit int, env *BlockEnv) *crew {
	if limit < 1 {
		limit = 1
	}
	if cost == nil {
		cost = NewCostPrior()
	}
	n := 0
	if env != nil {
		n = len(env.Txs)
	}
	c := &crew{
		cost:   cost,
		limit:  limit,
		n:      n,
		predG:  map[int]float64{},
		weight: make([]float64, n),
	}
	if env != nil {
		c.txs = env.Txs
		c.prev = env.PrevSame
		c.senders = env.Senders
	}
	if n >= 8 {
		for _, p := range []int{n / 4, n / 2, (3 * n) / 4} {
			if p > 0 && (len(c.checks) == 0 || c.checks[len(c.checks)-1] != p) {
				c.checks = append(c.checks, p)
			}
		}
	} else if n >= 2 {
		c.checks = []int{n / 2}
		if c.checks[0] < 1 {
			c.checks[0] = 1
		}
	}
	return c
}

func (c *crew) setClock(fn func() (time.Time, int64)) { c.clock = fn }

// begin seeds weights, picks the first C, and returns it. width is the
// current structural frontier.
func (c *crew) begin(width int) int {
	c.seedWeights()
	if width < 1 {
		width = 1
	}
	c.liveWidth = width
	c.widthE = float64(width)
	c.widthC = width
	if c.widthC > c.limit {
		c.widthC = c.limit
	}
	c.active = c.choose(true, true)
	c.bestC = c.active
	c.bodyG = c.predG[c.active]
	c.startG = c.bodyG
	c.startNS = c.cost != nil && c.cost.base > 0
	c.startScore = make(map[int]float64, len(c.predG)+1)
	nTx := c.n
	if nTx < 1 {
		nTx = 1
	}
	for n, g := range c.predG {
		if n >= 1 && n <= c.limit && g > 0 {
			c.startScore[n] = c.score(n, g, nTx)
		}
	}
	if c.active >= 1 && c.startG > 0 {
		c.startScore[c.active] = c.score(c.active, c.startG, nTx)
	}
	c.trace = []int{c.active}
	return c.active
}

// startSegment opens the body sample at the moment workers are released.
func (c *crew) startSegment() {
	if c == nil || c.open != nil {
		return
	}
	c.openSeg(c.active, false)
}

// closeSegments folds the segment that is still open at the end of the block.
func (c *crew) closeSegments() {
	if c == nil {
		return
	}
	c.foldOpen()
}

func procCPU() int64 {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}

// observe folds one scheduler event. done is a successful completion of tx.
// The returned active count changes at a checkpoint, when the live frontier
// drops below the active count, or at the tail.
func (c *crew) observe(tx, frontier, width int, gas uint64, done bool, rolls uint64) (int, bool) {
	if width < 1 {
		width = 1
	}
	if rolls > c.rolls {
		c.rolls = rolls
	}
	c.liveWidth = width
	if c.open == nil {
		c.openSeg(c.active, c.tail)
	}
	if done {
		c.execs++
		c.finals++
		if tx >= 0 && tx < len(c.weight) && gas > 0 {
			c.weight[tx] = float64(gas)
			c.learnSel(tx, float64(gas))
		}
		if c.open != nil && gas > 0 {
			c.open.gas += gas
			c.open.execs++
		}
	}
	if c.open != nil && width >= c.open.c && (c.open.widthMin == 0 || width < c.open.widthMin) {
		c.open.widthMin = width
	}
	c.frontier = frontier
	capChanged := c.smooth(width)
	remaining := c.n - frontier
	if remaining < 0 {
		remaining = 0
	}
	if !c.tail && c.n > 2 && remaining <= 2 {
		c.foldOpen()
		c.tail = true
		c.probing = false
		n := c.tailActive(width)
		c.openSeg(n, true)
		return c.apply(n)
	}
	if c.tail {
		n := c.tailActive(width)
		if c.open == nil || !c.open.tail || c.open.c != n {
			c.foldOpen()
			c.openSeg(n, true)
		}
		return c.apply(n)
	}
	checkpoint := done && c.nextAt < len(c.checks) && c.finals >= c.checks[c.nextAt]
	if width < c.active {
		if checkpoint {
			for c.nextAt < len(c.checks) && c.finals >= c.checks[c.nextAt] {
				c.nextAt++
			}
		}
		c.probing = false
		c.foldOpen()
		// Clamping to the live frontier is not a new body plan.
		n := c.choose(false, false)
		if n > width {
			n = width
		}
		c.openSeg(n, false)
		return c.apply(n)
	}
	// Hold the opening explore across an EWMA dip. A live frontier below
	// C already returned above. The first checkpoint folds this segment
	// and re-picks by the mean, which abandons a bad probe.
	if c.probing && !checkpoint {
		return c.active, false
	}
	if checkpoint {
		for c.nextAt < len(c.checks) && c.finals >= c.checks[c.nextAt] {
			c.nextAt++
		}
	}
	if checkpoint || (capChanged && c.active > c.widthC) {
		c.probing = false
		c.foldOpen()
		n := c.choose(false, true)
		c.openSeg(n, false)
		return c.apply(n)
	}
	return c.active, false
}

func (c *crew) tailActive(width int) int {
	n := width
	if c.widthC < n {
		n = c.widthC
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (c *crew) smooth(width int) bool {
	if c.widthE == 0 {
		c.widthE = float64(width)
	} else {
		c.widthE += 0.25 * (float64(width) - c.widthE)
	}
	prev := c.widthC
	if c.widthC < 1 {
		c.widthC = 1
	}
	if c.widthE >= float64(c.widthC)+1 && c.widthC < c.limit {
		c.widthC++
	} else if c.widthC > 1 && c.widthE <= float64(c.widthC)-1 {
		c.widthC--
	}
	if c.widthC > c.limit {
		c.widthC = c.limit
	}
	return c.widthC != prev
}

func (c *crew) choose(explore, commit bool) int {
	limit := c.widthC
	if c.limit < limit {
		limit = c.limit
	}
	if limit < 1 {
		limit = 1
	}
	cp, work := c.path(c.frontier)
	localR := 0.0
	if c.execs > 0 && c.rolls > 0 {
		localR = float64(c.rolls) / float64(c.execs)
	}
	nTx := c.n - c.frontier
	if nTx < 1 {
		nTx = 1
	}
	bestMean := 1
	bestMeanT := math.MaxFloat64
	bestLCB := 1
	bestLCBT := math.MaxFloat64
	g1 := c.predictGas(cp, work, 1, localR)
	for n := 1; n <= limit; n++ {
		g := c.predictGas(cp, work, n, localR)
		c.predG[n] = g
		t := c.score(n, g, nTx)
		if t < bestMeanT {
			bestMeanT = t
			bestMean = n
		}
		// The bonus is only for a C that shortens the critical path.
		// On a pure sender chain every C has the same gas term, and a
		// bonus would spend the block at a wider count for no speedup.
		samples := 0.0
		if c.cost != nil {
			samples = c.cost.samples[n]
		}
		bonus := 0.0
		if n == 1 || g < g1 {
			bonus = ucbBeta / math.Sqrt(samples+1)
			if bonus > 0.95 {
				bonus = 0.95
			}
		}
		lcb := t * (1 - bonus)
		if lcb < bestLCBT {
			bestLCBT = lcb
			bestLCB = n
		}
	}
	picked := bestMean
	if explore {
		picked = bestLCB
		// A baseline seeded from C>1 is not the C=1 clock. Spend the one
		// explore on C=1 so the next block can rescale inflation.
		if c.cost != nil && c.cost.refC > 1 && c.cost.samples[1] < 1 {
			picked = 1
		}
		c.probing = picked != bestMean
	} else if c.bestC >= 1 && c.bestC <= limit {
		g := c.predictGas(cp, work, c.bestC, localR)
		t := c.score(c.bestC, g, nTx)
		if t <= bestMeanT*1.05 {
			picked = c.bestC
		}
	}
	if commit && !c.tail {
		c.bestC = picked
		c.bodyG = c.predictGas(cp, work, picked, localR)
	}
	return picked
}

// score is nanoseconds when base is known, otherwise the gas-equivalent.
func (c *crew) score(n int, gas float64, nTx int) float64 {
	if gas <= 0 {
		gas = c.predG[n]
	}
	if gas <= 0 {
		gas = 1
	}
	if c.cost == nil || c.cost.base <= 0 {
		return gas
	}
	infl := c.cost.inflation(n)
	if infl <= 0 {
		infl = 1
	}
	if nTx < 1 {
		nTx = 1
	}
	return c.cost.fixedNs*infl + float64(nTx)*c.cost.txFixed*infl + gas*c.cost.base*infl
}

func (c *crew) predictGas(cp, work float64, n int, localR float64) float64 {
	if n < 1 {
		n = 1
	}
	r := c.cost.reexecRate(n)
	if localR > r {
		r = localR
	}
	par := work / float64(n)
	if cp > par {
		par = cp
	}
	if par < 1 {
		par = 1
	}
	return par * (1 + r)
}

// path is the critical path and the total work of transactions at index
// >= from. Same-sender edges come from PrevSame. A contract with
// cross-sender RAW adds a soft chain: max weight plus conflictProb times
// the rest of that contract's weight. A full serial chain (prob 1) matches
// a sender. The shrunk prob stays below 1, so sum/CP stays in the few-times
// range instead of collapsing to 1 after a handful of conflicts.
func (c *crew) path(from int) (cp, work float64) {
	type agg struct{ sum, max float64 }
	hot := map[common.Address]agg{}
	memo := make([]float64, c.n)
	var walk func(i int) float64
	walk = func(i int) float64 {
		if i < from || i >= c.n {
			return 0
		}
		if memo[i] > 0 {
			return memo[i]
		}
		w := 1.0
		if i < len(c.weight) && c.weight[i] > 0 {
			w = c.weight[i]
		}
		best := w
		if c.prev != nil && i < len(c.prev) {
			p := c.prev[i]
			if p >= from && p < i {
				if alt := walk(p) + w; alt > best {
					best = alt
				}
			}
		}
		memo[i] = best
		return best
	}
	for i := from; i < c.n; i++ {
		w := 1.0
		if i < len(c.weight) && c.weight[i] > 0 {
			w = c.weight[i]
		}
		work += w
		if v := walk(i); v > cp {
			cp = v
		}
		if addr, ok := c.contract(i); ok {
			if p := c.cost.conflictProb(addr); p > 0 {
				a := hot[addr]
				a.sum += w
				if w > a.max {
					a.max = w
				}
				hot[addr] = a
			}
		}
	}
	for addr, a := range hot {
		p := c.cost.conflictProb(addr)
		soft := a.max + p*(a.sum-a.max)
		if soft > cp {
			cp = soft
		}
	}
	if cp < 1 && c.n > from {
		cp = 1
	}
	return cp, work
}

func (c *crew) contract(i int) (common.Address, bool) {
	if c.txs == nil || i < 0 || i >= len(c.txs) || c.txs[i] == nil || c.txs[i].To() == nil {
		return common.Address{}, false
	}
	return *c.txs[i].To(), true
}

func (c *crew) sender(i int) (common.Address, bool) {
	if c.senders == nil || i < 0 || i >= len(c.senders) {
		return common.Address{}, false
	}
	return c.senders[i], true
}

// noteIO records this attempt's reads and writes. A read of a key an
// earlier different sender wrote is a cross-sender RAW on this contract.
// The caller holds the scheduler lock and must not be holding a ledger key
// lock; this method takes none.
func (c *crew) noteIO(tx int, reads, writes []rfstate.Key) {
	if c == nil || tx < 0 {
		return
	}
	if c.writers == nil {
		c.writers = map[rfstate.Key]int{}
	}
	conflict := false
	me, haveMe := c.sender(tx)
	for _, k := range reads {
		if k.Kind == rfstate.KindFee {
			continue
		}
		w, ok := c.writers[k]
		if !ok || w >= tx {
			continue
		}
		other, haveOther := c.sender(w)
		if haveMe && haveOther && other != me {
			conflict = true
			break
		}
	}
	if addr, ok := c.contract(tx); ok {
		c.cost.noteContract(addr, conflict)
	}
	for _, k := range writes {
		if k.Kind == rfstate.KindFee {
			continue
		}
		c.writers[k] = tx
	}
}

func (c *crew) seedWeights() {
	c.limitSum = 0
	for i := 0; i < c.n; i++ {
		c.weight[i] = c.priorWeight(i)
		if c.txs != nil && i < len(c.txs) && c.txs[i] != nil && c.txs[i].Gas() > 0 {
			c.limitSum += float64(c.txs[i].Gas())
		} else if c.weight[i] > 0 {
			c.limitSum += c.weight[i]
		}
	}
}

func (c *crew) priorWeight(i int) float64 {
	if c.txs == nil || i < 0 || i >= len(c.txs) || c.txs[i] == nil {
		if c.cost.meanGas > 0 {
			return c.cost.meanGas
		}
		return 1
	}
	tx := c.txs[i]
	prior := 0.0
	if g := tx.Gas(); g > 0 {
		prior = float64(g)
		if c.cost.util > 0 && c.cost.util < 1 {
			prior *= c.cost.util
		}
	} else if c.cost.meanGas > 0 {
		prior = c.cost.meanGas
	} else {
		prior = 1
	}
	if st, ok := c.cost.sel[selectorOf(tx)]; ok && st.n > 0 {
		base := prior
		if base <= 0 {
			base = st.gas
		}
		return (selShrink*base + st.n*st.gas) / (selShrink + st.n)
	}
	return prior
}

func selectorOf(tx *types.Transaction) selKey {
	var key selKey
	if tx.To() != nil {
		key.to = *tx.To()
	}
	data := tx.Data()
	if len(data) >= 4 {
		key.sel = binary.BigEndian.Uint32(data[:4])
	}
	return key
}

func (c *crew) learnSel(tx int, gas float64) {
	if c.txs == nil || tx < 0 || tx >= len(c.txs) || c.txs[tx] == nil || gas <= 0 {
		return
	}
	key := selectorOf(c.txs[tx])
	st := c.cost.sel[key]
	st.n++
	if st.n == 1 {
		st.gas = gas
	} else {
		st.gas += (gas - st.gas) / st.n
	}
	c.cost.sel[key] = st
}

func (c *crew) openSeg(n int, tail bool) {
	if n < 1 {
		n = 1
	}
	w := c.liveWidth
	if w < 1 {
		w = n
	}
	o := &openSeg{c: n, widthMin: w, tail: tail, rolls0: c.rolls}
	if c.clock != nil {
		o.start, o.cpu0 = c.clock()
	}
	c.open = o
}

func (c *crew) foldOpen() {
	if c.open == nil {
		return
	}
	seg := c.snapshot()
	c.segments = append(c.segments, seg)
	c.open = nil
	if seg.WallNs == 0 || seg.Tail {
		return
	}
	c.cost.ObserveSegments([]Segment{seg})
}

func (c *crew) snapshot() Segment {
	seg := Segment{
		C:     c.open.c,
		Gas:   c.open.gas,
		Execs: c.open.execs,
		Rolls: c.rolls - c.open.rolls0,
		Width: c.open.widthMin,
		Tail:  c.open.tail,
	}
	if seg.Width < 1 {
		seg.Width = seg.C
	}
	if c.clock != nil && !c.open.start.IsZero() {
		now, cpu := c.clock()
		if d := now.Sub(c.open.start); d > 0 {
			seg.WallNs = uint64(d)
		}
		if cpu > c.open.cpu0 {
			seg.CPUNs = uint64(cpu - c.open.cpu0)
		}
	}
	return seg
}

func (c *crew) apply(n int) (int, bool) {
	if n < 1 {
		n = 1
	}
	if n > c.limit {
		n = c.limit
	}
	if n == c.active {
		return c.active, false
	}
	c.active = n
	c.note()
	return c.active, true
}

func (c *crew) note() {
	if len(c.trace) == 0 || c.trace[len(c.trace)-1] != c.active {
		c.trace = append(c.trace, c.active)
	}
}

func (c *crew) Active() int { return c.active }

// Best is the last body choice. Tail drain does not change it.
func (c *crew) Best() int {
	if c.bestC > 0 {
		return c.bestC
	}
	if c.active > 0 {
		return c.active
	}
	return 1
}

// BodyGas is the gas-equivalent prediction of the starting choice.
func (c *crew) BodyGas() float64 {
	if c.startG > 0 {
		return c.startG
	}
	return c.bodyG
}

func (c *crew) Trace() string {
	if len(c.trace) == 0 {
		return strconv.Itoa(c.active)
	}
	parts := make([]string, len(c.trace))
	for i, n := range c.trace {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, "-")
}

// Curve is the opening full-block prediction for every feasible C, in
// nanoseconds when base is known and in gas-equivalents otherwise.
// model_pred_ns is the pair for the opening choice, captured before this
// block's segments are folded in.
func (c *crew) Curve() (unit string, chosen int, pred int64, text string) {
	chosen = c.Best()
	unit = "gas"
	if c.startNS {
		unit = "ns"
	}
	val := 0.0
	if c.startScore != nil {
		val = c.startScore[chosen]
	}
	if val <= 0 {
		nTx := c.n
		if nTx < 1 {
			nTx = 1
		}
		val = c.score(chosen, c.BodyGas(), nTx)
	}
	if unit == "ns" && val > 0 {
		pred = int64(val)
	}
	src := c.startScore
	if len(src) == 0 {
		src = map[int]float64{}
		nTx := c.n
		if nTx < 1 {
			nTx = 1
		}
		for n, g := range c.predG {
			src[n] = c.score(n, g, nTx)
		}
	}
	keys := make([]int, 0, len(src))
	for n := range src {
		if n >= 1 && n <= c.limit {
			keys = append(keys, n)
		}
	}
	sort.Ints(keys)
	var b strings.Builder
	b.WriteString(unit)
	b.WriteByte(':')
	for i, n := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(n))
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(int64(src[n]), 10))
	}
	return unit, chosen, pred, b.String()
}
