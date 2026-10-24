package harness_test

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

type mutationKind int

const (
	mutateInvalidFlag mutationKind = iota
	mutateCommittedStorage
)

// mutationExecutor được cố ý tạo lỗi và chỉ dùng trong kiểm thử. Nó mô phỏng
// hai dạng hồi quy: chấp nhận giao dịch không hợp lệ và chốt sai kết quả lưu trữ.
type mutationExecutor struct {
	base   evm.Executor
	kind   mutationKind
	target common.Address
}

func (m mutationExecutor) ExecuteBlock(state evm.StateReader, env evm.BlockEnv, txs []*types.Transaction) (*evm.BlockResult, error) {
	result, err := m.base.ExecuteBlock(state, env, txs)
	if err != nil {
		return nil, err
	}
	switch m.kind {
	case mutateInvalidFlag:
		result.Results[len(result.Results)-1].Invalid = false
	case mutateCommittedStorage:
		stateDB, ok := state.(*evm.PebbleStateDB)
		if !ok {
			return nil, fmt.Errorf("mutation requires PebbleStateDB, got %T", state)
		}
		stateDB.SetState(m.target, common.Hash{}, common.BigToHash(big.NewInt(999)))
		copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
		result.Writes, err = stateDB.PendingWrites()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func TestHarnessDetectsInjectedMutations(t *testing.T) {
	t.Run("invalid transaction accepted", func(t *testing.T) {
		workload := newFailureModesDifferentialWorkload(t)
		mutant := mutationExecutor{
			base: evm.NewSequentialExecutor(newChainConfig(), chainID),
			kind: mutateInvalidFlag,
		}
		observed := runPlutoDifferentialWorkload(t, mutant, workload)
		geth := runGethDifferentialWorkload(t, workload)
		mismatches := differentialMismatches(workload, "mutant", observed, "geth reference", geth)
		if len(mismatches) == 0 || !strings.Contains(mismatches[0], "tx[3]") {
			t.Fatalf("harness failed to identify invalid-transaction mutation: %v", mismatches)
		}
	})

	t.Run("wrong storage committed", func(t *testing.T) {
		workload := newCounterDifferentialWorkload(t)
		mutant := mutationExecutor{
			base:   evm.NewSequentialExecutor(newChainConfig(), chainID),
			kind:   mutateCommittedStorage,
			target: workload.contract,
		}
		observed := runPlutoDifferentialWorkload(t, mutant, workload)
		geth := runGethDifferentialWorkload(t, workload)
		mismatches := differentialMismatches(workload, "mutant", observed, "geth reference", geth)
		if len(mismatches) == 0 {
			t.Fatal("harness failed to identify committed-storage mutation")
		}
	})
}
