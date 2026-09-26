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

import "testing"

func TestSetActiveIfIgnoresStaleGen(t *testing.T) {
	p := NewPool([]int{0}, 2)
	defer p.Stop()
	idle := func(int) {}
	done := func() bool { return true }
	gen := p.Drive(2, idle, done)
	if !p.SetActiveIf(gen, 1) {
		t.Fatal("current generation was ignored")
	}
	if p.Active() != 1 {
		t.Fatalf("active %d", p.Active())
	}
	gen2 := p.Drive(2, idle, done)
	if gen2 == gen {
		t.Fatal("generation did not advance")
	}
	if p.SetActiveIf(gen, 1) {
		t.Fatal("stale generation applied")
	}
	if p.Active() != 2 {
		t.Fatalf("active after stale update %d", p.Active())
	}
}
