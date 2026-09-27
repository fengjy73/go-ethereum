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
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// selShrink pulls a selector estimate toward the gas-limit prior.
	// One observation does not replace the limit.
	selShrink = 4.0
	// hotShrink and hotMinSeen keep a contract from looking fully serial
	// after a handful of conflicts. hotRateMin is conflicts/(seen+shrink).
	hotShrink  = 8.0
	hotMinSeen = 4.0
	hotRateMin = 0.25
	// armPriorN is the pseudo-count on an untried worker count.
	// One real sample can overturn the prior; two would not, for a
	// four-times-faster rung.
	armPriorN = 1.0
	// armPriorStd is the untried arm's standard deviation, as a fraction
	// of its prior mean. Nearby rungs are drawn sometimes. A wide rung
	// whose prior mean sits well above C=1 is rarely drawn.
	armPriorStd = 0.25
	// minArmGas ignores a body too small to be a rate.
	minArmGas = 50_000
	// tinyTxCold is the bootstrap for "this block cannot pay for parallel
	// startup". It is replaced once a serial nanoseconds-per-gas rate exists.
	tinyTxCold = 48
	// startupInit is the bootstrap parallel-startup estimate, in nanoseconds.
	startupInit = 2.5e6
)

type selStat struct {
	gas float64
	n   float64
}

type hotStat struct {
	conflicts float64
	seen      float64
}

// armStat is one worker-count's wall-nanoseconds-per-gas, as a Welford
// mean. The decision does not fit per-C inflation, a baseline, or a
// fixed cost. Those regressions treated an unmeasured C as infl=1, which
// is as cheap as C=1, and then visited every integer up to the cap.
type armStat struct {
	n, mean, m2 float64
}

// CostPrior is the cross-block worker-count model. It is not the per-key
// Beta learner. -prior reset clears the Beta learner and does not clear
// this model.
//
// Each block runs one arm for its body. The arm set is a geometric grid
// inside the cap: powers of two, plus the cap when it is not a power of
// two. Speedup and overhead are multiplicative, and on ict21 the integer
// curve was saw-toothed noise (one block's T(1..7) was 141/199/63/70/44/28/57),
// so the integers between rungs are not arms.
//
// The reward is the body's wall time per gas. Tail drain is excluded.
// A guard that shrinks the active count does not move the reward onto
// the shrunk count: the whole body is charged to the arm that was chosen,
// so an arm that could not stay wide looks slow.
//
// The structural critical path is only a prior feature. An untried arm's
// mean is ref * priorRatio, which is above the serial rate. With no
// samples the draw is that mean with no noise, so the first block is C=1.
type CostPrior struct {
	Chosen     int
	arms       map[int]armStat
	sel        map[selKey]selStat
	hot        map[common.Address]hotStat
	util       float64
	meanGas    float64
	serialRate float64
	startupNs  float64
	// startupMin is the smallest positive startup excess measured so far.
	// The EMA must not fall below it: a run of blocks with no excess used
	// to decay startupNs to 0 and the tiny-block rule stopped firing.
	startupMin float64
	// step selects the Thompson seed. Clones copy it, so K runs of one
	// block draw the same arm. The run that is kept has stepped once.
	step uint64
}

type selKey struct {
	to  common.Address
	sel uint32
}

// NewCostPrior is a cold model. No arm has a sample, so the first block
// follows the prior mean and starts at one worker.
func NewCostPrior() *CostPrior {
	return &CostPrior{
		arms:      map[int]armStat{},
		sel:       map[selKey]selStat{},
		hot:       map[common.Address]hotStat{},
		startupNs: startupInit,
	}
}

func (c *CostPrior) Clone() *CostPrior {
	if c == nil {
		return NewCostPrior()
	}
	out := NewCostPrior()
	out.Chosen = c.Chosen
	out.util = c.util
	out.meanGas = c.meanGas
	out.serialRate = c.serialRate
	out.startupNs = c.startupNs
	out.startupMin = c.startupMin
	out.step = c.step
	for k, v := range c.arms {
		out.arms[k] = v
	}
	for k, v := range c.sel {
		out.sel[k] = v
	}
	for k, v := range c.hot {
		out.hot[k] = v
	}
	return out
}

