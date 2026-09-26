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
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// EngineSerial is the unmodified ApplyTransaction path.
	EngineSerial = "serial"
	// EngineOCC is the Block-STM-style baseline.
	EngineOCC = "occ"
	// EngineRF is RegionFence P0.
	EngineRF = "rf"

	maxAttempts = 10000
	// maxSettledSpins caps immediate re-executions of a wait whose producer
	// is already finished. A correct wait parks. Spinning here is the
	// nonce/ESTIMATE livelock: the read condition did not clear.
	maxSettledSpins = 8
)

type status uint8

const (
	stReady status = iota
	stRunning
	stFinished
	stParked
	stFinal
)

// ProcessRegionFence executes one block with RegionFence P0.
// The pool is persistent across calls; workers is the active count C.
// learner is mutated with this block's observations. Pass a clone when the
// caller's prior must be preserved. Nil learner uses an empty prior.
func ProcessRegionFence(env *BlockEnv, pool *rfstate.Pool, workers int, learner *rfstate.Learner) (*Outcome, error) {
	return execParallel(env, rfstate.ModeRF, pool, workers, learner)
}

// ExecEngine runs one named engine. serial ignores the pool and the learner.
func ExecEngine(env *BlockEnv, engine string, pool *rfstate.Pool, workers int, learner *rfstate.Learner) (*Outcome, error) {
	switch engine {
	case EngineSerial:
		return ExecSerial(env)
	case EngineOCC:
		return execParallel(env, rfstate.ModeOCC, pool, workers, nil)
	case EngineRF:
		return execParallel(env, rfstate.ModeRF, pool, workers, learner)
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
}

func execParallel(env *BlockEnv, mode rfstate.Mode, pool *rfstate.Pool, workers int, learner *rfstate.Learner) (*Outcome, error) {
	if pool == nil {
		return nil, fmt.Errorf("nil worker pool")
	}
	if workers < 1 {
		workers = 1
	}
	if learner == nil && mode == rfstate.ModeRF {
		learner = rfstate.NewLearner()
	}
	store := rfstate.NewStore(env.World)
	warmCrypto()
	t0 := time.Now()
	pre := directView(store, env.Header.Coinbase)
	if err := runPre(env, newEVM(env, pre)); err != nil {
		return nil, err
	}
	s := newSched(env, mode, store, learner)
	pool.Drive(workers, s.Step, s.Done)
	err := s.wait()
	pool.Release()
	if err != nil {
		return nil, err
	}
	s.ledger.Fold(store, env.Header.Coinbase)
	s.ledger.ClearVersions()
	receipts := s.receipts()
	var logs []*types.Log
	for _, r := range receipts {
		logs = append(logs, r.Logs...)
	}
	post := directView(store, env.Header.Coinbase)
	if err := runPost(env, newEVM(env, post), logs); err != nil {
		return nil, err
	}
	runWithdrawals(env, directView(store, env.Header.Coinbase))
	wall := time.Since(t0)
	root := types.DeriveSha(types.Receipts(receipts), newStackTrie())
	var gas uint64
	for _, r := range receipts {
		gas += r.GasUsed
	}
	return &Outcome{
		Receipts: receipts,
		GasUsed:  gas,
		Root:     root,
		Store:    store,
		Counters: s.counters(),
		Wall:     wall,
	}, nil
}

type txRec struct {
	status     status
	attempt    uint64
	abort      atomic.Bool
	ffUntil    int
	evm        *vm.EVM
	retract    bool
	estimate   bool
	validated  bool
	gasUsed    uint64
	failed     bool
	logs       []*types.Log
	reads      []rfstate.OccRead
	pass       []rfstate.Key
	park       int
	parkKind   rfstate.SigKind
	waiting    bool
	waitStart  time.Time
	deferNoted bool
	// settledSpins counts wait signals whose producer was already done.
	settledSpins int
	// lowerFinal is true when this attempt started with every lower
	// transaction already final. A consensus error on that attempt is real.
	// An error from an earlier attempt can be a stale speculative read.
	lowerFinal bool
}

type sched struct {
	mu       sync.Mutex
	cv       *sync.Cond
	env      *BlockEnv
	mode     rfstate.Mode
	store    *rfstate.Store
	ledger   *rfstate.Ledger
	learner  *rfstate.Learner
	txs      []txRec
	frontier int
	inflight int
	fatal    error
	ctr      Counters
}

func newSched(env *BlockEnv, mode rfstate.Mode, store *rfstate.Store, learner *rfstate.Learner) *sched {
	s := &sched{
		env:     env,
		mode:    mode,
		store:   store,
		learner: learner,
		txs:     make([]txRec, len(env.Txs)),
	}
	s.cv = sync.NewCond(&s.mu)
	s.ledger = rfstate.NewLedger(len(env.Txs), learner, s.onVictim)
	return s
}

func (s *sched) PrefixFinal(tx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// frontier is the first transaction that is not yet final, so every
	// index below it is final. Tx 0 has an empty prefix when frontier is 0.
	return s.frontier >= tx
}

func (s *sched) TxSettled(tx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx < 0 || tx >= len(s.txs) {
		return true
	}
	st := s.txs[tx].status
	return st == stFinished || st == stFinal
}

func (s *sched) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneLocked()
}

