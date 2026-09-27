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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
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
	for _, eng := range []string{EngineOCC, EngineRF, EngineAuto} {
		cs := []int{1, 2}
		if eng == EngineAuto {
			cs = []int{0}
		}
		for _, c := range cs {
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
		for _, eng := range []string{EngineOCC, EngineRF, EngineAuto} {
			runC := cs
			if eng == EngineAuto {
				runC = []int{0}
			}
			for _, c := range runC {
				out, err := ExecEngine(env, eng, pool, c, rfstate.NewLearner())
				if err != nil {
					t.Fatalf("%s block %d C=%d: %v", eng, env.Number, c, err)
				}
				if err := CheckAgainstSerial(serial, out, env.World); err != nil {
					t.Fatalf("%s block %d C=%d: %v", eng, env.Number, c, err)
				}
				t.Logf("block %d %s C=%d %s execs %d rollbacks %d active %d trace %s", env.Number, eng, c, out.Wall, out.Counters.Executions, out.Counters.Rollbacks, out.ActiveC, out.CTrace)
			}
		}
	}
}

// fixtureDirs is RF_FIXTURES (comma or path-list separated) or the default
// extracted trees: fixtures-a at /tmp/fixa and fixtures-b at /tmp/fixb.
func fixtureDirs() []string {
	if v := strings.TrimSpace(os.Getenv("RF_FIXTURES")); v != "" {
		var dirs []string
		for _, p := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == os.PathListSeparator
		}) {
			p = strings.TrimSpace(p)
			if p != "" {
				dirs = append(dirs, p)
			}
		}
		return dirs
	}
	var dirs []string
	for _, d := range []string{"/tmp/fixa", "/tmp/fixb"} {
		if _, err := os.Stat(d); err == nil {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func loadFixtures(t *testing.T) []*BlockEnv {
	t.Helper()
	dirs := fixtureDirs()
	if len(dirs) == 0 {
		return nil
	}
	blocks, err := LoadFixtures(dirs...)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func findBlockDir(number string) string {
	for _, dir := range fixtureDirs() {
		sub := filepath.Join(dir, number)
		if _, err := os.Stat(filepath.Join(sub, "block.json.gz")); err == nil {
			return sub
		}
	}
	return ""
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

// TestEarlyWriteDroppedOnWait is a speculative store followed by a fence
// wait. The waiting attempt publishes the store, then the retry takes the
// other branch and does not. The early bytes must not remain in the fold.
func TestEarlyWriteDroppedOnWait(t *testing.T) {
	env := earlyWaitEnv(t)
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
	slot1 := common.BigToHash(big.NewInt(1))
	if got := serial.DB.GetState(contract, slot1); got != (common.Hash{}) {
		t.Fatalf("serial slot1 %s", got.Hex())
	}
	learner := rfstate.NewLearner()
	hot := rfstate.SlotKeyOf(contract, slot1)
	fence := rfstate.SlotKeyOf(contract, common.BigToHash(big.NewInt(2)))
	learner.ObserveConflict(hot)
	learner.ObserveWriteShape(hot, true)
	for i := 0; i < 40; i++ {
		learner.ObserveConflict(fence)
	}
	learner.ObserveWriteShape(fence, true)
	// Conflicts alone do not fence. Half of a short producer is cheaper
	// than the likely re-execution, so this key waits.
	learner.ObserveProducer(fence, 100)
	learner.ObserveReexec(fence, 1000)
	pool := rfstate.NewPool([]int{0, 1}, 2)
	defer pool.Stop()
	var waits int
	for i := 0; i < 20; i++ {
		out, err := ExecEngine(env, EngineRF, pool, 2, learner.Clone())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		waits += int(out.Counters.WaitFinal)
		if err := CheckAgainstSerial(serial, out, env.World); err != nil {
			t.Fatalf("run %d waits %d: %v", i, out.Counters.WaitFinal, err)
		}
	}
	if waits == 0 {
		t.Fatal("fence wait never ran; the abandoned early write was not exercised")
	}
}

func earlyWaitEnv(t *testing.T) *BlockEnv {
	t.Helper()
	aliceKey := mustKey(t)
	bobKey := mustKey(t)
	alice := crypto.PubkeyToAddress(aliceKey.PublicKey)
	bob := crypto.PubkeyToAddress(bobKey.PublicKey)
	contract := common.HexToAddress("0x1000")
	coin := common.HexToAddress("0xc0ffee")
	// data 0x01 runs the producer: publish slot 2, burn gas, then clear slot 0.
	// data 0x00 loads slot 0 and, when it is still nonzero, stores slot 1,
	// then loads the fenced slot 2.
	code := assembleEarlyWait()
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
			Gas:       2_000_000,
			To:        &contract,
			Data:      data,
		})
	}
	txs := []*types.Transaction{
		mk(aliceKey, 0, []byte{1}),
		mk(bobKey, 0, []byte{0}),
	}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
	world := &rfstate.World{
		Accounts: map[common.Address]*rfstate.Account{
			alice:    {Exists: true, Balance: uint256.NewInt(0).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000)), Nonce: 0},
			bob:      {Exists: true, Balance: uint256.NewInt(0).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000)), Nonce: 0},
			contract: {Exists: true, Balance: uint256.NewInt(0), Nonce: 1, Code: code},
		},
		Slots: map[rfstate.SlotKey]common.Hash{
			{Addr: contract, Slot: common.BigToHash(big.NewInt(0))}: common.BigToHash(big.NewInt(1)),
		},
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