// armGrid is the worker counts a block may be assigned. Powers of two,
// then the cap if it is not already on that ladder.
func armGrid(cap int) []int {
	if cap < 1 {
		cap = 1
	}
	var arms []int
	for n := 1; n < cap; n *= 2 {
		arms = append(arms, n)
	}
	if len(arms) == 0 || arms[len(arms)-1] != cap {
		arms = append(arms, cap)
	}
	return arms
}

// priorRatio is how much more expensive an untried arm looks, per gas,
// than the serial rate. It has to clear two constraints at once.
//
// A fully parallel pen/gain of (1+log2(C))/sqrt(C) is 1.5 at C=4 and only
// about 1.06 at C=32. The C=32 value is not pessimistic: Thompson noise
// draws it as often as a neighbor, which is how every integer up to the
// cap got visited when the old model stored infl=1. The C=4 value is too
// pessimistic for the speedups these blocks actually have (about 1.4x, not
// 4x): one such sample cannot pull a 1.5x prior under the serial rate.
//
// The parallel prior is therefore 1+0.06*log2(C)^2: C=2 ≈ 1.06, C=4 ≈ 1.24
// (one 1.4x sample wins), C=32 = 2.5 (rarely drawn). When the structural
// speedup cannot fill the arm, the older pen/gain ratio replaces it if
// that ratio is higher, so a sender chain does not explore wide arms.
func priorRatio(arm int, speedup float64) float64 {
	if arm <= 1 {
		return 1
	}
	lg := math.Log2(float64(arm))
	ratio := 1 + 0.06*lg*lg
	if speedup < 1 {
		speedup = 1
	}
	if speedup < float64(arm) {
		gain := math.Sqrt(speedup)
		if gain < 1 {
			gain = 1
		}
		structural := (1 + lg) / gain
		if structural > ratio {
			ratio = structural
		}
	}
	return ratio
}

func (c *CostPrior) anySample() bool {
	if c == nil {
		return false
	}
	for _, st := range c.arms {
		if st.n > 0 {
			return true
		}
	}
	return false
}

// refRate is nanoseconds per gas from C=1, else from the smallest sampled
// arm, else 1 when nothing has been measured (the curve is then unitless
// and model_pred_ns stays 0).
func (c *CostPrior) refRate() float64 {
	if c == nil {
		return 1
	}
	if c.serialRate > 0 {
		return c.serialRate
	}
	best := 0
	for arm, st := range c.arms {
		if st.n > 0 && st.mean > 0 && (best == 0 || arm < best) {
			best = arm
		}
	}
	if best > 0 {
		return c.arms[best].mean
	}
	return 1
}

func (c *CostPrior) posterior(arm int, ref, speedup float64) (mean, std float64) {
	if ref <= 0 {
		ref = 1
	}
	mean0 := ref * priorRatio(arm, speedup)
	st := armStat{}
	if c != nil {
		st = c.arms[arm]
	}
	n := st.n
	mean = (armPriorN*mean0 + n*st.mean) / (armPriorN + n)
	priorVar := (armPriorStd * mean0) * (armPriorStd * mean0)
	std = math.Sqrt((armPriorN*priorVar + st.m2) / ((armPriorN + n) * (armPriorN + n)))
	return mean, std
}

// pick minimises a Thompson draw once any arm has a sample. With no
// samples it minimises the prior mean, so a cold model does not open at
// the cap. Equal draws keep the smaller arm.
func (c *CostPrior) pick(arms []int, ref, speedup float64) int {
	if c == nil || len(arms) == 0 {
		return 1
	}
	var rng *rand.Rand
	if c.anySample() {
		c.step++
		rng = rand.New(rand.NewPCG(1, c.step))
	}
	best := arms[0]
	bestV := math.MaxFloat64
	for _, a := range arms {
		if a < 1 {
			continue
		}
		mean, std := c.posterior(a, ref, speedup)
		v := mean
		if rng != nil && std > 0 {
			v = mean + std*rng.NormFloat64()
			if v < 0 {
				v = 0
			}
		}
		if v < bestV {
			bestV = v
			best = a
		}
	}
	if best < 1 {
		best = 1
	}
	c.Chosen = best
	return best
}

