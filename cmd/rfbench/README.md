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
# During an rf-auto block the process affinity and GOMAXPROCS shrink to the
# chosen arm (workers plus one coordinator CPU) and both are restored
# before the run returns. Fixed engines do not.
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
After `rf-auto` picks an arm it shrinks every thread's affinity and
`GOMAXPROCS` to that arm's workers plus one coordinator CPU, then restores
both before the run returns. Fixed engines are unchanged. Inactive workers
wait on the pool condition.

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

A key is fenced after its first invalidation. `WAIT_FINAL` is chosen only
when a lower producer exists and the expected wait is strictly shorter
than the conflict's re-execution, including the readers a bad pass would
invalidate:

```
E[wait] < P(conflict) * E[re-exec] * (1 + dependents)
```

`dependents` is the larger of the learned invalidation fanout and the
number of other readers already registered on the key. With no dependents
the factor is 1. Until both sides have a nanosecond estimate the choice
is PASS. The wait estimate is the measured park time after four samples,
otherwise half the successful producer's attempt. The re-execution
estimate is the wall time of an attempt that a write of that key
invalidated. The cold prior is Beta(1, 32) with no durations, so it
cannot satisfy the inequality. Nothing is injected from a global hot set.

### Learned worker count (`rf-auto`)

`-c` does not apply. One run per block uses at most as many workers as the
pin list, also capped by the `GOMAXPROCS` captured at block start, so an
explicit `GOMAXPROCS=1` stays at one. Inactive workers leave the scheduler
and wait on the pool condition; they are not parked inside `Step`. The
coordinator owns every later `GOMAXPROCS` write. After the arm `C` is
chosen, every OS thread is moved onto the CPUs of those `C` workers plus
one coordinator CPU when the pin list has one (the same footprint as a
fixed engine at that `C`). `GOMAXPROCS` becomes that footprint, capped by
the process cap captured at block start. The pool's active count stays at
`C`, so the extra P does not wake another worker. A generation-checked
loop applies later shrinks the same way, and `ExecAuto` restores both the
cap and the previous per-thread masks before it returns. A deferred
update carries the `Drive` generation and is ignored once the next block
has started. Fixed engines do not take this path; launch them as their
own process with `GOMAXPROCS=C+1`.

The count is one arm for the whole body, drawn from a geometric grid
inside the cap: powers of two, plus the cap when it is not a power of
two. Speedup and overhead are multiplicative. Sampling every integer
treated the gaps as signal; on a 32-wide cap that visited 6..31 and the
per-segment inflation model (unmeasured C stored as infl=1, which is as
cheap as C=1, plus a lower-confidence bonus) ended every trace at 1.

The reward is the body's wall nanoseconds per gas. The tail, entered when
more than two transactions remain in the block and at most two are left
ahead of the frontier, is not part of the reward. A guard that shrinks
the active count does not move the reward onto the shrunk count and does
not change `CrewBest`.

Each arm keeps a Welford mean. The prior pseudo-count is 1. An untried
arm's mean is `ref * priorRatio(C)`. `ref` is the C=1 nanoseconds per gas,
or 1 when nothing has been measured. On a block that can fill the arm,
`priorRatio(C) = 1 + 0.06*log2(C)^2` (1 at C=1, about 1.06 at C=2, 1.24 at
C=4, 2.5 at C=32). A plain `(1+log2(C))/sqrt(C)` is 1.5 at C=4, so one
sample of the ~1.4x speedup these blocks actually show cannot beat it,
and it is only ~1.06 at C=32, so Thompson noise draws that rung as often
as a neighbor. The quadratic log stays above the serial rate everywhere,
lets one 1.4x sample adopt C=4, and keeps an untried C=32 rare. If the
structural speedup cannot fill the arm, `(1+log2(C))/sqrt(speedup)`
replaces the quadratic when it is higher, so a sender chain does not
explore. `speedup` is work/CP from the structural model, capped by the
process limit, and is only this prior feature. With no samples there is
no draw. The first block uses the opening frontier (ready transactions
not blocked on an earlier one from the same sender), capped by the number
of distinct senders, or by PrevSame chain heads when senders were not
recovered. Distinct contracts do not lower it: one router and many
senders is still parallel. That probe is capped at 4. A cold model does
not open at 1 on a wide block, and it does not open at the process cap.
After that, each block draws once from Normal(posterior mean, variance
of the mean) over arms that have a sample and the untried grid neighbors
of those arms. A rung further out is not a candidate, so a model that
has tried 4 cannot jump to 32. Every fourth draw replaces a sampled
winner with one untried neighbor (the lower pessimistic mean), which is
how 2 and then 8 get a single bounded try. Clones of the same pre-block
model share the seed, so the K runs of one block pick the same arm.

The arm set is that grid for the cap only. The opening frontier width is
not an arm: a width of 25 on a cap of 32 used to add 25 to `{1,2,4,8,16,32}`.
The chosen arm is clamped down to the greatest grid rung that does not
exceed the width, so a single-sender chain still runs at 1.

