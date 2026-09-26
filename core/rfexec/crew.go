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
	"strconv"
	"strings"
	"time"
)

const (
	phaseMulti = iota
	phaseRefine
)

// crew chooses how many workers execute one block.
//
// The active count starts at the cross-block prior, capped by the structural
// width (ready transactions on the dependency frontier) and by the process
// limit. A cold prior of 0 starts at one. Best is that capped prior until a
// window measures something better, so a block that never closes a window
// still carries the prior. Tail drain may lower the active count, but it
// does not change Best: the next block must not start at the drained width.
//
// A window is a run of per-completion gas-per-nanosecond samples. It closes
// once there is a completion per active worker (at least two) and the
// standard error is at most half the mean, or the samples do not vary.
// Fewer completions have not seen a wave of this trial. A noisy window
// stays open until more completions shrink the error; it does not move the
// search on its own. The 4 in that sample-size rule is the count that makes
// the standard error half the mean; it is not a worker count. Two closed
// windows at the starting count form the baseline, and the bar is the
// faster of the two. A later slower slice at that same count does not lower
// the bar. The rate is gas over wall time for the whole window, not the
// mean of per-transaction rates. A probe that beats the bar by more than
// the sum of the two standard errors records Best and takes another
// multiplicative step (double, or half). A probe that is slower or only
// inside the noise returns to Best at once: noise is not a reason to spend
// the rest of the block away from the recorded best. That return switches
// to ±1 refinement. The next window at Best takes one step, and a worse
// refinement tries the other direction once. A window already at Best that
// is inside the noise does not move, unless that refinement step is still
// pending.
//
// The watchdog halves the active count on an abort storm (no completion and
// at least one abort per active worker, or more aborts than completions in
// a closed window) or on sustained idle (idle time greater than the wall
// time of every extra worker, over a closed window that still had a full
// frontier). It does not run while the width cap is holding the active
// count down. The shrink does not change Best: a storm at the tail must not
// become the next block's prior. This block does not climb back.
type crew struct {
	prior            int
	max              int
	width            int
	active           int
	bestC            int
	bestTP           float64
	baseSem          float64
	dir              int
	phase            int
	refineDir        int
	pinned           bool
	haveBase         bool
	noUp             bool
	refinePending    bool
	refineTriedOther bool

	samples    []float64
	gasSum     float64
	dtSum      float64
	bestN      int
	aborts     uint64
	dones      uint64
	idle       int64
	winStart   time.Time
	lastSample time.Time
	trace      []int
	now        func() time.Time
}

func newCrew(prior, max int) *crew {
	if max < 1 {
		max = 1
	}
	return &crew{prior: prior, max: max, now: time.Now}
}

// begin sets the starting trial from the prior and the current width.
// Best starts there too, so an unmeasured block carries the capped prior.
func (c *crew) begin(width int) int {
	c.width = width
	if c.width < 1 {
		c.width = 1
	}
	start := c.prior
	if start < 1 {
		start = 1
	}
	if start > c.limit() {
		start = c.limit()
	}
	c.active = start
	c.bestC = start
	c.dir = 0
	c.phase = phaseMulti
	c.pinned = false
	c.haveBase = false
	c.noUp = false
	c.winStart = c.now()
	c.lastSample = c.winStart
	c.trace = []int{start}
	return start
}

func (c *crew) limit() int {
	n := c.width
	if c.max < n {
		n = c.max
	}
	if n < 1 {
		return 1
	}
	return n
}

// setWidth applies a new structural cap. Shrinking below the active count
// parks the search (pinned): Best is left alone. Growing back to Best
// resumes at Best. A cap still below Best follows the cap and stays pinned.
func (c *crew) setWidth(width int) (int, bool) {
	if width < 1 {
		width = 1
	}
	c.width = width
	limit := c.limit()
	if c.pinned && c.bestC <= limit {
		c.pinned = false
		if c.active != c.bestC {
			c.active = c.bestC
			if c.active > limit {
				c.active = limit
			}
			c.resetWindow()
			c.note()
			return c.active, true
		}
		return c.active, false
	}
	if c.pinned {
		if c.active != limit {
			c.active = limit
			c.resetWindow()
			c.note()
			return c.active, true
		}
		return c.active, false
	}
	if c.active <= limit {
		return c.active, false
	}
	c.active = limit
	c.pinned = true
	c.resetWindow()
	c.note()
	return c.active, true
}

