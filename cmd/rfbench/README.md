# rfbench

RegionFence P0 and the benchmark harness. This is research code. The serial
block processor and the EIP-7928 parallel processor are unchanged.

## Base

The branch is cut from go-ethereum `master`, not from the latest release tag.
The pull request target is `master`, and `master` already carries Cancun,
Prague, and Osaka in `MainnetChainConfig`, plus the `vm.StateDB` methods the
prototype implements. A tag base would either miss post-Pectra blocks or show
up in the pull request as an unrelated tree of upstream commits.

## Build

```sh
go build -o rfbench ./cmd/rfbench
```

Go 1.25.0 is what this tree declares. `golang.org/x/sys` is already a
dependency and is used for `SchedSetaffinity`.

## Fixture format

Each block is a directory of gzipped JSON produced by `uploads/rpcdump.py`
(the RPC URL comes from `ETH_RPC_URL`; this tool does not dial a node):

| file | source |
| --- | --- |
| `block.json.gz` | `eth_getBlockByNumber` with full transactions |
| `prestate.json.gz` | `debug_traceBlockByNumber` prestateTracer, one entry per transaction |
| `receipts.json.gz` | `eth_getBlockReceipts` |
| `blockhashes.json.gz` | 256 ancestor hashes, decimal block number to hash |

The block pre-state is the union of per-transaction prestates, keeping the
first-seen account and slot in transaction order. System-contract code
(EIP-4788 / 2935 / 7002 / 7251) is seeded from `params` when the fork is
active and the tracer did not include it. Storage is not invented.

Two archives use this layout. `fixtures-a` holds Cancun blocks 22018250,
22102250, 22194250, 22411250, and 22418000. `fixtures-b` holds Cancun blocks
19951808, 20058000, and 20361898, plus post-Pectra blocks 26060000 and
26061000 (Osaka and BPO2 on mainnet). Tests read `/tmp/fixa` and `/tmp/fixb`
when `RF_FIXTURES` is unset, and skip a tree that is not present.

State is memory-only. The timed region is pre-execution, user transactions,
post-execution, and withdrawals. Loading, state-root calculation, and trie
commit are outside it. The Go KZG context (`kzg4844.UseCKZG(false)`) is
initialized before the timer: the first point-evaluation precompile otherwise
spends about two seconds inside whichever run touches it. That is library
setup, not an engine warm-up. Engine state is still fresh on every timed run.

## Run

```sh
# this VM (4 cores). Pass both fixture trees; they are sorted by block number.
# With GOMAXPROCS unset, the process sets it once to the largest -c (here 8),
# or to the worker pin list when rf-auto is selected. rf-auto ignores -c.
# During an rf-auto block the coordinator loop sets GOMAXPROCS to the active
# count and restores the cap before the run returns. Fixed engines do not.
GOGC=100 ./rfbench \
  -fixtures /path/to/fixa,/path/to/fixb \
  -engines serial,occ,rf,rf-auto \
  -c 1,2,4,8 \
  -k 1 \
  -cpus 0,1,2,3 \
  -prior reset \
  -gogc 100 \
  -out results.csv
```

256-core host, pinned to CPUs 128-255. Run **one process per engine and
C**. The CPU list is only the pin mask: it does not raise `GOMAXPROCS` or
the number of worker threads. If `GOMAXPROCS` is unset, the process sets it
once to the largest `-c` value (for `rf-auto`, to the worker pin list).
Fixed engines should set `GOMAXPROCS=C+1` so the coordinator has a P that
is not one of the pinned workers. `rf-auto` must not use that +1: its cap
is `min(pin list, GOMAXPROCS)`, and an extra P would wake an extra worker.
During an auto block the coordinator loop tracks `GOMAXPROCS` to the active
count and restores the cap before the run returns. Inactive workers wait
on the pool condition.

The default does **not** call `LockOSThread` on the coordinator. It asks
the OS to prefer the current thread on the first last-level cache, and the
goroutine is free to migrate inside that set. `-pin-coordinator` is
optional. It locks the main goroutine to the first CPU of that cache and
starts workers on the rest. On ict21, with `GOMAXPROCS=C`, that lock was
slower than an unpinned coordinator floating in the same CCX: fixed rf
C=4 about 22%, C=8 about 11%, occ C=4 about 25%. `GOMAXPROCS=C+1` recovered
C=4 and did not recover C=8. Leave the flag off unless you are measuring
that placement.

