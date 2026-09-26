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
	"runtime"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// Victim is a reader whose observed version was crossed by a write or retract.
type Victim struct {
	Tx      int
	Attempt uint64
	Key     Key
}

// ReadResult is one MVCC read of a key for a transaction index.
type ReadResult struct {
	// FromVersion is false when the caller should use the committed store.
	FromVersion bool
	Data        []byte
	ObsTx       int
	Estimate    bool
	EstTx       int
	// CodeHash is set for a code version. It is cached at publish so a later
	// load does not recompute keccak.
	CodeHash common.Hash
}

type version struct {
	tx       int
	data     []byte
	estimate bool
	codeHash common.Hash
}

type reader struct {
	tx      int
	attempt uint64
	obsTx   int
}

// feeReader is a transaction that observed the coinbase fee prefix.
type feeReader struct {
	tx      int
	attempt uint64
	sum     uint256.Int
}

type keyState struct {
	mu        sync.Mutex
	vers      []version
	readers   []reader
	producers []int // txs that published this key and have not dropped it
}

// touchSet is the read set and write set of one transaction. DropEstimates,
// RemoveReaders, Retract and MarkEstimate visit only these keys.
type touchSet struct {
	reads  map[Key]struct{}
	writes map[Key]struct{}
}

type keyShard struct {
	mu   sync.Mutex
	keys map[Key]*keyState
}

// Ledger is the per-key multi-version memory and reader registry.
// Writes publish at transaction finish (AT_FINISH). A published write
// invalidates higher-index readers that observed an older version.
// The key index is sharded so a read or publish does not take a process-wide lock.
type Ledger struct {
	shards []keyShard
	mask   uint32
	touch  []touchSet

	feeMu   sync.Mutex
	fees    []*uint256.Int
	feePre  []*uint256.Int // feePre[i] is the sum of fees[0:i] once that prefix is filled
	feeFill int            // fees[0:feeFill] are all recorded
	// feeReaders watched a coinbase balance and the fee sum they observed.
	// RecordFee invalidates a higher reader whose sum no longer matches.
	feeReaders []feeReader

	onInvalidate func(Victim)
	learner      *Learner
}

// ledgerStripes is one stripe per GOMAXPROCS, rounded up to a power of two.
// The count follows the process, it is not a tuned constant.
func ledgerStripes() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// NewLedger returns an empty ledger sized for n transactions.
func NewLedger(n int, learner *Learner, onInvalidate func(Victim)) *Ledger {
	stripes := ledgerStripes()
	l := &Ledger{
		shards:       make([]keyShard, stripes),
		mask:         uint32(stripes - 1),
		touch:        make([]touchSet, n),
		fees:         make([]*uint256.Int, n),
		feePre:       make([]*uint256.Int, n+1),
		onInvalidate: onInvalidate,
		learner:      learner,
	}
	l.feePre[0] = uint256.NewInt(0)
	for i := range l.shards {
		l.shards[i].keys = map[Key]*keyState{}
	}
	for i := range l.touch {
		l.touch[i].reads = map[Key]struct{}{}
		l.touch[i].writes = map[Key]struct{}{}
	}
	return l
}

func (l *Ledger) state(k Key) *keyState {
	sh := &l.shards[k.stripe(l.mask)]
	sh.mu.Lock()
	ks := sh.keys[k]
	if ks == nil {
		ks = &keyState{}
		sh.keys[k] = ks
	}
	sh.mu.Unlock()
	return ks
}

func (l *Ledger) existing(k Key) *keyState {
	sh := &l.shards[k.stripe(l.mask)]
	sh.mu.Lock()
	ks := sh.keys[k]
	sh.mu.Unlock()
	return ks
}

func (l *Ledger) noteRead(tx int, k Key) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	l.touch[tx].reads[k] = struct{}{}
}

func (l *Ledger) noteWrite(tx int, k Key) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	l.touch[tx].writes[k] = struct{}{}
}

