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

package rfstate

import (
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// OccRead is one optimistic read-set entry.
type OccRead struct {
	Key         Key
	Data        []byte
	ObsTx       int
	FromVersion bool
}

// TxView is a transaction-scoped vm.StateDB. Shared keys go through the
// ledger; intra-transaction snapshots use a private journal. The interpreter
// is unchanged.
type TxView struct {
	mode     Mode
	tx       int
	attempt  uint64
	ffUntil  int
	store    *Store
	ledger   *Ledger
	learner  *Learner
	deps     Deps
	abort    *atomic.Bool
	coinbase common.Address
	rules    params.Rules

	accs     map[common.Address]*localAcct
	mv       map[Key]ReadResult // first read of a key in this attempt
	early    bool               // publish final-looking hot writes before tx end
	parallel bool               // more than one worker; C=1 skips fence and abort loads
	wcount   map[Key]int        // writes per key, only when early
	readSeq  int
	regions  int
	pass     []Key
	wrote    []Key
	occ      []OccRead

	ownFee  *uint256.Int
	feeSum  *uint256.Int // lower-tx fees captured with the coinbase read
	coinObs *uint256.Int

	journal  []func()
	snaps    []rev
	nextSnap int
	free     []*localAcct

	access    *accList
	transient map[tkey]common.Hash
	refund    uint64
	logs      []*types.Log
	thash     common.Hash
	preimages map[common.Hash][]byte
}

type rev struct{ id, n int }

type tkey struct {
	addr common.Address
	slot common.Hash
}

type localAcct struct {
	exists, existKnown, created bool
	selfDestructed, newContract bool
	touched, forceDelete        bool

	bal                    *uint256.Int
	balKnown, balDirty     bool
	nonce                  uint64
	nonceKnown, nonceDirty bool
	code                   []byte
	codeHash               common.Hash
	codeKnown, codeDirty   bool

	storage      map[common.Hash]common.Hash
	storageOrig  map[common.Hash]common.Hash
	storageDirty map[common.Hash]struct{}

	// wipeTx is the transaction that wiped this account, or -1. It is
	// filled once per attempt so later slots do not read WipeKey again.
	wipeKnown bool
	wipeTx    int
}

var viewPool = sync.Pool{New: func() any {
	return &TxView{
		accs:      map[common.Address]*localAcct{},
		mv:        map[Key]ReadResult{},
		access:    newAccList(),
		transient: map[tkey]common.Hash{},
		ownFee:    uint256.NewInt(0),
	}
}}

// NewTxView builds a view. abort may be nil. deps may be nil (never waits).
// Release returns the view to a pool; forgetting Release only skips reuse.
func NewTxView(mode Mode, tx int, attempt uint64, ffUntil int, store *Store, ledger *Ledger, learner *Learner, deps Deps, abort *atomic.Bool, coinbase common.Address) *TxView {
	v := viewPool.Get().(*TxView)
	v.recycle()
	v.mode = mode
	v.tx = tx
	v.attempt = attempt
	v.ffUntil = ffUntil
	v.store = store
	v.ledger = ledger
	v.learner = learner
	v.deps = deps
	v.abort = abort
	v.coinbase = coinbase
	// Snapshot parallelism once. deps.Parallel takes the scheduler lock;
	// calling it on every read was per-op bookkeeping, and a C=1 attempt
	// never needs the fence or the abort flag.
	v.parallel = deps != nil && deps.Parallel()
	v.early = mode == ModeRF && v.parallel
	if v.ownFee == nil {
		v.ownFee = uint256.NewInt(0)
	} else {
		v.ownFee.Clear()
	}
	return v
}

// Release returns the view to the pool. Log objects already handed to the
// scheduler stay valid; the view only drops its references.
func (v *TxView) Release() {
	if v == nil {
		return
	}
	v.store = nil
	v.ledger = nil
	v.learner = nil
	v.deps = nil
	v.abort = nil
	viewPool.Put(v)
}

func (v *TxView) recycle() {
	for addr, a := range v.accs {
		a.recycle()
		v.free = append(v.free, a)
		delete(v.accs, addr)
	}
	clear(v.mv)
	clear(v.wcount)
	v.early = false
	v.parallel = false
	v.pass = v.pass[:0]
	v.wrote = v.wrote[:0]
	v.occ = v.occ[:0]
	v.journal = v.journal[:0]
	v.snaps = v.snaps[:0]
	v.logs = v.logs[:0]
	v.readSeq = 0
	v.regions = 0
	v.nextSnap = 0
	v.refund = 0
	v.feeSum = nil
	v.coinObs = nil
	v.thash = common.Hash{}
	if v.access != nil {
		v.access.reset()
	}
	clear(v.transient)
	clear(v.preimages)
}

// Regions is the number of fenced cuts taken in this attempt.
func (v *TxView) Regions() int { return v.regions }

// PassKeys are shared keys read under PASS (or an equivalent registered read).
func (v *TxView) PassKeys() []Key { return v.pass }

// WroteKeys are keys whose published bytes differed from the previous version.
func (v *TxView) WroteKeys() []Key { return v.wrote }

// OccReads returns the optimistic read set.
func (v *TxView) OccReads() []OccRead { return v.occ }

// CoinbaseObserved is the coinbase balance returned to this attempt and the
// lower-tx fee sum included in it. Both are nil if coinbase was not read.
func (v *TxView) CoinbaseObserved() (total, feeSum *uint256.Int) {
	return v.coinObs, v.feeSum
}

// OwnFee is the transaction-local coinbase fee credit.
func (v *TxView) OwnFee() *uint256.Int { return new(uint256.Int).Set(v.ownFee) }

func (v *TxView) guard() {
	if !v.parallel || v.mode == ModeDirect || v.abort == nil {
		return
	}
	if v.abort.Load() {
		panic(Signal{Kind: SigRollback, Seq: v.readSeq})
	}
}

func (v *TxView) undo(fn func()) { v.journal = append(v.journal, fn) }

func (v *TxView) acct(addr common.Address) *localAcct {
	a := v.accs[addr]
	if a != nil {
		return a
	}
	n := len(v.free)
	if n > 0 {
		a = v.free[n-1]
		v.free = v.free[:n-1]
	} else {
		a = &localAcct{}
	}
	if a.bal == nil {
		a.bal = uint256.NewInt(0)
	}
	v.accs[addr] = a
	return a
}

func (a *localAcct) recycle() {
	st, so, sd := a.storage, a.storageOrig, a.storageDirty
	*a = localAcct{}
	if st != nil {
		clear(st)
		a.storage = st
	}
	if so != nil {
		clear(so)
		a.storageOrig = so
	}
	if sd != nil {
		clear(sd)
		a.storageDirty = sd
	}
}

func (a *localAcct) slots() {
	if a.storage != nil {
		return
	}
	a.storage = map[common.Hash]common.Hash{}
	a.storageOrig = map[common.Hash]common.Hash{}
	a.storageDirty = map[common.Hash]struct{}{}
}

func (v *TxView) isCoinbaseBal(k Key) bool {
	return v.mode != ModeDirect && k.Kind == KindBalance && k.Addr == v.coinbase
}

// readMV selects a version and, on the first access in this attempt, applies
// the region fence. Direct mode reports FromVersion false.
func (v *TxView) readMV(k Key, doFence bool) ReadResult {
	if v.mode == ModeDirect || v.ledger == nil {
		v.guard()
		return ReadResult{ObsTx: -1}
	}
	if res, ok := v.mv[k]; ok {
		return res
	}
	if v.parallel {
		v.guard()
	}
	seq := v.readSeq
	v.readSeq++
	if v.parallel && doFence && seq >= v.ffUntil {
		v.fence(k, seq)
	}
	register := v.mode == ModeRF && !v.isCoinbaseBal(k)
	res := v.ledger.Read(v.tx, v.attempt, k, register)
	if res.Estimate {
		panic(Signal{Kind: SigWaitEstimate, Depend: res.EstTx, Seq: v.readSeq - 1})
	}
	if v.mode == ModeOCC {
		v.occ = append(v.occ, OccRead{
			Key:         k,
			Data:        append([]byte(nil), res.Data...),
			ObsTx:       res.ObsTx,
			FromVersion: res.FromVersion,
		})
	}
	if v.mv == nil {
		v.mv = map[Key]ReadResult{}
	}
	v.mv[k] = res
	return res
}

func (v *TxView) fence(k Key, seq int) {
	if v.mode != ModeRF {
		return
	}
	if v.isCoinbaseBal(k) {
		if v.deps != nil && !v.deps.PrefixFinal(v.tx) {
			panic(Signal{Kind: SigWaitPrefix, Seq: seq})
		}
		return
	}
	// A key with no conflict evidence has posterior mean 1/33, so the greedy
	// rule is PASS whether or not a lower producer exists. Skip the second
	// key lock and the learner map.
	if v.learner == nil || v.ledger == nil {
		return
	}
	// Safe observations are the attempt's read set, recorded when the read
	// is registered. Unfenced keys do not consult the producer list.
	if !v.learner.Fenced(k) {
		return
	}
	v.regions++
	v.Snapshot()
	prod, ok := v.ledger.LowerProducer(v.tx, k)
	if !ok || v.learner.Choose(k, true) != FenceWaitFinal {
		return
	}
	if v.deps != nil && !v.deps.TxSettled(prod) {
		panic(Signal{Kind: SigWaitFinal, Depend: prod, Seq: seq})
	}
}

func (v *TxView) loadExist(addr common.Address, a *localAcct) {
	if a.existKnown {
		return
	}
	res := v.readMV(ExistKey(addr), true)
	if res.FromVersion {
		a.exists = decBool(res.Data)
	} else if base := v.store.peek(addr); base != nil {
		a.exists = base.Exists
	}
	a.existKnown = true
}

func (v *TxView) loadBal(addr common.Address, a *localAcct) {
	if a.balKnown {
		return
	}
	if v.isCoinbaseBal(BalanceKey(addr)) {
		v.loadCoinbase(a)
		return
	}
	res := v.readMV(BalanceKey(addr), true)
	if res.FromVersion {
		a.bal = decBalance(res.Data)
	} else if base := v.store.peek(addr); base != nil && base.Exists {
		// Own the balance. GetBalance hands this pointer to the EVM, matching
		// StateDB, and must not alias the store.
		a.bal = new(uint256.Int)
		if base.Balance != nil {
			a.bal.Set(base.Balance)
		}
		if !a.existKnown {
			a.exists = true
			a.existKnown = true
		}
	} else {
		a.bal = uint256.NewInt(0)
	}
	if a.bal == nil {
		a.bal = uint256.NewInt(0)
	}
	a.balKnown = true
}

func (v *TxView) loadCoinbase(a *localAcct) {
	res := v.readMV(BalanceKey(v.coinbase), true)
	var base *uint256.Int
	if res.FromVersion {
		base = decBalance(res.Data)
	} else if acc := v.store.peek(v.coinbase); acc != nil {
		base = acc.Balance
		if !a.existKnown {
			a.exists = acc.Exists
			a.existKnown = true
		}
	}
	if base == nil {
		base = uint256.NewInt(0)
	}
	sum := uint256.NewInt(0)
	if v.ledger != nil {
		sum = v.ledger.SumFees(v.tx)
	}
	total := new(uint256.Int).Add(base, sum)
	if v.ownFee != nil && v.ownFee.Sign() != 0 {
		total.Add(total, v.ownFee)
	}
	a.bal = total
	a.balKnown = true
	v.feeSum = new(uint256.Int).Set(sum)
	v.coinObs = new(uint256.Int).Set(total)
}

func (v *TxView) loadNonce(addr common.Address, a *localAcct) {
	if a.nonceKnown {
		return
	}
	res := v.readMV(NonceKey(addr), true)
	if res.FromVersion {
		a.nonce = decU64(res.Data)
	} else if base := v.store.peek(addr); base != nil && base.Exists {
		a.nonce = base.Nonce
		if !a.existKnown {
			a.exists = true
			a.existKnown = true
		}
	}
	a.nonceKnown = true
}

func (v *TxView) loadCode(addr common.Address, a *localAcct) {
	if a.codeKnown {
		return
	}
	res := v.readMV(CodeKey(addr), true)
	if res.FromVersion {
		// Ledger version bytes are immutable. The hash was cached at publish.
		a.code = res.Data
		a.codeHash = res.CodeHash
		if a.codeHash == (common.Hash{}) {
			a.codeHash = CodeHash(true, a.code)
		}
		// Existence comes only from ExistKey or the store. A deleted account
		// publishes empty code; that version must not resurrect it.
		if !a.existKnown && len(a.code) > 0 {
			a.exists = true
			a.existKnown = true
		}
	} else if base := v.store.peek(addr); base != nil && base.Exists {
		a.code = base.Code
		a.codeHash = base.CodeHash
		if a.codeHash == (common.Hash{}) {
			a.codeHash = CodeHash(true, a.code)
		}
		if !a.existKnown {
			a.exists = true
			a.existKnown = true
		}
	}
	if a.codeHash == (common.Hash{}) {
		a.codeHash = CodeHash(a.exists, a.code)
	}
	a.codeKnown = true
}

func (v *TxView) ensureWrite(addr common.Address) *localAcct {
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		prevExists := a.exists
		prevCreated := a.created
		v.undo(func() {
			a.exists = prevExists
			a.created = prevCreated
		})
		a.exists = true
		a.existKnown = true
		a.created = true
		a.touched = true
	}
	return a
}

