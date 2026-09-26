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

	"github.com/ethereum/go-ethereum/common"
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
	// Every feasible C has a sample, so this block does not probe.
	// C=4 has been much slower per gas-equivalent than C=1.
	cost.rate[1] = 1
	cost.rate[2] = 4
	cost.rate[3] = 6
	cost.rate[4] = 8
	cost.samples[1] = 1
	cost.samples[2] = 1
	cost.samples[3] = 1
	cost.samples[4] = 1
	cost.deriveInfl()
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

func TestModelProbeThenSettle(t *testing.T) {
	prev := []int{-1, -1, -1, -1}
	env := chainEnv(4, prev, 100000)
	cost := NewCostPrior()
	c := newCrew(cost, 4, env)
	c.begin(4)
	// Record a wall sample as if C=4 took 4x the ideal time of one worker.
	cost.ObserveBlock(c.Best(), 4000, 400000, 4, 0, c.BodyGas(), 0)
	c2 := newCrew(cost, 4, env)
	got := c2.begin(4)
	if got != 2 {
		t.Fatalf("expected a downward probe, got %d", got)
	}
	cost.ObserveBlock(c2.Best(), 2000, 400000, 4, 0, c2.BodyGas(), 0)
	c3 := newCrew(cost, 4, env)
	got = c3.begin(4)
	if got != 1 {
		t.Fatalf("expected probe to 1, got %d", got)
	}
	cost.ObserveBlock(1, 1000, 400000, 4, 0, c3.BodyGas(), 0)
	c4 := newCrew(cost, 4, env)
	got = c4.begin(4)
	// C=1 wall was 1000 for the whole work. C=2 was 2000. C=4 was 4000.
	// The ideal split would favour 1 because the larger counts were slower
	// than the split predicts... rate = wall/L. L(4) is about work/4, so
	// rate[4] = 4000 / (work/4) = 16000/work. rate[1] = 1000/work.
	// T(4)=L(4)*rate[4]=4000, T(1)=L(1)*rate[1]=1000. Choose 1.
	if got != 1 {
		t.Fatalf("settled on %d, want 1", got)
	}
	if math.Abs(cost.inflation(4)-16) > 1 && cost.inflation(4) < 2 {
		t.Fatalf("infl(4)=%v, want a clear slowdown", cost.inflation(4))
	}
}

func TestObserveBlockDerivesInflation(t *testing.T) {
	c := NewCostPrior()
	c.ObserveBlock(1, 1000, 100, 1, 0, 100, 0)
	c.ObserveBlock(4, 2000, 100, 1, 0, 25, 0)
	// rate[1]=1000/100=10, rate[4]=2000/25=80, infl[4]=8.
	if math.Abs(c.inflation(4)-8) > 0.01 {
		t.Fatalf("infl(4)=%v", c.inflation(4))
	}
	if math.Abs(c.inflation(1)-1) > 0.01 {
		t.Fatalf("infl(1)=%v", c.inflation(1))
	}
}
