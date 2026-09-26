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

// Package rfexec executes mainnet block fixtures with the serial geth path,
// a Block-STM-style OCC baseline, and RegionFence P0.
package rfexec

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// BlockEnv is one fixture block, ready to execute. Loading it is outside the
// timed region.
type BlockEnv struct {
	Number    uint64
	Header    *types.Header
	BlockHash common.Hash
	Block     *types.Block
	Txs       []*types.Transaction
	Receipts  []*types.Receipt
	World     *rfstate.World
	Hashes    map[uint64]common.Hash
	Chain     *headerChain
	Senders   []common.Address
	PrevSame  []int
	Msgs      []*core.Message
}

// LoadFixture reads one block directory of gzipped RPC dumps.
func LoadFixture(dir string) (*BlockEnv, error) {
	blockRaw, err := readGzipJSON(filepath.Join(dir, "block.json.gz"))
	if err != nil {
		return nil, err
	}
	var header types.Header
	if err := header.UnmarshalJSON(blockRaw); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if header.Difficulty == nil {
		header.Difficulty = new(big.Int)
	}
	var side struct {
		Hash         common.Hash          `json:"hash"`
		Transactions []*types.Transaction `json:"transactions"`
		Withdrawals  []*types.Withdrawal  `json:"withdrawals"`
	}
	if err := json.Unmarshal(blockRaw, &side); err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	preRaw, err := readGzipJSON(filepath.Join(dir, "prestate.json.gz"))
	if err != nil {
		return nil, err
	}
	world, err := parsePrestate(preRaw)
	if err != nil {
		return nil, err
	}
	rcRaw, err := readGzipJSON(filepath.Join(dir, "receipts.json.gz"))
	if err != nil {
		return nil, err
	}
	var receipts []*types.Receipt
	if err := json.Unmarshal(rcRaw, &receipts); err != nil {
		return nil, fmt.Errorf("receipts: %w", err)
	}
	hashRaw, err := readGzipJSON(filepath.Join(dir, "blockhashes.json.gz"))
	if err != nil {
		return nil, err
	}
	var hashStr map[string]common.Hash
	if err := json.Unmarshal(hashRaw, &hashStr); err != nil {
		return nil, fmt.Errorf("blockhashes: %w", err)
	}
	hashes := make(map[uint64]common.Hash, len(hashStr))
	for k, v := range hashStr {
		n, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("blockhash key %q: %w", k, err)
		}
		hashes[n] = v
	}
	number := header.Number.Uint64()
	rfstate.SeedSystemContracts(world, params.MainnetChainConfig, number, header.Time)
	block := types.NewBlockWithHeader(&header).WithBody(types.Body{
		Transactions: side.Transactions,
		Withdrawals:  side.Withdrawals,
	})
	env := &BlockEnv{
		Number:    number,
		Header:    block.Header(),
		BlockHash: side.Hash,
		Block:     block,
		Txs:       side.Transactions,
		Receipts:  receipts,
		World:     world,
		Hashes:    hashes,
		Chain:     newHeaderChain(params.MainnetChainConfig, block.Header(), hashes),
	}
	if err := env.prepareMessages(); err != nil {
		return nil, err
	}
	return env, nil
}

// LoadFixtures loads every immediate subdirectory that contains block.json.gz
// and returns them sorted by block number.
func LoadFixtures(dirs ...string) ([]*BlockEnv, error) {
	var blocks []*BlockEnv
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", dir)
		}
		if _, err := os.Stat(filepath.Join(dir, "block.json.gz")); err == nil {
			env, err := LoadFixture(dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", dir, err)
			}
			blocks = append(blocks, env)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			info, err := os.Stat(sub)
			if err != nil || !info.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(sub, "block.json.gz")); err != nil {
				continue
			}
			env, err := LoadFixture(sub)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", sub, err)
			}
			blocks = append(blocks, env)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Number < blocks[j].Number })
	return blocks, nil
}

func (env *BlockEnv) prepareMessages() error {
	signer := types.MakeSigner(params.MainnetChainConfig, env.Header.Number, env.Header.Time)
	env.Msgs = make([]*core.Message, len(env.Txs))
	env.Senders = make([]common.Address, len(env.Txs))
	env.PrevSame = make([]int, len(env.Txs))
	last := map[common.Address]int{}
	for i, tx := range env.Txs {
		msg, err := core.TransactionToMessage(tx, signer, env.Header.BaseFee)
		if err != nil {
			return fmt.Errorf("tx %d: %w", i, err)
		}
		env.Msgs[i] = msg
		env.Senders[i] = msg.From
		if prev, ok := last[msg.From]; ok {
			env.PrevSame[i] = prev
		} else {
			env.PrevSame[i] = -1
		}
		last[msg.From] = i
	}
	return nil
}