func (a *localAcct) dirty() bool {
	return a.balDirty || a.nonceDirty || a.codeDirty || a.created || a.touched || a.selfDestructed || len(a.storageDirty) > 0
}

func (a *localAcct) isEmpty() bool {
	if a.nonce != 0 || len(a.code) != 0 {
		return false
	}
	return a.bal == nil || a.bal.IsZero()
}

// ---- vm.StateDB ----

func (v *TxView) CreateAccount(addr common.Address) {
	v.guard()
	a := v.acct(addr)
	v.loadExist(addr, a)
	if a.exists {
		return
	}
	v.undo(func() {
		a.exists = false
		a.created = false
		a.touched = false
	})
	a.exists = true
	a.existKnown = true
	a.created = true
	a.touched = true
	if !a.balKnown {
		a.bal = uint256.NewInt(0)
		a.balKnown = true
	}
	a.nonceKnown = true
	a.codeKnown = true
	a.codeHash = CodeHash(true, nil)
}

func (v *TxView) CreateContract(addr common.Address) {
	v.guard()
	a := v.ensureWrite(addr)
	if a.newContract {
		return
	}
	prev := a.newContract
	v.undo(func() { a.newContract = prev })
	a.newContract = true
}

func (v *TxView) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	v.guard()
	if amount == nil || amount.IsZero() {
		return v.AddBalance(addr, uint256.NewInt(0), reason)
	}
	a := v.ensureWrite(addr)
	v.loadBal(addr, a)
	prev := *a.bal
	next := new(uint256.Int).Sub(a.bal, amount)
	v.journalBal(a, a.bal)
	a.bal = next
	a.balDirty = true
	a.touched = true
	return prev
}

