package harness_test

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestAccessListContractContentionSequentialParallelReport compares sequential
// and parallel execution when distinct wallets call Counter with the correct slot.
func TestAccessListContractContentionSequentialParallelReport(t *testing.T) {
	runAccessListContractContentionReport(t, true)
}

// TestIncorrectAccessListContractContentionSequentialParallelReport checks that
// transactions remain correct with an incorrect access-list slot and measures gas.
func TestIncorrectAccessListContractContentionSequentialParallelReport(t *testing.T) {
	runAccessListContractContentionReport(t, false)
}

func runAccessListContractContentionReport(t *testing.T, accurate bool) {
	t.Helper()
	modes := []comparisonMode{
		{name: "Sequential", sequential: true},
		{name: "Parallel auto", automatic: true},
		{name: "Parallel 1 worker", workers: 1},
		{name: "Parallel 2 workers", workers: 2},
		{name: "Parallel 4 workers", workers: 4},
		{name: "Parallel 8 workers", workers: 8},
	}

	accessListDescription := "Correct access list: declares Counter and storage slot 0."
	caseName := "CORRECT ACCESS LIST"
	if !accurate {
		accessListDescription = "Incorrect access list: declares slot 1, but increment() uses slot 0. Transactions remain valid."
		caseName = "INCORRECT ACCESS LIST"
	}
	fmt.Fprintf(os.Stdout, "\nCASE: MANY TRANSACTIONS CALL ONE CONTRACT WITH %s\n", caseName)
	fmt.Fprintf(os.Stdout, "Go %s | %s/%s | CPU %d | GOMAXPROCS %d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintf(os.Stdout, "Each wallet sends one AccessListTx calling increment() on the same Counter; %s\n", accessListDescription)
	fmt.Fprintf(os.Stdout, "Measurement: median of %d samples × %d blocks/sample; ExecuteBlock only, including Counter deployment and calls.\n", comparisonReportRepetitions, comparisonBlocksPerSample)
	fmt.Fprintln(os.Stdout, "Correctness: compare with geth and sequential execution; final Counter.count must equal the call count.")
	fmt.Fprintln(os.Stdout, "Adaptive scheduling uses a serial fallback for same-contract calls with missing or overlapping slot hints; fixed-worker modes remain the speculative-parallel baseline.")

	parallelFaster, uncertain := 0, 0
	for _, callCount := range []int{16, 32, 64} {
		callCount := callCount
		t.Run(fmt.Sprintf("calls-%d", callCount), func(t *testing.T) {
			workload := newAccessListCounterContentionWorkload(t, callCount, accurate)
			assertDistinctCounterCallers(t, workload)
			assertCounterAccessLists(t, workload, accurate)

			geth := runGethDifferentialWorkload(t, workload)
			if err := checkCounterResult(workload, geth, callCount); err != nil {
				t.Fatalf("geth Counter result did not match the expected value: %v", err)
			}
			fmt.Fprintf(os.Stdout, "Geth gas/block: %d\n", totalObservedGas(geth))
			if !accurate {
				correctWorkload := newAccessListCounterContentionWorkload(t, callCount, true)
				correctGeth := runGethDifferentialWorkload(t, correctWorkload)
				if err := checkCounterResult(correctWorkload, correctGeth, callCount); err != nil {
					t.Fatalf("geth with the correct access list returned an unexpected Counter value: %v", err)
				}
				wrongGas, correctGas := totalObservedGas(geth), totalObservedGas(correctGeth)
				if wrongGas <= correctGas {
					t.Fatalf("incorrect access list did not increase gas as expected: incorrect=%d correct=%d", wrongGas, correctGas)
				}
				fmt.Fprintf(os.Stdout, "Correct access list: %d gas/block; incorrect list adds %d gas/block.\n", correctGas, wrongGas-correctGas)
			}

			stats := make(map[string]comparisonSummary, len(modes))
			for _, mode := range modes {
				stats[mode.name] = comparisonSummary{samples: make([]comparisonSample, 0, comparisonReportRepetitions)}
				warmup := runComparisonSample(t, workload, mode)
				if err := checkCounterResult(workload, warmup.result, callCount); err != nil {
					t.Fatalf("%s warmup returned an unexpected Counter value: %v", mode.name, err)
				}
				assertAdaptiveFallbackUsed(t, mode, warmup.result)
				if mismatches := differentialMismatches(workload, mode.name, warmup.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("%s warmup differs from geth: %s", mode.name, mismatches[0])
				}
			}

			for repetition := 0; repetition < comparisonReportRepetitions; repetition++ {
				sequential := runComparisonSample(t, workload, modes[0])
				assertAccessListSample(t, workload, modes[0].name, sequential, "geth", geth, callCount)
				seqSummary := stats[modes[0].name]
				seqSummary.samples = append(seqSummary.samples, sequential)
				stats[modes[0].name] = seqSummary

				// Rotate parallel configurations to reduce order-related measurement bias.
				for offset := 0; offset < len(modes)-1; offset++ {
					mode := modes[1+(offset+repetition)%(len(modes)-1)]
					sample := runComparisonSample(t, workload, mode)
					assertAccessListSample(t, workload, mode.name, sample, modes[0].name, sequential.result, callCount)
					assertAdaptiveFallbackUsed(t, mode, sample.result)
					if mismatches := differentialMismatches(workload, mode.name, sample.result, "geth", geth); len(mismatches) > 0 {
						t.Fatalf("sample %d %s differs from geth: %s", repetition+1, mode.name, mismatches[0])
					}
					modeSummary := stats[mode.name]
					modeSummary.samples = append(modeSummary.samples, sample)
					stats[mode.name] = modeSummary
				}
			}

			for _, mode := range modes {
				stats[mode.name] = summarizeComparisonSamples(stats[mode.name].samples)
			}
			sequential := stats[modes[0].name]
			fastestName, fastestDuration := modes[0].name, sequential.medianDuration
			totalTransactions := len(workload.transactions)

			fmt.Fprintf(os.Stdout, "\n--- %d Counter calls (%d transactions/block, including deployment) ---\n", callCount, totalTransactions)
			fmt.Fprintln(os.Stdout, "PERFORMANCE (throughput is executor-only, not network TPS)")
			fmt.Fprintln(os.Stdout, "Mode                 ms/block  tx/s         vs sequential  MiB/block  allocs/block")
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
				fmt.Fprintf(os.Stdout, "%-20s %-9s %-12s %-14s %-10s %s\n",
					mode.name, formatDuration(current.medianDuration), formatThroughput(totalTransactions, current.medianDuration),
					comparison, memory, formatCount64(current.medianAllocs))
				if current.medianDuration < fastestDuration {
					fastestName, fastestDuration = mode.name, current.medianDuration
				}
			}

			fmt.Fprintln(os.Stdout, "\nPARALLEL DETAILS")
			fmt.Fprintln(os.Stdout, "Mode                 configured  active workers min..max/avg  aborts/block  retries/block  result")
			for _, mode := range modes[1:] {
				current := stats[mode.name]
				configured := fmt.Sprintf("%d", mode.workers)
				if mode.automatic {
					configured = "auto"
				}
				active := "not reported"
				if current.workerMin > 0 {
					active = fmt.Sprintf("%d..%d / %.1f", current.workerMin, current.workerMax, current.workerAvg)
				}
				fmt.Fprintf(os.Stdout, "%-20s %-11s %-31s %-13.1f %-14.1f PASS\n", mode.name, configured, active, current.medianAborts, current.medianRetries)
			}
			fmt.Fprintf(os.Stdout, "PASS = all %d increment calls succeeded; tx results, gas, logs, balances, nonces, storage, and Pluto state root matched.\n", callCount)

			fastest := stats[fastestName]
			if sequential.coefficientVar > 0.10 || fastest.coefficientVar > 0.10 {
				uncertain++
				fmt.Fprintf(os.Stdout, "Assessment: %s has the lowest median, but timing varies by more than 10%%; repeat before claiming a speedup.\n", fastestName)
			} else if fastestName == modes[0].name {
				fmt.Fprintln(os.Stdout, "Assessment: sequential is fastest at this size; parallel has no measured speed advantage.")
			} else {
				parallelFaster++
				fmt.Fprintf(os.Stdout, "Assessment: %s is fastest here (%.2fx sequential speed).\n", fastestName, float64(sequential.medianDuration)/float64(fastest.medianDuration))
			}
		})
	}

	if accurate {
		fmt.Fprintln(os.Stdout, "\nCONCLUSION: these results apply only to the Counter storage-contention workload with the correct AccessListTx slot.")
	} else {
		fmt.Fprintln(os.Stdout, "\nCONCLUSION: an incorrect access list does not make a transaction revert; execution uses the actual slot, incurs cold-access gas, and must match geth.")
	}
	fmt.Fprintf(os.Stdout, "Parallel was fastest with stable measurements at %d/3 sizes; %d/3 sizes need a repeat due to timing variation.\n", parallelFaster, uncertain)
}

