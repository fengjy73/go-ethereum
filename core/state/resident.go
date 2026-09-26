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

package state

import (
	"maps"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// ResidentAccount is a post-execution image of one cached account, including
// storage written during the block. It exists so callers can compare execution
// results without committing a trie or depending on preimages.
type ResidentAccount struct {
	Exists  bool
	Balance *uint256.Int
	Nonce   uint64
	Code    []byte
	Slots   map[common.Hash]common.Hash
}

// Residents returns every account still cached after execution, plus accounts
// deleted during the block. Pending storage holds the post-transaction values.
func (s *StateDB) Residents() map[common.Address]*ResidentAccount {
	out := make(map[common.Address]*ResidentAccount, len(s.stateObjects)+len(s.stateObjectsDestruct))
	for addr, obj := range s.stateObjects {
		slots := make(map[common.Hash]common.Hash, len(obj.pendingStorage))
		maps.Copy(slots, obj.pendingStorage)
		out[addr] = &ResidentAccount{
			Exists:  true,
			Balance: new(uint256.Int).Set(obj.Balance()),
			Nonce:   obj.Nonce(),
			Code:    append([]byte(nil), obj.Code()...),
			Slots:   slots,
		}
	}
	for addr := range s.stateObjectsDestruct {
		if _, ok := out[addr]; ok {
			continue
		}
		out[addr] = &ResidentAccount{
			Exists:  false,
			Balance: uint256.NewInt(0),
			Slots:   map[common.Hash]common.Hash{},
		}
	}
	return out
}