func (v *TxView) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	v.guard()
	if amount == nil {
		amount = uint256.NewInt(0)
	}
	if v.mode != ModeDirect && addr == v.coinbase && reason == tracing.BalanceIncreaseRewardTransactionFee {
		return v.addCoinbaseFee(amount)
	}
	a := v.acct(addr)
	v.loadBal(addr, a)
	v.loadExist(addr, a)
	if amount.IsZero() {
		if !a.exists || v.accountEmptyLoaded(addr, a) {
			a.touched = true
		}
		if a.bal == nil {
			return uint256.Int{}
		}
		return *a.bal
	}
	if !a.exists {
		v.ensureWrite(addr)
		a = v.accs[addr]
		v.loadBal(addr, a)
	}
	prev := *a.bal
	v.journalBal(a, a.bal)
	a.bal = new(uint256.Int).Add(a.bal, amount)
	a.balDirty = true
	a.touched = true
	return prev
}

func (v *TxView) addCoinbaseFee(amount *uint256.Int) uint256.Int {
	prev := uint256.Int{}
	if a := v.accs[v.coinbase]; a != nil && a.balKnown {
		prev = *a.bal
		v.journalBal(a, a.bal)
		a.bal = new(uint256.Int).Add(a.bal, amount)
		a.balDirty = true
	}
	oldFee := new(uint256.Int).Set(v.ownFee)
	v.undo(func() { v.ownFee = oldFee })
	v.ownFee = new(uint256.Int).Add(v.ownFee, amount)
	return prev
}