// assembleEarlyWait builds the two-entry contract used by TestEarlyWriteDroppedOnWait.
func assembleEarlyWait() []byte {
	var code []byte
	emit := func(xs ...byte) { code = append(code, xs...) }
	push1 := func(n byte) { emit(0x60, n) }
	push2 := func(n uint16) { emit(0x61, byte(n>>8), byte(n)) }

	push1(0)
	emit(0x35) // CALLDATALOAD
	emit(0x15) // ISZERO
	tx1Jump := len(code)
	emit(0x60, 0, 0x57) // JUMPI tx1

	// Producer: SSTORE(2, 1), then 4000 warm SLOADs, then SSTORE(0, 0).
	push1(1)
	push1(2)
	emit(0x55)
	push2(4000)
	loop := len(code)
	emit(0x5b) // JUMPDEST
	push1(3)
	emit(0x54, 0x50) // SLOAD POP
	push1(1)
	emit(0x90, 0x03, 0x80) // SWAP1 SUB DUP1
	emit(0x60, byte(loop), 0x57)
	emit(0x50) // POP
	push1(0)
	push1(0)
	emit(0x55, 0x00)

	tx1 := len(code)
	code[tx1Jump+1] = byte(tx1)
	emit(0x5b)
	push1(0)
	emit(0x54, 0x15) // SLOAD ISZERO
	skipJump := len(code)
	emit(0x60, 0, 0x57)
	push1(0xbb)
	push1(1)
	emit(0x55) // SSTORE(1, 0xbb)
	skip := len(code)
	code[skipJump+1] = byte(skip)
	emit(0x5b)
	push1(2)
	emit(0x54, 0x50, 0x00) // SLOAD POP STOP
	return code
}