// GetHash returns an ancestor hash. BLOCKHASH only reaches back 256 blocks,
// which is exactly the fixture's blockhashes.json coverage.
func (env *BlockEnv) GetHash(n uint64) common.Hash {
	if env.Number <= n || env.Number-n > 256 {
		return common.Hash{}
	}
	return env.Hashes[n]
}

func readGzipJSON(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(zr); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return buf.Bytes(), nil
}

type preEntry struct {
	Result map[string]preAccount `json:"result"`
	TxHash common.Hash           `json:"txHash"`
}

type preAccount struct {
	Balance string            `json:"balance"`
	Nonce   json.RawMessage   `json:"nonce"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage"`
}

func parsePrestate(raw []byte) (*rfstate.World, error) {
	var entries []preEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("prestate: %w", err)
	}
	w := &rfstate.World{
		Accounts: map[common.Address]*rfstate.Account{},
		Slots:    map[rfstate.SlotKey]common.Hash{},
	}
	// seenAcct remembers the first observation even when that observation is
	// an empty account. A later entry is the post-state of an in-block create
	// and must not become the block pre-state.
	seenAcct := map[common.Address]struct{}{}
	// preexisted accounts may reveal more original slots in later transactions.
	// An account whose first observation is empty did not exist; later slots
	// are writes from an in-block creation and are not block pre-state.
	preexisted := map[common.Address]bool{}
	for i, e := range entries {
		for addrHex, acc := range e.Result {
			addr := common.HexToAddress(addrHex)
			if _, seen := seenAcct[addr]; !seen {
				seenAcct[addr] = struct{}{}
				bal, err := parseHexUint256(acc.Balance)
				if err != nil {
					return nil, fmt.Errorf("prestate tx %d balance %s: %w", i, addrHex, err)
				}
				nonce, err := parseNonce(acc.Nonce)
				if err != nil {
					return nil, fmt.Errorf("prestate tx %d nonce %s: %w", i, addrHex, err)
				}
				code, err := parseHexBytes(acc.Code)
				if err != nil {
					return nil, fmt.Errorf("prestate tx %d code %s: %w", i, addrHex, err)
				}
				// EIP-161: a zero balance, zero nonce and empty code is not an
				// account. The tracer still emits that shape for addresses a
				// transaction touched. Keeping them makes the parallel image
				// retain an empty account that serial finalisation deletes.
				if bal.Sign() != 0 || nonce != 0 || len(code) > 0 {
					preexisted[addr] = true
					w.Accounts[addr] = &rfstate.Account{
						Exists:  true,
						Balance: bal,
						Nonce:   nonce,
						Code:    code,
					}
				}
			}
			if !preexisted[addr] {
				continue
			}
			for slotHex, valHex := range acc.Storage {
				slot, err := parseHexHash(slotHex)
				if err != nil {
					return nil, fmt.Errorf("prestate tx %d slot %s: %w", i, slotHex, err)
				}
				val, err := parseHexHash(valHex)
				if err != nil {
					return nil, fmt.Errorf("prestate tx %d slot value %s: %w", i, addrHex, err)
				}
				k := rfstate.SlotKey{Addr: addr, Slot: slot}
				if _, seen := w.Slots[k]; seen {
					continue
				}
				w.Slots[k] = val
			}
		}
	}
	return w, nil
}

func parseHexBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0x" || s == "0X" {
		return nil, nil
	}
	if len(s) >= 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
		s = s[2:]
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func parseHexHash(s string) (common.Hash, error) {
	b, err := parseHexBytes(s)
	if err != nil {
		return common.Hash{}, err
	}
	if len(b) > 32 {
		return common.Hash{}, fmt.Errorf("longer than 32 bytes")
	}
	var h common.Hash
	copy(h[32-len(b):], b)
	return h, nil
}

func parseHexUint256(s string) (*uint256.Int, error) {
	b, err := parseHexBytes(s)
	if err != nil {
		return nil, err
	}
	if len(b) > 32 {
		return nil, fmt.Errorf("overflow")
	}
	v := uint256.NewInt(0)
	v.SetBytes(b)
	return v, nil
}

func parseNonce(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 0 && s[0] == '"' {
		var u hexutil.Uint64
		if err := u.UnmarshalJSON(raw); err != nil {
			return 0, err
		}
		return uint64(u), nil
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n, nil
}
