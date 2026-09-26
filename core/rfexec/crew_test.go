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
	"math"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
)

func chainEnv(n int, prev []int, gas uint64) *BlockEnv {
	txs := make([]*types.Transaction, n)
	for i := range txs {
		txs[i] = types.NewTx(&types.LegacyTx{Gas: gas, To: new(common.Address)})
	}
	return &BlockEnv{Txs: txs, PrevSame: prev}
}

func TestModelParallelPrefersWidth(t *testing.T) {
	prev := []int{-1, -1, -1, -1}
	c := newCrew(NewCostPrior(), 4, chainEnv(4, prev, 100000))
	if got := c.begin(4); got != 4 {
		t.Fatalf("cold parallel choice %d trace %s", got, c.Trace())
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestModelChainPrefersOne(t *testing.T) {
	// One sender: each transaction waits on the previous, so CP equals work.
	prev := []int{-1, 0, 1, 2}
	c := newCrew(NewCostPrior(), 4, chainEnv(4, prev, 100000))
	if got := c.begin(4); got != 1 {
		t.Fatalf("chain choice %d", got)
	}
}

func TestModelRatePrefersCheaper(t *testing.T) {
	prev := []int{-1, -1, -1, -1}
	cost := NewCostPrior()
	// Every feasible C has a sample. C=4's CPU inflation makes it slower
	// than one worker even after the work is split.
	cost.base = 1
	cost.refC = 1
	cost.infl[1] = 1
	cost.infl[2] = 4
	cost.infl[3] = 6
	cost.infl[4] = 8
	cost.samples[1] = 4
	cost.samples[2] = 4
	cost.samples[3] = 4
	cost.samples[4] = 4
	c := newCrew(cost, 4, chainEnv(4, prev, 100000))
	if got := c.begin(4); got != 1 {
		t.Fatalf("inflated C=4 chosen %d infl %v", got, cost.infl)
	}
}

func TestModelWidthDoesNotFlap(t *testing.T) {
	c := newCrew(NewCostPrior(), 4, chainEnv(8, []int{-1, -1, -1, -1, -1, -1, -1, -1}, 1000))
	c.begin(2)
	flips := 0
	prev := c.widthC
	for i := 0; i < 20; i++ {
		w := 1
		if i%2 == 0 {
			w = 2
		}
		c.observe(-1, 0, w, 0, false, 0)
		if c.widthC != prev {
			flips++
			prev = c.widthC
		}
	}
	if flips > 2 {
		t.Fatalf("width cap flipped %d times on a 1/2 alternation", flips)
	}
}

func TestModelTailKeepsBest(t *testing.T) {
	prev := []int{-1, -1, -1, -1, -1, -1, -1, -1}
	c := newCrew(NewCostPrior(), 4, chainEnv(8, prev, 100000))
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	// Two transactions left: tail may shrink the active count.
	if n, _ := c.observe(0, 6, 1, 100000, true, 0); n != 1 {
		t.Fatalf("tail active %d", n)
	}
	if c.Best() != 4 {
		t.Fatalf("tail rewrote best to %d trace %s", c.Best(), c.Trace())
	}
	if _, ch := c.observe(1, 7, 1, 100000, true, 0); c.Best() != 4 {
		t.Fatalf("later tail best %d changed %v", c.Best(), ch)
	}
}

func TestObserveBlockDerivesInflation(t *testing.T) {
	c := NewCostPrior()
	// CPU is the wall for this helper. infl(C) is CPU-per-gas over the C=1 baseline.
	// Gas is at the saturation floor. A shorter segment must not move inflation.
	c.ObserveBlock(1, 5_000_000, 500_000, 4, 0, 0, 0)
	c.ObserveBlock(4, 10_000_000, 500_000, 4, 0, 0, 0)
	if math.Abs(c.inflation(4)-2) > 0.01 {
		t.Fatalf("infl(4)=%v, want 2", c.inflation(4))
	}
	if math.Abs(c.inflation(1)-1) > 0.01 {
		t.Fatalf("infl(1)=%v", c.inflation(1))
	}
	if c.rate[4] <= 0 {
		t.Fatalf("rate(4) was not recorded")
	}
}

func TestTailSegmentDoesNotMoveRate(t *testing.T) {
	c := NewCostPrior()
	c.ObserveSegments([]Segment{{
		C: 1, WallNs: 2_000_000, CPUNs: 2_000_000, Gas: 1_000_000, Execs: 4, Width: 1,
	}})
	body := c.rate[1]
	if body <= 0 {
		t.Fatalf("body rate %v", body)
	}
	c.ObserveSegments([]Segment{{
		C: 1, WallNs: 50_000_000, CPUNs: 50_000_000, Gas: 1_000_000, Execs: 2, Width: 1, Tail: true,
	}})
	if c.rate[1] != body {
		t.Fatalf("tail moved rate(1) from %v to %v", body, c.rate[1])
	}
}

func TestAbandonedProbeUpdatesItsOwnC(t *testing.T) {
	cost := NewCostPrior()
	cost.ObserveSegments([]Segment{{
		C: 1, WallNs: 2_000_000, CPUNs: 2_000_000, Gas: 1_000_000, Execs: 4, Width: 1,
	}})
	// Four independent transactions, cap 4. n/2 is the first checkpoint.
	// C=4's structural split is 4x, so an inflation above ~4.2 loses to C=1
	// by more than the 5% stickiness and the probe is abandoned.
	prev := []int{-1, -1, -1, -1}
	env := chainEnv(4, prev, 100000)
	c := newCrew(cost, 4, env)
	clk := &fakeClock{t: time.Unix(0, 0)}
	c.setClock(clk.now)
	if got := c.begin(4); got <= 1 {
		t.Fatalf("opening %d, want a wider explore than the measured C=1", got)
	}
	probed := c.Active()
	c.startSegment()
	clk.t = clk.t.Add(3 * time.Millisecond)
	clk.cpu += 3_000_000
	if _, ch := c.observe(0, 0, 4, 250000, true, 0); ch {
		t.Fatalf("first completion changed C before the checkpoint, trace %s", c.Trace())
	}
	clk.t = clk.t.Add(3 * time.Millisecond)
	clk.cpu += 3_000_000
	if _, _ = c.observe(1, 1, 4, 250000, true, 0); c.Active() == probed {
		t.Fatalf("probe C=%d was not abandoned after its segment, trace %s infl %v", probed, c.Trace(), cost.infl)
	}
	if cost.rate[probed] <= 0 || cost.samples[probed] < 1 {
		t.Fatalf("abandoned C=%d rate %v samples %v", probed, cost.rate[probed], cost.samples[probed])
	}
	// The same probe is not the opening choice of the next block.
	c2 := newCrew(cost, 4, env)
	if got := c2.begin(4); got == probed && cost.inflation(probed) >= 4 {
		t.Fatalf("retried inflated C=%d", got)
	}
}

func TestExploreNotStuckAtOne(t *testing.T) {
	cost := NewCostPrior()
	cost.base = 5
	cost.refC = 1
	cost.samples[1] = 2
	cost.rate[1] = 5
	prev := []int{-1, -1, -1, -1, -1, -1, -1, -1}
	c := newCrew(cost, 8, chainEnv(8, prev, 100000))
	got := c.begin(8)
	if got <= 1 {
		t.Fatalf("parallel cap 8 stayed at %d after only C=1 was measured", got)
	}
}

func TestTerribleInflationIsNotRepicked(t *testing.T) {
	cost := NewCostPrior()
	cost.base = 1
	cost.refC = 1
	cost.infl[4] = 8
	cost.samples[1] = 3
	cost.samples[4] = 3
	cost.rate[1] = 1
	cost.rate[4] = 8
	prev := []int{-1, -1, -1, -1}
	for i := 0; i < 3; i++ {
		c := newCrew(cost, 4, chainEnv(4, prev, 100000))
		if got := c.begin(4); got == 4 {
			t.Fatalf("block %d repicked C=4", i)
		}
	}
}

func TestHotContractLengthensCriticalPath(t *testing.T) {
	prev := []int{-1, -1, -1, -1}
	env := chainEnv(4, prev, 100000)
	cold := newCrew(NewCostPrior(), 4, env)
	cold.seedWeights()
	cpCold, work := cold.path(0)
	cost := NewCostPrior()
	cost.hot[common.Address{}] = hotStat{conflicts: 8, seen: 8}
	hot := newCrew(cost, 4, env)
	hot.seedWeights()
	cpHot, workHot := hot.path(0)
	if cpCold > work/2 {
		t.Fatalf("cold cp %v is already a chain (work %v)", cpCold, work)
	}
	// p = 8/(8+8) = 0.5, so the soft chain is about half the work, not all of it.
	if cpHot < workHot*0.5 || cpHot > workHot*0.9 {
		t.Fatalf("hot cp %v, work %v", cpHot, workHot)
	}
}

func TestInBlockRAWChainsContract(t *testing.T) {
	env := chainEnv(6, []int{-1, -1, -1, -1, -1, -1}, 100000)
	env.Senders = []common.Address{{1}, {2}, {3}, {4}, {5}, {6}}
	c := newCrew(NewCostPrior(), 4, env)
	c.seedWeights()
	slot := rfstate.SlotKeyOf(common.Address{}, common.Hash{9})
	c.noteIO(0, nil, []rfstate.Key{slot})
	cp0, _ := c.path(0)
	// One cross-sender edge is not enough: the prior shrinks toward no conflict.
	c.noteIO(1, []rfstate.Key{slot}, []rfstate.Key{slot})
	cpOne, _ := c.path(0)
	if cpOne > cp0*1.5 {
		t.Fatalf("one RAW chained the contract, cp %v before %v", cpOne, cp0)
	}
	for i := 2; i < 5; i++ {
		c.noteIO(i, []rfstate.Key{slot}, []rfstate.Key{slot})
	}
	cp, work := c.path(0)
	// Four conflicts on five observations, shrunk by 8, is a partial chain.
	if cp < cp0*2 || cp > work*0.9 {
		t.Fatalf("after repeated RAW, cp %v work %v (before %v)", cp, work, cp0)
	}
	same := newCrew(NewCostPrior(), 4, env)
	same.senders = []common.Address{{1}, {1}, {1}, {1}, {1}, {1}}
	same.seedWeights()
	for i := 0; i < 5; i++ {
		var reads []rfstate.Key
		if i > 0 {
			reads = []rfstate.Key{slot}
		}
		same.noteIO(i, reads, []rfstate.Key{slot})
	}
	cpSame, workSame := same.path(0)
	if cpSame > workSame/2 {
		t.Fatalf("same-sender RAW chained the contract, cp %v work %v", cpSame, workSame)
	}
}

func TestSelectorShrinksTowardLimit(t *testing.T) {
	c := newCrew(NewCostPrior(), 1, chainEnv(1, []int{-1}, 100000))
	c.seedWeights()
	before := c.priorWeight(0)
	c.learnSel(0, 10000)
	after := c.priorWeight(0)
	// (4*100000 + 10000) / 5 = 82000. One observation must not become 10000.
	if after < 70000 || after > 90000 {
		t.Fatalf("shrunk weight %v, before %v", after, before)
	}
	if math.Abs(after-10000) < 1 {
		t.Fatalf("selector observation replaced the gas limit")
	}
}

func TestFixedCostOnTinyPrediction(t *testing.T) {
	cost := NewCostPrior()
	cost.base = 10
	cost.refC = 1
	cost.fixedNs = 5_000_000
	cost.samples[1] = 2
	prev := []int{-1, 0, 1, 2}
	c := newCrew(cost, 4, chainEnv(4, prev, 21000))
	if got := c.begin(4); got != 1 {
		t.Fatalf("chain choice %d", got)
	}
	_, chosen, pred, text := c.Curve()
	if chosen != 1 || pred < 5_000_000 {
		t.Fatalf("pred %d curve %s", pred, text)
	}
	// The gas term alone is about 4*21000*10. The fixed term has to be visible.
	if pred < int64(cost.fixedNs)+800_000 {
		t.Fatalf("pred %d did not include both fixedNs and gas", pred)
	}
}

type fakeClock struct {
	t   time.Time
	cpu int64
}

func (f *fakeClock) now() (time.Time, int64) { return f.t, f.cpu }
