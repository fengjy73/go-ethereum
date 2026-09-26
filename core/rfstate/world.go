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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// SlotKey identifies a storage cell in the block prestate.
type SlotKey struct {
	Addr common.Address
	Slot common.Hash
}

// Account is an immutable prestate (or folded) account.
// Code is shared and must not be mutated. CodeHash is the consensus hash of
// Code, computed when the account is sealed, so callers do not rehash.
type Account struct {
	Exists   bool
	Balance  *uint256.Int
	Nonce    uint64
	Code     []byte
	CodeHash common.Hash
}

// World is the immutable block pre-state: the union of per-tx prestates,
// taking the first-seen value of each account and slot in transaction order.
type World struct {
	Accounts map[common.Address]*Account
	Slots    map[SlotKey]common.Hash
}

// Store is the mutable committed overlay (pre-execution writes, then the
// folded result of parallel execution, then post-execution). It is not
// mutated while parallel workers run; those workers only read it.
type Store struct {
	world   *World
	acc     map[common.Address]*Account
	slots   map[SlotKey]common.Hash
	slotSet map[SlotKey]bool
	wiped   map[common.Address]bool
}

// NewStore returns an overlay that falls through to world.
func NewStore(world *World) *Store {
	if world == nil {
		world = &World{
			Accounts: map[common.Address]*Account{},
			Slots:    map[SlotKey]common.Hash{},
		}
	}
	return &Store{
		world:   world,
		acc:     map[common.Address]*Account{},
		slots:   map[SlotKey]common.Hash{},
		slotSet: map[SlotKey]bool{},
		wiped:   map[common.Address]bool{},
	}
}

// World returns the underlying prestate.
func (s *Store) World() *World { return s.world }

// Account resolves the committed account, overlay first.
func (s *Store) Account(addr common.Address) (Account, bool) {
	if a, ok := s.acc[addr]; ok {
		return cloneAccount(a), true
	}
	if s.world != nil {
		if a, ok := s.world.Accounts[addr]; ok {
			return cloneAccount(a), true
		}
	}
	return Account{Balance: uint256.NewInt(0)}, false
}

// Slot resolves a committed storage value. A wiped account hides prestate slots
// until a later explicit write.
func (s *Store) Slot(addr common.Address, slot common.Hash) common.Hash {
	k := SlotKey{addr, slot}
	if s.wiped[addr] {
		if s.slotSet[k] {
			return s.slots[k]
		}
		return common.Hash{}
	}
	if s.slotSet[k] {
		return s.slots[k]
	}
	if s.world != nil {
		return s.world.Slots[k]
	}
	return common.Hash{}
}

// PutAccount replaces the overlay account. The argument is copied.
func (s *Store) PutAccount(addr common.Address, a Account) {
	if a.CodeHash == (common.Hash{}) {
		a.Seal()
	}
	c := cloneAccount(&a)
	s.acc[addr] = &c
	if !a.Exists {
		s.Wipe(addr)
	}
}

// PutSlot writes a storage value and clears the wipe mask for that cell.
func (s *Store) PutSlot(addr common.Address, slot, val common.Hash) {
	k := SlotKey{addr, slot}
	s.slots[k] = val
	s.slotSet[k] = true
}

// Wiped reports whether prestate storage for addr is masked.
func (s *Store) Wiped(addr common.Address) bool { return s.wiped[addr] }

// Wipe marks the account's prestate storage as invisible.
func (s *Store) Wipe(addr common.Address) {
	s.wiped[addr] = true
	for k := range s.slotSet {
		if k.Addr == addr {
			delete(s.slotSet, k)
			delete(s.slots, k)
		}
	}
}

// EachAccount visits overlay accounts and prestate accounts not overridden.
func (s *Store) EachAccount(fn func(common.Address, Account)) {
	seen := map[common.Address]struct{}{}
	for addr, a := range s.acc {
		seen[addr] = struct{}{}
		fn(addr, cloneAccount(a))
	}
	if s.world == nil {
		return
	}
	for addr, a := range s.world.Accounts {
		if _, ok := seen[addr]; ok {
			continue
		}
		fn(addr, cloneAccount(a))
	}
}

// EachSlot visits every slot that has a committed value under the current mask.
func (s *Store) EachSlot(fn func(common.Address, common.Hash, common.Hash)) {
	seen := map[SlotKey]struct{}{}
	for k, ok := range s.slotSet {
		if !ok {
			continue
		}
		seen[k] = struct{}{}
		fn(k.Addr, k.Slot, s.slots[k])
	}
	if s.world == nil {
		return
	}
	for k, val := range s.world.Slots {
		if _, ok := seen[k]; ok {
			continue
		}
		if s.wiped[k.Addr] {
			continue
		}
		fn(k.Addr, k.Slot, val)
	}
}

func cloneAccount(a *Account) Account {
	if a == nil {
		return Account{Balance: uint256.NewInt(0)}
	}
	bal := uint256.NewInt(0)
	if a.Balance != nil {
		bal.Set(a.Balance)
	}
	return Account{
		Exists:   a.Exists,
		Balance:  bal,
		Nonce:    a.Nonce,
		Code:     a.Code,
		CodeHash: a.CodeHash,
	}
}

// Seal caches the code hash. Code bytes are treated as immutable afterwards.
func (a *Account) Seal() {
	if a == nil {
		return
	}
	a.CodeHash = CodeHash(a.Exists, a.Code)
}

// SeedSystemContracts inserts canonical system-contract code when the fixture
// prestate does not already contain it. Storage is left untouched so traced
// slots win. The beacon, history, withdrawal and consolidation contracts are
// invoked by PreExecution / PostExecution; without code those calls cannot run.
func SeedSystemContracts(w *World, cfg *params.ChainConfig, number uint64, time uint64) {
	if w.Accounts == nil {
		w.Accounts = map[common.Address]*Account{}
	}
	num := new(uint256.Int).SetUint64(number).ToBig()
	if cfg.IsCancun(num, time) {
		seedCode(w, params.BeaconRootsAddress, params.BeaconRootsCode)
	}
	if cfg.IsPrague(num, time) {
		seedCode(w, params.HistoryStorageAddress, params.HistoryStorageCode)
		seedCode(w, params.WithdrawalQueueAddress, params.WithdrawalQueueCode)
		seedCode(w, params.ConsolidationQueueAddress, params.ConsolidationQueueCode)
	}
}

func seedCode(w *World, addr common.Address, code []byte) {
	if len(code) == 0 {
		return
	}
	if a, ok := w.Accounts[addr]; ok && len(a.Code) > 0 {
		return
	}
	bal := uint256.NewInt(0)
	nonce := uint64(1)
	exists := true
	if a, ok := w.Accounts[addr]; ok {
		if a.Balance != nil {
			bal = new(uint256.Int).Set(a.Balance)
		}
		if a.Nonce != 0 {
			nonce = a.Nonce
		}
	}
	acc := &Account{
		Exists:  exists,
		Balance: bal,
		Nonce:   nonce,
		Code:    code,
	}
	acc.Seal()
	w.Accounts[addr] = acc
}

// CodeHash returns the consensus code hash for an account.
// Empty code uses the protocol empty-code hash; it is not recomputed.
func CodeHash(exists bool, code []byte) common.Hash {
	if !exists {
		return common.Hash{}
	}
	if len(code) == 0 {
		return types.EmptyCodeHash
	}
	return crypto.Keccak256Hash(code)
}