// PeekBelow returns the latest version written strictly below tx. It does
// not create a key entry and does not register a reader. The caller must
// be the only goroutine touching the ledger; a fixed single-worker block
// uses this so a cold read is a map miss instead of an inserted keyState,
// a second mutex, and a reader slot. Concurrent blocks must use Read.
func (l *Ledger) PeekBelow(tx int, k Key) ReadResult {
	sh := &l.shards[k.stripe(l.mask)]
	ks := sh.keys[k]
	res := ReadResult{ObsTx: -1}
	if ks == nil {
		return res
	}
	latest := -1
	for i := range ks.vers {
		v := &ks.vers[i]
		if v.tx >= tx {
			continue
		}
		if latest >= 0 && v.tx <= ks.vers[latest].tx {
			continue
		}
		latest = i
	}
	if latest < 0 {
		return res
	}
	v := ks.vers[latest]
	if v.estimate {
		res.Estimate = true
		res.EstTx = v.tx
		return res
	}
	res.FromVersion = true
	res.Data = v.data
	res.ObsTx = v.tx
	res.CodeHash = v.codeHash
	return res
}

// Read returns the latest version written by a transaction strictly below tx.
// When register is set, the reader is recorded under the key lock together
// with the version selection (R1).
func (l *Ledger) Read(tx int, attempt uint64, k Key, register bool) ReadResult {
	ks := l.state(k)
	ks.mu.Lock()
	latest := -1
	for i := range ks.vers {
		if ks.vers[i].tx < tx && (latest < 0 || ks.vers[i].tx > ks.vers[latest].tx) {
			latest = i
		}
	}
	res := ReadResult{ObsTx: -1}
	if latest >= 0 {
		v := ks.vers[latest]
		if v.estimate {
			res.Estimate = true
			res.EstTx = v.tx
			ks.mu.Unlock()
			return res
		}
		// Version bytes are immutable after publish. Callers must not write them.
		res.FromVersion = true
		res.Data = v.data
		res.ObsTx = v.tx
		res.CodeHash = v.codeHash
	}
	if register {
		ks.readers = append(ks.readers, reader{tx: tx, attempt: attempt, obsTx: res.ObsTx})
	}
	ks.mu.Unlock()
	if register {
		l.noteRead(tx, k)
	}
	return res
}

// Publish installs tx's version of k. Equal values do not invalidate readers
// that already observed those bytes. estimate marks a Block-STM ESTIMATE.
// changed is false when the installed bytes equal the version they replace
// (or the base value, for a first publish).
func (l *Ledger) Publish(tx int, k Key, data []byte, estimate bool, base []byte) (changed bool) {
	ks := l.state(k)
	ks.mu.Lock()
	// Own the bytes. Version slices are then immutable and reads can share them.
	data = append([]byte(nil), data...)
	var codeHash common.Hash
	if k.Kind == KindCode {
		codeHash = CodeHash(true, data)
	}
	prev := base
	replaced := false
	for i := range ks.vers {
		if ks.vers[i].tx == tx {
			prev = ks.vers[i].data
			ks.vers[i].data = data
			ks.vers[i].estimate = estimate
			ks.vers[i].codeHash = codeHash
			replaced = true
			break
		}
	}
	if !replaced {
		ks.vers = append(ks.vers, version{tx: tx, data: data, estimate: estimate, codeHash: codeHash})
	}
	noteProducer(ks, tx)
	changed = estimate || !bytesEqual(prev, data)
	var victims []Victim
	if !estimate {
		for _, r := range ks.readers {
			if r.tx <= tx || r.obsTx > tx {
				continue
			}
			// A reader that already observed this tx saw prev, which was
			// replaced above. versionData would return the new bytes.
			var observed []byte
			if r.obsTx == tx {
				observed = prev
			} else if r.obsTx >= 0 {
				observed = versionData(ks.vers, r.obsTx)
			} else {
				observed = base
			}
			if bytesEqual(observed, data) {
				continue
			}
			victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: k})
		}
	}
	ks.mu.Unlock()

	l.noteWrite(tx, k)
	if len(victims) > 0 && l.learner != nil {
		l.learner.ObserveConflict(k)
	}
	if l.onInvalidate != nil {
		for _, v := range victims {
			l.onInvalidate(v)
		}
	}
	return changed
}

