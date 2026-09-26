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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// CostPrior is the cross-block worker-count model. It is not the per-key
// Beta learner: conflict fences still come from that learner. This prior
// carries per-selector gas, a baseline nanoseconds-per-gas, a per-C
// execution inflation, and a per-C re-execution rate. -prior reset clears
// the Beta learner and does not clear this model; the inflation is a
// property of the machine, measured on previous blocks.
//
// T(C) = max(CP, Work/C) * (1+r(C)) * nsPerGas * infl(C)
//
// CP is the longest sender chain in the remaining transactions, stretched
// by the in-block re-execution rate when that rate exceeds r(C). Work is
// the sum of per-transaction gas (learned per contract and selector, else
// the running mean, else the transaction gas limit). infl(C) is the
// per-execution slowdown relative to the baseline. Wall time is not
// consulted inside a block. After the block it scales infl(C) so the next
// block's prediction moves toward the observed wall.
type CostPrior struct {
	Chosen  int
	meanGas float64
	infl    map[int]float64
	reexec  map[int]float64
	samples map[int]int
	selGas  map[selKey]float64
	// rate is wall nanoseconds per gas-equivalent L(C), before inflation.
	// L is on the actual-gas scale: the sample uses gas the block really
	// burned, and the next block's weights are selector gas or gas-limit
	// times util. infl(C) is rate[C]/rate[smallest measured C]. A C with
	// no sample is not eligible, except for one probe per block.
	rate map[int]float64
	// util is actual gas divided by the sum of gas limits.
	util float64
}

type selKey struct {
	to  common.Address
	sel uint32
}

// NewCostPrior is a cold model. Every C has inflation 1 until a block
// measures it. The first block therefore prefers the widest smoothed
// frontier; later blocks pay the measured slowdown.
func NewCostPrior() *CostPrior {
	return &CostPrior{
		infl:    map[int]float64{},
		reexec:  map[int]float64{},
		samples: map[int]int{},
		selGas:  map[selKey]float64{},
		rate:    map[int]float64{},
	}
}

func (c *CostPrior) Clone() *CostPrior {
	if c == nil {
		return NewCostPrior()
	}
	out := NewCostPrior()
	out.Chosen = c.Chosen
	out.meanGas = c.meanGas
	for k, v := range c.infl {
		out.infl[k] = v
	}
	for k, v := range c.reexec {
		out.reexec[k] = v
	}
	for k, v := range c.samples {
		out.samples[k] = v
	}
	for k, v := range c.selGas {
		out.selGas[k] = v
	}
	for k, v := range c.rate {
		out.rate[k] = v
	}
	out.util = c.util
	out.deriveInfl()
	return out
}

// deriveInfl sets infl(C) = rate[C] / rate[smallest measured C].
func (c *CostPrior) deriveInfl() {
	refC := 0
	for n := range c.rate {
		if c.rate[n] <= 0 {
			continue
		}
		if refC == 0 || n < refC {
			refC = n
		}
	}
	if refC == 0 {
		return
	}
	ref := c.rate[refC]
	if ref <= 0 {
		return
	}
	for n, r := range c.rate {
		if r <= 0 {
			continue
		}
		c.infl[n] = r / ref
	}
}

