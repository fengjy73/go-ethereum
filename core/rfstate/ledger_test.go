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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// parallelDeps lets a unit test take the early-publish path without a scheduler.
type parallelDeps struct{}

func (parallelDeps) PrefixFinal(int) bool { return true }
func (parallelDeps) TxSettled(int) bool   { return true }
func (parallelDeps) Parallel() bool       { return true }
func (parallelDeps) Solo() bool           { return false }

func TestPublishInvalidatesHigherReader(t *testing.T) {
	var victims []Victim
	l := NewLedger(4, NewLearner(), func(v Victim) { victims = append(victims, v) })
	key := SlotKeyOf(common.Address{1}, common.Hash{2})
	base := encHash(common.Hash{})
	if got := l.Read(1, 1, key, true); got.FromVersion {
		t.Fatal("expected base read")
	}
	if !l.Publish(0, key, encHash(common.Hash{9}), false, base) {
		t.Fatal("expected change")
	}
	if len(victims) != 1 || victims[0].Tx != 1 || victims[0].Attempt != 1 {
		t.Fatalf("victims %+v", victims)
	}
	if l.learner.Len() != 1 {
		t.Fatalf("learner keys %d", l.learner.Len())
	}
}

func TestEqualPublishDoesNotInvalidate(t *testing.T) {
	var n int
	l := NewLedger(2, nil, func(Victim) { n++ })
	key := BalanceKey(common.Address{3})
	val := encBalance(uint256.NewInt(5))
	l.Publish(0, key, val, false, val)
	l.Read(1, 1, key, true)
	if l.Publish(0, key, val, false, val) {
		t.Fatal("republish of the same bytes is not a change")
	}
	if n != 0 {
		t.Fatalf("invalidations %d", n)
	}
}

func TestRepublishInvalidatesReaderOfPreviousBytes(t *testing.T) {
	var victims []Victim
	l := NewLedger(3, nil, func(v Victim) { victims = append(victims, v) })
	key := SlotKeyOf(common.Address{7}, common.Hash{1})
	l.Publish(0, key, encHash(common.Hash{1}), false, encHash(common.Hash{}))
	if got := l.Read(1, 4, key, true); got.ObsTx != 0 || decHash(got.Data) != (common.Hash{1}) {
		t.Fatalf("read %+v", got)
	}
	victims = nil
	if !l.Publish(0, key, encHash(common.Hash{2}), false, encHash(common.Hash{})) {
		t.Fatal("expected a changed republish")
	}
	if len(victims) != 1 || victims[0].Tx != 1 || victims[0].Attempt != 4 {
		t.Fatalf("victims %+v", victims)
	}
}

func TestDropEstimatesRemovesUnpublishedKey(t *testing.T) {
	var victims []Victim
	l := NewLedger(3, NewLearner(), func(v Victim) { victims = append(victims, v) })
	key := SlotKeyOf(common.Address{5}, common.Hash{1})
	other := SlotKeyOf(common.Address{5}, common.Hash{2})
	l.Publish(0, key, encHash(common.Hash{9}), false, encHash(common.Hash{}))
	if got := l.Read(1, 3, key, true); got.ObsTx != 0 {
		t.Fatalf("read %+v", got)
	}
	l.MarkEstimate(0)
	if got := l.Read(2, 1, key, false); !got.Estimate || got.EstTx != 0 {
		t.Fatalf("expected estimate, got %+v", got)
	}
	// The new incarnation writes a different key and never republishes key.
	l.Publish(0, other, encHash(common.Hash{4}), false, encHash(common.Hash{}))
	victims = nil
	l.DropEstimates(0)
	if len(victims) != 1 || victims[0].Tx != 1 || victims[0].Attempt != 3 {
		t.Fatalf("victims %+v", victims)
	}
	if got := l.Read(2, 2, key, false); got.Estimate || got.FromVersion {
		t.Fatalf("leftover version %+v", got)
	}
	if _, ok := l.LowerProducer(2, key); ok {
		t.Fatal("dropped writer still listed")
	}
	if got := l.Read(2, 2, other, false); !got.FromVersion || got.ObsTx != 0 {
		t.Fatalf("republished key %+v", got)
	}
}