// DropEstimates removes versions of tx that are still ESTIMATE after the
// incarnation publishes. Block-STM marks the previous write set as ESTIMATE
// at re-execution; keys this incarnation does not publish must not stay
// ESTIMATE once the transaction is finished, or a higher reader retries
// forever (the producer is already settled, but the read still blocks).
// Readers that observed the removed version are invalidated.
func (l *Ledger) DropEstimates(tx int) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	writes := l.touch[tx].writes
	for k := range writes {
		ks := l.existing(k)
		if ks == nil {
			delete(writes, k)
			continue
		}
		ks.mu.Lock()
		had := false
		dst := ks.vers[:0]
		for _, v := range ks.vers {
			if v.tx == tx && v.estimate {
				had = true
				continue
			}
			dst = append(dst, v)
		}
		ks.vers = dst
		var victims []Victim
		if had {
			for _, r := range ks.readers {
				if r.tx > tx && r.obsTx == tx {
					victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: k})
				}
			}
		}
		still := false
		for _, v := range ks.vers {
			if v.tx == tx {
				still = true
				break
			}
		}
		if !still {
			dropProducer(ks, tx)
		}
		ks.mu.Unlock()
		if !still {
			delete(writes, k)
		}
		l.fire(k, victims)
	}
}

// MarkEstimate turns this transaction's published versions into ESTIMATE
// markers so higher readers block. Keys it has not written are left alone.
func (l *Ledger) MarkEstimate(tx int) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	for k := range l.touch[tx].writes {
		ks := l.existing(k)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		for i := range ks.vers {
			if ks.vers[i].tx == tx {
				ks.vers[i].estimate = true
			}
		}
		ks.mu.Unlock()
	}
}

// Retract removes tx's versions and invalidates readers that observed them (R3).
func (l *Ledger) Retract(tx int) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	// Keep the write set so the incarnation can drop keys it does not republish.
	writes := l.touch[tx].writes
	for k := range writes {
		ks := l.existing(k)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		had := false
		dst := ks.vers[:0]
		for _, v := range ks.vers {
			if v.tx == tx {
				had = true
				continue
			}
			dst = append(dst, v)
		}
		ks.vers = dst
		var victims []Victim
		if had {
			for _, r := range ks.readers {
				if r.tx > tx && r.obsTx == tx {
					victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: k})
				}
			}
		}
		rd := ks.readers[:0]
		for _, r := range ks.readers {
			if r.tx == tx {
				continue
			}
			rd = append(rd, r)
		}
		ks.readers = rd
		ks.mu.Unlock()
		l.fire(k, victims)
	}
}

// RemoveReaders drops reader registrations for one transaction without retracting writes.
func (l *Ledger) RemoveReaders(tx int) {
	if tx < 0 || tx >= len(l.touch) {
		return
	}
	l.removeFeeReader(tx)
	reads := l.touch[tx].reads
	for k := range reads {
		delete(reads, k)
		ks := l.existing(k)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		rd := ks.readers[:0]
		for _, r := range ks.readers {
			if r.tx != tx {
				rd = append(rd, r)
			}
		}
		ks.readers = rd
		ks.mu.Unlock()
	}
}

func (l *Ledger) fire(k Key, victims []Victim) {
	if len(victims) == 0 {
		return
	}
	if l.learner != nil {
		l.learner.ObserveConflict(k)
	}
	if l.onInvalidate != nil {
		for _, v := range victims {
			l.onInvalidate(v)
		}
	}
}

// LowerProducer is the highest transaction index below tx that has published k.
// An ESTIMATE version still counts: the producer is re-executing that key.
func (l *Ledger) LowerProducer(tx int, k Key) (int, bool) {
	ks := l.existing(k)
	if ks == nil {
		return 0, false
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	best := -1
	for _, p := range ks.producers {
		if p < tx && p > best {
			best = p
		}
	}
	return best, best >= 0
}

// ReadKeys returns the keys this attempt has registered as a reader.
func (l *Ledger) ReadKeys(tx int) []Key {
	if l == nil || tx < 0 || tx >= len(l.touch) {
		return nil
	}
	reads := l.touch[tx].reads
	if len(reads) == 0 {
		return nil
	}
	out := make([]Key, 0, len(reads))
	for k := range reads {
		out = append(out, k)
	}
	return out
}

// RetractKey removes tx's version of one key and invalidates readers that
// observed it. Used when an early publish is reverted inside the attempt.
func (l *Ledger) RetractKey(tx int, k Key) {
	if l == nil || tx < 0 {
		return
	}
	ks := l.existing(k)
	if ks == nil {
		return
	}
	ks.mu.Lock()
	had := false
	dst := ks.vers[:0]
	for _, v := range ks.vers {
		if v.tx == tx {
			had = true
			continue
		}
		dst = append(dst, v)
	}
	ks.vers = dst
	var victims []Victim
	if had {
		for _, r := range ks.readers {
			if r.tx > tx && r.obsTx == tx {
				victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: k})
			}
		}
		dropProducer(ks, tx)
	}
	ks.mu.Unlock()
	if had && tx < len(l.touch) {
		delete(l.touch[tx].writes, k)
	}
	l.fire(k, victims)
}

