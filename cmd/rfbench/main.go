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

// rfbench times serial, Block-STM-style OCC, and RegionFence P0 on RPC fixtures.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/core/rfexec"
	"github.com/ethereum/go-ethereum/core/rfstate"
)

func main() {
	var (
		fixtureArg = flag.String("fixtures", "", "comma-separated fixture directories (a block dir or a parent of block dirs)")
		engineArg  = flag.String("engines", "serial,occ,rf", "comma-separated engines: serial, occ, rf, rf-auto")
		cArg       = flag.String("c", "1,2,4,8", "comma-separated active worker counts (ignored by rf-auto)")
		runs       = flag.Int("k", 1, "timed runs per block and engine")
		cpuArg     = flag.String("cpus", "", "comma-separated CPUs to pin workers to (default 0..N-1)")
		prior      = flag.String("prior", rfexec.PriorReset, "carry or reset")
		gogc       = flag.Int("gogc", 100, "GOGC percent for the process")
		outPath    = flag.String("out", "", "CSV path (default stdout)")
		pinCoord   = flag.Bool("pin-coordinator", false, "lock the main goroutine to the first CPU of the first L3 group (optional; slower at GOMAXPROCS=C)")
	)
	flag.Parse()
	if *fixtureArg == "" {
		fmt.Fprintln(os.Stderr, "rfbench: -fixtures is required")
		os.Exit(2)
	}
	debug.SetGCPercent(*gogc)
	dirs := splitNonEmpty(*fixtureArg)
	engines := splitNonEmpty(*engineArg)
	cs, err := splitInts(*cArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cpus, err := splitInts(*cpuArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	maxC := 1
	for _, c := range cs {
		if c > maxC {
			maxC = c
		}
	}
	wantAuto := false
	for _, eng := range engines {
		if eng == rfexec.EngineAuto {
			wantAuto = true
		}
	}
	// Default pin list is 0..N-1. rf-auto's maximum is this list, so it is
	// not clipped to -c. Fixed engines still start only maxC workers.
	if len(cpus) == 0 {
		n := runtime.NumCPU()
		if n < 1 {
			n = 1
		}
		if !wantAuto && n > maxC {
			n = maxC
		}
		for i := 0; i < n; i++ {
			cpus = append(cpus, i)
		}
	}
	ordered, groups := rfstate.LayoutCPUs(cpus)
	if len(ordered) > 0 {
		cpus = ordered
	}
	var coordCPU int
	coordPinned := false
	if !*pinCoord {
		// Hint only: the goroutine is not locked to the thread, so it can
		// still migrate. A one-CPU set is not a hint; that is -pin-coordinator.
		if set := rfstate.FirstCacheCPUs(cpus); len(set) > 1 {
			if err := rfstate.HintCurrentThread(set); err != nil {
				fmt.Fprintln(os.Stderr, "rfbench: coordinator affinity hint:", err)
			}
		}
	}
	if *pinCoord {
		if len(cpus) < 2 {
			fmt.Fprintln(os.Stderr, "rfbench: -pin-coordinator needs at least two CPUs")
			os.Exit(2)
		}
		// ordered[0] is the first CPU of the first last-level cache.
		// Workers start at ordered[1], so the active prefix still fills
		// that cache before spilling, and the main thread is not on a
		// worker's CPU.
		coordCPU = cpus[0]
		cpus = append([]int(nil), cpus[1:]...)
		if err := rfstate.PinCurrentThread(coordCPU); err != nil {
			fmt.Fprintln(os.Stderr, "rfbench: pin coordinator:", err)
			os.Exit(1)
		}
		coordPinned = true
	}
	if wantAuto && len(cpus) > maxC {
		maxC = len(cpus)
	}
	// The pin list is a placement set. It must not inflate GOMAXPROCS or the
	// worker count: a 128-CPU mask with -c 4 used to start 128 threads.
	// rf-auto's pool, and when GOMAXPROCS is unset the process cap, equal
	// the worker pin list. Fixed engines should be started with
	// GOMAXPROCS=C+1 so the unpinned coordinator has a P that is not a
	// worker thread. rf-auto must not be launched at C+1: autoLimit would
	// wake an extra worker. After an arm is chosen, ExecAuto shrinks
	// GOMAXPROCS and every thread's affinity to that arm's workers plus
	// one coordinator CPU, then restores both before the run returns.
	// An explicit GOMAXPROCS is the cap and is not raised.
	// -pin-coordinator locks the main goroutine to one CPU. At GOMAXPROCS=C
	// that was slower than leaving it unpinned in the same cache.
	if _, ok := os.LookupEnv("GOMAXPROCS"); !ok {
		runtime.GOMAXPROCS(maxC)
	}
	blocks, err := rfexec.LoadFixtures(dirs...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(blocks) == 0 {
		fmt.Fprintln(os.Stderr, "rfbench: no block directories found")
		os.Exit(1)
	}
	pool := rfstate.NewPool(cpus, maxC)
	defer pool.Stop()
	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}
	fmt.Fprintf(os.Stderr, "rfbench blocks=%d engines=%v C=%v runs=%d cpus=%v groups=%v gogc=%d gomaxprocs=%d prior=%s pin_coordinator=%t",
		len(blocks), engines, cs, *runs, pool.CPUs(), groups, *gogc, runtime.GOMAXPROCS(0), *prior, coordPinned)
	if coordPinned {
		fmt.Fprintf(os.Stderr, " coord_cpu=%d", coordCPU)
	}
	fmt.Fprintln(os.Stderr)
	if coordPinned {
		fmt.Fprintln(os.Stderr, "rfbench: -pin-coordinator locks the main thread; with GOMAXPROCS=C this was slower than an unpinned coordinator in the same cache (rf C=4 about 22%, C=8 about 11%, occ C=4 about 25%). Fixed engines: GOMAXPROCS=C+1 and no -pin-coordinator.")
	}
	if pins := pool.PinReport(); len(pins) > 0 {
		for i, p := range pins {
			if p != "" {
				fmt.Fprintf(os.Stderr, "pin worker %d: %s\n", i, p)
			}
		}
	}
	if err := rfexec.RunBench(blocks, engines, cs, *runs, pool, *prior, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitInts(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("bad integer %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}
