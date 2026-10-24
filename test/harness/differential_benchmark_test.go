package harness_test

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// benchmarkExecutor đo một cách thực thi trên workload đã dựng sẵn. Hàm được
// dùng chung cho các bài benchmark tuần tự và song song.
func benchmarkExecutor(b *testing.B, executor evm.Executor, workload differentialWorkload) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runPlutoDifferentialWorkload(b, executor, workload)
	}
}

// BenchmarkDifferentialWorkloads tạo số liệu tuần tự có thể lặp lại cho các
// nhóm tải ít xung đột và nhiều xung đột.
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

// BenchmarkSequentialVsParallelIndependentTransfers so sánh hai bộ máy trên
// cùng workload hoàn toàn không giao nhau. Seed được cố định để tái lập kết quả.
func BenchmarkSequentialVsParallelIndependentTransfers(b *testing.B) {
	for _, txCount := range []int{128, 512, 1024} {
		b.Run(fmt.Sprintf("tx-%d", txCount), func(b *testing.B) {
			workload := newIndependentTransferComparisonWorkload(b, txCount)
			b.Run("sequential", func(b *testing.B) {
				benchmarkExecutorBlockOnly(b, evm.NewSequentialExecutor(newChainConfig(), chainID), workload)
			})
			for _, workers := range []int{1, 2, 4, 8} {
				b.Run(fmt.Sprintf("parallel-workers-%d", workers), func(b *testing.B) {
					benchmarkExecutorBlockOnly(b, evm.NewParallelExecutor(newChainConfig(), chainID, workers), workload)
				})
			}
		})
	}
}

func newIndependentTransferComparisonWorkload(t testing.TB, txCount int) differentialWorkload {
	t.Helper()
	const seed = 0x4d345f
	pool := newAccountPool(t, seed, txCount*2)
	workload := differentialWorkload{
		name:            "fully-disjoint-transfers",
		transactions:    make([]*types.Transaction, txCount),
		initialBalances: make(map[common.Address]*big.Int, len(pool)),
		accounts:        make([]common.Address, len(pool)),
		env:             evm.BlockEnv{Height: 22, Time: 22, GasLimit: 30_000_000, BaseFee: new(big.Int)},
	}
	for i, account := range pool {
		workload.accounts[i] = account.address
		workload.initialBalances[account.address] = big.NewInt(1_000_000)
	}
	for i := 0; i < txCount; i++ {
		sender, recipient := pool[i*2], pool[i*2+1]
		workload.transactions[i] = signedTx(t, sender.keyHex, 0, 21_000, &recipient.address, big.NewInt(1), nil)
	}
	return workload
}

// benchmarkExecutorBlockOnly không tính thời gian tạo Pebble và nạp trạng thái
// ban đầu. Mỗi lượt vẫn có trạng thái mới giống nhau để nonce và số dư đã đổi
// ở lượt trước không ảnh hưởng lượt sau.
func benchmarkExecutorBlockOnly(b *testing.B, executor evm.Executor, workload differentialWorkload) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, err := store.NewPebbleDB("differential-benchmark", b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		seed := evm.NewPebbleStateDB(db)
		for addr, balance := range workloadBalances(workload) {
			seed.AddBalanceBig(addr, balance)
		}
		if err := seed.Commit(); err != nil {
			_ = db.Close()
			b.Fatal(err)
		}
		stateDB := evm.NewPebbleStateDB(db)

		b.StartTimer()
		_, executeErr := executor.ExecuteBlock(stateDB, workload.env, workload.transactions)
		b.StopTimer()
		closeErr := db.Close()
		if executeErr != nil {
			b.Fatal(executeErr)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}