func (v *TxView) journalBal(a *localAcct, old *uint256.Int) {
	cp := new(uint256.Int).Set(old)
	dirty := a.balDirty
	v.undo(func() {
		a.bal = cp
		a.balDirty = dirty
	})
}

func (v *TxView) accountEmptyLoaded(addr common.Address, a *localAcct) bool {
	v.loadBal(addr, a)
	v.loadNonce(addr, a)
	v.loadCode(addr, a)
	return a.isEmpty()
}

func (v *TxView) GetBalance(addr common.Address) *uint256.Int {
	if a := v.accs[addr]; a != nil && a.balKnown && a.bal != nil {
		return a.bal
	}
	a := v.acct(addr)
	v.loadBal(addr, a)
	return a.bal
}

func (v *TxView) GetNonce(addr common.Address) uint64 {
	if a := v.accs[addr]; a != nil && a.nonceKnown && a.existKnown {
		if !a.exists {
			return 0
		}
		return a.nonce
	}
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return 0
	}
	v.loadNonce(addr, a)
	return a.nonce
}

func (v *TxView) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	v.guard()
	a := v.ensureWrite(addr)
	v.loadNonce(addr, a)
	if a.nonce == nonce && a.nonceDirty {
		return
	}
	old, dirty := a.nonce, a.nonceDirty
	k := NonceKey(addr)
	early := v.wantEarly(k)
	v.undo(func() {
		a.nonce = old
		a.nonceDirty = dirty
		if early {
			if dirty {
				v.pub(k, encU64(old))
			} else {
				v.ledger.RetractKey(v.tx, k)
			}
		}
	})
	a.nonce = nonce
	a.nonceKnown = true
	a.nonceDirty = true
	a.touched = true
	v.noteWriteCount(k)
	if early {
		v.pub(k, encU64(nonce))
	}
}