// tooSmall reports that parallel startup is not worth paying. Before a
// serial rate exists, fewer than tinyTxCold transactions is the stand-in
// (the 27-transaction block whose best fixed C is 1). Afterwards the
// comparison is learned: gas * serialRate < 2 * startupNs.
func (c *CostPrior) tooSmall(nTx int, gas float64) bool {
	if c != nil && c.serialRate > 0 && gas > 0 {
		startup := c.startupNs
		if startup < 0 {
			startup = 0
		}
		return gas*c.serialRate < 2*startup
	}
	return nTx > 0 && nTx < tinyTxCold
}

// ObserveArm records one body's wall per gas on the arm that was chosen
// for that body. gas below minArmGas is ignored. A C=1 sample replaces
// the serial rate. A wider sample updates the startup estimate from the
// wall that the serial rate does not explain, capped at half the wall so
// one slow block cannot swallow the threshold.
func (c *CostPrior) ObserveArm(arm int, wallNs, gas uint64) {
	if c == nil || arm < 1 || wallNs == 0 || gas < minArmGas {
		return
	}
	if c.arms == nil {
		c.arms = map[int]armStat{}
	}
	rate := float64(wallNs) / float64(gas)
	st := c.arms[arm]
	st.n++
	d := rate - st.mean
	st.mean += d / st.n
	st.m2 += d * (rate - st.mean)
	c.arms[arm] = st
	if arm == 1 {
		c.serialRate = st.mean
	}
	if arm > 1 && c.serialRate > 0 {
		excess := float64(wallNs) - c.serialRate*float64(gas)
		if excess < 0 {
			excess = 0
		}
		if excess > float64(wallNs)*0.5 {
			excess = float64(wallNs) * 0.5
		}
		if excess > 0 && (c.startupMin == 0 || excess < c.startupMin) {
			c.startupMin = excess
		}
		if c.startupNs <= 0 {
			c.startupNs = excess
		} else {
			c.startupNs = c.startupNs*0.8 + excess*0.2
		}
		if c.startupMin > 0 && c.startupNs < c.startupMin {
			c.startupNs = c.startupMin
		}
	}
	c.Chosen = arm
}

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
// begin picks one arm and that arm is Best for the whole block. observe
// only shrinks: smoothed frontier below the active count, the last two
// transactions, an abort storm, or sustained idle. None of those rewrite
// Best or attribute the body to a different arm. A block whose opening
// arm is 1 never grows, so the scheduler may take the solo path.
type crew struct {
	cost      *CostPrior
	limit     int
	n         int
	txs       []*types.Transaction
	prev      []int
	senders   []common.Address
	weight    []float64
	active    int
	bestC     int
	bodyGas   uint64
	frontier  int
	widthE    float64
	widthC    int
	liveWidth int
	finals    int
	limitSum  float64
	tail      bool
	noClimb   bool
	rewarded  bool
	rolls     uint64
	execs     uint64
	trace     []int

	clock    func() time.Time
	bodyAt   time.Time
	planUnit string
	planPred int64
	planText string

	writers map[rfstate.Key]int
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
		weight: make([]float64, n),
	}
	if env != nil {
		c.txs = env.Txs
		c.prev = env.PrevSame
		c.senders = env.Senders
	}
	return c
}

func (c *crew) setClock(fn func() time.Time) { c.clock = fn }