In-block changes are guards only. The frontier width is an EWMA with
alpha 0.25; the integer cap moves when the average is a full worker away.
The active count follows that cap down only after the smoothed width has
stayed below it for 8ms, and not while a re-execution is still queued.
It does not climb back. The last two transactions still drop to the
frontier immediately; that tail is what drains a short block. An abort
storm (at least eight completions and more rollbacks than completions)
or sustained idle (idle time above elapsed*(active-1) after four
completions) halves the active count, again without climbing back and
without changing the arm.

Idle workers block on the scheduler condition. A commit signals only as
many waiters as there are newly ready tasks (the finishing worker takes
one itself). The coordinator and the advance queue wait on their own
conditions, so that signal is not stolen and is not a broadcast. Waking
every idle worker at C=32 left the Go scheduler spinning processors that
found no work. Shrinking the active count still wakes them once, so the
extra workers leave the block.

A block that is too small to pay for parallel startup runs at C=1 for
the whole block and uses the solo path. Before a serial rate exists that
is fewer than 48 transactions. Afterwards it is
`gas * serialRate < 2 * startupNs`, where `startupNs` starts at 2.5ms and
is an EMA of the wall a wider arm spent above the serial rate, capped at
half that wall. The EMA never falls below the smallest positive excess
measured so far, so a long run of blocks with no excess cannot decay the
threshold to zero. Solo is safe only because the active count never grows.
A block that opens above 1 stays off the solo path when a guard later
drops it to 1.

Ten blocks do not support a separate posterior per conflict class or
transaction-count bucket: the arms would starve. The only split the
measurements support is tiny versus the rest, and that split is the
startup rule above rather than a second bandit.

`CP` and `Work` are not the score. They only set `speedup`. `CP` is the
longest same-sender chain, extended by a soft cross-sender RAW chain on
a hot contract: its heaviest transaction plus `conflicts / (seen + 8)`
times the rest of its weight, after at least four transactions and only
when that rate exceeds 0.25. `Work` is the sum of per-transaction weights.
A weight is the selector's observed mean gas shrunk toward the gas-limit
prior, `(4*prior + n*mean) / (4+n)`, else the gas limit times the learned
actual-to-limit ratio. One observation does not replace the limit.

`-prior reset` clears only the per-key Beta learner. The cost model is
carried across blocks either way. Each of the K runs clones the pre-block
model. After the block the model is the last run's.

`model_c` is the arm chosen for the body. `model_pred_ns` is that arm's
opening posterior mean times the estimated gas, in nanoseconds, or 0 when
the opening plan had no measured rate. It is not refit to this block's
wall. `model_curve` is `ns:` or `gas:` followed by `c=v` pairs for the
grid arms in that same opening plan, so `model_pred_ns` is the pair for
`model_c` when the unit is `ns`.

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

A genuine coinbase balance read (`BALANCE`, `SELFBALANCE`, a transfer
from the coinbase, or the coinbase as sender) waits for the prefix and is
a reader of the fee aggregate. The scheduler checks that sum again before
the attempt can become final. `Empty` and `Exist` of the coinbase do not
wait: they are predicates, recomputed once every lower transaction is
final, and a mismatch requeues the attempt. A credit to the coinbase from
a call value, selfdestruct, or the transaction fee is recorded into that
same aggregate rather than loaded as a balance. The C=1 fast path, which
skips fences, is used only when the engine is statically one worker for
the whole block.

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
| learned C, Thompson arms and shrink guards, proc gate | `core/rfexec/crew.go`, `core/rfexec/proc.go`, `ExecAuto` |
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

- Per-key fence costs are measured waits and re-executions, not a second Thompson sampler. The worker-count policy is the Thompson sampler.
- No `FIN_LASTW`. Early publish covers a fenced key whose attempts usually
  wrote it once, and only while more than one worker is active. A second
  write of that key still invalidates readers of the previous bytes.
  Abandoned attempts retract the early version; there is no separate
  cascade for values derived from it beyond that invalidation.
- No ORDER hand-off.
- Learned C is one Thompson arm per block on the geometric grid above.
  The critical path is only a prior feature. There is no per-C inflation
  or fixed-cost regression on the decision path. Tail drain does not
  become the next block's prior. The first block of a process has no
  measured rate, so its `model_pred_ns` is 0. Its arm is the structural
  cold cap above, not 1 and not the process cap.
- Rollback restarts the transaction and fast-forwards by read sequence.
  Fast-forward skips waits; it still re-registers readers. Interpreter
  frames are not restored from the snapshot.
- Decay is a fixed prior-weight fade (33/34 per block), not a fitted
  empirical-Bayes model. Conflict counts are not decayed.
- A fence is PASS until both the wait and the re-execution have a
  nanosecond estimate, and PASS again when the measured wait is not
  strictly cheaper than the conflict probability times the re-execution.
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