func assertCounterAccessLists(t *testing.T, workload differentialWorkload, accurate bool) {
	t.Helper()
	expectedSlot := common.HexToHash("0x1")
	if accurate {
		expectedSlot = common.Hash{}
	}
	for i, tx := range workload.transactions[1:] {
		if tx.Type() != types.AccessListTxType {
			t.Fatalf("Counter call %d has transaction type %d; want AccessListTx", i+1, tx.Type())
		}
		if tx.To() == nil || *tx.To() != workload.contract {
			t.Fatalf("transaction %d does not call the expected Counter", i+1)
		}
		if !bytes.Equal(tx.Data(), common.FromHex("0xd09de08a")) {
			t.Fatalf("transaction %d does not call increment()", i+1)
		}
		accessList := tx.AccessList()
		if len(accessList) != 1 || accessList[0].Address != workload.contract || len(accessList[0].StorageKeys) != 1 || accessList[0].StorageKeys[0] != expectedSlot {
			t.Fatalf("transaction %d has unexpected access-list slot; want %s, got %#v", i+1, expectedSlot, accessList)
		}
	}
}

func totalObservedGas(observation differentialObservation) uint64 {
	var total uint64
	for _, result := range observation.results {
		total += result.GasUsed
	}
	return total
}

func assertAdaptiveFallbackUsed(t *testing.T, mode comparisonMode, observation differentialObservation) {
	t.Helper()
	if !mode.automatic {
		return
	}
	if observation.metrics["parallel_auto_serial_fallback"] != 1 {
		t.Fatalf("adaptive executor did not select the serial contract-conflict fallback")
	}
	if observation.metrics["abort_total"] != 0 || observation.metrics["retry_total"] != 0 {
		t.Fatalf("adaptive serial fallback should avoid speculative aborts and retries; got aborts=%v retries=%v", observation.metrics["abort_total"], observation.metrics["retry_total"])
	}
}

func assertAccessListSample(t *testing.T, workload differentialWorkload, mode string, sample comparisonSample, otherName string, other differentialObservation, expectedCalls int) {
	t.Helper()
	if err := checkCounterResult(workload, sample.result, expectedCalls); err != nil {
		t.Fatalf("%s returned an unexpected Counter value: %v", mode, err)
	}
	if mismatches := differentialMismatches(workload, mode, sample.result, otherName, other); len(mismatches) > 0 {
		t.Fatalf("%s differs from %s: %s", mode, otherName, mismatches[0])
	}
}

func formatComparisonEnglish(sequential, parallel time.Duration) string {
	if sequential <= 0 || parallel <= 0 {
		return "unknown"
	}
	ratio := float64(sequential) / float64(parallel)
	change := (float64(parallel)/float64(sequential) - 1) * 100
	if change < -0.05 {
		return fmt.Sprintf("%.2fx faster", ratio)
	}
	if change > 0.05 {
		return fmt.Sprintf("%.2fx slower", 1/ratio)
	}
	return "about equal"
}
