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
# With GOMAXPROCS unset, the process sets it once to the largest -c (here 8).
# rf-auto ignores -c. Its maximum is the pin list. GOMAXPROCS stays at that
# cap; the pool gates how many workers take tasks.
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
C**, with `GOMAXPROCS=C`. The CPU list is only the pin mask: it does not
raise `GOMAXPROCS` or the number of worker threads. If `GOMAXPROCS` is unset,
the process sets it once to the largest `-c` value and does not change it
again. `rf-auto` is one process whose `GOMAXPROCS` is the pin-list length
when the variable is unset. Inactive workers wait on the pool; the process
does not call `GOMAXPROCS` per trial.

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
    GOMAXPROCS=$c GOGC=100 ./rfbench \
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
pin list (also capped by `GOMAXPROCS`, which is only read, so an explicit
`GOMAXPROCS=1` stays at one). Inactive workers leave the scheduler and wait
on the pool condition; they are not parked inside `Step`. The process does
not change `GOMAXPROCS` when the active count changes. A deferred update
carries the `Drive` generation and is ignored once the next block has
started, so a late shrink cannot leave the pool at one worker.

The active count starts at the cross-block prior, capped by the structural
width and by that process limit. The width is the number of transactions
from the frontier onward that are ready or running, not parked on a fence,
and not blocked on an earlier transaction from the same sender. It is not
the number of remaining distinct senders. A cold prior of 0 starts at 1.
The next block's prior is `CrewBest`: the capped prior, or the trial with
the highest measured gas per nanosecond if a window closed. K runs all
start from the same pre-block prior. Tail drain may shrink the active
count (the trace can still end at 1) but it does not change `CrewBest`.

Samples are per-completion gas per wall nanosecond. A window closes after
one completion per active worker (at least two), once the standard error
is at most half the mean. A noisy pair stays open until more completions
shrink that error. The point estimate is gas over wall time for the whole
window, not the mean of the per-completion rates. Two windows at the
starting count form the baseline, and the bar is the faster of the two. A
later slower slice at that same count does not lower the bar. A probe that
beats the bar by more than the sum of the two standard errors records that
trial and doubles (or halves, when already at the cap). A probe that is
slower, or only inside that noise, returns to the best trial at once and
refines by one worker on the next window at that trial; if that step is
slower, it tries the other direction once. A window already at the best
trial that is inside the noise does not move, unless that refinement step
is still pending. The watchdog halves the active count on an abort storm
(no completion and at least one abort per active worker, or more aborts
than completions in a closed window) or on sustained idle while the
frontier is still wide enough, and that block does not climb back. The
shrink does not change the recorded best, so the next block retries it.

Early publication of a storage slot or nonce runs only when more than one
worker is active and the key is fenced with more single-write attempts than
multi-write attempts. A fixed C=1 run never takes that path. Reverting the
write inside the EVM journal retracts that version. A fence wait, prefix
wait, or same-sender park does not run the journal, so the scheduler marks
the attempt for retract and the next `prepare` drops those versions before
they can be folded. A selfdestruct or empty-account wipe retracts early
slot versions from the same transaction: the wipe and the slot would
otherwise share a tx index, and the slot would stay visible. Publishing
the same bytes again at transaction end does not invalidate readers.

### CSV

`block, engine, C, run, wall_ns, executions, rollbacks, invalidations, wait_final, wait_prefix, wait_defer, wait_order, wait_ns, idle_ns, gc_pause_ns, active_c, c_trace, park_ns`

`serial` is recorded once per run with `C=1`. `wait_ns` and `idle_ns` are
sums across workers. `wait_order` is reserved for the later ORDER fence and
stays zero in P0. Estimate waits in the OCC baseline are counted under
`wait_final`. `park_ns` is the same sum as `wait_ns` (time transactions
spent parked). `active_c` is the worker count at the end of the run.
`c_trace` is the sequence of trials, for example `1-2-4`. For `rf-auto` the
`C` column is the maximum the run was allowed to wake, not the chosen
count. Fixed engines set `active_c` and `c_trace` to that fixed C. After
every run the final accounts, storage, code, nonces, balances, and receipts
are compared to the serial oracle. A mismatch exits non-zero.

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
| learned C, width, hill-climb, watchdog | `core/rfexec/crew.go`, `ExecAuto` |
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
- Learned C is a multiplicative hill climb on gas per nanosecond, capped
  by the ready-transaction frontier, plus an abort/idle watchdog. It is
  not a fitted model of critical-path width. Tail drain does not become
  the next block's prior.
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