// TestFinishUnlockDoesNotRaceTouchMap forces the coinbase-validation
// unlock inside tryAdvanceLocked and writes touch[idx] from another
// goroutine for the whole window. finish used to iterate that map on
// the way out (crew noteIO). Concurrent iteration and write is fatal
// even without the race detector.
func TestFinishUnlockDoesNotRaceTouchMap(t *testing.T) {
	env := &BlockEnv{
		Txs:      make([]*types.Transaction, 2),
		PrevSame: []int{-1, -1},
		Header:   &types.Header{Coinbase: common.Address{9}},
	}
	s := newSched(env, rfstate.ModeRF, nil, nil)
	s.crew = newCrew(NewCostPrior(), 2, env)
	s.inflight = 1
	s.txs[0].status = stFinished
	s.txs[0].preds = []rfstate.CoinPred{{Empty: true, Val: false}}
	s.txs[1].status = stRunning
	seed := rfstate.SlotKeyOf(common.Address{1}, common.Hash{1})
	s.ledger.Read(1, 0, seed, true)

	stop := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	s.testDuringUnlock = func() {
		once.Do(func() {
			go func() {
				close(started)
				k := rfstate.SlotKeyOf(common.Address{2}, common.Hash{2})
				for {
					select {
					case <-stop:
						return
					default:
						s.ledger.Read(1, 3, k, true)
					}
				}
			}()
		})
		<-started
	}
	defer close(stop)

	var abort atomic.Bool
	view := rfstate.NewTxView(rfstate.ModeRF, 1, 0, 0, nil, s.ledger, nil, nil, &abort, common.Address{})
	defer view.Release()
	s.finish(1, 0, view, &core.ExecutionResult{UsedGas: 21000})
}