// begin seeds weights, picks the arm, and returns it. width is the
// opening structural frontier. The arm set is armGrid(limit) only: powers
// of two up to the cap, plus the cap. A frontier of 25 must not become an
// arm. The chosen arm is then clamped down to the greatest grid rung that
// does not exceed width, so a single-sender chain still runs at 1.
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
	gasEst := c.workEstimate()
	cp, work := c.path(0)
	speedup := 1.0
	if cp > 0 && work > cp {
		speedup = work / cp
	}
	if speedup > float64(c.limit) {
		speedup = float64(c.limit)
	}
	ref := 1.0
	if c.cost != nil {
		ref = c.cost.refRate()
	}
	grid := armGrid(c.limit)
	picked := 1
	if c.cost != nil && !c.cost.tooSmall(c.n, gasEst) {
		picked = c.cost.pick(grid, ref, speedup)
	}
	if picked > c.widthC {
		picked = gridFloor(grid, c.widthC)
	}
	if picked < 1 {
		picked = 1
	}
	if c.cost != nil {
		c.cost.Chosen = picked
	}
	c.active = picked
	c.bestC = picked
	c.trace = []int{picked}
	c.snapshotPlan(picked, ref, speedup, gasEst)
	return picked
}

// gridFloor is the greatest arm in grid that is still <= width.
func gridFloor(grid []int, width int) int {
	best := 1
	for _, a := range grid {
		if a <= width && a >= best {
			best = a
		}
	}
	return best
}

func (c *crew) snapshotPlan(picked int, ref, speedup, gasEst float64) {
	arms := armGrid(c.limit)
	unit := "gas"
	if c.cost != nil && (c.cost.serialRate > 0 || c.cost.anySample()) {
		unit = "ns"
	}
	var b strings.Builder
	b.WriteString(unit)
	b.WriteByte(':')
	pred := 0.0
	seen := false
	for i, a := range arms {
		mean := ref
		if c.cost != nil {
			mean, _ = c.cost.posterior(a, ref, speedup)
		}
		v := mean * gasEst
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(a))
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(int64(v), 10))
		if a == picked {
			pred = v
			seen = true
		}
	}
	if !seen && c.cost != nil {
		mean, _ := c.cost.posterior(picked, ref, speedup)
		pred = mean * gasEst
	}
	c.planUnit = unit
	c.planText = b.String()
	if unit == "ns" && pred > 0 {
		c.planPred = int64(pred)
	}
}

// startSegment opens the body clock when workers are released.
func (c *crew) startSegment() {
	if c == nil || !c.bodyAt.IsZero() {
		return
	}
	if c.clock != nil {
		c.bodyAt = c.clock()
		return
	}
	c.bodyAt = time.Now()
}

// closeSegments records the body on its arm if the tail has not already.
func (c *crew) closeSegments() {
	if c == nil {
		return
	}
	c.rewardBody()
}

func (c *crew) rewardBody() {
	if c == nil || c.rewarded {
		return
	}
	c.rewarded = true
	if c.cost == nil {
		return
	}
	var wall uint64
	if c.clock != nil && !c.bodyAt.IsZero() {
		now := c.clock()
		if d := now.Sub(c.bodyAt); d > 0 {
			wall = uint64(d)
		}
	}
	arm := c.bestC
	if arm < 1 {
		arm = 1
	}
	c.cost.ObserveArm(arm, wall, c.bodyGas)
}

// observe applies shrink-only guards. done is a successful completion.
// The returned count never exceeds the opening arm. Best is unchanged.
func (c *crew) observe(tx, frontier, width int, gas uint64, done bool, rolls uint64, idleNs int64) (int, bool) {
	if c == nil {
		return 1, false
	}
	if width < 1 {
		width = 1
	}
	if rolls > c.rolls {
		c.rolls = rolls
	}
	c.liveWidth = width
	if done {
		c.execs++
		c.finals++
		if tx >= 0 && tx < len(c.weight) && gas > 0 {
			c.weight[tx] = float64(gas)
			c.learnSel(tx, float64(gas))
		}
		if !c.tail && gas > 0 {
			c.bodyGas += gas
		}
	}
	c.frontier = frontier
	c.smooth(width)
	remaining := c.n - frontier
	if remaining < 0 {
		remaining = 0
	}
	if !c.tail && c.n > 2 && remaining <= 2 {
		c.rewardBody()
		c.tail = true
		c.noClimb = true
		return c.apply(c.clamp(width))
	}
	if c.tail {
		return c.apply(c.clamp(width))
	}
	if !c.noClimb && c.active > 1 && c.execs >= 8 && c.rolls > c.execs {
		c.noClimb = true
		return c.apply(c.half())
	}
	if !c.noClimb && c.active > 1 && c.execs >= 4 {
		if el := c.bodyElapsed(); el > 0 {
			if idleNs > 0 && float64(idleNs) > float64(el)*(float64(c.active)-1) {
				c.noClimb = true
				return c.apply(c.half())
			}
		}
	}
	if c.widthC < c.active {
		c.noClimb = true
		n := c.widthC
		if n < 1 {
			n = 1
		}
		return c.apply(n)
	}
	return c.active, false
}

