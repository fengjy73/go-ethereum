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
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/rfstate"
)

// TestStageProfile is the stage-1b measurement. It is skipped unless
// RF_PROFILE=1. One process should set GOMAXPROCS to RF_PROFILE_C.
//
//	RF_PROFILE=1 RF_PROFILE_BLOCK=22418000 RF_PROFILE_C=1 \
//	  go test -c -o /tmp/rfprof.test ./core/rfexec/
//	GOMAXPROCS=1 /tmp/rfprof.test -test.run TestStageProfile -test.v -test.count=1
func TestStageProfile(t *testing.T) {
	if os.Getenv("RF_PROFILE") == "" {
		t.Skip("set RF_PROFILE=1")
	}
	block := os.Getenv("RF_PROFILE_BLOCK")
	c, err := strconv.Atoi(os.Getenv("RF_PROFILE_C"))
	if err != nil || c < 1 || block == "" {
		t.Fatal("RF_PROFILE_BLOCK and RF_PROFILE_C are required")
	}
	sub := findBlockDir(block)
	if sub == "" {
		t.Fatalf("fixture %s not found", block)
	}
	env, err := LoadFixture(sub)
	if err != nil {
		t.Fatal(err)
	}
	pool := rfstate.NewPool(nil, c)
	defer pool.Stop()
	// One untimed pass pays for KZG and page faults.
	if _, err := ExecEngine(env, EngineRF, pool, c, rfstate.NewLearner()); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := ExecEngine(env, EngineRF, pool, c, rfstate.NewLearner())
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("ALLOC block=%s C=%d wall=%s total_alloc_mib=%.2f mallocs=%d gcs=%d\n",
		block, c, out.Wall,
		float64(after.TotalAlloc-before.TotalAlloc)/(1<<20),
		after.Mallocs-before.Mallocs,
		after.NumGC-before.NumGC)

	loops := 8
	if c > 1 {
		loops = 4
	}
	cpuPath := os.Getenv("RF_PROFILE_CPU")
	if cpuPath == "" {
		cpuPath = fmt.Sprintf("/tmp/pprof-rf-%s-c%d.out", block, c)
	}
	f, err := os.Create(cpuPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	for i := 0; i < loops; i++ {
		if _, err := ExecEngine(env, EngineRF, pool, c, rfstate.NewLearner()); err != nil {
			pprof.StopCPUProfile()
			t.Fatal(err)
		}
	}
	pprof.StopCPUProfile()
	f.Close()
	fmt.Printf("CPU block=%s C=%d loops=%d profile_wall=%s file=%s gomaxprocs=%d\n",
		block, c, loops, time.Since(t0), cpuPath, runtime.GOMAXPROCS(0))
}