func (s *sched) doneLocked() bool {
	return s.fatal != nil || (s.frontier == len(s.txs) && s.inflight == 0)
}

func (s *sched) wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.doneLocked() {
		s.cv.Wait()
	}
	return s.fatal
}

func (s *sched) counters() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctr
}

func (s *sched) Step(worker int) {
	s.mu.Lock()
	for {
		if s.doneLocked() {
			s.mu.Unlock()
			return
		}
		idx := s.pickLocked()
		if idx < 0 {
			start := time.Now()
			s.cv.Wait()
			s.ctr.IdleNs += time.Since(start).Nanoseconds()
			continue
		}
		attempt := s.txs[idx].attempt
		ff := s.txs[idx].ffUntil
		s.txs[idx].lowerFinal = s.frontier >= idx
		s.txs[idx].status = stRunning
		s.txs[idx].abort.Store(false)
		s.inflight++
		s.ctr.Executions++
		s.mu.Unlock()
		s.execute(idx, attempt, ff)
		s.mu.Lock()
	}
}

func (s *sched) pickLocked() int {
	for i := range s.txs {
		if s.txs[i].status != stReady {
			continue
		}
		if s.mode == rfstate.ModeRF {
			prev := s.env.PrevSame[i]
			if prev >= 0 {
				st := s.txs[prev].status
				if st != stFinished && st != stFinal {
					if !s.txs[i].deferNoted {
						s.txs[i].deferNoted = true
						s.ctr.WaitDefer++
					}
					continue
				}
			}
		}
		return i
	}
	return -1
}

func (s *sched) execute(idx int, attempt uint64, ff int) {
	defer func() {
		if rec := recover(); rec != nil {
			sig, ok := rec.(rfstate.Signal)
			if !ok {
				s.mu.Lock()
				s.fatal = fmt.Errorf("block %d tx %d panic: %v", s.env.Number, idx, rec)
				s.inflight--
				s.cv.Broadcast()
				s.mu.Unlock()
				return
			}
			s.onSignal(idx, attempt, sig)
		}
	}()
	s.prepare(idx)
	view := rfstate.NewTxView(s.mode, idx, attempt, ff, s.store, s.ledger, s.learner, s, &s.txs[idx].abort, s.env.Header.Coinbase)
	evm := newEVM(s.env, view)
	s.mu.Lock()
	if s.txs[idx].attempt != attempt || s.txs[idx].status != stRunning {
		s.inflight--
		s.cv.Broadcast()
		s.mu.Unlock()
		return
	}
	s.txs[idx].evm = evm
	s.mu.Unlock()

	tx := s.env.Txs[idx]
	view.SetTxContext(tx.Hash(), idx, uint32(idx+1))
	evm.SetTxContext(core.NewEVMTxContext(s.env.Msgs[idx]))
	gp := core.NewGasPool(s.env.Header.GasLimit)
	result, err := core.ApplyMessage(evm, s.env.Msgs[idx], gp)
	if s.superseded(idx, attempt) {
		return
	}
	if s.aborted(idx, attempt, evm) {
		s.discard(idx, attempt)
		return
	}
	if err != nil {
		s.onErr(idx, attempt, err)
		return
	}
	view.Finalise(evm.GetRules())
	if s.aborted(idx, attempt, evm) {
		s.discard(idx, attempt)
		return
	}
	view.Publish()
	s.finish(idx, attempt, view, result)
}

