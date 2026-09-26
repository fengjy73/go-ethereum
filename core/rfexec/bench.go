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
	"encoding/csv"
	"fmt"
	"io"
	"runtime/debug"
	"strconv"

	"github.com/ethereum/go-ethereum/core/rfstate"
)

// PriorCarry learns block N from blocks strictly below N, in number order.
// PriorReset starts every timed run from an empty learner.
const (
	PriorCarry = "carry"
	PriorReset = "reset"
)

// CSVHeader is the benchmark record schema.
var CSVHeader = []string{
	"block", "engine", "C", "run", "wall_ns",
	"executions", "rollbacks", "invalidations",
	"wait_final", "wait_prefix", "wait_defer", "wait_order",
	"wait_ns", "idle_ns", "gc_pause_ns",
}

// RunBench executes the requested engines. There is no warm-up. Each timed
// run builds a fresh state and ledger. With PriorCarry, the learner given to
// a timed run of block N is a clone of the posterior produced by an untimed
// RegionFence pass over the earlier fixture blocks only; in-block updates
// from the timed run are discarded. A single-block invocation therefore
// starts from an empty prior unless earlier blocks were supplied in the
// fixture set. PriorReset uses an empty prior for every run.
func RunBench(blocks []*BlockEnv, engines []string, cs []int, runs int, pool *rfstate.Pool, prior string, out io.Writer) error {
	if runs < 1 {
		runs = 1
	}
	if prior != PriorCarry && prior != PriorReset {
		return fmt.Errorf("prior must be %q or %q", PriorCarry, PriorReset)
	}
	w := csv.NewWriter(out)
	if err := w.Write(CSVHeader); err != nil {
		return err
	}
	defer w.Flush()
	carried := rfstate.NewLearner()
	for _, env := range blocks {
		base := rfstate.NewLearner()
		if prior == PriorCarry {
			base = carried.Clone()
		}
		oracle, err := ExecSerial(env)
		if err != nil {
			return fmt.Errorf("oracle block %d: %w", env.Number, err)
		}
		if err := CheckFixture(env, oracle); err != nil {
			return fmt.Errorf("fixture block %d: %w", env.Number, err)
		}
		for run := 0; run < runs; run++ {
			for _, eng := range engines {
				workers := cs
				if eng == EngineSerial {
					workers = []int{1}
				}
				for _, c := range workers {
					row, err := timedRun(env, eng, pool, c, base, oracle)
					if err != nil {
						return fmt.Errorf("block %d engine %s C %d run %d: %w", env.Number, eng, c, run, err)
					}
					rec := []string{
						strconv.FormatUint(env.Number, 10),
						eng,
						strconv.Itoa(c),
						strconv.Itoa(run),
						strconv.FormatInt(row.Wall.Nanoseconds(), 10),
						strconv.FormatUint(row.Counters.Executions, 10),
						strconv.FormatUint(row.Counters.Rollbacks, 10),
						strconv.FormatUint(row.Counters.Invalidations, 10),
						strconv.FormatUint(row.Counters.WaitFinal, 10),
						strconv.FormatUint(row.Counters.WaitPrefix, 10),
						strconv.FormatUint(row.Counters.WaitDefer, 10),
						strconv.FormatUint(row.Counters.WaitOrder, 10),
						strconv.FormatInt(row.Counters.WaitNs, 10),
						strconv.FormatInt(row.Counters.IdleNs, 10),
						strconv.FormatInt(row.Counters.GCPauseNs, 10),
					}
					if err := w.Write(rec); err != nil {
						return err
					}
					w.Flush()
				}
			}
		}
		if prior == PriorCarry {
			learned := base.Clone()
			if _, err := execParallel(env, rfstate.ModeRF, pool, 1, learned); err != nil {
				return fmt.Errorf("learn block %d: %w", env.Number, err)
			}
			carried.Absorb(learned)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return nil
}

func timedRun(env *BlockEnv, eng string, pool *rfstate.Pool, c int, base *rfstate.Learner, oracle *Outcome) (*Outcome, error) {
	var before, after debug.GCStats
	debug.ReadGCStats(&before)
	var learner *rfstate.Learner
	if eng == EngineRF {
		learner = base.Clone()
	}
	out, err := ExecEngine(env, eng, pool, c, learner)
	debug.ReadGCStats(&after)
	if err != nil {
		return nil, err
	}
	out.Counters.GCPauseNs = after.PauseTotal.Nanoseconds() - before.PauseTotal.Nanoseconds()
	if eng == EngineSerial {
		if err := CheckFixture(env, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := CheckAgainstSerial(oracle, out, env.World); err != nil {
		return nil, err
	}
	return out, nil
}
