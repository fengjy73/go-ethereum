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
	"github.com/holiman/uint256"
)

// CoinPred is one speculative Empty or Exist of the coinbase. It is checked
// again when every lower transaction is final. Empty is true for Empty and
// false for Exist. Val is the boolean returned to the caller. OwnFee is the
// transaction-local credit at the moment of the call, so a later credit in
// the same transaction does not make the check disagree with itself.
type CoinPred struct {
	Empty  bool
	Val    bool
	OwnFee uint256.Int
}

func readBelow(led *Ledger, tx int, k Key) ReadResult {
	if led == nil {
		return ReadResult{ObsTx: -1}
	}
	return led.Read(tx, 0, k, false)
}

// CoinbaseExist reports whether the coinbase account exists for tx, from
// versions below tx or the prestate. It does not register a reader and does
// not wait. Fee credits do not publish an existence key; Fold marks the
// account existing when it adds the aggregate.
func CoinbaseExist(store *Store, led *Ledger, tx int, coin common.Address) bool {
	res := readBelow(led, tx, ExistKey(coin))
	if res.FromVersion {
		return decBool(res.Data)
	}
	if store != nil {
		if acc := store.peek(coin); acc != nil {
			return acc.Exists
		}
	}
	return false
}

// CoinbaseEmpty is the EIP-161 predicate for coin. Balance is the version
// below tx (fees stripped) plus SumFees(tx) plus own. A caller that has not
// yet credited the coinbase passes a zero own.
func CoinbaseEmpty(store *Store, led *Ledger, tx int, coin common.Address, own *uint256.Int) bool {
	if !CoinbaseExist(store, led, tx, coin) {
		return true
	}
	nonce := uint64(0)
	if res := readBelow(led, tx, NonceKey(coin)); res.FromVersion {
		nonce = decU64(res.Data)
	} else if store != nil {
		if acc := store.peek(coin); acc != nil {
			nonce = acc.Nonce
		}
	}
	codeLen := 0
	if res := readBelow(led, tx, CodeKey(coin)); res.FromVersion {
		codeLen = len(res.Data)
	} else if store != nil {
		if acc := store.peek(coin); acc != nil {
			codeLen = len(acc.Code)
		}
	}
	bal := uint256.NewInt(0)
	if res := readBelow(led, tx, BalanceKey(coin)); res.FromVersion {
		if got := decBalance(res.Data); got != nil {
			bal = got
		}
	} else if store != nil {
		if acc := store.peek(coin); acc != nil && acc.Balance != nil {
			bal = new(uint256.Int).Set(acc.Balance)
		}
	}
	if led != nil {
		bal.Add(bal, led.SumFees(tx))
	}
	if own != nil && own.Sign() != 0 {
		bal.Add(bal, own)
	}
	return nonce == 0 && codeLen == 0 && (bal == nil || bal.IsZero())
}
