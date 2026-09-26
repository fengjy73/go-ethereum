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
		engineArg  = flag.String("engines", "serial,occ,rf", "comma-separated engines: serial, occ, rf")
		cArg       = flag.String("c", "1,2,4,8", "comma-separated active worker counts")
		runs       = flag.Int("k", 1, "timed runs per block and engine")
		cpuArg     = flag.String("cpus", "", "comma-separated CPUs to pin workers to (default 0..N-1)")
		prior      = flag.String("prior", rfexec.PriorReset, "carry or reset")
		gogc       = flag.Int("gogc", 100, "GOGC percent for the process")
		outPath    = flag.String("out", "", "CSV path (default stdout)")
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
	// The pin list is a placement set. It must not inflate GOMAXPROCS or the
	// worker count: a 128-CPU mask with -c 4 used to start 128 threads.
	// An explicit GOMAXPROCS (one process per C in the scan) is left as set.
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
	fmt.Fprintf(os.Stderr, "rfbench blocks=%d engines=%v C=%v runs=%d cpus=%v gogc=%d gomaxprocs=%d prior=%s\n",
		len(blocks), engines, cs, *runs, pool.CPUs(), *gogc, runtime.GOMAXPROCS(0), *prior)
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