func TestLoadFixtureShape(t *testing.T) {
	sub := findBlockDir("22411250")
	if sub == "" {
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

func TestFrontierWidthCountsReadyHeads(t *testing.T) {
	env := &BlockEnv{
		Txs:      make([]*types.Transaction, 4),
		PrevSame: []int{-1, 0, -1, 2},
	}
	s := newSched(env, rfstate.ModeRF, nil, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, scan := s.widthNowLocked(), s.frontierWidthLocked(); got != 2 || scan != 2 {
		t.Fatalf("chain heads incremental %d scan %d", got, scan)
	}
	s.txs[0].status = stParked
	s.refreshWidthLocked(0)
	if got, scan := s.widthNowLocked(), s.frontierWidthLocked(); got != 1 || scan != 1 {
		t.Fatalf("parked head incremental %d scan %d", got, scan)
	}
	s.txs[0].status = stFinal
	s.frontier = 1
	s.refreshWidthLocked(0)
	if got, scan := s.widthNowLocked(), s.frontierWidthLocked(); got != 2 || scan != 2 {
		t.Fatalf("unblocked successor incremental %d scan %d", got, scan)
	}
}

func TestAutoPriorSurvivesFixtureTail(t *testing.T) {
	sub := findBlockDir("22411250")
	if sub == "" {
		t.Skip("fixture not present")
	}
	env, err := LoadFixture(sub)
	if err != nil {
		t.Fatal(err)
	}
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	pool := rfstate.NewPool([]int{0, 1, 2, 3}, 4)
	defer pool.Stop()
	out, err := ExecAuto(env, pool, rfstate.NewLearner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("trace %s active %d best %d execs %d rollbacks %d idle %d", out.CTrace, out.ActiveC, out.CrewBest, out.Counters.Executions, out.Counters.Rollbacks, out.Counters.IdleNs)
	open := out.CTrace
	if i := strings.IndexByte(open, '-'); i >= 0 {
		open = open[:i]
	}
	if open != strconv.Itoa(out.CrewBest) {
		t.Fatalf("tail rewrote the arm to %d trace %s", out.CrewBest, out.CTrace)
	}
}

func TestAutoDoesNotChangeGOMAXPROCS(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	env := syntheticEnv(t)
	pool := rfstate.NewPool([]int{0, 1, 2, 3}, 4)
	defer pool.Stop()
	if _, err := ExecAuto(env, pool, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := runtime.GOMAXPROCS(0); got != 4 {
		t.Fatalf("GOMAXPROCS %d", got)
	}
}

// TestCoinbaseReadSurvivesShrink forces the window that dropped coinbase
// fees on ict21: a lower transaction is still inside execute, the pool
// shrinks to one worker, and a higher transaction reads the coinbase.
// The read used to sample the live active count and skip the prefix fence,
// and the balance was not registered, so the missing fee committed.
func TestCoinbaseReadSurvivesShrink(t *testing.T) {
	env := coinbaseShrinkEnv(t)
	serial, err := ExecSerial(env)
	if err != nil {
		t.Fatal(err)
	}
	prev := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(prev)
	pool := rfstate.NewPool([]int{0, 1}, 2)
	defer pool.Stop()
	started := make(chan struct{})
	sawHigher := make(chan struct{})
	release := make(chan struct{})
	env.BeforeView = func(tx int) {
		switch tx {
		case 0:
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
		case 1:
			pool.SetActive(1)
			select {
			case <-sawHigher:
			default:
				close(sawHigher)
			}
		}
	}
	type result struct {
		out *Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Two transactions are below the cold tiny-block cutoff, which would
		// pin the block at one worker and never reach the higher transaction
		// while the lower one is parked in BeforeView. A measured arm of 2
		// is what this race is about: start above 1, then shrink.
		out, err := ExecAuto(env, pool, rfstate.NewLearner(), preferArm(2))
		done <- result{out, err}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("lower transaction did not start")
	}
	select {
	case <-sawHigher:
	case res := <-done:
		t.Fatalf("higher transaction did not start: %v", res.err)
	case <-time.After(10 * time.Second):
		t.Fatal("higher transaction did not reach its attempt")
	}
	close(release)
	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("execution did not finish after the shrink")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	if err := CheckAgainstSerial(serial, res.out, env.World); err != nil {
		t.Fatalf("shrink committed a stale coinbase read: %v", err)
	}
}

func coinbaseShrinkEnv(t *testing.T) *BlockEnv {
	t.Helper()
	aliceKey := mustKey(t)
	coinKey := mustKey(t)
	alice := crypto.PubkeyToAddress(aliceKey.PublicKey)
	coin := crypto.PubkeyToAddress(coinKey.PublicKey)
	bob := common.HexToAddress("0xb0b")
	contract := common.HexToAddress("0x1000")
	// BALANCE of the embedded coinbase, stored at slot 0.
	code := append([]byte{0x73}, coin.Bytes()...)
	code = append(code, 0x31, 0x60, 0x00, 0x55, 0x00)
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
	rich := new(uint256.Int).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000))
	tip := big.NewInt(2_000_000_000)
	tx0 := types.MustSignNewTx(aliceKey, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: tip,
		GasFeeCap: tip,
		Gas:       21000,
		To:        &bob,
		Value:     big.NewInt(1),
	})
	tx1 := types.MustSignNewTx(coinKey, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       100_000,
		To:        &contract,
		Value:     big.NewInt(0),
	})
	txs := []*types.Transaction{tx0, tx1}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
	env := &BlockEnv{
		Number:    header.Number.Uint64(),
		Header:    block.Header(),
		BlockHash: block.Hash(),
		Block:     block,
		Txs:       txs,
		World: &rfstate.World{
			Accounts: map[common.Address]*rfstate.Account{
				alice:    {Exists: true, Balance: rich.Clone(), Nonce: 0},
				coin:     {Exists: true, Balance: rich.Clone(), Nonce: 0},
				bob:      {Exists: true, Balance: uint256.NewInt(0), Nonce: 0},
				contract: {Exists: true, Balance: uint256.NewInt(0), Nonce: 1, Code: code},
			},
			Slots: map[rfstate.SlotKey]common.Hash{},
		},
		Hashes: map[uint64]common.Hash{},
		Chain:  newHeaderChain(params.MainnetChainConfig, block.Header(), map[uint64]common.Hash{}),
	}
	if err := env.prepareMessages(); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestLoadPostPectraShape(t *testing.T) {
	sub := findBlockDir("26060000")
	if sub == "" {
		t.Skip("post-pectra fixture not present")
	}
	env, err := LoadFixture(sub)
	if err != nil {
		t.Fatal(err)
	}
	if env.Number != 26060000 || len(env.Txs) == 0 || len(env.Receipts) != len(env.Txs) {
		t.Fatalf("loaded %+v txs %d receipts %d", env.Number, len(env.Txs), len(env.Receipts))
	}
	if env.Header.ParentBeaconRoot == nil || env.Header.RequestsHash == nil {
		t.Fatal("post-pectra header missing beacon root or requests hash")
	}
	if !params.MainnetChainConfig.IsOsaka(env.Header.Number, env.Header.Time) {
		t.Fatal("26060000 is not on the Osaka rules this tree uses")
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

func TestBribeToCoinbaseDoesNotPrefixWait(t *testing.T) {
	env := bribeEnv(t, false)
	serial, err := ExecSerial(env)
	if err != nil {
		t.Fatal(err)
	}
	pool := rfstate.NewPool([]int{0, 1}, 2)
	defer pool.Stop()
	out, err := ProcessRegionFence(env, pool, 2, rfstate.NewLearner())
	if err != nil {
		t.Fatal(err)
	}
	if out.Counters.WaitPrefix != 0 {
		t.Fatalf("bribe waited on the prefix %d times", out.Counters.WaitPrefix)
	}
	if err := CheckAgainstSerial(serial, out, env.World); err != nil {
		t.Fatal(err)
	}
}

func TestBalanceSeesCoinbaseCredits(t *testing.T) {
	env := bribeEnv(t, true)
	serial, err := ExecSerial(env)
	if err != nil {
		t.Fatal(err)
	}
	pool := rfstate.NewPool([]int{0, 1}, 2)
	defer pool.Stop()
	out, err := ProcessRegionFence(env, pool, 2, rfstate.NewLearner())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckAgainstSerial(serial, out, env.World); err != nil {
		t.Fatal(err)
	}
	t.Logf("wait_prefix %d", out.Counters.WaitPrefix)
}

func bribeEnv(t *testing.T, withBalance bool) *BlockEnv {
	t.Helper()
	aliceKey := mustKey(t)
	bobKey := mustKey(t)
	carolKey := mustKey(t)
	alice := crypto.PubkeyToAddress(aliceKey.PublicKey)
	bob := crypto.PubkeyToAddress(bobKey.PublicKey)
	carol := crypto.PubkeyToAddress(carolKey.PublicKey)
	coin := common.HexToAddress("0xc0ffee")
	contract := common.HexToAddress("0x1000")
	code := append([]byte{0x73}, coin.Bytes()...)
	code = append(code, 0x31, 0x5f, 0x55, 0x00) // BALANCE, PUSH0, SSTORE, STOP
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
	fund := new(uint256.Int).Mul(uint256.NewInt(1_000_000_000_000_000), uint256.NewInt(1000))
	mk := func(key *ecdsa.PrivateKey, nonce uint64, to common.Address, value int64, data []byte) *types.Transaction {
		return types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   big.NewInt(1),
			Nonce:     nonce,
			GasTipCap: big.NewInt(1_000_000_000),
			GasFeeCap: big.NewInt(2_000_000_000),
			Gas:       100_000,
			To:        &to,
			Value:     big.NewInt(value),
			Data:      data,
		})
	}
	txs := []*types.Transaction{
		mk(aliceKey, 0, bob, 1, nil),
		mk(carolKey, 0, coin, 1000, nil),
	}
	world := &rfstate.World{
		Accounts: map[common.Address]*rfstate.Account{
			alice: {Exists: true, Balance: new(uint256.Int).Set(fund), Nonce: 0},
			bob:   {Exists: true, Balance: new(uint256.Int).Set(fund), Nonce: 0},
			carol: {Exists: true, Balance: new(uint256.Int).Set(fund), Nonce: 0},
			coin:  {Exists: true, Balance: uint256.NewInt(1_000_000_000_000_000_000), Nonce: 0},
		},
		Slots: map[rfstate.SlotKey]common.Hash{},
	}
	if withBalance {
		txs = append(txs, mk(bobKey, 0, contract, 0, nil))
		world.Accounts[contract] = &rfstate.Account{Exists: true, Balance: uint256.NewInt(0), Nonce: 1, Code: code}
	}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
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
