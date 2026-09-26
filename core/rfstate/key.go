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
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// Kind is a field-level state key. Balance conflicts must not be confused
// with nonce, code, or an unrelated slot of the same account.
type Kind uint8

const (
	KindBalance Kind = iota
	KindNonce
	KindCode
	KindExist
	KindWipe
	KindSlot
)

// Key is one shared-state location.
type Key struct {
	Kind Kind
	Addr common.Address
	Slot common.Hash
}

func BalanceKey(addr common.Address) Key { return Key{Kind: KindBalance, Addr: addr} }
func NonceKey(addr common.Address) Key   { return Key{Kind: KindNonce, Addr: addr} }
func CodeKey(addr common.Address) Key    { return Key{Kind: KindCode, Addr: addr} }
func ExistKey(addr common.Address) Key   { return Key{Kind: KindExist, Addr: addr} }
func WipeKey(addr common.Address) Key    { return Key{Kind: KindWipe, Addr: addr} }
func SlotKeyOf(addr common.Address, slot common.Hash) Key {
	return Key{Kind: KindSlot, Addr: addr, Slot: slot}
}

func encU64(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

func decU64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b[:8])
}

func encBalance(v *uint256.Int) []byte {
	if v == nil {
		return make([]byte, 32)
	}
	b := v.Bytes32()
	return b[:]
}

func decBalance(b []byte) *uint256.Int {
	v := uint256.NewInt(0)
	if len(b) == 0 {
		return v
	}
	v.SetBytes(b)
	return v
}

func encHash(h common.Hash) []byte { return append([]byte(nil), h[:]...) }

func decHash(b []byte) common.Hash {
	var h common.Hash
	copy(h[:], b)
	return h
}

func encBool(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

func decBool(b []byte) bool { return len(b) > 0 && b[0] == 1 }
