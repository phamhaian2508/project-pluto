// Package callframe is an isolated M5.1 spike for checkpoints at complete EVM
// call boundaries. It is not used by Pluto's production executor.
package callframe

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
)

var errNestedResumeUnsupported = errors.New("geth hooks cannot restore a parent interpreter after a nested call")

type callExit struct {
	depth    int
	output   []byte
	gasUsed  uint64
	err      error
	reverted bool
}

type recorder struct {
	entered []int
	exited  []callExit
}

func (r *recorder) hooks() *tracing.Hooks {
	return &tracing.Hooks{
		OnEnter: func(depth int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			r.entered = append(r.entered, depth)
		},
		OnExit: func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
			r.exited = append(r.exited, callExit{
				depth: depth, output: append([]byte(nil), output...), gasUsed: gasUsed, err: err, reverted: reverted,
			})
		},
	}
}

// boundaryCheckpoint deliberately stores only data available outside the EVM.
// Restoring it permits replaying the entire top-level frame, not resuming a
// suspended parent interpreter after a nested CALL.
type boundaryCheckpoint struct {
	snapshot int
	to       common.Address
	input    []byte
	gas      uint64
}

func takeBoundaryCheckpoint(st *state.StateDB, to common.Address, input []byte, gas uint64) boundaryCheckpoint {
	return boundaryCheckpoint{snapshot: st.Snapshot(), to: to, input: append([]byte(nil), input...), gas: gas}
}

func (c boundaryCheckpoint) restore(st *state.StateDB) { st.RevertToSnapshot(c.snapshot) }

// nestedCheckpoint documents the public-hook limitation: callExit has the
// child's result but no parent PC, stack, memory, remaining gas or return-data
// buffer, so an exact continuation cannot be constructed.
func nestedCheckpoint(callExit) error { return errNestedResumeUnsupported }
