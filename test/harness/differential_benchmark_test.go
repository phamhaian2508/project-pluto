package harness_test

import (
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// benchmarkExecutor measures one implementation on an already-built workload.
// It is shared by sequential and future parallel benchmark subtests.
func benchmarkExecutor(b *testing.B, executor evm.Executor, workload differentialWorkload) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runPlutoDifferentialWorkload(b, executor, workload)
	}
}

// BenchmarkDifferentialWorkloads establishes repeatable sequential baselines
// for low-conflict and high-conflict workload classes.
func BenchmarkDifferentialWorkloads(b *testing.B) {
	workloads := []struct {
		name string
		make func(testing.TB) differentialWorkload
	}{
		{name: "transfers-independent", make: func(tb testing.TB) differentialWorkload { return newTransferWorkload(tb, 1001, 100, 32, 0) }},
		{name: "transfers-hot-wallet-80pct", make: func(tb testing.TB) differentialWorkload { return newTransferWorkload(tb, 1002, 100, 32, 0.8) }},
		{name: "counter-storage-contention", make: func(tb testing.TB) differentialWorkload { return newCounterContentionWorkload(tb, 64) }},
		{name: "erc20-transfer", make: func(tb testing.TB) differentialWorkload { return newERC20DifferentialWorkload(tb) }},
		{name: "constant-product-amm", make: func(tb testing.TB) differentialWorkload { return newAMMDifferentialWorkload(tb, 32) }},
	}
	for _, workloadCase := range workloads {
		b.Run(workloadCase.name, func(b *testing.B) {
			workload := workloadCase.make(b)
			executor := evm.NewSequentialExecutor(newChainConfig(), chainID)
			benchmarkExecutor(b, executor, workload)
		})
	}
}
