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
}

type version struct {
	tx       int
	data     []byte
	estimate bool
}

type reader struct {
	tx      int
	attempt uint64
	obsTx   int
}

type keyState struct {
	mu      sync.Mutex
	vers    []version
	readers []reader
}

// Ledger is the per-key multi-version memory and reader registry.
// Writes publish at transaction finish (AT_FINISH). A published write
// invalidates higher-index readers that observed an older version.
type Ledger struct {
	mu      sync.Mutex
	keys    map[Key]*keyState
	writers map[Key][]int // txs that have published this key at least once

	feeMu sync.Mutex
	fees  []*uint256.Int

	onInvalidate func(Victim)
	learner      *Learner
}

// NewLedger returns an empty ledger sized for n transactions.
func NewLedger(n int, learner *Learner, onInvalidate func(Victim)) *Ledger {
	return &Ledger{
		keys:         map[Key]*keyState{},
		writers:      map[Key][]int{},
		fees:         make([]*uint256.Int, n),
		onInvalidate: onInvalidate,
		learner:      learner,
	}
}

func (l *Ledger) state(k Key) *keyState {
	l.mu.Lock()
	ks := l.keys[k]
	if ks == nil {
		ks = &keyState{}
		l.keys[k] = ks
	}
	l.mu.Unlock()
	return ks
}

// Read returns the latest version written by a transaction strictly below tx.
// When register is set, the reader is recorded under the key lock together
// with the version selection (R1).
func (l *Ledger) Read(tx int, attempt uint64, k Key, register bool) ReadResult {
	ks := l.state(k)
	ks.mu.Lock()
	defer ks.mu.Unlock()
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
			return res
		}
		res.FromVersion = true
		res.Data = append([]byte(nil), v.data...)
		res.ObsTx = v.tx
	}
	if register {
		ks.readers = append(ks.readers, reader{tx: tx, attempt: attempt, obsTx: res.ObsTx})
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
	data = append([]byte(nil), data...)
	prev := append([]byte(nil), base...)
	replaced := false
	for i := range ks.vers {
		if ks.vers[i].tx == tx {
			prev = append([]byte(nil), ks.vers[i].data...)
			ks.vers[i].data = data
			ks.vers[i].estimate = estimate
			replaced = true
			break
		}
	}
	if !replaced {
		ks.vers = append(ks.vers, version{tx: tx, data: data, estimate: estimate})
	}
	changed = estimate || !bytesEqual(prev, data)
	var victims []Victim
	if !estimate {
		for _, r := range ks.readers {
			if r.tx <= tx || r.obsTx >= tx {
				continue
			}
			observed := base
			if r.obsTx >= 0 {
				observed = versionData(ks.vers, r.obsTx)
			}
			if bytesEqual(observed, data) {
				continue
			}
			victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: k})
		}
	}
	// Drop reader entries that belong to finalized-looking duplicates of this tx.
	if len(ks.readers) > 4096 {
		dst := ks.readers[:0]
		for _, r := range ks.readers {
			if r.tx == tx {
				continue
			}
			dst = append(dst, r)
		}
		ks.readers = dst
	}
	ks.mu.Unlock()

	l.noteWriter(k, tx)
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
	l.mu.Lock()
	keys := make([]Key, 0, len(l.keys))
	states := make([]*keyState, 0, len(l.keys))
	for k, ks := range l.keys {
		keys = append(keys, k)
		states = append(states, ks)
	}
	l.mu.Unlock()
	for i, ks := range states {
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
					victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: keys[i]})
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
		ks.mu.Unlock()
		if had && !still {
			l.forgetWriter(keys[i], tx)
		}
		if len(victims) > 0 && l.learner != nil {
			l.learner.ObserveConflict(keys[i])
		}
		if l.onInvalidate != nil {
			for _, v := range victims {
				l.onInvalidate(v)
			}
		}
	}
}

func (l *Ledger) forgetWriter(k Key, tx int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ws := l.writers[k]
	dst := make([]int, 0, len(ws))
	for _, w := range ws {
		if w != tx {
			dst = append(dst, w)
		}
	}
	if len(dst) == 0 {
		delete(l.writers, k)
		return
	}
	l.writers[k] = dst
}