func noteProducer(ks *keyState, tx int) {
	for _, p := range ks.producers {
		if p == tx {
			return
		}
	}
	ks.producers = append(ks.producers, tx)
}

func dropProducer(ks *keyState, tx int) {
	dst := ks.producers[:0]
	for _, p := range ks.producers {
		if p != tx {
			dst = append(dst, p)
		}
	}
	ks.producers = dst
}

// RecordFee stores the transaction-local coinbase fee. It is not a balance write.
// A contiguous prefix sum is extended so a later SumFees of finalized lower
// transactions does not scan the fee array. A higher transaction that already
// observed a different prefix is invalidated after the lock is released.
func (l *Ledger) RecordFee(tx int, fee *uint256.Int) {
	l.feeMu.Lock()
	if tx < 0 || tx >= len(l.fees) {
		l.feeMu.Unlock()
		return
	}
	if fee == nil {
		l.fees[tx] = uint256.NewInt(0)
	} else {
		l.fees[tx] = new(uint256.Int).Set(fee)
	}
	// A re-execution can publish a different fee after later transactions
	// have already extended the prefix. Sums at and above tx are stale.
	if tx < l.feeFill {
		l.feeFill = tx
	}
	for l.feeFill < len(l.fees) && l.fees[l.feeFill] != nil {
		sum := new(uint256.Int).Set(l.feePre[l.feeFill])
		sum.Add(sum, l.fees[l.feeFill])
		l.feePre[l.feeFill+1] = sum
		l.feeFill++
	}
	victims := l.feeVictimsLocked(tx)
	l.feeMu.Unlock()
	l.fire(FeeKey(), victims)
}

// WatchFees records that tx observed sum as the fees of [0, tx). A later
// RecordFee from a lower transaction invalidates this attempt when the sum
// changes. The scheduler also checks the sum again before the attempt can
// become final.
func (l *Ledger) WatchFees(tx int, attempt uint64, sum *uint256.Int) {
	if l == nil || tx < 0 {
		return
	}
	var got uint256.Int
	if sum != nil {
		got.Set(sum)
	}
	l.feeMu.Lock()
	for i := range l.feeReaders {
		if l.feeReaders[i].tx == tx {
			l.feeReaders[i].attempt = attempt
			l.feeReaders[i].sum = got
			l.feeMu.Unlock()
			return
		}
	}
	l.feeReaders = append(l.feeReaders, feeReader{tx: tx, attempt: attempt, sum: got})
	l.feeMu.Unlock()
}

func (l *Ledger) removeFeeReader(tx int) {
	l.feeMu.Lock()
	dst := l.feeReaders[:0]
	for _, r := range l.feeReaders {
		if r.tx != tx {
			dst = append(dst, r)
		}
	}
	l.feeReaders = dst
	l.feeMu.Unlock()
}

// feeVictimsLocked drops higher readers whose observed prefix no longer
// matches. Caller holds feeMu.
func (l *Ledger) feeVictimsLocked(tx int) []Victim {
	var victims []Victim
	dst := l.feeReaders[:0]
	for _, r := range l.feeReaders {
		if r.tx <= tx {
			dst = append(dst, r)
			continue
		}
		now := l.sumFeesLocked(r.tx)
		if now.Cmp(&r.sum) != 0 {
			victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: FeeKey()})
			continue
		}
		dst = append(dst, r)
	}
	l.feeReaders = dst
	return victims
}

// SumFees returns the sum of recorded fees of transactions in [0, before).
func (l *Ledger) SumFees(before int) *uint256.Int {
	l.feeMu.Lock()
	defer l.feeMu.Unlock()
	return l.sumFeesLocked(before)
}

func (l *Ledger) sumFeesLocked(before int) *uint256.Int {
	if before > len(l.fees) {
		before = len(l.fees)
	}
	if before < 0 {
		before = 0
	}
	if before <= l.feeFill {
		if l.feePre[before] == nil {
			return uint256.NewInt(0)
		}
		return l.feePre[before]
	}
	sum := uint256.NewInt(0)
	for i := 0; i < before; i++ {
		if l.fees[i] != nil {
			sum.Add(sum, l.fees[i])
		}
	}
	return sum
}