func (s *sched) prepare(idx int) {
	s.mu.Lock()
	retract := s.txs[idx].retract
	estimate := s.txs[idx].estimate
	s.txs[idx].retract = false
	s.txs[idx].estimate = false
	s.mu.Unlock()
	s.ledger.RemoveReaders(idx)
	if retract {
		s.ledger.Retract(idx)
	}
	if estimate {
		s.ledger.MarkEstimate(idx)
	}
}

func (s *sched) superseded(idx int, attempt uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs[idx].attempt == attempt && s.txs[idx].status == stRunning {
		return false
	}
	s.inflight--
	s.cv.Broadcast()
	return true
}

func (s *sched) aborted(idx int, attempt uint64, evm *vm.EVM) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs[idx].attempt != attempt {
		return false
	}
	return s.txs[idx].abort.Load() || (evm != nil && evm.Cancelled())
}

func (s *sched) onSignal(idx int, attempt uint64, sig rfstate.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs[idx].attempt != attempt || s.txs[idx].status != stRunning {
		s.inflight--
		s.cv.Broadcast()
		return
	}
	if s.txs[idx].abort.Load() {
		sig.Kind = rfstate.SigRollback
	}
	s.inflight--
	s.ctr.Rollbacks++
	s.txs[idx].evm = nil
	switch sig.Kind {
	case rfstate.SigWaitFinal, rfstate.SigWaitEstimate, rfstate.SigWaitPrefix:
		s.noteWaitLocked(sig.Kind)
		s.txs[idx].ffUntil = sig.Seq
		s.txs[idx].park = sig.Depend
		s.txs[idx].parkKind = sig.Kind
		s.txs[idx].status = stParked
		s.txs[idx].waiting = true
		s.txs[idx].waitStart = time.Now()
		if s.parkDoneLocked(idx) {
			// The producer is already done. One retry is enough when the
			// read condition cleared; repeating means the marker is stuck.
			s.txs[idx].waiting = false
			s.txs[idx].status = stReady
			s.txs[idx].settledSpins++
			if s.txs[idx].settledSpins > maxSettledSpins {
				s.fatal = fmt.Errorf("block %d tx %d spinning on settled producer (signal %d depend %d)", s.env.Number, idx, sig.Kind, sig.Depend)
			}
		} else {
			s.txs[idx].settledSpins = 0
		}
		s.txs[idx].attempt++
	default:
		s.requeueRunningLocked(idx)
	}
	if s.txs[idx].attempt > maxAttempts {
		s.fatal = fmt.Errorf("block %d tx %d exceeded %d attempts (last signal %d depend %d)", s.env.Number, idx, maxAttempts, sig.Kind, sig.Depend)
	}
	s.wakeLocked()
	s.cv.Broadcast()
}

func (s *sched) noteWaitLocked(k rfstate.SigKind) {
	switch k {
	case rfstate.SigWaitFinal, rfstate.SigWaitEstimate:
		s.ctr.WaitFinal++
	case rfstate.SigWaitPrefix:
		s.ctr.WaitPrefix++
	}
}

func (s *sched) onErr(idx int, attempt uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs[idx].attempt != attempt || s.txs[idx].status != stRunning {
		s.inflight--
		s.cv.Broadcast()
		return
	}
	s.inflight--
	s.txs[idx].evm = nil
	if s.txs[idx].lowerFinal {
		s.fatal = fmt.Errorf("block %d tx %d: %w", s.env.Number, idx, err)
		s.cv.Broadcast()
		return
	}
	s.ctr.Rollbacks++
	prev := s.env.PrevSame[idx]
	if prev >= 0 {
		st := s.txs[prev].status
		if st != stFinished && st != stFinal {
			s.noteWaitLocked(rfstate.SigWaitFinal)
			s.txs[idx].park = prev
			s.txs[idx].parkKind = rfstate.SigWaitFinal
			s.txs[idx].status = stParked
			s.txs[idx].waiting = true
			s.txs[idx].waitStart = time.Now()
			s.txs[idx].ffUntil = 0
			if !s.txs[idx].deferNoted {
				s.txs[idx].deferNoted = true
				s.ctr.WaitDefer++
			}
			s.wakeLocked()
			s.cv.Broadcast()
			return
		}
	}
	s.requeueRunningLocked(idx)
	if s.txs[idx].attempt > maxAttempts {
		s.fatal = fmt.Errorf("block %d tx %d exceeded %d attempts: %w", s.env.Number, idx, maxAttempts, err)
	}
	s.wakeLocked()
	s.cv.Broadcast()
}