func (v *TxView) GetCodeHash(addr common.Address) common.Hash {
	if a := v.accs[addr]; a != nil && a.codeKnown && a.existKnown {
		if !a.exists {
			return common.Hash{}
		}
		return a.codeHash
	}
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return common.Hash{}
	}
	v.loadCode(addr, a)
	return a.codeHash
}

func (v *TxView) GetCode(addr common.Address) []byte {
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return nil
	}
	v.loadCode(addr, a)
	return a.code
}

func (v *TxView) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	v.guard()
	a := v.ensureWrite(addr)
	v.loadCode(addr, a)
	prev := a.code
	cp := append([]byte(nil), code...)
	oldHash, oldDirty := a.codeHash, a.codeDirty
	v.undo(func() {
		a.code = prev
		a.codeHash = oldHash
		a.codeDirty = oldDirty
	})
	a.code = cp
	a.codeHash = CodeHash(true, cp)
	a.codeKnown = true
	a.codeDirty = true
	a.touched = true
	return prev
}

func (v *TxView) GetCodeSize(addr common.Address) int {
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return 0
	}
	v.loadCode(addr, a)
	return len(a.code)
}

func (v *TxView) AddRefund(gas uint64) {
	v.guard()
	prev := v.refund
	v.undo(func() { v.refund = prev })
	v.refund += gas
}

func (v *TxView) SubRefund(gas uint64) {
	v.guard()
	prev := v.refund
	v.undo(func() { v.refund = prev })
	if gas > v.refund {
		panic("refund counter below zero")
	}
	v.refund -= gas
}

func (v *TxView) GetRefund() uint64 { return v.refund }

func (v *TxView) GetStateAndCommittedState(addr common.Address, hash common.Hash) (common.Hash, common.Hash) {
	cur := v.GetState(addr, hash)
	a := v.accs[addr]
	orig, ok := a.storageOrig[hash]
	if !ok {
		orig = cur
	}
	return cur, orig
}

func (v *TxView) GetState(addr common.Address, hash common.Hash) common.Hash {
	if a := v.accs[addr]; a != nil && a.storage != nil {
		if val, ok := a.storage[hash]; ok {
			return val
		}
	}
	if v.parallel {
		v.guard()
	}
	a := v.acct(addr)
	a.slots()
	if val, ok := a.storage[hash]; ok {
		return val
	}
	// A wiped local account does not see prestate or lower slots.
	if a.forceDelete {
		a.storage[hash] = common.Hash{}
		a.storageOrig[hash] = common.Hash{}
		return common.Hash{}
	}
	wipeTx := v.cachedWipe(addr, a)
	slotRes := v.readMV(SlotKeyOf(addr, hash), true)
	var val common.Hash
	slotTx := -1
	if slotRes.FromVersion {
		val = decHash(slotRes.Data)
		slotTx = slotRes.ObsTx
	} else if wipeTx < 0 {
		val = v.store.Slot(addr, hash)
	}
	if wipeTx > slotTx {
		val = common.Hash{}
	}
	a.storage[hash] = val
	a.storageOrig[hash] = val
	return val
}

// cachedWipe reads the account wipe once per attempt and keeps it on the
// local account. Later slots of the same account skip that ledger read.
func (v *TxView) cachedWipe(addr common.Address, a *localAcct) int {
	if a.wipeKnown {
		return a.wipeTx
	}
	wipe := v.readMV(WipeKey(addr), true)
	wipeTx := -1
	if wipe.FromVersion && decBool(wipe.Data) {
		wipeTx = wipe.ObsTx
	} else if v.mode == ModeDirect && v.store != nil && v.store.Wiped(addr) {
		wipeTx = 0 // mask prestate; direct writes are in the local map
	}
	a.wipeTx = wipeTx
	a.wipeKnown = true
	return wipeTx
}

