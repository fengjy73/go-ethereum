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
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// footprintCPUs is the CPUs a fixed engine at this worker count would use:
// one CPU per worker, plus one more for the coordinator when the pin list
// has it. A cap-sized mask left idle CPUs in the process, and runtime
// threads landed on those downclocked cores.
func footprintCPUs(cpus []int, workers int) []int {
	if workers < 1 {
		workers = 1
	}
	if len(cpus) == 0 {
		out := make([]int, workers)
		for i := range out {
			out[i] = i
		}
		return out
	}
	n := workers
	if len(cpus) > workers {
		n = workers + 1
	}
	if n > len(cpus) {
		n = len(cpus)
	}
	return append([]int(nil), cpus[:n]...)
}

// procsFor is how many Ps match that footprint, never above the process cap
// captured at block start. The pool's active count stays at workers; the
// extra P, when there is one, is the coordinator.
func procsFor(cpus []int, workers, cap int) int {
	n := len(footprintCPUs(cpus, workers))
	if cap > 0 && n > cap {
		n = cap
	}
	if n < 1 {
		n = 1
	}
	return n
}

// procMask shrinks every OS thread in the process to the arm's footprint
// and restores the masks captured at arm. A shrink whose generation does
// not match is dropped, including one that lost the race with restore.
type procMask struct {
	mu    sync.Mutex
	gen   uint64
	armed bool
	cpus  []int
	saved map[int]unix.CPUSet
}

func newProcMask(cpus []int) *procMask {
	return &procMask{cpus: append([]int(nil), cpus...)}
}

func (m *procMask) lock() {
	if m == nil {
		return
	}
	m.mu.Lock()
}

func (m *procMask) unlock() {
	if m == nil {
		return
	}
	m.mu.Unlock()
}

// arm records gen and, the first time, the affinity of every thread.
func (m *procMask) arm(gen uint64) {
	if m == nil {
		return
	}
	m.lock()
	defer m.unlock()
	m.gen = gen
	m.armed = true
	if m.saved == nil {
		m.saved = captureMasks()
	}
}

// shrink sets every thread onto the footprint of workers. Stale generations
// do not apply. The lock is held across the system calls so a restore cannot
// land and then be overwritten.
func (m *procMask) shrink(gen uint64, workers int) {
	if m == nil {
		return
	}
	m.lock()
	defer m.unlock()
	if !m.armed || m.gen != gen {
		return
	}
	applyCPUSet(cpuSetOf(footprintCPUs(m.cpus, workers)))
}

// restore puts each captured thread back and ignores later shrink calls.
func (m *procMask) restore() {
	if m == nil {
		return
	}
	m.lock()
	defer m.unlock()
	m.armed = false
	m.gen++
	saved := m.saved
	m.saved = nil
	if len(saved) == 0 {
		return
	}
	var fallback unix.CPUSet
	fallback.Zero()
	for _, c := range m.cpus {
		if c >= 0 {
			fallback.Set(c)
		}
	}
	for _, tid := range taskIDs() {
		if set, ok := saved[tid]; ok {
			_ = unix.SchedSetaffinity(tid, &set)
			continue
		}
		_ = unix.SchedSetaffinity(tid, &fallback)
	}
}

func cpuSetOf(cpus []int) unix.CPUSet {
	var set unix.CPUSet
	set.Zero()
	for _, c := range cpus {
		if c >= 0 {
			set.Set(c)
		}
	}
	return set
}

func applyCPUSet(set unix.CPUSet) {
	for _, tid := range taskIDs() {
		_ = unix.SchedSetaffinity(tid, &set)
	}
}

func captureMasks() map[int]unix.CPUSet {
	ids := taskIDs()
	out := make(map[int]unix.CPUSet, len(ids))
	for _, tid := range ids {
		var set unix.CPUSet
		if err := unix.SchedGetaffinity(tid, &set); err != nil {
			continue
		}
		out[tid] = set
	}
	return out
}

func taskIDs() []int {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return []int{unix.Gettid()}
	}
	out := make([]int, 0, len(ents))
	for _, e := range ents {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		out = append(out, tid)
	}
	if len(out) == 0 {
		return []int{unix.Gettid()}
	}
	return out
}