func (s *sched) discard(idx int, attempt uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs[idx].attempt != attempt || s.txs[idx].status != stRunning {
		s.inflight--
		s.cv.Broadcast()
		return
	}
	s.inflight--
	s.ctr.Rollbacks++
	s.txs[idx].evm = nil
	s.requeueRunningLocked(idx)
	if s.txs[idx].attempt > maxAttempts {
		s.fatal = fmt.Errorf("block %d tx %d exceeded %d attempts", s.env.Number, idx, maxAttempts)
	}
	s.wakeLocked()
	s.cv.Broadcast()
}

func (s *sched) requeueRunningLocked(idx int) {
	s.txs[idx].attempt++
	s.txs[idx].abort.Store(false)
	s.txs[idx].settledSpins = 0
	s.txs[idx].ffUntil = 0
	s.txs[idx].validated = false
	s.txs[idx].status = stReady
	if s.mode == rfstate.ModeOCC {
		s.txs[idx].estimate = true
	} else {
		s.txs[idx].retract = true
	}
}

func (s *sched) finish(idx int, attempt uint64, view *rfstate.TxView, result *core.ExecutionResult) {
	if s.mode == rfstate.ModeOCC {
		s.bump(idx, view.WroteKeys())
	}
	s.mu.Lock()
	if s.txs[idx].attempt != attempt || s.txs[idx].status != stRunning || s.txs[idx].abort.Load() {
		s.mu.Unlock()
		s.ledger.Retract(idx)
		s.discard(idx, attempt)
		return
	}
	s.txs[idx].gasUsed = result.UsedGas
	s.txs[idx].failed = result.Failed()
	s.txs[idx].logs = append([]*types.Log(nil), view.Logs()...)
	s.txs[idx].reads = append([]rfstate.OccRead(nil), view.OccReads()...)
	s.txs[idx].pass = append([]rfstate.Key(nil), view.PassKeys()...)
	s.txs[idx].validated = s.mode != rfstate.ModeOCC
	s.txs[idx].settledSpins = 0
	s.txs[idx].status = stFinished
	s.txs[idx].evm = nil
	s.inflight--
	safe := s.tryAdvanceLocked()
	s.wakeLocked()
	s.cv.Broadcast()
	s.mu.Unlock()
	if s.learner != nil {
		for _, k := range safe {
			s.learner.ObserveSafe(k)
		}
	}
}

func (s *sched) tryAdvanceLocked() []rfstate.Key {
	var safe []rfstate.Key
	for s.frontier < len(s.txs) {
		t := &s.txs[s.frontier]
		if t.status == stFinal {
			s.frontier++
			continue
		}
		if t.status != stFinished {
			return safe
		}
		if s.mode == rfstate.ModeOCC && !t.validated {
			ok, est := s.validateLocked(s.frontier)
			if est >= 0 {
				t.status = stParked
				t.park = est
				t.parkKind = rfstate.SigWaitEstimate
				t.waiting = true
				t.waitStart = time.Now()
				s.ctr.WaitFinal++
				return safe
			}
			if !ok {
				s.ctr.Rollbacks++
				s.requeueFinishedLocked(s.frontier)
				return safe
			}
			t.validated = true
		}
		if s.mode == rfstate.ModeRF {
			safe = append(safe, t.pass...)
		}
		t.status = stFinal
		s.frontier++
	}
	return safe
}

func (s *sched) validateLocked(tx int) (bool, int) {
	for _, r := range s.txs[tx].reads {
		res := s.ledger.Read(tx, s.txs[tx].attempt, r.Key, false)
		if res.Estimate {
			return false, res.EstTx
		}
		if res.FromVersion != r.FromVersion || res.ObsTx != r.ObsTx || !bytes.Equal(res.Data, r.Data) {
			return false, -1
		}
	}
	return true, -1
}

func (s *sched) requeueFinishedLocked(idx int) {
	s.txs[idx].attempt++
	s.txs[idx].abort.Store(false)
	s.txs[idx].settledSpins = 0
	s.txs[idx].ffUntil = 0
	s.txs[idx].validated = false
	s.txs[idx].status = stReady
	if s.mode == rfstate.ModeOCC {
		s.txs[idx].estimate = true
	} else {
		s.txs[idx].retract = true
	}
	if s.frontier > idx {
		s.frontier = idx
	}
}

