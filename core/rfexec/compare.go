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
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rfstate"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

// CheckFixture compares an execution against the RPC receipts and header.
func CheckFixture(env *BlockEnv, out *Outcome) error {
	if out == nil {
		return fmt.Errorf("nil outcome")
	}
	if out.GasUsed != env.Header.GasUsed {
		return fmt.Errorf("block %d gasUsed got %d header %d", env.Number, out.GasUsed, env.Header.GasUsed)
	}
	if len(out.Receipts) != len(env.Receipts) {
		return fmt.Errorf("block %d receipt count got %d fixture %d", env.Number, len(out.Receipts), len(env.Receipts))
	}
	for i := range out.Receipts {
		if err := cmpReceipt(env.Number, i, out.Receipts[i], env.Receipts[i]); err != nil {
			return err
		}
	}
	if env.Header.ReceiptHash != (common.Hash{}) && out.Root != env.Header.ReceiptHash {
		return fmt.Errorf("block %d receiptsRoot got %s header %s", env.Number, out.Root.Hex(), env.Header.ReceiptHash.Hex())
	}
	return nil
}

// CheckAgainstSerial compares a parallel outcome to the serial oracle.
func CheckAgainstSerial(serial, other *Outcome, world *rfstate.World) error {
	if err := compareReceiptLists(serial.Receipts, other.Receipts); err != nil {
		return err
	}
	if serial.GasUsed != other.GasUsed {
		return fmt.Errorf("gasUsed serial %d other %d", serial.GasUsed, other.GasUsed)
	}
	return compareState(serial.DB, other.Store, world)
}

func compareReceiptLists(a, b []*types.Receipt) error {
	if len(a) != len(b) {
		return fmt.Errorf("receipt count %d vs %d", len(a), len(b))
	}
	for i := range a {
		if err := cmpReceipt(0, i, b[i], a[i]); err != nil {
			return err
		}
	}
	return nil
}

func cmpReceipt(block uint64, i int, got, want *types.Receipt) error {
	if got.Status != want.Status || got.GasUsed != want.GasUsed || got.CumulativeGasUsed != want.CumulativeGasUsed {
		return fmt.Errorf("block %d tx %d status/gas got %d/%d/%d want %d/%d/%d", block, i,
			got.Status, got.GasUsed, got.CumulativeGasUsed, want.Status, want.GasUsed, want.CumulativeGasUsed)
	}
	if got.ContractAddress != want.ContractAddress {
		return fmt.Errorf("block %d tx %d contract got %s want %s", block, i, got.ContractAddress.Hex(), want.ContractAddress.Hex())
	}
	if len(got.Logs) != len(want.Logs) {
		return fmt.Errorf("block %d tx %d logs %d vs %d", block, i, len(got.Logs), len(want.Logs))
	}
	for j := range got.Logs {
		gl, wl := got.Logs[j], want.Logs[j]
		if gl.Address != wl.Address || !bytes.Equal(gl.Data, wl.Data) || len(gl.Topics) != len(wl.Topics) {
			return fmt.Errorf("block %d tx %d log %d mismatch", block, i, j)
		}
		for t := range gl.Topics {
			if gl.Topics[t] != wl.Topics[t] {
				return fmt.Errorf("block %d tx %d log %d topic %d", block, i, j, t)
			}
		}
	}
	return nil
}

func compareState(db *state.StateDB, store *rfstate.Store, world *rfstate.World) error {
	if db == nil || store == nil {
		return fmt.Errorf("missing state image")
	}
	addrs := map[common.Address]struct{}{}
	if world != nil {
		for a := range world.Accounts {
			addrs[a] = struct{}{}
		}
	}
	res := db.Residents()
	for a := range res {
		addrs[a] = struct{}{}
	}
	store.EachAccount(func(a common.Address, _ rfstate.Account) { addrs[a] = struct{}{} })

	slots := map[rfstate.SlotKey]struct{}{}
	if world != nil {
		for k := range world.Slots {
			slots[k] = struct{}{}
		}
	}
	for a, r := range res {
		for slot := range r.Slots {
			slots[rfstate.SlotKey{Addr: a, Slot: slot}] = struct{}{}
		}
	}
	store.EachSlot(func(a common.Address, slot, _ common.Hash) {
		slots[rfstate.SlotKey{Addr: a, Slot: slot}] = struct{}{}
	})

	for addr := range addrs {
		se, sb, sn, sc := readDB(db, addr)
		pe, pb, pn, pc := readStore(store, addr)
		if se != pe || sn != pn || sb.Cmp(pb) != 0 || !bytes.Equal(norm(sc), norm(pc)) {
			return fmt.Errorf("account %s exist %v/%v nonce %d/%d bal %s/%s code %d/%d",
				addr.Hex(), se, pe, sn, pn, sb.Hex(), pb.Hex(), len(sc), len(pc))
		}
	}
	for k := range slots {
		sv := db.GetState(k.Addr, k.Slot)
		pv := store.Slot(k.Addr, k.Slot)
		if sv != pv {
			return fmt.Errorf("slot %s %s serial %s parallel %s", k.Addr.Hex(), k.Slot.Hex(), sv.Hex(), pv.Hex())
		}
	}
	return nil
}

func readDB(db *state.StateDB, addr common.Address) (bool, *uint256.Int, uint64, []byte) {
	ex := db.Exist(addr)
	bal := new(uint256.Int).Set(db.GetBalance(addr))
	if !ex {
		return false, uint256.NewInt(0), 0, nil
	}
	return true, bal, db.GetNonce(addr), append([]byte(nil), db.GetCode(addr)...)
}

func readStore(store *rfstate.Store, addr common.Address) (bool, *uint256.Int, uint64, []byte) {
	acc, ok := store.Account(addr)
	if !ok || !acc.Exists {
		return false, uint256.NewInt(0), 0, nil
	}
	bal := uint256.NewInt(0)
	if acc.Balance != nil {
		bal.Set(acc.Balance)
	}
	return true, bal, acc.Nonce, append([]byte(nil), acc.Code...)
}

func norm(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