`rf-auto`, one process, `GOMAXPROCS` unset so the cap is the pin list:

```sh
cpus=$(seq -s, 128 255)
GOGC=100 ./rfbench \
  -fixtures /path/to/fixa,/path/to/fixb \
  -engines rf-auto \
  -k 10 \
  -cpus "$cpus" \
  -prior carry \
  -gogc 100 \
  -out results-rf-auto-carry.csv
```

The same command with `-prior reset` clears the per-key Beta learner. The
worker-count model is carried either way. Run a second process with
`-out results-rf-auto-reset.csv` when the comparison needs a cold Beta prior.

The pin list is compacted by last-level cache before workers start. The
group key is `shared_cpu_list` of the highest-index cache under
`/sys/devices/system/cpu/cpuN/cache`. On the EPYC hosts used for the scan
that list is a CCX (eight CPUs); the size is read from sysfs and is not
hardcoded. Groups are ordered by their smallest CPU id, and CPUs inside a
group are sorted. `NewPool` pins worker `i` to `ordered[i % len]`. The
active set is workers `0..k-1`, so it is a prefix of that list and fills
one cache before spilling. A list such as 129-136, which straddles
128-135 and 136-143, is reordered so 129-135 come first and 136 is last.
Missing sysfs keeps the given order, and worker `i` is then pinned to the
caller's `i`-th CPU. The stderr line `groups=` prints one label per group
in that pin order.

```sh
cpus=$(seq -s, 128 255)
for eng in serial occ rf; do
  for c in 1 2 4 8 16 32 64 128; do
    if [ "$eng" = serial ] && [ "$c" != 1 ]; then
      continue
    fi
    # C+1 leaves the coordinator a P. Do not pass -pin-coordinator.
    GOMAXPROCS=$((c + 1)) GOGC=100 ./rfbench \
      -fixtures /path/to/fixa,/path/to/fixb \
      -engines "$eng" \
      -c "$c" \
      -k 10 \
      -cpus "$cpus" \
      -prior carry \
      -gogc 100 \
      -out "results-${eng}-c${c}.csv"
  done
done
```

There is no warm-up. Each timed run uses a fresh state and ledger.

### Prior

Blocks are sorted by number. Priors for block N come only from blocks below N.

- `reset`: every timed run starts from an empty per-key Beta prior.
- `carry`: each timed RegionFence run clones the pre-block posterior, so
  in-block updates do not leak into sibling runs or into that block's own
  prior. After the block's timed runs, the carried posterior is decayed and
  then updated with the observations of one timed run (the highest C in this
  process; K runs are not summed). Decay multiplies the excess over the
  Beta(1, 32) prior by 33/34, so the prior is the stationary point: a run of
  safe observations cannot push a key's mean to zero without bound, and a
  key that stops conflicting falls back toward the prior. Conflict counts
  are not decayed, so a key stays fenced after its first invalidation.
  An untimed C=1 pass is not used. That pass only records safe reads and
  used to drift every key to PASS. A single-block command has no earlier
  block, so the prior is empty even in `carry` mode.

The P0 rule is greedy: a key is fenced after its first invalidation, and
`WAIT_FINAL` is chosen when a lower producer exists and the posterior mean
exceeds 1/2. The cold prior is Beta(1, 32). Nothing is injected from a
global hot set.

### Learned worker count (`rf-auto`)

`-c` does not apply. One run per block uses at most as many workers as the
pin list, also capped by the `GOMAXPROCS` captured at block start, so an
explicit `GOMAXPROCS=1` stays at one. Inactive workers leave the scheduler
and wait on the pool condition; they are not parked inside `Step`. The
coordinator owns every later `GOMAXPROCS` write: it sets the count to the
plan before workers are released, a generation-checked loop applies later
changes, and `ExecAuto` restores the cap before it returns. A deferred
update carries the `Drive` generation and is ignored once the next block
has started.

The count minimises

```
T(C) = fixedNs*infl(C) + nTx*txFixed*infl(C) + max(CP, Work/C)*(1+r(C))*base*infl(C)
```

over every integer `C` from 1 to the smoothed frontier, capped by the
process limit. Until `base` is known, `T` is the gas-equivalent
`max(CP, Work/C)*(1+r)` and `model_pred_ns` is 0.

