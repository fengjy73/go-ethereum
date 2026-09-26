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
	"runtime"
	"sync"
)

// procGate is the only goroutine that changes GOMAXPROCS during a block.
// The coordinator starts it and joins it. Workers enqueue a count; a
// request whose generation does not match is dropped. restore invalidates
// the generation, wakes the loop, and waits until GOMAXPROCS is back at
// the process cap. A set that races with restore cannot land afterwards:
// either the loop applies it and then restores the cap, or the request
// sees the generation and is ignored.
type procGate struct {
	mu      sync.Mutex
	cv      *sync.Cond
	cap     int
	gen     uint64
	armed   bool
	shut    bool
	cur     int
	pending int
	has     bool
	stopped chan struct{}
}

func newProcGate(capN int) *procGate {
	if capN < 1 {
		capN = 1
	}
	g := &procGate{cap: capN, cur: capN, stopped: make(chan struct{})}
	g.cv = sync.NewCond(&g.mu)
	go g.loop()
	return g
}

func (g *procGate) arm(gen uint64) {
	g.mu.Lock()
	g.gen = gen
	g.armed = true
	g.mu.Unlock()
}

func (g *procGate) request(gen uint64, n int) {
	if g == nil || n < 1 {
		return
	}
	g.mu.Lock()
	if !g.armed || g.gen != gen {
		g.mu.Unlock()
		return
	}
	if n > g.cap {
		n = g.cap
	}
	if n == g.cur && !g.has {
		g.mu.Unlock()
		return
	}
	g.pending = n
	g.has = true
	g.cv.Signal()
	g.mu.Unlock()
}

// restore returns GOMAXPROCS to the cap and waits until that write has
// happened. Later request calls are ignored.
func (g *procGate) restore() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.shut = true
	g.armed = false
	g.gen++
	g.mu.Unlock()
	g.cv.Broadcast()
	<-g.stopped
}

func (g *procGate) loop() {
	for {
		g.mu.Lock()
		for !g.shut && !(g.armed && g.has) {
			g.cv.Wait()
		}
		shut := g.shut
		n := g.pending
		g.has = false
		gen := g.gen
		g.mu.Unlock()
		if shut {
			runtime.GOMAXPROCS(g.cap)
			g.mu.Lock()
			g.cur = g.cap
			g.mu.Unlock()
			close(g.stopped)
			return
		}
		g.mu.Lock()
		if g.shut || !g.armed || g.gen != gen {
			g.mu.Unlock()
			continue
		}
		g.mu.Unlock()
		runtime.GOMAXPROCS(n)
		g.mu.Lock()
		if g.gen == gen && !g.shut {
			g.cur = n
		}
		g.mu.Unlock()
	}
}