func TestRetractInvalidatesObserver(t *testing.T) {
	var victims []Victim
	l := NewLedger(3, NewLearner(), func(v Victim) { victims = append(victims, v) })
	key := NonceKey(common.Address{4})
	l.Publish(0, key, encU64(1), false, encU64(0))
	got := l.Read(1, 7, key, true)
	if got.ObsTx != 0 || decU64(got.Data) != 1 {
		t.Fatalf("read %+v", got)
	}
	victims = nil
	l.Retract(0)
	if len(victims) != 1 || victims[0].Attempt != 7 {
		t.Fatalf("victims %+v", victims)
	}
	if again := l.Read(1, 8, key, false); again.FromVersion {
		t.Fatal("retracted version still visible")
	}
}

func TestCoinbaseFeesAndFoldOrder(t *testing.T) {
	l := NewLedger(3, nil, nil)
	coin := common.Address{9}
	l.RecordFee(0, uint256.NewInt(10))
	l.RecordFee(2, uint256.NewInt(4))
	if got := l.SumFees(2); got.Uint64() != 10 {
		t.Fatalf("sum before 2 = %d", got.Uint64())
	}
	// tx 0 deletes the account, tx 1 recreates it and writes a slot.
	l.Publish(0, WipeKey(coin), encBool(true), false, encBool(false))
	l.Publish(0, ExistKey(coin), encBool(false), false, encBool(true))
	l.Publish(0, BalanceKey(coin), encBalance(uint256.NewInt(0)), false, encBalance(uint256.NewInt(3)))
	l.Publish(1, ExistKey(coin), encBool(true), false, encBool(false))
	l.Publish(1, BalanceKey(coin), encBalance(uint256.NewInt(7)), false, encBalance(uint256.NewInt(0)))
	l.Publish(1, SlotKeyOf(coin, common.Hash{1}), encHash(common.Hash{8}), false, encHash(common.Hash{}))
	world := &World{Accounts: map[common.Address]*Account{
		coin: {Exists: true, Balance: uint256.NewInt(3), Nonce: 1},
	}, Slots: map[SlotKey]common.Hash{}}
	store := NewStore(world)
	l.Fold(store, coin)
	acc, ok := store.Account(coin)
	if !ok || !acc.Exists {
		t.Fatal("account should exist after recreate")
	}
	// Non-fee balance 7 plus fees 10+4.
	if acc.Balance.Uint64() != 21 {
		t.Fatalf("balance %d", acc.Balance.Uint64())
	}
	if got := store.Slot(coin, common.Hash{1}); got != (common.Hash{8}) {
		t.Fatalf("slot %x", got)
	}
}

func TestFeePrefixRebuildsOnRepublish(t *testing.T) {
	l := NewLedger(3, nil, nil)
	l.RecordFee(0, uint256.NewInt(10))
	l.RecordFee(1, uint256.NewInt(1))
	l.RecordFee(2, uint256.NewInt(4))
	if got := l.SumFees(3); got.Uint64() != 15 {
		t.Fatalf("filled sum %d", got.Uint64())
	}
	l.RecordFee(0, uint256.NewInt(30))
	if got := l.SumFees(3); got.Uint64() != 35 {
		t.Fatalf("rebuilt sum %d", got.Uint64())
	}
	if got := l.SumFees(1); got.Uint64() != 30 {
		t.Fatalf("prefix 1 = %d", got.Uint64())
	}
}

func TestRecordFeeInvalidatesCoinbaseReader(t *testing.T) {
	var victims []Victim
	l := NewLedger(2, nil, func(v Victim) { victims = append(victims, v) })
	// Tx 1 observed an empty prefix while tx 0 had not recorded its fee.
	l.WatchFees(1, 3, uint256.NewInt(0))
	l.RecordFee(0, uint256.NewInt(50))
	if len(victims) != 1 || victims[0].Tx != 1 || victims[0].Attempt != 3 || victims[0].Key != FeeKey() {
		t.Fatalf("victims %+v", victims)
	}
	// The same attempt is not invalidated again. A matching observation is kept.
	victims = nil
	l.WatchFees(1, 4, l.SumFees(1))
	l.RecordFee(0, uint256.NewInt(50))
	if len(victims) != 0 {
		t.Fatalf("unchanged fee invalidated %+v", victims)
	}
}