`CP` is the longest chain still ahead of the frontier. Same-sender edges
come from the nonce order. A contract with cross-sender read-after-write
adds a soft chain: its heaviest transaction plus
`conflicts / (seen + 8)` times the rest of its weight, after at least four
transactions and only when that rate exceeds 0.25. Unseen contracts add
nothing, and the rate does not turn the whole contract into one sender chain.
`Work` is the sum of per-transaction weights. A weight is the selector's
observed mean gas shrunk toward the gas-limit prior,
`(4*prior + n*mean) / (4+n)`, else the gas limit times the learned
actual-to-limit ratio. One observation does not replace the limit.
`r(C)` is the learned re-execution rate. `base` is process CPU nanoseconds
per gas at C=1. `infl(C)` is CPU-per-gas at that C divided by `base`,
clamped to `[0.25, 8]`. It is not `rate(C)/rate(1)`: that ratio mixes a
parallelism mistake into the slowdown. `fixedNs` and `txFixed` are the
C=1 wall that `base*gas` does not explain, plus time outside the segments.

Each time the active count changes, the wall, CPU, and gas of the segment
that just ended are attributed to the C that was actually running. An
abandoned probe updates `rate(C)` and `infl(C)` for that probe. A tail
segment, and a body segment whose frontier width was below C, do not move
`rate(C)`. The narrow body segment still takes a fractional sample so the
next block does not open the same unmeasured probe. A short segment
(under 200k gas) moves the EMA by `gas/(gas+500k)` and adds 0.25 of a
sample; a longer one adds a full sample.

The opening choice minimises `T(C) * (1 - 0.35/sqrt(samples(C)+1))`.
Unmeasured C stay eligible, so the first block's choice cannot be the only
C that is ever sampled. That opening value is the one exploration for the
block. It is held until the first completion checkpoint even if the width
EWMA dips, unless the live frontier drops below C, in which case the
segment is closed and the count drops. The checkpoint folds the segment
into the model and re-picks by the posterior mean, staying on the
incumbent unless another C is more than 5% better. Checkpoints are
completions `n/4`, `n/2`, and `3n/4` when `n >= 8`, or `n/2` when
`2 <= n < 8`. The frontier width is an EWMA with alpha 0.25. The integer
cap moves only when the average is a full worker away from the cap. When
more than two transactions remain in the block and at most two are left
ahead of the frontier, the active count may shrink. That tail does not
change `CrewBest` and is not a rate sample.

If `base` was seeded from a C other than 1, the next block's one explore
is C=1 so inflation can be rescaled onto the serial clock.

`-prior reset` clears only the per-key Beta learner. The cost model is
carried across blocks either way. Each of the K runs clones the pre-block
model. After the block the model is the last run's.

`model_c` is the body choice at the end of the run (the opening choice,
unless a checkpoint abandoned it). `model_pred_ns` is the opening
full-block `T` for that C in nanoseconds, or 0 when the opening plan had
no `base` yet. It is not refit to this block's wall. `model_curve` is
`ns:` or `gas:` followed by `c=v` pairs for that same opening plan, so
`model_pred_ns` is the pair for `model_c` when that C was scored up front.

Early publication of a storage slot or nonce runs only when the block is
not statically single-worker and the key is fenced with more single-write
attempts than multi-write attempts. A fixed C=1 run never takes that path.
`rf-auto` does not, even while the active count is 1: another worker's
attempt can still be in flight. Reverting the write inside the EVM journal
retracts that version. A fence wait, prefix wait, or same-sender park does
not run the journal, so the scheduler marks the attempt for retract and
the next `prepare` drops those versions before they can be folded. A
selfdestruct or empty-account wipe retracts early slot versions from the
same transaction: the wipe and the slot would otherwise share a tx index,
and the slot would stay visible. Publishing the same bytes again at
transaction end does not invalidate readers.

A coinbase balance read is a reader of the fee aggregate, not only of the
prefix fence. The scheduler checks the observed prefix again before the
attempt can become final. The C=1 fast path, which skips fences, is used
only when the engine is statically one worker for the whole block.

### CSV

`block, engine, C, run, wall_ns, executions, rollbacks, invalidations, wait_final, wait_prefix, wait_defer, wait_order, wait_ns, idle_ns, gc_pause_ns, active_c, c_trace, park_ns, model_c, model_pred_ns, model_curve`

