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
	"crypto/ecdsa"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestSyntheticConflictAndNonceChain(t *testing.T) {
	env := syntheticEnv(t)
	serial, err := ExecSerial(env)
	if err != nil {
		t.Fatal(err)
	}
	var contract common.Address
	for addr, acc := range env.World.Accounts {
		if len(acc.Code) > 0 {
			contract = addr
		}
	}
	slot := common.BigToHash(big.NewInt(1))
	if got := serial.DB.GetState(contract, slot); got != common.BigToHash(big.NewInt(5)) {
		t.Fatalf("serial slot1 %s", got.Hex())
	}
	pool := rfstate.NewPool([]int{0, 1}, 2)
	defer pool.Stop()
	for _, eng := range []string{EngineOCC, EngineRF} {
		for _, c := range []int{1, 2} {
			out, err := ExecEngine(env, eng, pool, c, rfstate.NewLearner())
			if err != nil {
				t.Fatalf("%s C=%d: %v", eng, c, err)
			}
			if err := CheckAgainstSerial(serial, out, env.World); err != nil {
				t.Fatalf("%s C=%d: %v", eng, c, err)
			}
		}
	}
}

func TestFixtureEngines(t *testing.T) {
	blocks := loadFixtures(t)
	if len(blocks) == 0 {
		t.Skip("no fixtures")
	}
	pool := rfstate.NewPool(nil, 8)
	defer pool.Stop()
	cs := []int{1, 2, 4, 8}
	for _, env := range blocks {
		serial, err := ExecSerial(env)
		if err != nil {
			t.Fatalf("serial %d: %v", env.Number, err)
		}
		if err := CheckFixture(env, serial); err != nil {
			t.Fatalf("fixture %d: %v", env.Number, err)
		}
		t.Logf("block %d serial %s gas %d", env.Number, serial.Wall, serial.GasUsed)
		for _, eng := range []string{EngineOCC, EngineRF} {
			for _, c := range cs {
				out, err := ExecEngine(env, eng, pool, c, rfstate.NewLearner())
				if err != nil {
					t.Fatalf("%s block %d C=%d: %v", eng, env.Number, c, err)
				}
				if err := CheckAgainstSerial(serial, out, env.World); err != nil {
					t.Fatalf("%s block %d C=%d: %v", eng, env.Number, c, err)
				}
				t.Logf("block %d %s C=%d %s execs %d rollbacks %d", env.Number, eng, c, out.Wall, out.Counters.Executions, out.Counters.Rollbacks)
			}
		}
	}
}

func loadFixtures(t *testing.T) []*BlockEnv {
	t.Helper()
	dir := os.Getenv("RF_FIXTURES")
	if dir == "" {
		dir = "/tmp/fixa"
	}
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	blocks, err := LoadFixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func syntheticEnv(t *testing.T) *BlockEnv {
	t.Helper()
	aliceKey := mustKey(t)
	bobKey := mustKey(t)
	alice := crypto.PubkeyToAddress(aliceKey.PublicKey)
	bob := crypto.PubkeyToAddress(bobKey.PublicKey)
	contract := common.HexToAddress("0x1000")
	coin := common.HexToAddress("0xc0ffee")
	code := []byte{
		0x60, 0x00, 0x35, 0x15, 0x60, 0x0e, 0x57,
		0x60, 0x00, 0x54, 0x60, 0x01, 0x55, 0x00,
		0x5b, 0x60, 0x05, 0x60, 0x00, 0x55, 0x00,
	}
	header := &types.Header{
		ParentHash: common.HexToHash("0x01"),
		Coinbase:   coin,
		Number:     big.NewInt(21_000_000),
		GasLimit:   30_000_000,
		Time:       *params.MainnetChainConfig.CancunTime + 10,
		Difficulty: big.NewInt(0),
		BaseFee:    big.NewInt(1_000_000_000),
	}
	signer := types.LatestSigner(params.MainnetChainConfig)
	mk := func(key *ecdsa.PrivateKey, nonce uint64, data []byte) *types.Transaction {
		return types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   big.NewInt(1),
			Nonce:     nonce,
			GasTipCap: big.NewInt(1_000_000_000),
			GasFeeCap: big.NewInt(2_000_000_000),
			Gas:       100_000,
			To:        &contract,
			Value:     big.NewInt(0),
			Data:      data,
		})
	}
	txs := []*types.Transaction{
		mk(aliceKey, 0, nil),
		mk(bobKey, 0, []byte{1}),
		mk(aliceKey, 1, []byte{1}),
	}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
	world := &rfstate.World{
		Accounts: map[common.Address]*rfstate.Account{
			alice:    {Exists: true, Balance: uint256.NewInt(0).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000)), Nonce: 0},
			bob:      {Exists: true, Balance: uint256.NewInt(0).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000)), Nonce: 0},
			contract: {Exists: true, Balance: uint256.NewInt(0), Nonce: 1, Code: code},
		},
		Slots: map[rfstate.SlotKey]common.Hash{},
	}
	env := &BlockEnv{
		Number:    header.Number.Uint64(),
		Header:    block.Header(),
		BlockHash: block.Hash(),
		Block:     block,
		Txs:       txs,
		World:     world,
		Hashes:    map[uint64]common.Hash{},
		Chain:     newHeaderChain(params.MainnetChainConfig, block.Header(), map[uint64]common.Hash{}),
	}
	if err := env.prepareMessages(); err != nil {
		t.Fatal(err)
	}
	return env
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestLoadFixtureShape(t *testing.T) {
	dir := os.Getenv("RF_FIXTURES")
	if dir == "" {
		dir = "/tmp/fixa"
	}
	sub := filepath.Join(dir, "22411250")
	if _, err := os.Stat(sub); err != nil {
		t.Skip("fixture not present")
	}
	env, err := LoadFixture(sub)
	if err != nil {
		t.Fatal(err)
	}
	if env.Number != 22411250 || len(env.Txs) == 0 || len(env.Receipts) != len(env.Txs) {
		t.Fatalf("loaded %+v txs %d receipts %d", env.Number, len(env.Txs), len(env.Receipts))
	}
	if len(env.Hashes) != 256 {
		t.Fatalf("hashes %d", len(env.Hashes))
	}
	out, err := ExecSerial(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFixture(env, out); err != nil {
		t.Fatal(err)
	}
	t.Logf("serial %s gas %d", out.Wall, out.GasUsed)
}