func (s *sched) parkDoneLocked(idx int) bool {
	t := &s.txs[idx]
	switch t.parkKind {
	case rfstate.SigWaitPrefix:
		return s.frontier >= idx
	case rfstate.SigWaitFinal, rfstate.SigWaitEstimate:
		if t.park < 0 || t.park >= len(s.txs) {
			return true
		}
		st := s.txs[t.park].status
		return st == stFinished || st == stFinal
	default:
		return true
	}
}

func (s *sched) wakeLocked() {
	for i := range s.txs {
		if s.txs[i].status != stParked {
			continue
		}
		if !s.parkDoneLocked(i) {
			continue
		}
		if s.txs[i].waiting {
			s.ctr.WaitNs += time.Since(s.txs[i].waitStart).Nanoseconds()
			s.txs[i].waiting = false
		}
		s.txs[i].status = stReady
	}
}

func (s *sched) onVictim(v rfstate.Victim) {
	s.mu.Lock()
	ev := s.armLocked(v.Tx, v.Attempt)
	s.mu.Unlock()
	if ev != nil {
		ev.Cancel()
	}
}

func (s *sched) bump(writer int, keys []rfstate.Key) {
	if len(keys) == 0 {
		return
	}
	set := make(map[rfstate.Key]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	s.mu.Lock()
	var evms []*vm.EVM
	for j := writer + 1; j < len(s.txs); j++ {
		if !readsHit(s.txs[j].reads, set) {
			continue
		}
		if ev := s.armLocked(j, s.txs[j].attempt); ev != nil {
			evms = append(evms, ev)
		}
	}
	s.mu.Unlock()
	for _, ev := range evms {
		ev.Cancel()
	}
}

func readsHit(reads []rfstate.OccRead, keys map[rfstate.Key]struct{}) bool {
	for _, r := range reads {
		if _, ok := keys[r.Key]; ok {
			return true
		}
	}
	return false
}

func (s *sched) armLocked(tx int, attempt uint64) *vm.EVM {
	if tx < 0 || tx >= len(s.txs) {
		return nil
	}
	t := &s.txs[tx]
	if t.attempt != attempt || t.status == stFinal {
		return nil
	}
	s.ctr.Invalidations++
	ev := t.evm
	if t.status == stRunning {
		t.abort.Store(true)
		s.cv.Broadcast()
		return ev
	}
	t.attempt++
	t.abort.Store(false)
	t.ffUntil = 0
	t.validated = false
	t.status = stReady
	if s.mode == rfstate.ModeOCC {
		t.estimate = true
	} else {
		t.retract = true
	}
	if s.frontier > tx {
		s.frontier = tx
	}
	s.wakeLocked()
	s.cv.Broadcast()
	return ev
}

func (s *sched) receipts() []*types.Receipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.Receipt, len(s.txs))
	var cum uint64
	var logIndex uint
	blockNum := s.env.Header.Number
	var blobFee *big.Int
	if s.env.Header.ExcessBlobGas != nil {
		blobFee = s.env.BlockContext().BlobBaseFee
	}
	for i, tx := range s.env.Txs {
		t := &s.txs[i]
		cum += t.gasUsed
		rc := &types.Receipt{
			Type:              tx.Type(),
			CumulativeGasUsed: cum,
			GasUsed:           t.gasUsed,
			TxHash:            tx.Hash(),
			BlockHash:         s.env.BlockHash,
			BlockNumber:       new(big.Int).Set(blockNum),
			TransactionIndex:  uint(i),
		}
		if t.failed {
			rc.Status = types.ReceiptStatusFailed
		} else {
			rc.Status = types.ReceiptStatusSuccessful
		}
		if tx.To() == nil {
			rc.ContractAddress = crypto.CreateAddress(s.env.Senders[i], tx.Nonce())
		}
		if tx.Type() == types.BlobTxType {
			rc.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
			if blobFee != nil {
				rc.BlobGasPrice = new(big.Int).Set(blobFee)
			}
		}
		for _, lg := range t.logs {
			cp := *lg
			cp.Index = logIndex
			logIndex++
			cp.TxIndex = uint(i)
			cp.BlockHash = s.env.BlockHash
			cp.BlockNumber = blockNum.Uint64()
			rc.Logs = append(rc.Logs, &cp)
		}
		rc.Bloom = types.CreateBloom(rc)
		out[i] = rc
	}
	return out
}
