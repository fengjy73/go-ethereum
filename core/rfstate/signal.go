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

// SigKind classifies a cooperative yield out of EVM execution.
type SigKind uint8

const (
	// SigRollback means a lower writer invalidated this attempt.
	SigRollback SigKind = iota
	// SigWaitFinal waits for a lower producer to finish.
	SigWaitFinal
	// SigWaitPrefix waits until every lower transaction is final.
	SigWaitPrefix
	// SigWaitEstimate waits out a Block-STM ESTIMATE marker.
	SigWaitEstimate
)

// Signal is panicked by TxView and recovered by the scheduler.
// It is not an execution failure.
type Signal struct {
	Kind   SigKind
	Depend int
	Seq    int
}

func (s Signal) Error() string { return "rfstate signal" }

// Deps is the scheduler surface TxView needs for fences. Nil deps never block.
type Deps interface {
	PrefixFinal(tx int) bool
	TxSettled(tx int) bool
	// Parallel is true when more than one worker is executing this block.
	// Early publication is skipped otherwise so a single worker pays nothing.
	Parallel() bool
}

// Mode selects the concurrency-control read path.
type Mode uint8

const (
	ModeRF Mode = iota
	ModeOCC
	ModeDirect
)
