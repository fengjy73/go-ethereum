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

func independentPrev(n int) []int {
	prev := make([]int, n)
	for i := range prev {
		prev[i] = -1
	}
	return prev
}

// preferArm builds a model whose posterior mean ranks arm ahead of C=1,
// and whose startup threshold does not classify a normal block as tiny.
func preferArm(arm int) *CostPrior {
	cost := NewCostPrior()
	gas := uint64(1_000_000)
	for i := 0; i < 8; i++ {
		cost.ObserveArm(1, 8*gas, gas)
		if arm > 1 {
			cost.ObserveArm(arm, 2*gas, gas)
		}
	}
	cost.startupNs = 1
	return cost
}

func TestArmGrid(t *testing.T) {
	cases := []struct {
		cap  int
		want string
	}{
		{1, "1"},
		{4, "1,2,4"},
		{6, "1,2,4,6"},
		{12, "1,2,4,8,12"},
		{32, "1,2,4,8,16,32"},
	}
	for _, tc := range cases {
		got := ""
		for i, a := range armGrid(tc.cap) {
			if i > 0 {
				got += ","
			}
			got += itoa(a)
		}
		if got != tc.want {
			t.Fatalf("cap %d grid %s, want %s", tc.cap, got, tc.want)
		}
	}
}

func itoa(n int) string {
	return strconvItoa(n)
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestColdPriorDoesNotOpenAtCap(t *testing.T) {
	c := newCrew(NewCostPrior(), 32, chainEnv(64, independentPrev(64), 100000))
	if got := c.begin(32); got != 1 {
		t.Fatalf("cold opening %d, want 1 (no Thompson noise on an empty model)", got)
	}
	ref := c.cost.refRate()
	for _, arm := range []int{2, 4, 8, 16, 32} {
		mean, _ := c.cost.posterior(arm, ref, 32)
		if mean <= ref {
			t.Fatalf("untried arm %d mean %v is not above ref %v", arm, mean, ref)
		}
	}
	// The same pre-block model always draws the same arm. A clone does too.
	other := newCrew(c.cost.Clone(), 32, chainEnv(64, independentPrev(64), 100000))
	if got := other.begin(32); got != 1 {
		t.Fatalf("clone opening %d", got)
	}
}

func TestModestSpeedupAdoptsFour(t *testing.T) {
	// About 1.4x, which is what fixed C=4 actually delivers on these
	// blocks. A 1.5x prior cannot adopt that in one sample.
	cost := NewCostPrior()
	cost.ObserveArm(1, 8_000_000, 1_000_000)
	cost.ObserveArm(4, 5_714_286, 1_000_000) // ~8e6/1.4
	ref := cost.refRate()
	m1, _ := cost.posterior(1, ref, 4)
	m4, _ := cost.posterior(4, ref, 4)
	if m4 >= m1 {
		t.Fatalf("C=4 posterior %v is not below C=1 %v after a 1.4x sample", m4, m1)
	}
	env := chainEnv(64, independentPrev(64), 100000)
	c := newCrew(cost, 4, env)
	// Stack the same evidence so the draw is stable, then ask begin.
	for i := 0; i < 4; i++ {
		cost.ObserveArm(4, 5_714_286, 1_000_000)
	}
	if got := c.begin(4); got != 4 {
		t.Fatalf("opening %d after repeated 1.4x samples", got)
	}
}

func TestOneFastSampleBeatsSerial(t *testing.T) {
	cost := NewCostPrior()
	cost.ObserveArm(1, 8_000_000, 1_000_000)
	cost.ObserveArm(4, 2_000_000, 1_000_000)
	ref := cost.refRate()
	m1, _ := cost.posterior(1, ref, 4)
	m4, _ := cost.posterior(4, ref, 4)
	if m4 >= m1 {
		t.Fatalf("C=4 posterior %v is not below C=1 %v (ref %v)", m4, m1, ref)
	}
}

func TestMeasuredArmWins(t *testing.T) {
	env := chainEnv(64, independentPrev(64), 100000)
	c := newCrew(preferArm(4), 4, env)
	if got := c.begin(4); got != 4 {
		t.Fatalf("opening %d, want 4", got)
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestWideUntriedArmIsRare(t *testing.T) {
	cost := NewCostPrior()
	cost.ObserveArm(1, 8_000_000, 1_000_000)
	arms := armGrid(32)
	n32 := 0
	n2 := 0
	const draws = 200
	for i := 0; i < draws; i++ {
		switch cost.pick(arms, cost.refRate(), 32) {
		case 32:
			n32++
		case 2:
			n2++
		}
	}
	if n32 > draws/10 {
		t.Fatalf("untried C=32 drawn %d/%d times", n32, draws)
	}
	if n2 == 0 {
		t.Fatalf("untried C=2 was never drawn in %d tries", draws)
	}
}

func TestChainPrefersOne(t *testing.T) {
	n := 64
	prev := make([]int, n)
	prev[0] = -1
	for i := 1; i < n; i++ {
		prev[i] = i - 1
	}
	cost := NewCostPrior()
	cost.serialRate = 5
	cost.startupNs = 1
	c := newCrew(cost, 8, chainEnv(n, prev, 100000))
	if got := c.begin(8); got != 1 {
		t.Fatalf("chain choice %d", got)
	}
}

func TestTinyBlockRunsOne(t *testing.T) {
	cold := newCrew(NewCostPrior(), 4, chainEnv(27, independentPrev(27), 21000))
	if got := cold.begin(4); got != 1 {
		t.Fatalf("cold tiny opening %d", got)
	}
	cost := NewCostPrior()
	cost.serialRate = 10
	cost.startupNs = 5_000_000
	// 4 * 21000 * 10 = 840000 < 2*5e6, so the learned threshold still says solo.
	learned := newCrew(cost, 4, chainEnv(4, independentPrev(4), 21000))
	if got := learned.begin(4); got != 1 {
		t.Fatalf("learned tiny opening %d", got)
	}
	cost.startupNs = 1
	big := newCrew(cost.Clone(), 4, chainEnv(64, independentPrev(64), 100000))
	// No arm samples other than the serial rate living on the field, and
	// anySample is false, so this is still the prior mean: C=1. The point
	// is that it is not the tiny rule. A measured C=4 then wins.
	if got := big.begin(4); got != 1 {
		t.Fatalf("unsampled large block %d", got)
	}
	armed := newCrew(preferArm(4), 4, chainEnv(64, independentPrev(64), 100000))
	if got := armed.begin(4); got != 4 {
		t.Fatalf("large measured block %d", got)
	}
}

func TestModelWidthDoesNotFlap(t *testing.T) {
	c := newCrew(preferArm(4), 4, chainEnv(64, independentPrev(64), 1000))
	c.begin(2)
	flips := 0
	prev := c.widthC
	for i := 0; i < 20; i++ {
		w := 1
		if i%2 == 0 {
			w = 2
		}
		c.observe(-1, 0, w, 0, false, 0, 0)
		if c.widthC != prev {
			flips++
			prev = c.widthC
		}
	}
	if flips > 2 {
		t.Fatalf("width cap flipped %d times on a 1/2 alternation", flips)
	}
}

func TestTailKeepsBestAndExcludesTail(t *testing.T) {
	env := chainEnv(8, independentPrev(8), 100000)
	cost := preferArm(4)
	n4 := cost.arms[4].n
	n1 := cost.arms[1].n
	c := newCrew(cost, 4, env)
	clk := &fakeClock{t: time.Unix(0, 0)}
	c.setClock(clk.now)
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	c.startSegment()
	clk.t = clk.t.Add(2 * time.Millisecond)
	// Crossing into the last two transactions closes the body first.
	if n, _ := c.observe(0, 6, 1, 100000, true, 0, 0); n != 1 {
		t.Fatalf("tail active %d", n)
	}
	if c.Best() != 4 {
		t.Fatalf("tail rewrote best to %d trace %s", c.Best(), c.Trace())
	}
	body := c.bodyGas
	clk.t = clk.t.Add(20 * time.Millisecond)
	if _, ch := c.observe(1, 7, 1, 100000, true, 0, 0); c.Best() != 4 {
		t.Fatalf("later tail best %d changed %v", c.Best(), ch)
	}
	c.closeSegments()
	if c.bodyGas != body {
		t.Fatalf("tail gas joined the body, gas %d body %d", c.bodyGas, body)
	}
	if cost.arms[4].n != n4+1 {
		t.Fatalf("arm 4 samples %v, want %v", cost.arms[4].n, n4+1)
	}
	if cost.arms[1].n != n1 {
		t.Fatalf("shrink sampled C=1, n %v want %v", cost.arms[1].n, n1)
	}
}

func TestShrinkDoesNotRetargetReward(t *testing.T) {
	cost := preferArm(4)
	n4 := cost.arms[4].n
	n1 := cost.arms[1].n
	c := newCrew(cost, 4, chainEnv(64, independentPrev(64), 100000))
	clk := &fakeClock{t: time.Unix(0, 0)}
	c.setClock(clk.now)
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	c.startSegment()
	clk.t = clk.t.Add(time.Millisecond)
	// A single narrow sample does not move the smoothed cap by a worker.
	if n, ch := c.observe(0, 0, 1, 80000, true, 0, 0); ch || n != 4 {
		t.Fatalf("first narrow sample changed active to %d", n)
	}
	for i := 0; i < 12; i++ {
		c.observe(-1, 0, 1, 0, false, 0, 0)
	}
	if c.Active() >= 4 {
		t.Fatalf("smoothed width did not shrink, active %d widthC %d", c.Active(), c.widthC)
	}
	if c.Best() != 4 {
		t.Fatalf("shrink rewrote best %d", c.Best())
	}
	clk.t = clk.t.Add(3 * time.Millisecond)
	// Width recovering must not climb back.
	for i := 0; i < 20; i++ {
		c.observe(-1, 0, 4, 0, false, 0, 0)
	}
	if c.Active() > c.Best() || c.Active() >= 4 {
		t.Fatalf("climbed after shrink, active %d", c.Active())
	}
	c.closeSegments()
	if cost.arms[4].n != n4+1 {
		t.Fatalf("body was not charged to arm 4, samples %v want %v", cost.arms[4].n, n4+1)
	}
	if cost.arms[1].n != n1 {
		t.Fatalf("shrunk count took a sample, n %v", cost.arms[1].n)
	}
}

func TestAbortStormHalvesAndStays(t *testing.T) {
	c := newCrew(preferArm(4), 4, chainEnv(64, independentPrev(64), 100000))
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	for i := 0; i < 8; i++ {
		rolls := uint64(i)
		if i == 7 {
			rolls = 9
		}
		c.observe(i, i, 4, 1000, true, rolls, 0)
	}
	if c.Active() != 2 {
		t.Fatalf("abort storm active %d, want 2", c.Active())
	}
	if c.Best() != 4 {
		t.Fatalf("storm rewrote best %d", c.Best())
	}
	if _, ch := c.observe(8, 8, 4, 1000, true, 20, 0); ch || c.Active() != 2 {
		t.Fatalf("climbed after the storm, active %d changed %v", c.Active(), ch)
	}
}

func TestIdleHalves(t *testing.T) {
	c := newCrew(preferArm(4), 4, chainEnv(64, independentPrev(64), 100000))
	clk := &fakeClock{t: time.Unix(0, 0)}
	c.setClock(clk.now)
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	c.startSegment()
	clk.t = clk.t.Add(time.Millisecond)
	for i := 0; i < 3; i++ {
		c.observe(i, i, 4, 1000, true, 0, 0)
	}
	// elapsed is 1ms, active-1 is 3, so idle above 3ms halves.
	clk.t = clk.t.Add(time.Millisecond)
	n, ch := c.observe(3, 3, 4, 1000, true, 0, 10_000_000)
	if !ch || n != 2 {
		t.Fatalf("idle active %d changed %v", n, ch)
	}
	if c.Best() != 4 {
		t.Fatalf("idle rewrote best %d", c.Best())
	}
}

func TestOpeningCurveMatchesPred(t *testing.T) {
	c := newCrew(preferArm(4), 4, chainEnv(64, independentPrev(64), 100000))
	if got := c.begin(4); got != 4 {
		t.Fatalf("start %d", got)
	}
	unit, chosen, pred, text := c.Curve()
	if unit != "ns" || chosen != 4 || pred <= 0 {
		t.Fatalf("curve %s unit %s chosen %d pred %d", text, unit, chosen, pred)
	}
	want := "4=" + strconvItoa(int(pred))
	if !contains(text, want) {
		t.Fatalf("pred %d not on curve %s", pred, text)
	}
	// Folding the body must not change the captured plan.
	c.closeSegments()
	_, _, pred2, text2 := c.Curve()
	if pred2 != pred || text2 != text {
		t.Fatalf("curve changed after the sample, %d %s", pred2, text2)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestHotContractLengthensCriticalPath(t *testing.T) {
	prev := independentPrev(4)
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
	if cpHot < workHot*0.5 || cpHot > workHot*0.9 {
		t.Fatalf("hot cp %v, work %v", cpHot, workHot)
	}
}

func TestInBlockRAWChainsContract(t *testing.T) {
	env := chainEnv(6, independentPrev(6), 100000)
	env.Senders = []common.Address{{1}, {2}, {3}, {4}, {5}, {6}}
	c := newCrew(NewCostPrior(), 4, env)
	c.seedWeights()
	slot := rfstate.SlotKeyOf(common.Address{}, common.Hash{9})
	c.noteIO(0, nil, []rfstate.Key{slot})
	cp0, _ := c.path(0)
	c.noteIO(1, []rfstate.Key{slot}, []rfstate.Key{slot})
	cpOne, _ := c.path(0)
	if cpOne > cp0*1.5 {
		t.Fatalf("one RAW chained the contract, cp %v before %v", cpOne, cp0)
	}
	for i := 2; i < 5; i++ {
		c.noteIO(i, []rfstate.Key{slot}, []rfstate.Key{slot})
	}
	cp, work := c.path(0)
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
	if after < 70000 || after > 90000 {
		t.Fatalf("shrunk weight %v, before %v", after, before)
	}
	if math.Abs(after-10000) < 1 {
		t.Fatalf("selector observation replaced the gas limit")
	}
}

type fakeClock struct {
	t   time.Time
	cpu int64
}

func (f *fakeClock) now() (time.Time, int64) { return f.t, f.cpu }
