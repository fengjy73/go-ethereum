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
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// warmCrypto initializes the process-global Go KZG context. The first
// point-evaluation precompile otherwise spends about two seconds inside
// whichever timed run touches it.
func warmCrypto() {
	_ = kzg4844.UseCKZG(false)
}

// Outcome is one engine run. DB is set for the serial path; Store is set for
// the parallel paths. Both images include pre-execution, transactions, and
// post-execution withdrawals.
type Outcome struct {
	Receipts []*types.Receipt
	GasUsed  uint64
	Root     common.Hash
	DB       *state.StateDB
	Store    *rfstate.Store
	Counters Counters
	Wall     time.Duration
}

// Counters are the per-run measurements written to the benchmark CSV.
// Wait and idle times are sums across workers, not wall-clock overlaps.
type Counters struct {
	Executions    uint64
	Rollbacks     uint64
	Invalidations uint64
	WaitFinal     uint64
	WaitPrefix    uint64
	WaitDefer     uint64
	WaitOrder     uint64
	WaitNs        int64
	IdleNs        int64
	GCPauseNs     int64
}

// ExecSerial runs the unmodified ApplyTransaction path on a fresh in-memory
// state. State construction is outside the timed region. Pre-execution,
// transactions, post-execution, and withdrawals are inside it.
func ExecSerial(env *BlockEnv) (*Outcome, error) {
	db, err := buildState(env)
	if err != nil {
		return nil, err
	}
	evm := newEVM(env, db)
	gp := core.NewGasPool(env.Header.GasLimit)
	var receipts []*types.Receipt
	warmCrypto()
	t0 := time.Now()
	if err := runPre(env, evm); err != nil {
		return nil, err
	}
	ctx := context.Background()
	for i, tx := range env.Txs {
		db.SetTxContext(tx.Hash(), i, uint32(i+1))
		evm.SetTxContext(core.NewEVMTxContext(env.Msgs[i]))
		receipt, _, err := core.ApplyTransactionWithEVM(ctx, env.Msgs[i], gp, db, env.Header.Number, env.BlockHash, env.Header.Time, tx, evm)
		if err != nil {
			return nil, fmt.Errorf("serial tx %d: %w", i, err)
		}
		receipts = append(receipts, receipt)
	}
	var logs []*types.Log
	for _, r := range receipts {
		logs = append(logs, r.Logs...)
	}
	if err := runPost(env, evm, logs); err != nil {
		return nil, err
	}
	runWithdrawals(env, db)
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
		DB:       db,
		Wall:     wall,
	}, nil
}

func buildState(env *BlockEnv) (*state.StateDB, error) {
	db := state.NewDatabaseForTesting()
	sdb, err := state.New(types.EmptyRootHash, db)
	if err != nil {
		return nil, err
	}
	for addr, acc := range env.World.Accounts {
		if acc == nil || !acc.Exists {
			continue
		}
		bal := uint256.NewInt(0)
		if acc.Balance != nil {
			bal.Set(acc.Balance)
		}
		sdb.SetBalance(addr, bal, tracing.BalanceChangeUnspecified)
		sdb.SetNonce(addr, acc.Nonce, tracing.NonceChangeUnspecified)
		if len(acc.Code) > 0 {
			sdb.SetCode(addr, append([]byte(nil), acc.Code...), tracing.CodeChangeUnspecified)
		}
	}
	for k, val := range env.World.Slots {
		sdb.SetState(k.Addr, k.Slot, val)
	}
	rules := params.MainnetChainConfig.Rules(env.Header.Number, true, env.Header.Time)
	sdb.Finalise(rules)
	return sdb, nil
}

func newEVM(env *BlockEnv, db vm.StateDB) *vm.EVM {
	return vm.NewEVM(env.BlockContext(), db, params.MainnetChainConfig, vm.Config{DisableParallelExecution: true})
}
