package harness_test

import (
	"fmt"
	"math/big"
	"os"
	"runtime"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestContractContentionSequentialParallelReport compares executors on a block
// where distinct wallets call the same Counter and contend on one storage slot.
func TestContractContentionSequentialParallelReport(t *testing.T) {
	modes := []comparisonMode{
		{name: "Sequential", sequential: true},
		{name: "Parallel auto", automatic: true},
		{name: "Parallel 1w", workers: 1},
		{name: "Parallel 2w", workers: 2},
		{name: "Parallel 4w", workers: 4},
		{name: "Parallel 8w", workers: 8},
	}

	fmt.Fprintln(os.Stdout, "\nCASE: MANY TRANSACTIONS CALL ONE COUNTER CONTRACT")
	fmt.Fprintf(os.Stdout, "Go %s | %s/%s | CPU %d | GOMAXPROCS %d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintln(os.Stdout, "Workload: one deployer plus distinct caller wallets; all callers invoke increment() on the same Counter.count slot 0.")
	fmt.Fprintf(os.Stdout, "Measurement: median of %d samples; %d blocks/sample; includes deployment and calls; ExecuteBlock only.\n", comparisonReportRepetitions, comparisonBlocksPerSample)
	fmt.Fprintln(os.Stdout, "Correctness gate: compare against geth and Sequential, then require Counter.count == increment call count.")
	fmt.Fprintln(os.Stdout, "Memory values are allocated bytes per block, not peak process RAM.")

	parallelFaster := 0
	unstable := 0
	for _, incrementCount := range []int{16, 32, 64} {
		incrementCount := incrementCount
		t.Run(fmt.Sprintf("increments-%d", incrementCount), func(t *testing.T) {
			workload := newMultiSenderCounterContentionWorkload(t, incrementCount)
			assertDistinctCounterCallers(t, workload)
			geth := runGethDifferentialWorkload(t, workload)
			if err := checkCounterResult(workload, geth, incrementCount); err != nil {
				t.Fatalf("geth reference failed expected Counter result: %v", err)
			}

			stats := make(map[string]comparisonSummary, len(modes))
			for _, mode := range modes {
				stats[mode.name] = comparisonSummary{samples: make([]comparisonSample, 0, comparisonReportRepetitions)}
				warmup := runComparisonSample(t, workload, mode)
				assertAdaptiveFallbackUsed(t, mode, warmup.result)
				if err := checkCounterResult(workload, warmup.result, incrementCount); err != nil {
					t.Fatalf("%s warmup failed expected Counter result: %v", mode.name, err)
				}
				if mismatches := differentialMismatches(workload, mode.name, warmup.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("%s warmup differs from geth: %s", mode.name, mismatches[0])
				}
			}

			for repetition := 0; repetition < comparisonReportRepetitions; repetition++ {
				sequential := runComparisonSample(t, workload, modes[0])
				if err := checkCounterResult(workload, sequential.result, incrementCount); err != nil {
					t.Fatalf("sample %d sequential Counter result: %v", repetition+1, err)
				}
				if mismatches := differentialMismatches(workload, modes[0].name, sequential.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("sample %d sequential differs from geth: %s", repetition+1, mismatches[0])
				}
				seqSummary := stats[modes[0].name]
				seqSummary.samples = append(seqSummary.samples, sequential)
				stats[modes[0].name] = seqSummary

				// Rotate the parallel modes to avoid always measuring one worker count first.
				for offset := 0; offset < len(modes)-1; offset++ {
					mode := modes[1+(offset+repetition)%(len(modes)-1)]
					sample := runComparisonSample(t, workload, mode)
					assertAdaptiveFallbackUsed(t, mode, sample.result)
					if err := checkCounterResult(workload, sample.result, incrementCount); err != nil {
						t.Fatalf("sample %d %s Counter result: %v", repetition+1, mode.name, err)
					}
					if mismatches := differentialMismatches(workload, mode.name, sample.result, modes[0].name, sequential.result); len(mismatches) > 0 {
						t.Fatalf("sample %d %s differs from Sequential: %s", repetition+1, mode.name, mismatches[0])
					}
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
			fastestName := modes[0].name
			fastestDuration := sequential.medianDuration
			totalTx := len(workload.transactions)

			fmt.Fprintf(os.Stdout, "\n--- %d increment calls (%d total transactions/block, including deploy) ---\n", incrementCount, totalTx)
			fmt.Fprintln(os.Stdout, "PERFORMANCE")
			fmt.Fprintln(os.Stdout, "Mode           ms/block  tx/s*      vs seq        MiB/block       allocs/block")
			for _, mode := range modes {
				current := stats[mode.name]
				comparison := "baseline"
				if !mode.sequential {
					comparison = formatTimeComparison(sequential.medianDuration, current.medianDuration)
				}
				memory := formatMiB(current.medianBytes)
				if mode.sequential {
					memory += " (base)"
				} else if sequential.medianBytes > 0 {
					memory += fmt.Sprintf(" (%+.0f%%)", (float64(current.medianBytes)/float64(sequential.medianBytes)-1)*100)
				}
				fmt.Fprintf(os.Stdout, "%-14s %-9s %-10s %-13s %-15s %s\n",
					mode.name,
					formatDuration(current.medianDuration),
					formatThroughput(totalTx, current.medianDuration),
					comparison,
					memory,
					formatCount64(current.medianAllocs),
				)
				if current.medianDuration < fastestDuration {
					fastestName, fastestDuration = mode.name, current.medianDuration
				}
			}

			fmt.Fprintln(os.Stdout, "\nPARALLEL DETAILS")
			fmt.Fprintln(os.Stdout, "Mode           configured  active workers (min..max / avg)  abort/block  retry/block  result")
			for _, mode := range modes[1:] {
				current := stats[mode.name]
				configured := "auto"
				if !mode.automatic {
					configured = fmt.Sprintf("%d", mode.workers)
				}
				active := "not reported"
				if current.workerMin > 0 {
					active = fmt.Sprintf("%d..%d / %.1f", current.workerMin, current.workerMax, current.workerAvg)
				}
				fmt.Fprintf(os.Stdout, "%-14s %-11s %-33s %-12.1f %-12.1f PASS\n", mode.name, configured, active, current.medianAborts, current.medianRetries)
			}
			fmt.Fprintf(os.Stdout, "PASS = Counter.count ended at %d; tx results, gas, logs, account state and Pluto state root matched.\n", incrementCount)
			fmt.Fprintln(os.Stdout, "Auto uses GOMAXPROCS as its worker limit and adjusts concurrency based on observed conflicts.")

			fastest := stats[fastestName]
			if sequential.medianDuration <= 0 || fastest.medianDuration <= 0 || sequential.coefficientVar > 0.10 || fastest.coefficientVar > 0.10 {
				unstable++
				fmt.Fprintf(os.Stdout, "Assessment: %s has the lowest median, but timing is too unstable (>10%% variation) for a firm speed claim.\n", fastestName)
			} else if fastestName == modes[0].name {
				fmt.Fprintln(os.Stdout, "Assessment: Sequential is fastest for this contention level.")
			} else {
				parallelFaster++
				fmt.Fprintf(os.Stdout, "Assessment: %s is fastest here (%.2fx sequential speed).\n", fastestName, float64(sequential.medianDuration)/float64(fastest.medianDuration))
			}
		})
	}

	fmt.Fprintln(os.Stdout, "\nFINAL: each table is only for the Counter hot-storage workload; performance is not generalized to other contracts.")
	if unstable > 0 {
		fmt.Fprintf(os.Stdout, "Parallel was faster and stable in %d/3 cases; %d/3 cases require a repeat measurement.\n", parallelFaster, unstable)
	} else {
		fmt.Fprintf(os.Stdout, "Parallel was fastest and stable in %d/3 cases.\n", parallelFaster)
	}
}

func checkCounterResult(workload differentialWorkload, observation differentialObservation, expected int) error {
	account, ok := observation.accounts[workload.contract]
	if !ok {
		return fmt.Errorf("contract %s is missing from observed state", workload.contract)
	}
	got := account.storage[common.Hash{}]
	want := common.BigToHash(big.NewInt(int64(expected)))
	if got != want {
		return fmt.Errorf("Counter.count = %s, want %d", got, expected)
	}
	if len(observation.results) != len(workload.transactions) {
		return fmt.Errorf("got %d transaction results, want %d", len(observation.results), len(workload.transactions))
	}
	for i, result := range observation.results {
		if result.Invalid || result.Failed {
			return fmt.Errorf("transaction %d invalid=%t failed=%t: %s", i, result.Invalid, result.Failed, result.ErrMsg)
		}
	}
	return nil
}

func assertDistinctCounterCallers(t *testing.T, workload differentialWorkload) {
	t.Helper()
	if len(workload.transactions) < 2 || workload.transactions[0].To() != nil {
		t.Fatal("workload must deploy Counter before the contract calls")
	}
	seen := make(map[common.Address]struct{}, len(workload.transactions)-1)
	signer := types.LatestSignerForChainID(chainID)
	for i, tx := range workload.transactions[1:] {
		if tx.To() == nil || *tx.To() != workload.contract {
			t.Fatalf("call transaction %d does not target Counter %s", i+1, workload.contract)
		}
		sender, err := types.Sender(signer, tx)
		if err != nil {
			t.Fatalf("recover caller for transaction %d: %v", i+1, err)
		}
		if _, duplicate := seen[sender]; duplicate {
			t.Fatalf("caller wallet %s is reused; expected distinct senders", sender)
		}
		seen[sender] = struct{}{}
	}
}
