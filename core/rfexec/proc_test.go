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
	"runtime"
	"testing"
	"time"
)

func TestProcGateRestoresAndIgnoresStale(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	g := newProcGate(4)
	g.arm(1)
	g.request(1, 1)
	deadline := time.Now().Add(2 * time.Second)
	for runtime.GOMAXPROCS(0) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := runtime.GOMAXPROCS(0); got != 1 {
		t.Fatalf("active GOMAXPROCS %d", got)
	}
	g.restore()
	if got := runtime.GOMAXPROCS(0); got != 4 {
		t.Fatalf("restored GOMAXPROCS %d", got)
	}
	g.request(1, 1)
	time.Sleep(20 * time.Millisecond)
	if got := runtime.GOMAXPROCS(0); got != 4 {
		t.Fatalf("stale request set GOMAXPROCS %d", got)
	}
}
