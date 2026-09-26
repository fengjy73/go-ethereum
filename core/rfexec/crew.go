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
	"strconv"
	"strings"
	"time"
)

// crew chooses how many workers execute one block.
//
// The active count is the hill-climb trial, capped by the structural width
// (distinct senders that still have a non-final transaction) and by the
// process limit (pin-list length and GOMAXPROCS at block start). The
// cross-block prior is the starting trial, not a ceiling: a cold prior of 0
// starts at 1 and may step up, and a prior of 4 may step to 5 when width
// allows. The next block's prior is Best, the trial with the highest
// measured gas per nanosecond, not an unmeasured step past it.
//
// Throughput samples are equal-gas intervals. The first completed
// transaction sets the quantum; that interval is only a baseline. A faster
// interval keeps the current direction. A slower one steps back to the best
// trial and stops, so the search does not oscillate. An unmeasured probe in
// the opposite direction is not started: the rest of the block would run
// there if that interval never closed.
//
// The watchdog shrinks on a measured storm: more aborts than completions in
// the interval, or summed idle time greater than the wall time of every
// extra worker (each of the active-1 workers idle for the whole interval).
// A run of aborts with no completion, one per active worker, also shrinks
// before the quantum can fill. Those are comparisons of counts the interval
// already collected.
type crew struct {
	prior    int
	max      int
	width    int
	active   int
	bestC    int
	bestTP   float64
	dir      int
	haveBase bool
	quantum  uint64
	gas      uint64
	aborts   uint64
	dones    uint64
	idle     int64
	winStart time.Time
	trace    []int
	now      func() time.Time
}

func newCrew(prior, max int) *crew {
	if max < 1 {
		max = 1
	}
	return &crew{prior: prior, max: max, now: time.Now}
}

// begin sets the starting trial from the prior and the current width.
func (c *crew) begin(width int) int {
	c.width = width
	if c.width < 1 {
		c.width = 1
	}
	start := 1
	if c.prior > 0 {
		start = c.prior
	}
	if start > c.width {
		start = c.width
	}
	if start > c.max {
		start = c.max
	}
	if start < 1 {
		start = 1
	}
	c.active = start
	c.bestC = start
	limit := c.limit()
	if start < limit {
		c.dir = 1
	} else if start > 1 {
		c.dir = -1
	} else {
		c.dir = 0
	}
	c.winStart = c.now()
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

// setWidth lowers the structural cap. The active count follows it down.
func (c *crew) setWidth(width int) (int, bool) {
	if width < 1 {
		width = 1
	}
	c.width = width
	if c.active <= c.width {
		return c.active, false
	}
	c.active = c.width
	if c.active > c.max {
		c.active = c.max
	}
	c.haveBase = false
	c.bestC = c.active
	c.bestTP = 0
	c.dir = 0
	c.resetWindow()
	c.note()
	return c.active, true
}

// tick folds one scheduler event into the current interval.
// gas is the gas of a completion (0 on a rollback). dones is 1 when this
// event finalized a transaction. aborts and idle are deltas since the
// previous tick.
func (c *crew) tick(gas uint64, idleNs int64, aborts, dones uint64) (int, bool) {
	if c.winStart.IsZero() {
		c.winStart = c.now()
	}
	var add uint64
	if dones > 0 {
		add = gas
		if add == 0 {
			add = 1
		}
		if c.quantum == 0 {
			c.quantum = add
		}
		c.gas += add
	}
	c.idle += idleNs
	c.aborts += aborts
	c.dones += dones

	// No completion yet, and every active worker has already aborted.
	if c.active > 1 && c.dones == 0 && c.aborts >= uint64(c.active) {
		return c.shrink()
	}
	if c.quantum == 0 || c.gas < c.quantum {
		return c.active, false
	}
	elapsed := c.now().Sub(c.winStart).Nanoseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	if c.active > 1 && (c.aborts > c.dones || (c.dones > 0 && c.idle > elapsed*int64(c.active-1))) {
		return c.shrink()
	}
	tp := float64(c.gas) / float64(elapsed)
	c.resetWindow()
	if !c.haveBase {
		c.haveBase = true
		c.bestTP = tp
		c.bestC = c.active
		return c.step()
	}
	if tp > c.bestTP {
		c.bestTP = tp
		c.bestC = c.active
		return c.step()
	}
	return c.onWorse()
}

func (c *crew) shrink() (int, bool) {
	if c.active <= 1 {
		c.resetWindow()
		return c.active, false
	}
	c.active--
	if c.active > c.limit() {
		c.active = c.limit()
	}
	// Stay at the reduced trial. Climbing again in the same block re-enters
	// the storm that caused the shrink. The next block can start from Best.
	c.haveBase = false
	c.bestC = c.active
	c.bestTP = 0
	c.dir = 0
	c.resetWindow()
	c.note()
	return c.active, true
}

func (c *crew) step() (int, bool) {
	if c.dir == 0 {
		return c.active, false
	}
	next := c.active + c.dir
	if next < 1 || next > c.limit() {
		c.dir = 0
		return c.active, false
	}
	c.active = next
	c.note()
	return c.active, true
}

func (c *crew) onWorse() (int, bool) {
	changed := false
	if c.active != c.bestC {
		c.active = c.bestC
		if c.active > c.limit() {
			c.active = c.limit()
			c.bestC = c.active
		}
		changed = true
		c.note()
	}
	c.dir = 0
	return c.active, changed
}

func (c *crew) resetWindow() {
	c.gas = 0
	c.aborts = 0
	c.dones = 0
	c.idle = 0
	c.winStart = c.now()
}

func (c *crew) note() {
	if len(c.trace) == 0 || c.trace[len(c.trace)-1] != c.active {
		c.trace = append(c.trace, c.active)
	}
}

// Active is the current trial.
func (c *crew) Active() int { return c.active }

// Best is the trial with the highest measured throughput, or the start
// when the block ended before an interval closed.
func (c *crew) Best() int {
	if c.bestC > 0 {
		return c.bestC
	}
	if c.active > 0 {
		return c.active
	}
	return 1
}

// Trace is the chosen active counts, joined as "1-4-2".
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
