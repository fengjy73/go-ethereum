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
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// headerChain serves ancestor headers for BLOCKHASH and consensus.Author.
// Header.Hash of a synthetic ancestor is not the real block hash; BLOCKHASH
// uses ParentHash walking, and EIP-2935 is given block.ParentHash directly.
type headerChain struct {
	cfg     *params.ChainConfig
	engine  consensus.Engine
	current *types.Header
	byNum   map[uint64]*types.Header
}

func newHeaderChain(cfg *params.ChainConfig, current *types.Header, hashes map[uint64]common.Hash) *headerChain {
	c := &headerChain{
		cfg:     cfg,
		engine:  beacon.New(ethash.NewFaker()),
		current: current,
		byNum:   map[uint64]*types.Header{},
	}
	num := current.Number.Uint64()
	for m := uint64(1); m <= 256 && num >= m; m++ {
		n := num - m
		h := &types.Header{
			Number:     new(big.Int).SetUint64(n),
			ParentHash: hashes[n-1],
			Difficulty: new(big.Int),
		}
		c.byNum[n] = h
	}
	return c
}

func (c *headerChain) Config() *params.ChainConfig { return c.cfg }

func (c *headerChain) CurrentHeader() *types.Header { return c.current }

func (c *headerChain) Engine() consensus.Engine { return c.engine }

func (c *headerChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	return c.byNum[number]
}

func (c *headerChain) GetHeaderByNumber(number uint64) *types.Header {
	if c.current != nil && c.current.Number.Uint64() == number {
		return c.current
	}
	return c.byNum[number]
}

func (c *headerChain) GetHeaderByHash(hash common.Hash) *types.Header {
	if c.current != nil && c.current.Hash() == hash {
		return c.current
	}
	for _, h := range c.byNum {
		if h.ParentHash == hash || h.Hash() == hash {
			return h
		}
	}
	return nil
}

// BlockContext builds the EVM block environment. GetHash reads the fixture
// map and is safe to call from many transactions at once.
func (env *BlockEnv) BlockContext() vm.BlockContext {
	header := env.Header
	beneficiary, _ := env.Chain.Engine().Author(header)
	var baseFee *big.Int
	if header.BaseFee != nil {
		baseFee = new(big.Int).Set(header.BaseFee)
	}
	var blobBaseFee *big.Int
	if header.ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(env.Chain.Config(), header)
	}
	var random *common.Hash
	if header.Difficulty.Sign() == 0 {
		mix := header.MixDigest
		random = &mix
	}
	var slot uint64
	if header.SlotNumber != nil {
		slot = *header.SlotNumber
	}
	return vm.BlockContext{
		CanTransfer:      core.CanTransfer,
		Transfer:         core.Transfer,
		GetHash:          env.GetHash,
		Coinbase:         beneficiary,
		BlockNumber:      new(big.Int).Set(header.Number),
		Time:             header.Time,
		Difficulty:       new(big.Int).Set(header.Difficulty),
		BaseFee:          baseFee,
		BlobBaseFee:      blobBaseFee,
		GasLimit:         header.GasLimit,
		Random:           random,
		SlotNum:          slot,
		CostPerStateByte: params.CostPerStateByte,
	}
}
