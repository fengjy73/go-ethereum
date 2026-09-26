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
	"runtime"
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
	"active_c", "c_trace", "park_ns",
}

// RunBench executes the requested engines. There is no warm-up. Each timed
// run builds a fresh state and ledger. With PriorCarry, the learner given to
// a timed run of block N is a clone of the posterior carried from blocks
// strictly below N. That posterior is the previous carry after Decay, plus
// the conflict and safe observations of one timed RegionFence run of block
// N-1 (the highest fixed C in this process, or the rf-auto run when no
// fixed C was timed, one run, not multiplied by K). An untimed C=1 pass is
// not used: it only observes safe reads. A single-block invocation starts
// from an empty prior. PriorReset uses an empty prior for every run.
// In-block updates stay inside the run's clone.
//
// rf-auto keeps a separate worker-count prior. Each timed auto run of a
// block starts from the pre-block best trial (0 on the first block). K runs
// are not chained. After the block, the prior becomes that last run's
// measured-best trial.
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
	crewPrior := 0
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
		var learned *rfstate.Learner
		learnedC := -1
		var learnedAuto *rfstate.Learner
		nextCrew := crewPrior
		sawAuto := false
		for run := 0; run < runs; run++ {
			for _, eng := range engines {
				if eng == EngineAuto {
					row, runLearner, err := timedAuto(env, pool, base, crewPrior, oracle)
					if err != nil {
						return fmt.Errorf("block %d engine %s run %d: %w", env.Number, eng, run, err)
					}
					learnedAuto = runLearner
					sawAuto = true
					if row.CrewBest > 0 {
						nextCrew = row.CrewBest
					} else if row.ActiveC > 0 {
						nextCrew = row.ActiveC
					}
					capC := autoLimit(pool, runtime.GOMAXPROCS(0))
					if err := w.Write(benchRecord(env, eng, capC, run, row)); err != nil {
						return err
					}
					w.Flush()
					continue
				}
				workers := cs
				if eng == EngineSerial {
					workers = []int{1}
				}
				for _, c := range workers {
					row, runLearner, err := timedRun(env, eng, pool, c, base, oracle)
					if err != nil {
						return fmt.Errorf("block %d engine %s C %d run %d: %w", env.Number, eng, c, run, err)
					}
					if eng == EngineRF && c >= learnedC && runLearner != nil {
						learned = runLearner
						learnedC = c
					}
					if err := w.Write(benchRecord(env, eng, c, run, row)); err != nil {
						return err
					}
					w.Flush()
				}
			}
		}
		if prior == PriorCarry && learned != nil {
			// Fade the history first, then add this block's observations at
			// full weight. The timed run cloned base before the fade, so the
			// delta is only what that run observed.
			carried.Decay()
			carried.ApplyDelta(base, learned)
		} else if prior == PriorCarry && learnedAuto != nil {
			carried.Decay()
			carried.ApplyDelta(base, learnedAuto)
		}
		if sawAuto {
			crewPrior = nextCrew
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return nil
}

func benchRecord(env *BlockEnv, eng string, c, run int, row *Outcome) []string {
	active := row.ActiveC
	if active < 1 {
		active = c
		if eng == EngineSerial {
			active = 1
		}
	}
	trace := row.CTrace
	if trace == "" {
		trace = strconv.Itoa(active)
	}
	return []string{
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
		strconv.Itoa(active),
		trace,
		strconv.FormatInt(row.Counters.WaitNs, 10),
	}
}

func timedAuto(env *BlockEnv, pool *rfstate.Pool, base *rfstate.Learner, crewPrior int, oracle *Outcome) (*Outcome, *rfstate.Learner, error) {
	var before, after debug.GCStats
	debug.ReadGCStats(&before)
	learner := base.Clone()
	out, err := ExecAuto(env, pool, learner, crewPrior)
	debug.ReadGCStats(&after)
	if err != nil {
		return nil, nil, err
	}
	out.Counters.GCPauseNs = after.PauseTotal.Nanoseconds() - before.PauseTotal.Nanoseconds()
	if err := CheckAgainstSerial(oracle, out, env.World); err != nil {
		return nil, nil, err
	}
	return out, learner, nil
}

func timedRun(env *BlockEnv, eng string, pool *rfstate.Pool, c int, base *rfstate.Learner, oracle *Outcome) (*Outcome, *rfstate.Learner, error) {
	var before, after debug.GCStats
	debug.ReadGCStats(&before)
	var learner *rfstate.Learner
	if eng == EngineRF {
		learner = base.Clone()
	}
	out, err := ExecEngine(env, eng, pool, c, learner)
	debug.ReadGCStats(&after)
	if err != nil {
		return nil, nil, err
	}
	out.Counters.GCPauseNs = after.PauseTotal.Nanoseconds() - before.PauseTotal.Nanoseconds()
	if eng == EngineSerial {
		if err := CheckFixture(env, out); err != nil {
			return nil, nil, err
		}
		return out, nil, nil
	}
	if err := CheckAgainstSerial(oracle, out, env.World); err != nil {
		return nil, nil, err
	}
	return out, learner, nil
}
