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
	if c.Best() != 1 {
		t.Fatalf("best %d", c.Best())
	}
	cur := time.Unix(1_000, 0)
	c.now = func() time.Time { return cur }
	c.winStart = cur
	c.lastSample = cur
	cur = cur.Add(10 * time.Millisecond)
	if n, ch := c.tick(100, 0, 0, 1); n != 1 || ch {
		t.Fatalf("first %d changed %v", n, ch)
	}
	cur = cur.Add(10 * time.Millisecond)
	if n, ch := c.tick(100, 0, 0, 1); n != 1 || ch {
		t.Fatalf("grew to %d changed %v", n, ch)
	}
}

func TestCrewPriorStartsAtPrior(t *testing.T) {
	c := newCrew(4, 32)
	if got := c.begin(32); got != 4 {
		t.Fatalf("start %d", got)
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewWidthCapsPrior(t *testing.T) {
	c := newCrew(8, 32)
	if got := c.begin(2); got != 2 {
		t.Fatalf("start %d", got)
	}
	if c.Best() != 2 {
		t.Fatalf("best %d", c.Best())
	}
}

// window closes one window: one completion per active worker, at least two.
func window(c *crew, cur *time.Time, dt time.Duration, gas uint64) (int, bool) {
	n := c.active
	if n < 2 {
		n = 2
	}
	var got int
	var ch bool
	for i := 0; i < n; i++ {
		*cur = cur.Add(dt)
		got, ch = c.tick(gas, 0, 0, 1)
	}
	return got, ch
}

func TestCrewDoublesOnFaster(t *testing.T) {
	cur := time.Unix(1_000, 0)
	c := newCrew(4, 32)
	c.now = func() time.Time { return cur }
	if got := c.begin(32); got != 4 {
		t.Fatalf("start %d", got)
	}
	if n, ch := window(c, &cur, 10*time.Millisecond, 100); ch || n != 4 {
		t.Fatalf("first baseline %d changed %v", n, ch)
	}
	if n, ch := window(c, &cur, 10*time.Millisecond, 100); !ch || n != 8 {
		t.Fatalf("double %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.Best() != 4 {
		t.Fatalf("best after baseline %d", c.Best())
	}
	if n, ch := window(c, &cur, 5*time.Millisecond, 100); !ch || n != 16 {
		t.Fatalf("double again %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.Best() != 8 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewSlowerRefines(t *testing.T) {
	cur := time.Unix(3_000, 0)
	c := newCrew(4, 32)
	c.now = func() time.Time { return cur }
	c.begin(32)
	window(c, &cur, 10*time.Millisecond, 100)
	if n, _ := window(c, &cur, 10*time.Millisecond, 100); n != 8 {
		t.Fatalf("probe %d", n)
	}
	if n, ch := window(c, &cur, 40*time.Millisecond, 100); !ch || n != 4 {
		t.Fatalf("back %d changed %v trace %s", n, ch, c.Trace())
	}
	n, ch := window(c, &cur, 10*time.Millisecond, 100)
	if !ch || n != 5 {
		t.Fatalf("refine %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.Trace() != "4-8-4-5" {
		t.Fatalf("trace %s", c.Trace())
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewNoiseStays(t *testing.T) {
	cur := time.Unix(4_000, 0)
	c := newCrew(4, 32)
	c.now = func() time.Time { return cur }
	c.begin(32)
	window(c, &cur, 10*time.Millisecond, 100)
	if n, _ := window(c, &cur, 10*time.Millisecond, 100); n != 8 {
		t.Fatalf("probe %d", n)
	}
	// The same rate is not a clear improvement, so the probe returns.
	n, ch := window(c, &cur, 10*time.Millisecond, 100)
	if !ch || n != 4 {
		t.Fatalf("noise stayed at %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewAbortStorm(t *testing.T) {
	cur := time.Unix(5_000, 0)
	c := newCrew(4, 8)
	c.now = func() time.Time { return cur }
	if c.begin(8) != 4 {
		t.Fatal(c.active)
	}
	n, ch := c.tick(0, 0, 4, 0)
	if !ch || n != 2 {
		t.Fatalf("storm got %d changed %v", n, ch)
	}
	if c.Best() != 4 {
		t.Fatalf("storm rewrote best to %d", c.Best())
	}
	cur = cur.Add(time.Millisecond)
	c.tick(100, 0, 0, 1)
	cur = cur.Add(time.Millisecond)
	if n, ch := c.tick(100, 0, 0, 1); n != 2 || ch {
		t.Fatalf("climbed after storm to %d changed %v", n, ch)
	}
	if c.Best() != 4 {
		t.Fatalf("best after fast window %d", c.Best())
	}
}

func TestCrewIdleStorm(t *testing.T) {
	cur := time.Unix(2_000, 0)
	c := newCrew(4, 8)
	c.now = func() time.Time { return cur }
	c.begin(8)
	// One completion per worker closes the window. Idle of each sample is
	// just over (active-1) times 10ms, so the sum exceeds elapsed*(active-1).
	idle := int64(10*time.Millisecond)*3 + 1
	var n int
	var ch bool
	for i := 0; i < 3; i++ {
		cur = cur.Add(10 * time.Millisecond)
		if n, ch = c.tick(100, idle, 0, 1); ch || n != 4 {
			t.Fatalf("sample %d got %d changed %v", i, n, ch)
		}
	}
	cur = cur.Add(10 * time.Millisecond)
	n, ch = c.tick(100, idle, 0, 1)
	if !ch || n != 2 {
		t.Fatalf("idle got %d changed %v", n, ch)
	}
}

func TestCrewTailDrainKeepsBest(t *testing.T) {
	cur := time.Unix(6_000, 0)
	c := newCrew(4, 32)
	c.now = func() time.Time { return cur }
	c.begin(32)
	window(c, &cur, 10*time.Millisecond, 100)
	if n, _ := window(c, &cur, 10*time.Millisecond, 100); n != 8 {
		t.Fatalf("probe %d", n)
	}
	n, ch := c.setWidth(1)
	if !ch || n != 1 {
		t.Fatalf("drain %d changed %v", n, ch)
	}
	if c.Best() != 4 {
		t.Fatalf("best after drain %d", c.Best())
	}
	cur = cur.Add(10 * time.Millisecond)
	if n, ch := c.tick(100, 0, 0, 1); ch || n != 1 {
		t.Fatalf("tail tick %d changed %v", n, ch)
	}
	cur = cur.Add(5 * time.Millisecond)
	if n, ch := c.tick(100, 0, 0, 1); ch || n != 1 || c.Best() != 4 {
		t.Fatalf("tail moved active %d best %d changed %v", n, c.Best(), ch)
	}
	n, ch = c.setWidth(32)
	if !ch || n != 4 {
		t.Fatalf("restore %d changed %v", n, ch)
	}
	if c.Best() != 4 {
		t.Fatalf("best %d", c.Best())
	}
}

func TestCrewRefineTriesOtherSide(t *testing.T) {
	cur := time.Unix(7_000, 0)
	c := newCrew(4, 32)
	c.now = func() time.Time { return cur }
	c.begin(32)
	window(c, &cur, 10*time.Millisecond, 100)
	if n, _ := window(c, &cur, 10*time.Millisecond, 100); n != 8 {
		t.Fatalf("probe %d", n)
	}
	if n, _ := window(c, &cur, 40*time.Millisecond, 100); n != 4 {
		t.Fatalf("back %d trace %s", n, c.Trace())
	}
	if n, _ := window(c, &cur, 10*time.Millisecond, 100); n != 5 {
		t.Fatalf("refine up %d trace %s", n, c.Trace())
	}
	if n, _ := window(c, &cur, 40*time.Millisecond, 100); n != 4 {
		t.Fatalf("refine back %d trace %s", n, c.Trace())
	}
	n, ch := window(c, &cur, 10*time.Millisecond, 100)
	if !ch || n != 3 {
		t.Fatalf("other side %d changed %v trace %s", n, ch, c.Trace())
	}
	if c.Trace() != "4-8-4-5-4-3" {
		t.Fatalf("trace %s", c.Trace())
	}
}