func (v *TxView) SetState(addr common.Address, key, value common.Hash) common.Hash {
	v.guard()
	a := v.ensureWrite(addr)
	prev := v.GetState(addr, key)
	if prev == value {
		return prev
	}
	old := prev
	_, wasDirty := a.storageDirty[key]
	k := SlotKeyOf(addr, key)
	early := v.wantEarly(k)
	var prevEnc []byte
	if early && wasDirty {
		prevEnc = encHash(old)
	}
	v.undo(func() {
		a.storage[key] = old
		if !wasDirty {
			delete(a.storageDirty, key)
		}
		if early {
			if wasDirty {
				v.pub(k, prevEnc)
			} else {
				v.ledger.RetractKey(v.tx, k)
			}
		}
	})
	a.storage[key] = value
	a.storageDirty[key] = struct{}{}
	a.touched = true
	v.noteWriteCount(k)
	if early {
		v.pub(k, encHash(value))
	}
	return prev
}

func (v *TxView) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return v.transient[tkey{addr, key}]
}

func (v *TxView) SetTransientState(addr common.Address, key, value common.Hash) {
	v.guard()
	k := tkey{addr, key}
	prev := v.transient[k]
	if prev == value {
		return
	}
	v.undo(func() { v.transient[k] = prev })
	v.transient[k] = value
}

func (v *TxView) SelfDestruct(addr common.Address) {
	v.guard()
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists || a.selfDestructed {
		return
	}
	prev := a.selfDestructed
	v.undo(func() { a.selfDestructed = prev })
	a.selfDestructed = true
	a.touched = true
}

func (v *TxView) HasSelfDestructed(addr common.Address) bool {
	a := v.accs[addr]
	return a != nil && a.selfDestructed
}

func (v *TxView) Exist(addr common.Address) bool {
	a := v.acct(addr)
	v.loadExist(addr, a)
	if a.selfDestructed {
		return true
	}
	return a.exists
}

func (v *TxView) Touch(addr common.Address) {
	v.guard()
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return
	}
	if !a.touched {
		v.undo(func() { a.touched = false })
		a.touched = true
	}
}

func (v *TxView) IsNewContract(addr common.Address) bool {
	a := v.accs[addr]
	return a != nil && a.newContract
}

func (v *TxView) Empty(addr common.Address) bool {
	a := v.acct(addr)
	v.loadExist(addr, a)
	if !a.exists {
		return true
	}
	return v.accountEmptyLoaded(addr, a)
}

func (v *TxView) AddressInAccessList(addr common.Address) bool {
	return v.access.hasAddr(addr)
}

func (v *TxView) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	return v.access.hasSlot(addr, slot)
}

func (v *TxView) AddAddressToAccessList(addr common.Address) {
	v.guard()
	if v.access.addAddr(addr) {
		v.undo(func() { v.access.delAddr(addr) })
	}
}

func (v *TxView) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	v.guard()
	addrNew, slotNew := v.access.addSlot(addr, slot)
	if addrNew {
		v.undo(func() { v.access.delAddr(addr) })
	}
	if slotNew {
		v.undo(func() { v.access.delSlot(addr, slot) })
	}
}

func (v *TxView) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, list types.AccessList) {
	v.rules = rules
	if v.access == nil {
		v.access = newAccList()
	} else {
		v.access.reset()
	}
	if v.transient == nil {
		v.transient = map[tkey]common.Hash{}
	} else {
		clear(v.transient)
	}
	if rules.IsEIP2929 {
		v.access.addAddr(sender)
		if dest != nil {
			v.access.addAddr(*dest)
		}
		for _, p := range precompiles {
			v.access.addAddr(p)
		}
		for _, el := range list {
			v.access.addAddr(el.Address)
			for _, key := range el.StorageKeys {
				v.access.addSlot(el.Address, key)
			}
		}
		if rules.IsShanghai {
			v.access.addAddr(coinbase)
		}
	}
}

func (v *TxView) Snapshot() int {
	id := v.nextSnap
	v.nextSnap++
	v.snaps = append(v.snaps, rev{id: id, n: len(v.journal)})
	return id
}

