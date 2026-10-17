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

// BenchmarkSequentialVsParallelIndependentTransfers compares the two engines
// on the same fully disjoint workload. The seed is fixed for reproducibility.
func BenchmarkSequentialVsParallelIndependentTransfers(b *testing.B) {
	const seed = 0x4d345f
	for _, txCount := range []int{128, 512, 1024} {
		b.Run(fmt.Sprintf("tx-%d", txCount), func(b *testing.B) {
			pool := newAccountPool(b, seed, txCount*2)
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
				workload.transactions[i] = signedTx(b, sender.keyHex, 0, 21_000, &recipient.address, big.NewInt(1), nil)
			}
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

// benchmarkExecutorBlockOnly excludes Pebble creation and initial-state setup
// from the timer. Each iteration still receives a fresh identical state so
// nonce and balance changes from one iteration cannot affect the next.
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
