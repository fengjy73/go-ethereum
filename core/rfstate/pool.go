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

package rfstate

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Pool is a process-wide set of OS-thread-locked workers. The active count can
// change between blocks; workers are not spawned per block. Each worker calls
// SchedSetaffinity after LockOSThread. Goroutines cannot be pinned to a P, so
// the worker itself runs tasks and never blocks inside the EVM: waits are
// cooperative signals recovered by the scheduler, which keeps a pinned thread
// from stalling the producer (the nonce busy-wait deadlock).
type Pool struct {
	cpus   []int
	active atomic.Int32
	// gen identifies the block Drive published. A SetActive from a previous
	// block is ignored once the next Drive has bumped it.
	gen atomic.Uint64

	mu     sync.Mutex
	cv     *sync.Cond
	sched  atomic.Pointer[runner]
	stop   bool
	pinned []string
}

// runner is the per-block hook implemented by rfexec. It lives here as a
// function pair so the pool does not import the executor.
type runner struct {
	// Step executes at most one task. It returns when the block is finished
	// or the worker should look at the pool again.
	Step func(worker int)
	// Done reports that the block has reached a terminal state.
	Done func() bool
}

// NewPool starts maxC persistent workers. cpus is the pin placement set;
// worker i is pinned to cpus[i%len(cpus)]. An empty list pins workers to
// CPU 0..maxC-1. Idle workers block on the pool condition.
func NewPool(cpus []int, maxC int) *Pool {
	if maxC < 1 {
		maxC = 1
	}
	if len(cpus) == 0 {
		n := runtime.NumCPU()
		if n > maxC {
			n = maxC
		}
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			cpus = append(cpus, i)
		}
	}
	// One goroutine per active slot. C is the upper bound the caller will
	// Drive; extra CPUs in the pin list are a placement set, not extra threads.
	// Idle workers block on the pool cond, they do not poll.
	n := maxC
	if n < 1 {
		n = 1
	}
	p := &Pool{cpus: append([]int(nil), cpus...), pinned: make([]string, n)}
	p.cv = sync.NewCond(&p.mu)
	p.active.Store(int32(n))
	for i := 0; i < n; i++ {
		cpu := cpus[i%len(cpus)]
		go p.loop(i, cpu)
	}
	return p
}

// CPUs returns the configured affinity list.
func (p *Pool) CPUs() []int { return append([]int(nil), p.cpus...) }

// Width is the number of persistent worker goroutines.
func (p *Pool) Width() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pinned)
}

// PinReport returns per-worker affinity errors (empty string on success).
func (p *Pool) PinReport() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pinned...)
}

// SetActive changes how many workers take tasks. Workers with id >= c stay parked.
// Prefer SetActiveIf from a block that may already have ended.
func (p *Pool) SetActive(c int) {
	if c < 1 {
		c = 1
	}
	p.mu.Lock()
	p.active.Store(int32(c))
	p.cv.Broadcast()
	p.mu.Unlock()
}

// Generation is the block id of the latest Drive.
func (p *Pool) Generation() uint64 { return p.gen.Load() }

// SetActiveIf changes the active count only when gen is still the latest
// Drive. A deferred update from the previous block returns false.
func (p *Pool) SetActiveIf(gen uint64, c int) bool {
	if c < 1 {
		c = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen.Load() != gen {
		return false
	}
	p.active.Store(int32(c))
	p.cv.Broadcast()
	return true
}

// Active is the current worker limit.
func (p *Pool) Active() int { return int(p.active.Load()) }

// Drive publishes one block and returns its generation. The active count is
// stored in the same critical section as the generation bump, so a stale
// SetActiveIf cannot land between the two. GOMAXPROCS is not changed.
func (p *Pool) Drive(active int, step func(worker int), done func() bool) uint64 {
	if active < 1 {
		active = 1
	}
	r := &runner{Step: step, Done: done}
	p.mu.Lock()
	gen := p.gen.Add(1)
	p.active.Store(int32(active))
	p.sched.Store(r)
	p.cv.Broadcast()
	p.mu.Unlock()
	// The caller waits on its own scheduler condition; Drive only publishes
	// the hook. rfexec waits for frontier completion separately and then
	// calls Release.
	return gen
}

// Release unhooks the current block so workers return to the pool wait.
func (p *Pool) Release() {
	p.sched.Store(nil)
	p.mu.Lock()
	p.cv.Broadcast()
	p.mu.Unlock()
}

// Stop terminates workers. The pool cannot be reused.
func (p *Pool) Stop() {
	p.mu.Lock()
	p.stop = true
	p.cv.Broadcast()
	p.mu.Unlock()
}

func (p *Pool) loop(id, cpu int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := pinThread(cpu); err != nil {
		p.mu.Lock()
		if id < len(p.pinned) {
			p.pinned[id] = err.Error()
		}
		p.mu.Unlock()
	}
	for {
		p.mu.Lock()
		for !p.stop {
			r := p.sched.Load()
			if r != nil && id < int(p.active.Load()) && !r.Done() {
				break
			}
			p.cv.Wait()
		}
		if p.stop {
			p.mu.Unlock()
			return
		}
		r := p.sched.Load()
		p.mu.Unlock()
		if r == nil || r.Step == nil {
			continue
		}
		r.Step(id)
	}
}

func pinThread(cpu int) error {
	var set unix.CPUSet
	set.Zero()
	if cpu < 0 {
		return fmt.Errorf("negative cpu %d", cpu)
	}
	set.Set(cpu)
	return unix.SchedSetaffinity(unix.Gettid(), &set)
}

// HintCurrentThread asks the OS to run the current thread on cpus. It does
// not lock the goroutine to that thread, so a later schedule can move it.
// rfbench uses this as a hint that the coordinator float inside the first
// last-level cache. The measured regression is LockOSThread onto one CPU.
func HintCurrentThread(cpus []int) error {
	if len(cpus) == 0 {
		return nil
	}
	var set unix.CPUSet
	set.Zero()
	for _, cpu := range cpus {
		if cpu < 0 {
			return fmt.Errorf("negative cpu %d", cpu)
		}
		set.Set(cpu)
	}
	return unix.SchedSetaffinity(unix.Gettid(), &set)
}

// PinCurrentThread locks the calling goroutine to its OS thread and pins
// that thread to one cpu. The lock is held for the life of the goroutine.
// -pin-coordinator uses this. On a host with GOMAXPROCS equal to the worker
// count it was slower than leaving the coordinator unpinned in the same
// last-level cache, because the locked thread occupies one of the C Ps.
func PinCurrentThread(cpu int) error {
	runtime.LockOSThread()
	if err := pinThread(cpu); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	return nil
}