`serial` is recorded once per run with `C=1`. `wait_ns` and `idle_ns` are
sums across workers. `wait_order` is reserved for the later ORDER fence and
stays zero in P0. Estimate waits in the OCC baseline are counted under
`wait_final`. `park_ns` is the same sum as `wait_ns` (time transactions
spent parked). `active_c` is the worker count at the end of the run.
`c_trace` is the sequence of active counts, for example `4-1` when the body
ran at 4 and the tail drained to 1. For `rf-auto` the `C` column is the
maximum the run was allowed to wake, not the chosen count. `model_c` is
the body choice. `active_c` is the count at the end of the run, which is
often the tail. Fixed engines set `active_c` and `c_trace` to that fixed C
and leave the model columns at zero or empty. After every run the final
accounts, storage, code, nonces, balances, and receipts are compared to
the serial oracle. A mismatch exits non-zero.

## Design to code

| design | code |
| --- | --- |
| multi-version ledger, reader registry, writer push | `core/rfstate/ledger.go` |
| `vm.StateDB` over the ledger, interpreter unchanged | `core/rfstate/txview.go` |
| PASS / WAIT_FINAL / WAIT_PREFIX | `TxView.fence` |
| DEFER_TX for same-sender nonce chains | `sched.pickLocked` |
| cooperative yield (no busy-wait, no pinned-thread block) | `Signal` panic, recovered in `sched.execute` |
| AT_FINISH publish, P0 restart + fast-forward | `TxView.Publish`, `ffUntil` |
| coinbase fee recorded per transaction; prefix wait only on a real balance read | `addCoinbaseFee`, `Ledger.RecordFee`, `Fold` |
| per-key Beta, greedy P0 rule | `core/rfstate/learner.go` |
| fixed C, pool can change C, pinned persistent workers | `core/rfstate/pool.go` |
| learned C, list-scheduling cost model, proc gate | `core/rfexec/crew.go`, `core/rfexec/proc.go`, `ExecAuto` |
| pin active workers onto the fewest last-level caches | `core/rfstate/topology.go` |
| early publish of a single-write hot key when C>1 | `TxView.SetState`, `TxView.SetNonce` |
| frontier termination (no lone tail) | `sched.tryAdvanceLocked`, `Pool` stays until `Done` |
| Block-STM OCC baseline on the same plumbing | `sched` in `ModeOCC` |
| serial oracle | `ExecSerial` via `ApplyTransactionWithEVM` |
| ProcessRegionFence beside the existing processor | `rfexec.ProcessRegionFence` |

Correctness for RegionFence is writer-push invalidation to the region
checkpoint, not read-set revalidation. The OCC baseline does validate and
re-execute. Both refuse to spin when a producer has already finished: a
nonce or estimate miss parks on the lower transaction.

## Known gaps versus design section 6

- Greedy threshold instead of Thompson sampling.
- No `FIN_LASTW`. Early publish covers a fenced key whose attempts usually
  wrote it once, and only while more than one worker is active. A second
  write of that key still invalidates readers of the previous bytes.
  Abandoned attempts retract the early version; there is no separate
  cascade for values derived from it beyond that invalidation.
- No ORDER hand-off.
- Learned C is the list-scheduling model above: critical path (including
  a hot-contract RAW chain), work, a C=1 baseline, a per-C CPU inflation,
  a fixed overhead, and a re-execution rate. It is not a per-transaction
  simulator. Tail drain does not become the next block's prior. The first
  block of a process has no baseline, so its `model_pred_ns` is 0.
- Rollback restarts the transaction and fast-forwards by read sequence.
  Fast-forward skips waits; it still re-registers readers. Interpreter
  frames are not restored from the snapshot.
- Decay is a fixed prior-weight fade (33/34 per block), not a fitted
  empirical-Bayes model. Conflict counts are not decayed.
- Replay cost and wait cost are both 1.
- OCC parks on a nonce producer instead of retrying immediately, so it is
  not a byte-for-byte Block-STM loop. The change is there to avoid the
  known livelock.
- System-contract storage is absent from the fixtures. Only canonical code
  is seeded.
- EIP-2935 is given `header.ParentHash`. A synthetic parent header's
  `Hash()` is not the real parent hash.
- Pre/post system calls use a dummy block access list so the exported
  helpers do not merge into a nil receiver. `state_processor.go` is not
  modified.
- No delta or commutative fee trick, and no EIP-7928 block access list.