func (c *CostPrior) inflation(n int) float64 {
	if c == nil || n < 1 {
		return 1
	}
	if v, ok := c.infl[n]; ok && v > 0 {
		return v
	}
	bestD := int(^uint(0) >> 1)
	best := 1.0
	for k, v := range c.infl {
		if v <= 0 {
			continue
		}
		d := k - n
		if d < 0 {
			d = -d
		}
		if d < bestD {
			bestD = d
			best = v
		}
	}
	return best
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

// ObserveBlock records wall nanoseconds per gas-equivalent L(C) and
// derives inflation from the smallest measured C. A single block's rate
// is clamped to [0.25, 8] times the reference so a stall cannot erase
// the other samples. The clamp bounds the slowdown, not the worker count.
func (c *CostPrior) ObserveBlock(chosen int, wallNs, gas, execs, rolls uint64, predGas, limitSum float64) {
	if c == nil || chosen < 1 || wallNs == 0 || predGas <= 0 {
		return
	}
	sample := float64(wallNs) / predGas
	if ref := c.refRate(); ref > 0 {
		if sample < ref*0.25 {
			sample = ref * 0.25
		}
		if sample > ref*8 {
			sample = ref * 8
		}
	}
	if prev := c.rate[chosen]; prev > 0 {
		c.rate[chosen] = prev*0.75 + sample*0.25
	} else {
		c.rate[chosen] = sample
	}
	if execs > 0 {
		r := float64(rolls) / float64(execs)
		prev := c.reexec[chosen]
		if c.samples[chosen] <= 0 {
			c.reexec[chosen] = r
		} else {
			c.reexec[chosen] = prev*0.75 + r*0.25
		}
	}
	if gas > 0 && execs > 0 {
		mean := float64(gas) / float64(execs)
		if c.meanGas <= 0 {
			c.meanGas = mean
		} else {
			c.meanGas = c.meanGas*0.75 + mean*0.25
		}
	}
	if limitSum > 0 && gas > 0 {
		u := float64(gas) / limitSum
		if u < 0.02 {
			u = 0.02
		}
		if u > 1 {
			u = 1
		}
		if c.util <= 0 {
			c.util = u
		} else {
			c.util = c.util*0.75 + u*0.25
		}
	}
	c.samples[chosen]++
	c.Chosen = chosen
	c.deriveInfl()
}

func (c *CostPrior) refRate() float64 {
	refC := 0
	for n, r := range c.rate {
		if r <= 0 {
			continue
		}
		if refC == 0 || n < refC {
			refC = n
		}
	}
	if refC == 0 {
		return 0
	}
	return c.rate[refC]
}

func (c *CostPrior) measured(n int) bool {
	return c != nil && c.samples[n] > 0 && c.rate[n] > 0
}

// crew plans the active worker count for one block.
//
// The choice is argmin T(C) over C that fit the smoothed frontier width
// and the process limit. The frontier width is an EWMA. The integer cap
// moves only when the average is a full worker away from the cap, so a
// 1-vs-2 flap on successive scheduling events does not move it. The plan
// is recomputed at a few completion checkpoints, not on every event.
// Tail drain may lower the active count once two or fewer transactions
// remain. It does not change Best and it is not the sample ObserveBlock
// keeps: Best is the last body choice.
type crew struct {
	cost       *CostPrior
	limit      int
	n          int
	txs        []*types.Transaction
	prev       []int
	weight     []float64
	active     int
	bestC      int
	predG      map[int]float64 // gas-equivalent L(C) for the latest plan
	bodyG      float64         // L(best) for the latest plan
	startG     float64         // L(best) at block start; the reported prediction uses this
	frontier   int
	widthE     float64
	widthC     int
	finals     int
	limitSum   float64
	startScore map[int]float64 // full-block T(C) at the opening plan
	nextAt     int
	checks     []int
	tail       bool
	probing    bool
	rolls      uint64
	execs      uint64
	trace      []int
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

// begin seeds weights, picks the first C, and returns it. width is the
// current structural frontier.
func (c *crew) begin(width int) int {
	c.seedWeights()
	if width < 1 {
		width = 1
	}
	c.widthE = float64(width)
	c.widthC = width
	if c.widthC > c.limit {
		c.widthC = c.limit
	}
	c.active = c.choose(false)
	c.bestC = c.active
	c.bodyG = c.predG[c.active]
	c.startG = c.bodyG
	c.startScore = make(map[int]float64, len(c.predG)+1)
	for n, g := range c.predG {
		if n >= 1 && n <= c.limit && g > 0 {
			c.startScore[n] = c.score(n, g)
		}
	}
	if c.active >= 1 && c.startG > 0 {
		c.startScore[c.active] = c.score(c.active, c.startG)
	}
	c.trace = []int{c.active}
	return c.active
}

// realizedGas is L(Best) from the gas the block actually used. The rate
// sample uses this, not the opening estimate, so a gas-limit weight and an
// actual-gas weight are not mixed into the same nanoseconds-per-gas.
func (c *crew) realizedGas() float64 {
	if c == nil || c.n == 0 {
		return 0
	}
	cp, work := c.path(0)
	r := 0.0
	if c.execs > 0 && c.rolls > 0 {
		r = float64(c.rolls) / float64(c.execs)
	}
	return c.predictGas(cp, work, c.Best(), r)
}

// probePick is the one unmeasured C this block may run so the next block
// has a wall rate to compare. Cold models do not probe; they take the
// argmin at inflation 1. After that, unmeasured C are filled downward to
// 1 and then one step upward, one block at a time.
func (c *crew) probePick(limit int, cp, work, localR float64) (int, bool) {
	if c.cost == nil {
		return 0, false
	}
	measured := 0
	best := 0
	bestT := math.MaxFloat64
	for n := 1; n <= limit; n++ {
		if !c.cost.measured(n) {
			continue
		}
		measured++
		g := c.predictGas(cp, work, n, localR)
		t := c.score(n, g)
		if t < bestT {
			bestT = t
			best = n
		}
	}
	if measured == 0 || best == 0 {
		return 0, false
	}
	down := best / 2
	if down < 1 {
		down = 1
	}
	if down != best && !c.cost.measured(down) {
		return down, true
	}
	up := best * 2
	if up <= best {
		up = best + 1
	}
	if up <= limit && !c.cost.measured(up) {
		return up, true
	}
	return 0, false
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
	if g, ok := c.cost.selGas[selectorOf(tx)]; ok && g > 0 {
		return g
	}
	if g := tx.Gas(); g > 0 {
		w := float64(g)
		if c.cost.util > 0 {
			w *= c.cost.util
		}
		return w
	}
	if c.cost.meanGas > 0 {
		return c.cost.meanGas
	}
	return 1
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

// observe folds one scheduler event. done is a successful completion of tx.
// rolls is the run's rollback counter. The returned active count changes
// only at a checkpoint, a smoothed-width step, or the tail.
func (c *crew) observe(tx, frontier, width int, gas uint64, done bool, rolls uint64) (int, bool) {
	if width < 1 {
		width = 1
	}
	if rolls > c.rolls {
		c.rolls = rolls
	}
	capChanged := c.smooth(width)
	if done {
		c.execs++
		c.finals++
		if tx >= 0 && tx < len(c.weight) && gas > 0 {
			c.weight[tx] = float64(gas)
			c.learnSel(tx, float64(gas))
		}
	}
	c.frontier = frontier
	remaining := c.n - frontier
	if remaining < 0 {
		remaining = 0
	}
	if !c.tail && c.n > 2 && remaining <= 2 {
		c.tail = true
		return c.apply(c.tailActive(width))
	}
	if c.tail {
		return c.apply(c.tailActive(width))
	}
	if capChanged && c.active > c.widthC {
		return c.apply(c.choose(true))
	}
	if done && c.nextAt < len(c.checks) && c.finals >= c.checks[c.nextAt] {
		for c.nextAt < len(c.checks) && c.finals >= c.checks[c.nextAt] {
			c.nextAt++
		}
		return c.apply(c.choose(true))
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

func (c *crew) choose(sticky bool) int {
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
	anyMeasured := false
	for n := 1; n <= c.limit; n++ {
		if c.cost.measured(n) {
			anyMeasured = true
			break
		}
	}
	bestC := 1
	bestT := math.MaxFloat64
	for n := 1; n <= limit; n++ {
		if anyMeasured && !c.cost.measured(n) {
			continue
		}
		g := c.predictGas(cp, work, n, localR)
		c.predG[n] = g
		t := c.score(n, g)
		if t < bestT {
			bestT = t
			bestC = n
		}
	}
	// A probe runs for the whole body so its wall rate is about one C.
	// Checkpoints do not abandon it. Tail drain still may shrink later.
	if !sticky {
		if n, ok := c.probePick(limit, cp, work, localR); ok {
			c.probing = true
			g := c.predictGas(cp, work, n, localR)
			c.predG[n] = g
			bestC = n
		}
	} else if c.probing && c.bestC >= 1 && c.bestC <= limit {
		bestC = c.bestC
	} else if sticky && c.bestC >= 1 && c.bestC <= limit && (!anyMeasured || c.cost.measured(c.bestC)) {
		// Stay unless another measured C is more than 5% better.
		g := c.predictGas(cp, work, c.bestC, localR)
		t := c.score(c.bestC, g)
		if t <= bestT*1.05 {
			bestC = c.bestC
		}
	}
	if !c.tail {
		c.bestC = bestC
		c.bodyG = c.predictGas(cp, work, bestC, localR)
	}
	return bestC
}

// score is the comparable prediction. Unmeasured C uses inflation 1.
// Measured C uses L(C) * rate[C], which already includes that C's slowdown.
func (c *crew) score(n int, gas float64) float64 {
	if gas <= 0 {
		gas = c.predG[n]
	}
	if gas <= 0 {
		gas = 1
	}
	if c.cost.measured(n) {
		return gas * c.cost.rate[n]
	}
	if ref := c.cost.refRate(); ref > 0 {
		return gas * c.cost.inflation(n) * ref
	}
	return gas
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

// path is the sender-chain critical path and the total work of transactions
// at index >= from. A transaction depends on PrevSame when that predecessor
// is still in the window.
func (c *crew) path(from int) (cp, work float64) {
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
	}
	if cp < 1 && c.n > from {
		cp = 1
	}
	return cp, work
}

func (c *crew) learnSel(tx int, gas float64) {
	if c.txs == nil || tx < 0 || tx >= len(c.txs) || c.txs[tx] == nil || gas <= 0 {
		return
	}
	key := selectorOf(c.txs[tx])
	prev := c.cost.selGas[key]
	if prev <= 0 {
		c.cost.selGas[key] = gas
		return
	}
	c.cost.selGas[key] = prev*0.75 + gas*0.25
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
// Wall feedback uses it, not a later suffix re-plan.
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

// Curve is the last body prediction for every feasible C, in nanoseconds
// when the model has a scale and in gas-equivalents otherwise. The prefix
// is "ns" or "gas".
func (c *crew) Curve() (unit string, chosen int, pred int64, text string) {
	chosen = c.Best()
	unit = "gas"
	if c.cost.measured(chosen) || c.cost.refRate() > 0 {
		unit = "ns"
	}
	val := 0.0
	if c.startScore != nil {
		val = c.startScore[chosen]
	}
	if val <= 0 {
		val = c.score(chosen, c.BodyGas())
	}
	if unit == "ns" && val > 0 {
		pred = int64(val)
	}
	src := c.startScore
	if len(src) == 0 {
		src = map[int]float64{}
		for n, g := range c.predG {
			src[n] = c.score(n, g)
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
