package harness_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// TestAllWorkloadsMatchSequentialParallelAndGeth tạo ma trận đối chiếu các
// workload với Sequential, OCC, Grouped, Predicted-OCC và Geth tích hợp.
// Tên Parallel được giữ để các lệnh cũ trong tài liệu vẫn chạy được.
func TestAllWorkloadsMatchSequentialParallelAndGeth(t *testing.T) {
	cases := []struct {
		name string
		make func(testing.TB) differentialWorkload
	}{
		{name: "Chuyển coin giữa các ví", make: func(tb testing.TB) differentialWorkload { return newIndependentTransferComparisonWorkload(tb, 128) }},
		{name: "Chuyển coin qua ví nóng", make: func(tb testing.TB) differentialWorkload { return newTransferWorkload(tb, 0xA110, 128, 16, 0.8) }},
		{name: "Counter và giao dịch thường", make: func(tb testing.TB) differentialWorkload { return newCounterDifferentialWorkload(tb) }},
		{name: "Nhiều ví gọi cùng Counter", make: func(tb testing.TB) differentialWorkload { return newMultiSenderCounterContentionWorkload(tb, 32) }},
		{name: "Access list đúng", make: func(tb testing.TB) differentialWorkload { return newAccessListCounterContentionWorkload(tb, 16, true) }},
		{name: "Access list sai", make: func(tb testing.TB) differentialWorkload { return newAccessListCounterContentionWorkload(tb, 16, false) }},
		{name: "ERC-20 deploy và chuyển token", make: func(tb testing.TB) differentialWorkload { return newERC20DifferentialWorkload(tb) }},
		{name: "AMM swap", make: func(tb testing.TB) differentialWorkload { return newAMMDifferentialWorkload(tb, 8) }},
		{name: "Gọi contract lồng nhau", make: func(tb testing.TB) differentialWorkload { return newNestedCallDifferentialWorkload(tb) }},
		{name: "Revert, hết gas, sai nonce", make: func(tb testing.TB) differentialWorkload { return newFailureModesDifferentialWorkload(tb) }},
		{name: "Block trộn 10.000 giao dịch", make: func(tb testing.TB) differentialWorkload { return newMixedWorkload(tb, 20261012, 10_000, 16, 0.8) }},
	}

	fmt.Fprintln(os.Stdout, "\nMA TRẬN KIỂM TRA TÍNH ĐÚNG ĐẮN")
	fmt.Fprintln(os.Stdout, "Mỗi workload chạy trên Sequential, OCC auto, OCC 4 worker, Grouped, Predicted-OCC và Geth.")
	fmt.Fprintln(os.Stdout, "PASS chỉ in khi các kết quả giao dịch và trạng thái được harness so sánh khớp.")
	fmt.Fprintln(os.Stdout, "Trường hợp                            | Số giao dịch | Kết quả")

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			workload := testCase.make(t)
			occAuto := evm.NewAdaptiveParallelExecutor(newChainConfig(), chainID, 0)
			occAuto.SlidingWindow = true
			occFixed := evm.NewParallelExecutor(newChainConfig(), chainID, 4)
			occFixed.SlidingWindow = true
			compareAllExecutors(t, workload,
				namedExecutor{name: "Sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
				namedExecutor{name: "OCC auto workers", ev: occAuto},
				namedExecutor{name: "OCC 4 workers", ev: occFixed},
				namedExecutor{name: "Grouped", ev: evm.NewGroupedExecutor(newChainConfig(), chainID, 4)},
				namedExecutor{name: "Predicted-OCC", ev: evm.NewPredictedOCCExecutor(newChainConfig(), chainID, 4)},
			)
			fmt.Fprintf(os.Stdout, "%-36s | %6d | PASS\n", testCase.name, len(workload.transactions))
		})
	}
}