// Fee returns the recorded fee for tx, or nil if it has not been published.
func (l *Ledger) Fee(tx int) *uint256.Int {
	l.feeMu.Lock()
	defer l.feeMu.Unlock()
	if tx < 0 || tx >= len(l.fees) || l.fees[tx] == nil {
		return nil
	}
	return new(uint256.Int).Set(l.fees[tx])
}

// Fold applies every non-estimate version into store. Coinbase fees are NOT
// folded into the balance key; the caller adds them when materialising the
// coinbase account. Wipe versions clear prestate slots.
func (l *Ledger) Fold(store *Store, coinbase common.Address) {
	type pair struct {
		key Key
		ks  *keyState
	}
	var owned []pair
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		for k, ks := range sh.keys {
			owned = append(owned, pair{k, ks})
		}
		sh.mu.Unlock()
	}

	type folded struct {
		key  Key
		data []byte
		tx   int
	}
	var rows []folded
	for _, p := range owned {
		ks := p.ks
		ks.mu.Lock()
		latest := map[int]version{} // one per tx, last wins (there is only one)
		bestTx := -1
		var best version
		for _, v := range ks.vers {
			if v.estimate {
				continue
			}
			latest[v.tx] = v
			if v.tx > bestTx {
				bestTx = v.tx
				best = v
			}
		}
		ks.mu.Unlock()
		if bestTx >= 0 {
			rows = append(rows, folded{key: p.key, data: best.data, tx: best.tx})
		}
		_ = latest
	}
	// Apply in transaction order so a wipe hides earlier slots and a later
	// write is visible again. Existence is applied after the other fields of
	// the same transaction so a delete wins over the zeroed balance publish.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].tx != rows[j].tx {
			return rows[i].tx < rows[j].tx
		}
		return kindOrder(rows[i].key.Kind) < kindOrder(rows[j].key.Kind)
	})
	for _, row := range rows {
		l.applyFold(store, row.key, row.data)
	}
	// Coinbase balance key is the non-fee absolute balance. Add every fee.
	if coinbase != (common.Address{}) {
		acc, _ := store.Account(coinbase)
		if acc.Balance == nil {
			acc.Balance = uint256.NewInt(0)
		}
		acc.Balance.Add(acc.Balance, l.SumFees(len(l.fees)))
		acc.Exists = true
		store.PutAccount(coinbase, acc)
	}
}

func kindOrder(k Kind) int {
	switch k {
	case KindWipe:
		return 0
	case KindExist:
		return 2
	default:
		return 1
	}
}

func (l *Ledger) applyFold(store *Store, k Key, data []byte) {
	switch k.Kind {
	case KindWipe:
		store.Wipe(k.Addr)
	case KindSlot:
		store.PutSlot(k.Addr, k.Slot, decHash(data))
	case KindBalance:
		acc, _ := store.Account(k.Addr)
		acc.Balance = decBalance(data)
		acc.Exists = true
		store.PutAccount(k.Addr, acc)
	case KindNonce:
		acc, _ := store.Account(k.Addr)
		acc.Nonce = decU64(data)
		acc.Exists = true
		store.PutAccount(k.Addr, acc)
	case KindCode:
		acc, _ := store.Account(k.Addr)
		acc.Code = append([]byte(nil), data...)
		acc.Exists = true
		store.PutAccount(k.Addr, acc)
	case KindExist:
		acc, ok := store.Account(k.Addr)
		if !ok {
			acc.Balance = uint256.NewInt(0)
		}
		acc.Exists = decBool(data)
		if !acc.Exists {
			acc.Balance = uint256.NewInt(0)
			acc.Nonce = 0
			acc.Code = nil
			store.PutAccount(k.Addr, acc)
			store.Wipe(k.Addr)
		} else {
			store.PutAccount(k.Addr, acc)
		}
	}
}

// ClearVersions drops multi-version state after a fold. Fees are retained
// until the ledger is discarded.
func (l *Ledger) ClearVersions() {
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		sh.keys = map[Key]*keyState{}
		sh.mu.Unlock()
	}
	for i := range l.touch {
		clear(l.touch[i].reads)
		clear(l.touch[i].writes)
	}
}

func versionData(vers []version, tx int) []byte {
	for i := range vers {
		if vers[i].tx == tx {
			return vers[i].data
		}
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
