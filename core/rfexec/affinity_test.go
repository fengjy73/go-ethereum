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
	"testing"

	"github.com/ethereum/go-ethereum/core/rfstate"
	"golang.org/x/sys/unix"
)

func TestFootprintCPUs(t *testing.T) {
	cpus := []int{136, 137, 138, 139, 140, 141, 142, 143, 144}
	got := footprintCPUs(cpus, 4)
	if len(got) != 5 || got[0] != 136 || got[4] != 140 {
		t.Fatalf("arm 4 footprint %v, want 136..140", got)
	}
	if got := footprintCPUs(cpus[:4], 4); len(got) != 4 || got[3] != 139 {
		t.Fatalf("exact worker list %v", got)
	}
	if got := procsFor(cpus, 4, 9); got != 5 {
		t.Fatalf("procs %d", got)
	}
	if got := procsFor(cpus, 4, 4); got != 4 {
		t.Fatalf("capped procs %d", got)
	}
}

func TestAutoShrinksAffinityToArm(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	before := affinityCPUs(unix.Gettid())

	env := syntheticEnv(t)
	var during []int
	var procs int
	var once sync.Once
	env.BeforeView = func(tx int) {
		once.Do(func() {
			procs = runtime.GOMAXPROCS(0)
			during = affinityCPUs(unix.Gettid())
		})
	}
	pool := rfstate.NewPool([]int{0, 1, 2, 3}, 4)
	defer pool.Stop()
	out, err := ExecAuto(env, pool, rfstate.NewLearner(), preferArm(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("arm %d trace %s procs_during %d affinity_during %v", out.CrewBest, out.CTrace, procs, during)
	if out.CrewBest != 2 {
		t.Fatalf("arm %d", out.CrewBest)
	}
	if procs != 3 {
		t.Fatalf("GOMAXPROCS during the block was %d, want 3 (2 workers + coordinator)", procs)
	}
	if !sameInts(during, []int{0, 1, 2}) {
		t.Fatalf("thread affinity during the block %v, want [0 1 2]", during)
	}
	if got := runtime.GOMAXPROCS(0); got != 4 {
		t.Fatalf("GOMAXPROCS after return %d", got)
	}
	after := affinityCPUs(unix.Gettid())
	if !sameInts(after, before) {
		t.Fatalf("affinity after return %v, before %v", after, before)
	}
}

func affinityCPUs(tid int) []int {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(tid, &set); err != nil {
		return nil
	}
	var out []int
	for cpu := 0; cpu < 256; cpu++ {
		if set.IsSet(cpu) {
			out = append(out, cpu)
		}
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