func (v *TxView) RevertToSnapshot(id int) {
	v.guard()
	idx := -1
	for i := range v.snaps {
		if v.snaps[i].id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	n := v.snaps[idx].n
	for i := len(v.journal) - 1; i >= n; i-- {
		v.journal[i]()
	}
	v.journal = v.journal[:n]
	v.snaps = v.snaps[:idx]
}

func (v *TxView) AddLog(log *types.Log) {
	v.guard()
	if log == nil {
		return
	}
	cp := *log
	cp.Topics = append([]common.Hash(nil), log.Topics...)
	cp.Data = append([]byte(nil), log.Data...)
	cp.TxHash = v.thash
	cp.TxIndex = uint(v.tx)
	n := len(v.logs)
	v.undo(func() { v.logs = v.logs[:n] })
	v.logs = append(v.logs, &cp)
}

// Logs returns the logs of this attempt.
func (v *TxView) Logs() []*types.Log { return v.logs }

func (v *TxView) AddPreimage(hash common.Hash, preimage []byte) {
	if v.preimages == nil {
		v.preimages = map[common.Hash][]byte{}
	}
	if _, ok := v.preimages[hash]; ok {
		return
	}
	v.preimages[hash] = append([]byte(nil), preimage...)
}

func (v *TxView) Witness() *stateless.Witness { return nil }

func (v *TxView) AccessEvents() *state.AccessEvents { return nil }

func (v *TxView) Finalise(rules params.Rules) *bal.ConstructionBlockAccessList {
	v.rules = rules
	for addr, a := range v.accs {
		if !a.dirty() && !a.selfDestructed {
			continue
		}
		// Load fields so emptiness sees nonce and code, not just a balance write.
		v.loadBal(addr, a)
		v.loadNonce(addr, a)
		v.loadCode(addr, a)
		if a.selfDestructed || (rules.IsEIP158 && a.isEmpty()) {
			a.forceDelete = true
			a.exists = false
			a.bal = uint256.NewInt(0)
			a.nonce = 0
			a.code = nil
			a.codeHash = common.Hash{}
			a.balDirty = true
			a.nonceDirty = true
			a.codeDirty = true
		}
	}
	if v.mode == ModeDirect {
		v.flushDirect()
	}
	v.journal = v.journal[:0]
	v.snaps = v.snaps[:0]
	v.refund = 0
	return nil
}

func (v *TxView) SetTxContext(thash common.Hash, ti int, blockAccessIndex uint32) {
	v.thash = thash
}

// Publish installs AT_FINISH versions for a parallel attempt.
// Direct-mode views flush in Finalise and should not call Publish.
func (v *TxView) Publish() {
	if v.ledger == nil || v.mode == ModeDirect {
		return
	}
	for addr, a := range v.accs {
		if a.forceDelete {
			// A same-transaction wipe does not outrank a slot version from
			// this tx. Drop early slot publishes so readers see the wipe.
			if v.early {
				v.retractEarlySlots(addr)
			}
			v.pub(ExistKey(addr), encBool(false))
			v.pub(BalanceKey(addr), encBalance(uint256.NewInt(0)))
			v.pub(NonceKey(addr), encU64(0))
			v.pub(CodeKey(addr), nil)
			v.pub(WipeKey(addr), encBool(true))
			continue
		}
		if a.created {
			v.pub(ExistKey(addr), encBool(true))
		}
		if a.balDirty {
			v.publishBalance(addr, a)
		}
		if a.nonceDirty {
			v.pub(NonceKey(addr), encU64(a.nonce))
		}
		if a.codeDirty {
			// Publish copies once. Code bytes are not mutated after this.
			v.pub(CodeKey(addr), a.code)
		}
		for slot := range a.storageDirty {
			v.pub(SlotKeyOf(addr, slot), encHash(a.storage[slot]))
		}
	}
	v.ledger.RecordFee(v.tx, v.ownFee)
	// Keys this incarnation did not publish must not remain ESTIMATE.
	v.ledger.DropEstimates(v.tx)
	if v.early && v.learner != nil {
		for k, n := range v.wcount {
			v.learner.ObserveWriteShape(k, n == 1)
		}
	}
}

func (v *TxView) retractEarlySlots(addr common.Address) {
	for k := range v.wcount {
		if k.Kind == KindSlot && k.Addr == addr {
			v.ledger.RetractKey(v.tx, k)
		}
	}
}

func (v *TxView) noteWriteCount(k Key) {
	if !v.early {
		return
	}
	if v.wcount == nil {
		v.wcount = map[Key]int{}
	}
	v.wcount[k]++
}

// wantEarly publishes this write before transaction end only when more than
// one worker is running and past attempts usually wrote the key once.
func (v *TxView) wantEarly(k Key) bool {
	return v.early && v.learner != nil && v.learner.EarlyWrite(k)
}

func (v *TxView) publishBalance(addr common.Address, a *localAcct) {
	bal := new(uint256.Int).Set(a.bal)
	if addr == v.coinbase {
		if v.ownFee != nil {
			bal.Sub(bal, v.ownFee)
		}
		if v.feeSum != nil {
			bal.Sub(bal, v.feeSum)
		}
	}
	v.pub(BalanceKey(addr), encBalance(bal))
}

func (v *TxView) pub(k Key, data []byte) {
	if v.ledger.Publish(v.tx, k, data, false, v.baseBytes(k)) {
		v.wrote = append(v.wrote, k)
	}
}

func (v *TxView) baseBytes(k Key) []byte {
	switch k.Kind {
	case KindBalance:
		if a := v.store.peek(k.Addr); a != nil {
			return encBalance(a.Balance)
		}
		return encBalance(nil)
	case KindNonce:
		if a := v.store.peek(k.Addr); a != nil {
			return encU64(a.Nonce)
		}
		return encU64(0)
	case KindCode:
		if a := v.store.peek(k.Addr); a != nil {
			// Store code is immutable. Equality checks must not copy it.
			return a.Code
		}
		return nil
	case KindExist:
		if a := v.store.peek(k.Addr); a != nil {
			return encBool(a.Exists)
		}
		return encBool(false)
	case KindWipe:
		return encBool(v.store.Wiped(k.Addr))
	case KindSlot:
		return encHash(v.store.Slot(k.Addr, k.Slot))
	default:
		return nil
	}
}

func (v *TxView) flushDirect() {
	for addr, a := range v.accs {
		if a.forceDelete {
			acc, _ := v.store.Account(addr)
			acc.Exists = false
			acc.Balance = uint256.NewInt(0)
			acc.Nonce = 0
			acc.Code = nil
			v.store.PutAccount(addr, acc)
			continue
		}
		if !(a.balDirty || a.nonceDirty || a.codeDirty || a.created) {
			for slot := range a.storageDirty {
				v.store.PutSlot(addr, slot, a.storage[slot])
			}
			continue
		}
		acc, _ := v.store.Account(addr)
		if a.balDirty && a.bal != nil {
			acc.Balance = new(uint256.Int).Set(a.bal)
		}
		if a.nonceDirty {
			acc.Nonce = a.nonce
		}
		if a.codeDirty {
			acc.Code = append([]byte(nil), a.code...)
		}
		if a.created || a.balDirty || a.nonceDirty || a.codeDirty {
			acc.Exists = a.exists
		}
		v.store.PutAccount(addr, acc)
		for slot := range a.storageDirty {
			v.store.PutSlot(addr, slot, a.storage[slot])
		}
		a.balDirty, a.nonceDirty, a.codeDirty = false, false, false
		clear(a.storageDirty)
	}
}

var _ vm.StateDB = (*TxView)(nil)

// ---- access list ----

type accList struct {
	addrs map[common.Address]map[common.Hash]struct{}
}

func newAccList() *accList {
	return &accList{addrs: map[common.Address]map[common.Hash]struct{}{}}
}

func (a *accList) hasAddr(addr common.Address) bool {
	_, ok := a.addrs[addr]
	return ok
}

func (a *accList) hasSlot(addr common.Address, slot common.Hash) (bool, bool) {
	m, ok := a.addrs[addr]
	if !ok {
		return false, false
	}
	_, sok := m[slot]
	return true, sok
}

func (a *accList) addAddr(addr common.Address) bool {
	if _, ok := a.addrs[addr]; ok {
		return false
	}
	a.addrs[addr] = map[common.Hash]struct{}{}
	return true
}

func (a *accList) addSlot(addr common.Address, slot common.Hash) (addrNew bool, slotNew bool) {
	m, ok := a.addrs[addr]
	if !ok {
		m = map[common.Hash]struct{}{}
		a.addrs[addr] = m
		addrNew = true
	}
	if _, ok := m[slot]; !ok {
		m[slot] = struct{}{}
		slotNew = true
	}
	return addrNew, slotNew
}

func (a *accList) reset() {
	clear(a.addrs)
}

func (a *accList) delAddr(addr common.Address) { delete(a.addrs, addr) }

func (a *accList) delSlot(addr common.Address, slot common.Hash) {
	if m, ok := a.addrs[addr]; ok {
		delete(m, slot)
	}
}