// tick folds one scheduler event into the current window.
// gas is the gas of a completion (0 on a rollback). dones is 1 when this
// event finalized a transaction. aborts and idle are deltas since the
// previous tick. While the width cap is holding the active count down,
// the event is ignored so tail drain cannot move Best.
func (c *crew) tick(gas uint64, idleNs int64, aborts, dones uint64) (int, bool) {
	if c.pinned {
		return c.active, false
	}
	if c.winStart.IsZero() {
		c.winStart = c.now()
		c.lastSample = c.winStart
	}
	now := c.now()
	if dones > 0 {
		add := gas
		if add == 0 {
			add = 1
		}
		dt := now.Sub(c.lastSample).Nanoseconds()
		if dt < 1 {
			dt = 1
		}
		c.samples = append(c.samples, float64(add)/float64(dt))
		c.gasSum += float64(add)
		c.dtSum += float64(dt)
		c.lastSample = now
		c.dones += dones
	}
	c.idle += idleNs
	c.aborts += aborts

	if c.active > 1 && c.dones == 0 && c.aborts >= uint64(c.active) {
		return c.shrinkStorm()
	}
	if !c.windowClosed() {
		return c.active, false
	}
	elapsed := now.Sub(c.winStart).Nanoseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	if c.watchdog(elapsed) {
		return c.shrinkStorm()
	}
	return c.decide()
}

func (c *crew) windowClosed() bool {
	n := len(c.samples)
	// One completion per active worker, and at least two. A shorter window
	// has not seen a full wave of the current trial, so parallel overlap is
	// invisible and a serial slice can look faster.
	needN := c.active
	if needN < 2 {
		needN = 2
	}
	if n < needN {
		return false
	}
	mean, std := c.meanStd()
	if mean <= 0 || std == 0 {
		return true
	}
	ratio := std / mean
	// n >= 4*(std/mean)^2 makes the standard error at most half the mean.
	// A noisy pair of samples must not close the window early: that margin
	// is already wider than the mean, so the next samples have to shrink it.
	need := 4 * ratio * ratio
	return float64(n)+1e-9 >= need
}

func (c *crew) meanStd() (mean, std float64) {
	n := float64(len(c.samples))
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range c.samples {
		sum += x
	}
	mean = sum / n
	if n < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range c.samples {
		d := x - mean
		ss += d * d
	}
	std = math.Sqrt(ss / (n - 1))
	return mean, std
}

func (c *crew) watchdog(elapsed int64) bool {
	if c.active <= 1 {
		return false
	}
	if c.aborts > c.dones {
		return true
	}
	// Idle of every extra worker for the whole window, and the frontier
	// still had room for this many workers (otherwise this is tail drain).
	if c.dones >= 2 && c.limit() >= c.active && c.idle > elapsed*int64(c.active-1) {
		return true
	}
	return false
}

func (c *crew) decide() (int, bool) {
	_, std := c.meanStd()
	n := float64(len(c.samples))
	sem := 0.0
	if n > 0 {
		sem = std / math.Sqrt(n)
	}
	tp := 0.0
	if c.dtSum > 0 {
		tp = c.gasSum / c.dtSum
	}
	c.resetWindow()
	if !c.haveBase {
		// A shrink before the baseline leaves the prior in place.
		if c.active != c.bestC {
			return c.active, false
		}
		// Two windows before the first probe, so the bar is not a single
		// heavy slice of the block. The bar is the faster of the two.
		c.bestN++
		if tp > c.bestTP {
			c.bestTP = tp
			c.baseSem = sem
		}
		if c.bestN < 2 {
			return c.active, false
		}
		c.haveBase = true
		c.dir = c.probeDir()
		return c.stepMulti()
	}
	if c.active == c.bestC {
		// Keep the faster slice. A later slow slice must not lower the bar
		// and let a smaller trial look like a win.
		if tp > c.bestTP {
			c.bestTP = tp
			c.baseSem = sem
		}
		if c.refinePending {
			return c.stepRefine()
		}
		return c.active, false
	}
	thresh := sem + c.baseSem
	if tp > c.bestTP+thresh {
		c.bestTP = tp
		c.bestC = c.active
		c.baseSem = sem
		c.bestN = 1
		if c.phase == phaseRefine {
			return c.stepRefine()
		}
		return c.stepMulti()
	}
	// Not a clear improvement: leave the probe. Noise is not a reason to
	// run the rest of the block away from the recorded best.
	return c.onWorse()
}

