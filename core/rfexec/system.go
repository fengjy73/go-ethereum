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
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// runPre applies EIP-4788 and EIP-2935. A non-nil access list is passed because
// the exported helpers merge into the receiver; a nil receiver panics on
// Cancun even though the merge result is discarded. The serial processor is
// not modified.
func runPre(env *BlockEnv, evm *vm.EVM) error {
	cfg := params.MainnetChainConfig
	dummy := bal.NewConstructionBlockAccessList()
	if cfg.IsCancun(env.Header.Number, env.Header.Time) && env.Header.ParentBeaconRoot != nil {
		core.ProcessBeaconBlockRoot(*env.Header.ParentBeaconRoot, evm, dummy)
	}
	if cfg.IsPrague(env.Header.Number, env.Header.Time) || cfg.IsUBT(env.Header.Number, env.Header.Time) {
		// The fixture does not include a full parent header, so Header.Hash of
		// a synthetic parent would not be the real parent hash.
		core.ProcessParentBlockHash(env.Header.ParentHash, evm, dummy)
	}
	return nil
}

func runPost(env *BlockEnv, evm *vm.EVM, logs []*types.Log) error {
	cfg := params.MainnetChainConfig
	if !cfg.IsPrague(env.Header.Number, env.Header.Time) && !cfg.IsUBT(env.Header.Number, env.Header.Time) {
		return nil
	}
	rules := cfg.Rules(env.Header.Number, true, env.Header.Time)
	dummy := bal.NewConstructionBlockAccessList()
	var requests [][]byte
	if err := core.ParseDepositLogs(&requests, logs, cfg); err != nil {
		return fmt.Errorf("deposit logs: %w", err)
	}
	if err := core.ProcessWithdrawalQueue(&requests, rules, evm, 0, dummy); err != nil {
		return err
	}
	if err := core.ProcessConsolidationQueue(&requests, rules, evm, 0, dummy); err != nil {
		return err
	}
	return nil
}

func runWithdrawals(env *BlockEnv, db vm.StateDB) {
	var list *bal.ConstructionBlockAccessList
	if params.MainnetChainConfig.IsAmsterdam(env.Header.Number, env.Header.Time) {
		list = bal.NewConstructionBlockAccessList()
	}
	env.Chain.Engine().Finalize(env.Chain, env.Header, db, env.Block.Body(), uint32(len(env.Txs)+1), list)
	// Beacon withdrawals credit balances without a transaction finalise.
	// The serial StateDB applies AddBalance in place. A direct TxView keeps
	// the credit in its journal until Finalise flushes it to the store.
	if view, ok := db.(*rfstate.TxView); ok {
		rules := params.MainnetChainConfig.Rules(env.Header.Number, true, env.Header.Time)
		view.Finalise(rules)
	}
}

func directView(store *rfstate.Store, coinbase common.Address) *rfstate.TxView {
	return rfstate.NewTxView(rfstate.ModeDirect, -1, 0, 0, store, nil, nil, nil, nil, coinbase)
}

func newStackTrie() *trie.StackTrie { return trie.NewStackTrie(nil) }
