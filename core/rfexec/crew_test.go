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
	"testing"
	"time"
)

func TestCrewWidthOneNeverGrows(t *testing.T) {
	c := newCrew(4, 8)
	if got := c.begin(1); got != 1 {
		t.Fatalf("start %d", got)
	}
	if n, ch := c.tick(100, 0, 0, 1); n != 1 || ch {
		t.Fatalf("after baseline %d changed %v", n, ch)
	}
	if n, _ := c.tick(100, 0, 0, 1); n != 1 {
		t.Fatalf("grew to %d", n)
	}
}

func TestCrewPriorIsStartNotCeiling(t *testing.T) {
	cur := time.Unix(1_000, 0)
	c := newCrew(4, 8)
	c.now = func() time.Time { return cur }
	if got := c.begin(8); got != 4 {
		t.Fatalf("start %d", got)
	}
	// Faster interval than the baseline: the prior is not a cap.
	cur = cur.Add(10 * time.Millisecond)
	n, ch := c.tick(100, 0, 0, 1)
	if !ch || n != 5 {
		t.Fatalf("probe up got %d changed %v", n, ch)
	}
	cur = cur.Add(5 * time.Millisecond)
	n, ch = c.tick(100, 0, 0, 1)
	if !ch || n != 6 {
		t.Fatalf("continue got %d changed %v", n, ch)
	}
	if c.Best() != 5 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewAbortStormShrinks(t *testing.T) {
	c := newCrew(4, 8)
	if c.begin(8) != 4 {
		t.Fatal(c.active)
	}
	n, ch := c.tick(100, 0, 3, 1)
	if !ch || n != 3 {
		t.Fatalf("storm got %d changed %v", n, ch)
	}
	// A later faster interval must not climb back into the storm.
	if n, ch := c.tick(100, 0, 0, 1); n != 3 || ch {
		t.Fatalf("climbed after storm to %d changed %v", n, ch)
	}
}

func TestCrewIdleStormShrinks(t *testing.T) {
	cur := time.Unix(2_000, 0)
	c := newCrew(4, 8)
	c.now = func() time.Time { return cur }
	c.begin(8)
	cur = cur.Add(10 * time.Millisecond)
	// Three extra workers idle for the whole 10ms, plus a bit.
	idle := int64(10*time.Millisecond)*3 + 1
	n, ch := c.tick(100, idle, 0, 1)
	if !ch || n != 3 {
		t.Fatalf("idle got %d changed %v", n, ch)
	}
}

func TestCrewWorseStepsBackAndStops(t *testing.T) {
	cur := time.Unix(3_000, 0)
	c := newCrew(1, 4)
	c.now = func() time.Time { return cur }
	c.begin(4)
	cur = cur.Add(10 * time.Millisecond)
	if n, _ := c.tick(100, 0, 0, 1); n != 2 {
		t.Fatalf("probe %d", n)
	}
	cur = cur.Add(5 * time.Millisecond)
	if n, _ := c.tick(100, 0, 0, 1); n != 3 {
		t.Fatalf("continue %d", n)
	}
	// Slower than C=2: step back to the measured best and stop.
	cur = cur.Add(40 * time.Millisecond)
	n, ch := c.tick(100, 0, 0, 1)
	if !ch || n != 2 {
		t.Fatalf("back %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.dir != 0 {
		t.Fatalf("dir %d", c.dir)
	}
	if c.Best() != 2 {
		t.Fatalf("best %d", c.Best())
	}
	if c.Trace() != "1-2-3-2" {
		t.Fatalf("trace %s", c.Trace())
	}
}