func (c *crew) probeDir() int {
	if c.noUp {
		return 0
	}
	// Step up whenever the cap still has room. stepMulti clamps a double
	// that would pass the cap, so a prior of 3 with room for 4 probes 4
	// rather than halving.
	if c.active < c.limit() {
		return 1
	}
	if c.active > 1 {
		return -1
	}
	return 0
}

func (c *crew) stepMulti() (int, bool) {
	if c.dir == 0 || (c.noUp && c.dir > 0) {
		return c.active, false
	}
	var next int
	if c.dir > 0 {
		next = c.active * 2
		if next <= c.active {
			next = c.active + 1
		}
	} else {
		next = c.active / 2
		if next < 1 || next == c.active {
			next = c.active - 1
		}
	}
	if next < 1 {
		next = 1
	}
	if next > c.limit() {
		next = c.limit()
	}
	if next == c.active {
		c.dir = 0
		return c.active, false
	}
	c.active = next
	c.note()
	return c.active, true
}

func (c *crew) onWorse() (int, bool) {
	changed := false
	probedUp := c.active > c.bestC
	if c.active != c.bestC {
		c.active = c.bestC
		if c.active > c.limit() {
			c.active = c.limit()
			c.bestC = c.active
		}
		changed = true
		c.note()
	}
	if c.phase == phaseMulti {
		c.phase = phaseRefine
		if probedUp || c.bestC <= 1 {
			c.refineDir = 1
		} else {
			c.refineDir = -1
		}
		c.refinePending = true
		c.refineTriedOther = false
		return c.active, changed
	}
	if !c.refineTriedOther {
		c.refineDir = -c.refineDir
		if c.refineDir == 0 {
			c.refineDir = -1
		}
		c.refinePending = true
		c.refineTriedOther = true
		return c.active, changed
	}
	c.refinePending = false
	c.dir = 0
	return c.active, changed
}

func (c *crew) stepRefine() (int, bool) {
	c.refinePending = false
	if c.refineDir == 0 || (c.noUp && c.refineDir > 0) {
		return c.active, false
	}
	next := c.active + c.refineDir
	if next < 1 || next > c.limit() {
		if !c.refineTriedOther {
			c.refineDir = -c.refineDir
			c.refineTriedOther = true
			next = c.active + c.refineDir
		}
		if next < 1 || next > c.limit() || next == c.active {
			c.dir = 0
			return c.active, false
		}
	}
	c.active = next
	c.note()
	return c.active, true
}

func (c *crew) shrinkStorm() (int, bool) {
	if c.active <= 1 {
		c.resetWindow()
		return c.active, false
	}
	next := c.active / 2
	if next < 1 || next >= c.active {
		next = c.active - 1
	}
	if next > c.limit() {
		next = c.limit()
	}
	c.active = next
	c.noUp = true
	c.dir = 0
	c.refinePending = false
	c.resetWindow()
	c.note()
	return c.active, true
}

func (c *crew) resetWindow() {
	c.samples = c.samples[:0]
	c.gasSum = 0
	c.dtSum = 0
	c.aborts = 0
	c.dones = 0
	c.idle = 0
	c.winStart = c.now()
	c.lastSample = c.winStart
}

func (c *crew) note() {
	if len(c.trace) == 0 || c.trace[len(c.trace)-1] != c.active {
		c.trace = append(c.trace, c.active)
	}
}

// Active is the current trial.
func (c *crew) Active() int { return c.active }

// Best is the trial with the highest measured throughput. Before any window
// closes it is the capped prior, never a later tail-drain width.
func (c *crew) Best() int {
	if c.bestC > 0 {
		return c.bestC
	}
	if c.active > 0 {
		return c.active
	}
	return 1
}

// Trace is the chosen active counts, joined as "4-8-4".
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