func (c *crew) bodyElapsed() int64 {
	if c.clock == nil || c.bodyAt.IsZero() {
		return 0
	}
	now := c.clock()
	d := now.Sub(c.bodyAt)
	if d <= 0 {
		return 0
	}
	return d.Nanoseconds()
}

func (c *crew) half() int {
	n := c.active / 2
	if n < 1 {
		n = 1
	}
	return n
}

func (c *crew) clamp(width int) int {
	n := width
	if c.widthC < n {
		n = c.widthC
	}
	if c.active > 0 && n > c.active {
		n = c.active
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (c *crew) smooth(width int) {
	if c.widthE == 0 {
		c.widthE = float64(width)
	} else {
		c.widthE += 0.25 * (float64(width) - c.widthE)
	}
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

func (c *crew) workEstimate() float64 {
	var s float64
	for _, w := range c.weight {
		s += w
	}
	if s < 1 {
		s = 1
	}
	return s
}

func (c *crew) priorWeight(i int) float64 {
	if c.txs == nil || i < 0 || i >= len(c.txs) || c.txs[i] == nil {
		if c.cost != nil && c.cost.meanGas > 0 {
			return c.cost.meanGas
		}
		return 1
	}
	tx := c.txs[i]
	prior := 0.0
	if g := tx.Gas(); g > 0 {
		prior = float64(g)
		if c.cost != nil && c.cost.util > 0 && c.cost.util < 1 {
			prior *= c.cost.util
		}
	} else if c.cost != nil && c.cost.meanGas > 0 {
		prior = c.cost.meanGas
	} else {
		prior = 1
	}
	if c.cost != nil {
		if st, ok := c.cost.sel[selectorOf(tx)]; ok && st.n > 0 {
			base := prior
			if base <= 0 {
				base = st.gas
			}
			return (selShrink*base + st.n*st.gas) / (selShrink + st.n)
		}
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
	if c.cost == nil || c.txs == nil || tx < 0 || tx >= len(c.txs) || c.txs[tx] == nil || gas <= 0 {
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
	c.cost.meanGas = ema(c.cost.meanGas, gas, 0.25)
}

func (c *crew) apply(n int) (int, bool) {
	if n < 1 {
		n = 1
	}
	if n > c.limit {
		n = c.limit
	}
	if c.active > 0 && n > c.active {
		n = c.active
	}
	if n == c.active {
		return c.active, false
	}
	c.active = n
	if len(c.trace) == 0 || c.trace[len(c.trace)-1] != c.active {
		c.trace = append(c.trace, c.active)
	}
	return c.active, true
}

func (c *crew) Active() int { return c.active }

// Best is the arm chosen for the body. Guards and tail drain do not change it.
func (c *crew) Best() int {
	if c.bestC > 0 {
		return c.bestC
	}
	if c.active > 0 {
		return c.active
	}
	return 1
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

// Curve is the opening plan over the arm grid, captured before this
// block's body is folded in. model_pred_ns is the pair for the chosen
// arm when the unit is nanoseconds, and 0 when nothing has been measured.
func (c *crew) Curve() (unit string, chosen int, pred int64, text string) {
	unit = c.planUnit
	if unit == "" {
		unit = "gas"
	}
	return unit, c.Best(), c.planPred, c.planText
}
