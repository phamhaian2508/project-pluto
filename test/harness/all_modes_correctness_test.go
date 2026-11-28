package harness_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// TestAllExecutionModesMatchSequentialAndGeth chạy cùng một workload qua cả
// bốn executor để tránh suy rộng kết quả từ các bài kiểm thử chuyên biệt.
func TestAllExecutionModesMatchSequentialAndGeth(t *testing.T) {
	cases := []struct {
		name string
		make func(testing.TB) differentialWorkload
	}{
		{name: "independent-transfers", make: func(tb testing.TB) differentialWorkload {
			return newIndependentTransferComparisonWorkload(tb, 64)
		}},
		{name: "shared-counter-contention", make: func(tb testing.TB) differentialWorkload {
			return newMultiSenderCounterContentionWorkload(tb, 16)
		}},
		{name: "correct-access-list", make: func(tb testing.TB) differentialWorkload {
			return newAccessListCounterContentionWorkload(tb, 8, true)
		}},
		{name: "incorrect-access-list", make: func(tb testing.TB) differentialWorkload {
			return newAccessListCounterContentionWorkload(tb, 8, false)
		}},
		{name: "erc20-transfers", make: func(tb testing.TB) differentialWorkload {
			return newERC20DifferentialWorkload(tb)
		}},
		{name: "amm-swaps", make: func(tb testing.TB) differentialWorkload {
			return newAMMDifferentialWorkload(tb, 8)
		}},
		{name: "mixed-transactions", make: func(tb testing.TB) differentialWorkload {
			return newMixedWorkload(tb, 0x4f43435f, 64, 12, 0.5)
		}},
		{name: "nested-contract-calls", make: func(tb testing.TB) differentialWorkload {
			return newNestedCallDifferentialWorkload(tb)
		}},
		{name: "revert-oog-invalid-transactions", make: func(tb testing.TB) differentialWorkload {
			return newFailureModesDifferentialWorkload(tb)
		}},
	}

	fmt.Fprintln(os.Stdout, "\nMA TRẬN ĐỐI CHIẾU ĐÚNG ĐẮN CÁC EXECUTOR")
	fmt.Fprintln(os.Stdout, "Cùng giao dịch và trạng thái đầu vào; đối chiếu receipt, gas, log, trạng thái quan sát được và Pluto state root với Sequential và geth.")
	fmt.Fprintln(os.Stdout, "Chế độ: Sequential, OCC, Grouped, Predicted-OCC.")

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			workload := tc.make(t)
			compareAllExecutors(t, workload,
				namedExecutor{name: "Sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
				namedExecutor{name: "OCC", ev: evm.NewParallelExecutor(newChainConfig(), chainID, 4)},
				namedExecutor{name: "Grouped", ev: evm.NewGroupedExecutor(newChainConfig(), chainID, 4)},
				namedExecutor{name: "Predicted-OCC", ev: evm.NewPredictedOCCExecutor(newChainConfig(), chainID, 4)},
			)
			fmt.Fprintf(os.Stdout, "%-28s | %5d giao dịch | PASS\n", tc.name, len(workload.transactions))
		})
	}
}