// MarkEstimate turns every version of tx into an ESTIMATE so higher readers block.
func (l *Ledger) MarkEstimate(tx int) {
	l.mu.Lock()
	keys := make([]*keyState, 0, len(l.keys))
	for _, ks := range l.keys {
		keys = append(keys, ks)
	}
	l.mu.Unlock()
	for _, ks := range keys {
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
	l.mu.Lock()
	keys := make([]Key, 0, len(l.keys))
	states := make([]*keyState, 0, len(l.keys))
	for k, ks := range l.keys {
		keys = append(keys, k)
		states = append(states, ks)
	}
	l.mu.Unlock()
	for i, ks := range states {
		ks.mu.Lock()
		had := false
		dst := ks.vers[:0]
		var removed []byte
		for _, v := range ks.vers {
			if v.tx == tx {
				had = true
				removed = v.data
				continue
			}
			dst = append(dst, v)
		}
		ks.vers = dst
		var victims []Victim
		if had {
			for _, r := range ks.readers {
				if r.tx > tx && r.obsTx == tx {
					victims = append(victims, Victim{Tx: r.tx, Attempt: r.attempt, Key: keys[i]})
				}
			}
			_ = removed
		}
		// Forget this tx's own reader entries.
		rd := ks.readers[:0]
		for _, r := range ks.readers {
			if r.tx == tx {
				continue
			}
			rd = append(rd, r)
		}
		ks.readers = rd
		ks.mu.Unlock()
		if len(victims) > 0 && l.learner != nil {
			l.learner.ObserveConflict(keys[i])
		}
		if l.onInvalidate != nil {
			for _, v := range victims {
				l.onInvalidate(v)
			}
		}
	}
}

// RemoveReaders drops reader registrations for one transaction without retracting writes.
func (l *Ledger) RemoveReaders(tx int) {
	l.mu.Lock()
	states := make([]*keyState, 0, len(l.keys))
	for _, ks := range l.keys {
		states = append(states, ks)
	}
	l.mu.Unlock()
	for _, ks := range states {
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

// LowerProducer is the highest transaction index below tx that has published k.
func (l *Ledger) LowerProducer(tx int, k Key) (int, bool) {
	l.mu.Lock()
	ws := append([]int(nil), l.writers[k]...)
	l.mu.Unlock()
	best := -1
	for _, w := range ws {
		if w < tx && w > best {
			best = w
		}
	}
	return best, best >= 0
}

func (l *Ledger) noteWriter(k Key, tx int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.writers[k] {
		if w == tx {
			return
		}
	}
	l.writers[k] = append(l.writers[k], tx)
}

// RecordFee stores the transaction-local coinbase fee. It is not a balance write.
func (l *Ledger) RecordFee(tx int, fee *uint256.Int) {
	l.feeMu.Lock()
	defer l.feeMu.Unlock()
	if fee == nil {
		l.fees[tx] = uint256.NewInt(0)
		return
	}
	l.fees[tx] = new(uint256.Int).Set(fee)
}

// SumFees returns the sum of recorded fees of transactions in [0, before).
func (l *Ledger) SumFees(before int) *uint256.Int {
	l.feeMu.Lock()
	defer l.feeMu.Unlock()
	sum := uint256.NewInt(0)
	if before > len(l.fees) {
		before = len(l.fees)
	}
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
	l.mu.Lock()
	keys := make([]Key, 0, len(l.keys))
	states := make([]*keyState, 0, len(l.keys))
	for k, ks := range l.keys {
		keys = append(keys, k)
		states = append(states, ks)
	}
	l.mu.Unlock()

	type folded struct {
		key  Key
		data []byte
		tx   int
	}
	var rows []folded
	for i, ks := range states {
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
			rows = append(rows, folded{key: keys[i], data: best.data, tx: best.tx})
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
	l.mu.Lock()
	l.keys = map[Key]*keyState{}
	l.writers = map[Key][]int{}
	l.mu.Unlock()
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