func TestPeekBelowMissDoesNotCreateKey(t *testing.T) {
	l := NewLedger(2, nil, nil)
	k := SlotKeyOf(common.Address{1}, common.Hash{2})
	if res := l.PeekBelow(1, k); res.FromVersion || res.Estimate {
		t.Fatalf("cold peek %+v", res)
	}
	// A miss must not insert a key the later publish would treat as existing.
	l.Publish(0, k, []byte{9}, false, nil)
	res := l.PeekBelow(1, k)
	if !res.FromVersion || len(res.Data) != 1 || res.Data[0] != 9 || res.ObsTx != 0 {
		t.Fatalf("peek %+v", res)
	}
	if res := l.PeekBelow(0, k); res.FromVersion {
		t.Fatal("writer saw its own version")
	}
}

func TestLearnerGreedy(t *testing.T) {
	l := NewLearner()
	k := SlotKeyOf(common.Address{1}, common.Hash{1})
	if l.Fenced(k) {
		t.Fatal("unseen key is not fenced")
	}
	if l.Choose(k, true) != FencePass {
		t.Fatal("cold prior should pass")
	}
	for i := 0; i < 40; i++ {
		l.ObserveConflict(k)
	}
	if !l.Fenced(k) {
		t.Fatal("conflicts should fence")
	}
	if l.Choose(k, true) != FenceWaitFinal {
		t.Fatal("mean above 1/2 should wait")
	}
	if l.Choose(k, false) != FencePass {
		t.Fatal("no producer cannot wait-final")
	}
	cp := l.Clone()
	cp.ObserveSafe(k)
	if l.Choose(k, true) != FenceWaitFinal {
		t.Fatal("clone must not mutate the source")
	}
}

func TestLearnerDecayAndDelta(t *testing.T) {
	base := NewLearner()
	k := SlotKeyOf(common.Address{2}, common.Hash{2})
	for i := 0; i < 40; i++ {
		base.ObserveConflict(k)
	}
	before := base.Clone()
	run := before.Clone()
	run.ObserveConflict(k)
	run.ObserveSafe(k)

	carried := before.Clone()
	carried.Decay()
	carried.ApplyDelta(before, run)

	carried.mu.Lock()
	got := carried.post[k]
	carried.mu.Unlock()
	before.mu.Lock()
	prev := before.post[k]
	before.mu.Unlock()

	w := priorAlpha + priorBeta
	fade := w / (w + 1)
	wantA := priorAlpha + (prev.Alpha-priorAlpha)*fade + 1
	wantB := priorBeta + (prev.Beta-priorBeta)*fade + 1
	if got.Alpha != wantA || got.Beta != wantB {
		t.Fatalf("posterior alpha=%v beta=%v, want %v %v", got.Alpha, got.Beta, wantA, wantB)
	}
	if got.Conflicts != prev.Conflicts+1 {
		t.Fatalf("conflicts %d, want %d", got.Conflicts, prev.Conflicts+1)
	}
	// A second apply of the same single-run delta must not be how K runs
	// accumulate: the caller passes one run. Repeating it here would double
	// the observation, which is the behavior the bench must avoid.
	if got.Conflicts == prev.Conflicts {
		t.Fatal("delta dropped the conflict")
	}
}

// An early slot publish must not survive a same-transaction wipe. The wipe
// version and the slot version share a tx index, so a later reader would
// keep the slot unless the publish path retracts it.
func TestEarlySlotRetractedOnWipe(t *testing.T) {
	addr := common.Address{9}
	slot := common.Hash{3}
	world := &World{Accounts: map[common.Address]*Account{
		addr: {Exists: true, Balance: uint256.NewInt(1), Nonce: 1, Code: []byte{0x00}},
	}}
	store := NewStore(world)
	k := SlotKeyOf(addr, slot)
	learner := NewLearner()
	learner.ObserveConflict(k)
	learner.ObserveWriteShape(k, true)
	ledger := NewLedger(2, learner, nil)
	v := NewTxView(ModeRF, 0, 1, 0, store, ledger, learner, parallelDeps{}, nil, common.Address{})
	defer v.Release()
	if v.SetState(addr, slot, common.Hash{0xab}) == (common.Hash{0xab}) {
		t.Fatal("store was already the written value")
	}
	v.SelfDestruct(addr)
	v.Finalise(params.Rules{})
	v.Publish()

	r := NewTxView(ModeRF, 1, 1, 0, store, ledger, nil, nil, nil, common.Address{})
	defer r.Release()
	if got := r.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("wiped slot visible to later tx: %x", got)
	}
}
