package harness_test

import (
	"fmt"
	"math/big"
	"os"
	"runtime"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// TestSameContractDisjointAccessListSequentialParallelReport is the positive
// control: calls target one contract but each transaction writes a distinct slot.
func TestSameContractDisjointAccessListSequentialParallelReport(t *testing.T) {
	modes := []comparisonMode{
		{name: "Sequential", sequential: true},
		{name: "Parallel auto", automatic: true},
		{name: "Parallel 1 worker", workers: 1},
		{name: "Parallel 2 workers", workers: 2},
		{name: "Parallel 4 workers", workers: 4},
		{name: "Parallel 8 workers", workers: 8},
	}
	fmt.Fprintln(os.Stdout, "\nCASE: MANY TRANSACTIONS CALL ONE CONTRACT, EACH WITH A DISTINCT CORRECT ACCESS-LIST SLOT")
	fmt.Fprintf(os.Stdout, "Go %s | %s/%s | CPU %d | GOMAXPROCS %d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintf(os.Stdout, "Each call increments a different storage slot and performs %d hash operations; median of %d samples × %d blocks/sample.\n", shardedStorageHashIterations, comparisonReportRepetitions, comparisonBlocksPerSample)
	fmt.Fprintln(os.Stdout, "Correctness: each run must match geth and sequential execution; every requested slot must end at 1.")
	fmt.Fprintln(os.Stdout, "Unlike a shared Counter slot, these calls have independent writes and are eligible for parallel execution.")

	for _, callCount := range []int{32, 64, 128} {
		callCount := callCount
		t.Run(fmt.Sprintf("calls-%d", callCount), func(t *testing.T) {
			workload := newShardedStorageWorkload(t, callCount)
			geth := runGethDifferentialWorkload(t, workload)
			checkShardedStorageResult(t, workload, "geth", geth, callCount)

			stats := make(map[string]comparisonSummary, len(modes))
			for _, mode := range modes {
				stats[mode.name] = comparisonSummary{samples: make([]comparisonSample, 0, comparisonReportRepetitions)}
				warmup := runComparisonSample(t, workload, mode)
				checkShardedStorageResult(t, workload, mode.name+" warmup", warmup.result, callCount)
				if mismatches := differentialMismatches(workload, mode.name, warmup.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("%s warmup differs from geth: %s", mode.name, mismatches[0])
				}
				assertParallelShardedPath(t, mode, warmup.result)
			}

			for repetition := 0; repetition < comparisonReportRepetitions; repetition++ {
				sequential := runComparisonSample(t, workload, modes[0])
				checkShardedStorageResult(t, workload, "Sequential", sequential.result, callCount)
				if mismatches := differentialMismatches(workload, "Sequential", sequential.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("sample %d sequential differs from geth: %s", repetition+1, mismatches[0])
				}
				seqSummary := stats[modes[0].name]
				seqSummary.samples = append(seqSummary.samples, sequential)
				stats[modes[0].name] = seqSummary

				for offset := 0; offset < len(modes)-1; offset++ {
					mode := modes[1+(offset+repetition)%(len(modes)-1)]
					sample := runComparisonSample(t, workload, mode)
					checkShardedStorageResult(t, workload, mode.name, sample.result, callCount)
					if mismatches := differentialMismatches(workload, mode.name, sample.result, "Sequential", sequential.result); len(mismatches) > 0 {
						t.Fatalf("sample %d %s differs from sequential: %s", repetition+1, mode.name, mismatches[0])
					}
					if mismatches := differentialMismatches(workload, mode.name, sample.result, "geth", geth); len(mismatches) > 0 {
						t.Fatalf("sample %d %s differs from geth: %s", repetition+1, mode.name, mismatches[0])
					}
					assertParallelShardedPath(t, mode, sample.result)
					modeSummary := stats[mode.name]
					modeSummary.samples = append(modeSummary.samples, sample)
					stats[mode.name] = modeSummary
				}
			}

			for _, mode := range modes {
				stats[mode.name] = summarizeComparisonSamples(stats[mode.name].samples)
			}
			sequential := stats[modes[0].name]
			fmt.Fprintf(os.Stdout, "\n--- %d calls (%d transactions/block) ---\n", callCount, len(workload.transactions))
			fmt.Fprintln(os.Stdout, "Mode                 ms/block  tx/s         vs sequential  MiB/block  allocs/block")
			fastestName, fastestDuration := modes[0].name, sequential.medianDuration
			for _, mode := range modes {
				current := stats[mode.name]
				comparison := "baseline"
				if !mode.sequential {
					comparison = formatComparisonEnglish(sequential.medianDuration, current.medianDuration)
				}
				memory := formatMiB(current.medianBytes)
				if !mode.sequential && sequential.medianBytes > 0 {
					memory += fmt.Sprintf(" (%+.0f%%)", (float64(current.medianBytes)/float64(sequential.medianBytes)-1)*100)
				}
				fmt.Fprintf(os.Stdout, "%-20s %-9s %-12s %-14s %-10s %s\n", mode.name,
					formatDuration(current.medianDuration), formatThroughput(len(workload.transactions), current.medianDuration),
					comparison, memory, formatCount64(current.medianAllocs))
				if current.medianDuration < fastestDuration {
					fastestName, fastestDuration = mode.name, current.medianDuration
				}
			}
			fmt.Fprintln(os.Stdout, "\nPARALLEL VALIDATION")
			for _, mode := range modes[1:] {
				current := stats[mode.name]
				fmt.Fprintf(os.Stdout, "%-20s workers=%d..%d avg=%.1f aborts/block=%.1f retries/block=%.1f PASS\n",
					mode.name, current.workerMin, current.workerMax, current.workerAvg, current.medianAborts, current.medianRetries)
			}
			fmt.Fprintln(os.Stdout, "PASS = transaction results, gas, logs, balances, nonces, code, slots, and Pluto state root matched.")
			if sequential.coefficientVar > 0.10 || stats[fastestName].coefficientVar > 0.10 {
				fmt.Fprintf(os.Stdout, "Assessment: %s has the lowest median, but timing varies by more than 10%%; repeat before claiming a speedup.\n", fastestName)
			} else if fastestName == modes[0].name {
				fmt.Fprintln(os.Stdout, "Assessment: sequential is fastest at this size.")
			} else {
				fmt.Fprintf(os.Stdout, "Assessment: %s is fastest here (%.2fx sequential speed).\n", fastestName, float64(sequential.medianDuration)/float64(stats[fastestName].medianDuration))
			}
		})
	}
}

const shardedStorageHashIterations = 256

func newShardedStorageWorkload(t testing.TB, calls int) differentialWorkload {
	t.Helper()
	if calls < 2 {
		t.Fatal("sharded storage workload requires at least two calls")
	}
	pool := newAccountPool(t, 0x5A4D, calls)
	contract := common.HexToAddress("0x0000000000000000000000000000000000005A4D")
	runtimeCode := shardedStorageRuntime(shardedStorageHashIterations)
	workload := differentialWorkload{
		name:            "same-contract-disjoint-storage-slots",
		transactions:    make([]*types.Transaction, 0, calls),
		contract:        contract,
		accounts:        make([]common.Address, 0, calls+1),
		initialBalances: make(map[common.Address]*big.Int, calls),
		initialCode:     map[common.Address][]byte{contract: runtimeCode},
		storageSlots:    map[common.Address][]common.Hash{contract: make([]common.Hash, 0, calls)},
		env:             evm.BlockEnv{Height: 31, Time: 31, GasLimit: blockGasLimit, Random: common.HexToHash("0x5A4D"), BaseFee: new(big.Int)},
	}
	for i, account := range pool {
		slot := common.BigToHash(big.NewInt(int64(i)))
		data := append([]byte{0x12, 0x34, 0x56, 0x78}, slot[:]...)
		accessList := types.AccessList{{Address: contract, StorageKeys: []common.Hash{slot}}}
		workload.transactions = append(workload.transactions, signedAccessListTx(t, account.keyHex, 0, 200_000, &contract, new(big.Int), data, accessList))
		workload.accounts = append(workload.accounts, account.address)
		workload.initialBalances[account.address] = new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
		workload.storageSlots[contract] = append(workload.storageSlots[contract], slot)
	}
	workload.accounts = append(workload.accounts, contract)
	return workload
}

func shardedStorageRuntime(iterations uint16) []byte {
	return []byte{
		0x60, 0x04, 0x35, 0x5f, 0x52, // Copy the requested storage slot to memory[0].
		0x61, byte(iterations >> 8), byte(iterations),
		0x5b,                         // Loop entry at byte offset 8.
		0x60, 0x20, 0x5f, 0x20, 0x50, // Hash memory[0:32] to add independent EVM work.
		0x60, 0x01, 0x90, 0x03, // Decrement the loop counter.
		0x80, 0x60, 0x08, 0x57, // Repeat while the counter is nonzero.
		0x5f, 0x51, 0x80, 0x54, // Load this transaction's storage slot and current value.
		0x60, 0x01, 0x01, 0x90, 0x55, 0x00, // Increment and store the value.
	}
}

func checkShardedStorageResult(t *testing.T, workload differentialWorkload, executor string, got differentialObservation, calls int) {
	t.Helper()
	if len(got.results) != calls {
		t.Fatalf("%s returned %d transaction results; want %d", executor, len(got.results), calls)
	}
	for i, result := range got.results {
		if result.Invalid || result.Failed {
			t.Fatalf("%s transaction %d failed: invalid=%t failed=%t err=%s", executor, i, result.Invalid, result.Failed, result.ErrMsg)
		}
	}
	account, ok := got.accounts[workload.contract]
	if !ok {
		t.Fatalf("%s did not return the contract account", executor)
	}
	for i, slot := range workload.storageSlots[workload.contract] {
		if account.storage[slot] != common.BigToHash(big.NewInt(1)) {
			t.Fatalf("%s storage slot %d = %s; want 1", executor, i, account.storage[slot])
		}
	}
}

func assertParallelShardedPath(t *testing.T, mode comparisonMode, got differentialObservation) {
	t.Helper()
	if mode.sequential {
		return
	}
	if got.metrics["parallel_auto_serial_fallback"] != 0 {
		t.Fatalf("%s unexpectedly used the serial contract fallback", mode.name)
	}
	if got.metrics["abort_total"] != 0 || got.metrics["retry_total"] != 0 {
		t.Fatalf("%s had unexpected conflicts for disjoint slots: aborts=%v retries=%v", mode.name, got.metrics["abort_total"], got.metrics["retry_total"])
	}
	if !mode.automatic && mode.workers == 1 {
		return
	}
	if got.metrics["tx_parallel"] == 0 {
		t.Fatalf("%s did not execute transactions concurrently", mode.name)
	}
}
