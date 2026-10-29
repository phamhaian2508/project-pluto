package harness_test

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const comparisonReportRepetitions = 5
const comparisonBlocksPerSample = 5

type comparisonMode struct {
	name       string
	workers    int
	sequential bool
	automatic  bool
}

type comparisonSample struct {
	duration  time.Duration
	bytes     uint64
	allocs    uint64
	workerMin int
	workerMax int
	workerAvg float64
	aborts    float64
	retries   float64
	result    differentialObservation
}

type comparisonSummary struct {
	medianDuration time.Duration
	medianBytes    uint64
	medianAllocs   uint64
	workerMin      int
	workerMax      int
	workerAvg      float64
	medianAborts   float64
	medianRetries  float64
	coefficientVar float64
	samples        []comparisonSample
}

// TestSequentialParallelPerformanceReport emits a compact, repeatable report.
// Each measured result is checked against both the sequential executor and geth.
func TestSequentialParallelPerformanceReport(t *testing.T) {
	const seed = 0x4d345f
	modes := []comparisonMode{
		{name: "Sequential", sequential: true},
		{name: "Parallel auto", automatic: true},
		{name: "Parallel 1w", workers: 1},
		{name: "Parallel 2w", workers: 2},
		{name: "Parallel 4w", workers: 4},
		{name: "Parallel 8w", workers: 8},
	}

	fmt.Fprintln(os.Stdout, "\nPLUTO EXECUTOR COMPARISON")
	fmt.Fprintf(os.Stdout, "Go %s | %s/%s | CPU %d | GOMAXPROCS %d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintf(os.Stdout, "Workload: independent native-coin transfers | seed 0x%x\n", seed)
	fmt.Fprintf(os.Stdout, "Measurement: median of %d samples; %d blocks per sample; ExecuteBlock only\n", comparisonReportRepetitions, comparisonBlocksPerSample)
	fmt.Fprintln(os.Stdout, "Correctness: each mode checked against Sequential; visible state and tx outputs also checked against geth")
	fmt.Fprintln(os.Stdout, "Memory: allocated bytes and allocation count per block; not peak process RAM")

	parallelFasterSizes := 0
	uncertainSizes := 0
	for _, txCount := range []int{128, 512, 1024} {
		t.Run(fmt.Sprintf("transactions-%d", txCount), func(t *testing.T) {
			workload := newIndependentTransferComparisonWorkload(t, txCount)
			geth := runGethDifferentialWorkload(t, workload)
			stats := make(map[string]comparisonSummary, len(modes))

			// Warm up each executor; correctness is checked, but timings are discarded.
			for _, mode := range modes {
				got := runComparisonSample(t, workload, mode).result
				if mismatches := differentialMismatches(workload, mode.name, got, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("warmup correctness mismatch: %s", mismatches[0])
				}
			}

			for _, mode := range modes {
				stats[mode.name] = comparisonSummary{samples: make([]comparisonSample, 0, comparisonReportRepetitions)}
			}
			for repetition := 0; repetition < comparisonReportRepetitions; repetition++ {
				sequential := runComparisonSample(t, workload, modes[0])
				if mismatches := differentialMismatches(workload, modes[0].name, sequential.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("sample %d sequential/geth mismatch: %s", repetition+1, mismatches[0])
				}
				seqSummary := stats[modes[0].name]
				seqSummary.samples = append(seqSummary.samples, sequential)
				stats[modes[0].name] = seqSummary

				// Rotate parallel modes so a fixed worker count is not always measured first.
				for offset := 0; offset < len(modes)-1; offset++ {
					mode := modes[1+(offset+repetition)%(len(modes)-1)]
					sample := runComparisonSample(t, workload, mode)
					if mismatches := differentialMismatches(workload, mode.name, sample.result, modes[0].name, sequential.result); len(mismatches) > 0 {
						t.Fatalf("sample %d %s differs from sequential: %s", repetition+1, mode.name, mismatches[0])
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
			seq := stats[modes[0].name]
			fastestName := modes[0].name
			fastestTime := seq.medianDuration

			fmt.Fprintf(os.Stdout, "\n--- %s transactions/block ---\n", formatCount(txCount))
			fmt.Fprintln(os.Stdout, "PERFORMANCE (throughput is executor-only, not network TPS)")
			fmt.Fprintln(os.Stdout, "Mode           ms/block  tx/s*      vs seq       MiB/block       allocs/block")
			for _, mode := range modes {
				current := stats[mode.name]
				speedup := "baseline"
				if !mode.sequential {
					speedup = formatTimeComparison(seq.medianDuration, current.medianDuration)
				}
				memory := formatMiB(current.medianBytes)
				if mode.sequential {
					memory += " (base)"
				} else if seq.medianBytes > 0 {
					memory += fmt.Sprintf(" (%+.0f%%)", (float64(current.medianBytes)/float64(seq.medianBytes)-1)*100)
				}
				fmt.Fprintf(os.Stdout, "%-14s %-9s %-10s %-12s %-15s %s\n",
					mode.name,
					formatDuration(current.medianDuration),
					formatThroughput(txCount, current.medianDuration),
					speedup,
					memory,
					formatCount64(current.medianAllocs),
				)
				if current.medianDuration < fastestTime {
					fastestName, fastestTime = mode.name, current.medianDuration
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
			fmt.Fprintln(os.Stdout, "PASS = tx status/gas/logs and visible state matched; Pluto state root matched Sequential.")
			fmt.Fprintln(os.Stdout, "Auto uses GOMAXPROCS as its worker limit and adjusts for conflicts; independent transfers may keep the full limit.")

			fastest := stats[fastestName]
			if seq.medianDuration <= 0 || fastest.medianDuration <= 0 {
				uncertainSizes++
				fmt.Fprintln(os.Stdout, "Assessment: measurement too short; increase samples before drawing a speed conclusion.")
			} else if seq.coefficientVar > 0.10 || fastest.coefficientVar > 0.10 {
				uncertainSizes++
				fmt.Fprintf(os.Stdout, "Assessment: %s has the lowest median, but timing varies by more than 10%%; repeat on an idle machine.\n", fastestName)
			} else if fastestName == modes[0].name {
				fmt.Fprintln(os.Stdout, "Assessment: sequential is fastest for this workload size; parallel has no measured speed benefit here.")
			} else {
				parallelFasterSizes++
				fmt.Fprintf(os.Stdout, "Assessment: %s is fastest here (%.2fx sequential speed).\n", fastestName, float64(seq.medianDuration)/float64(fastest.medianDuration))
			}
		})
	}

	fmt.Fprintln(os.Stdout, "\nFINAL: performance conclusion applies only to this transfer workload and recorded machine; correctness was checked for every row.")
	if uncertainSizes > 0 {
		fmt.Fprintf(os.Stdout, "Parallel was faster and stable at %d/3 sizes; %d/3 sizes need a more stable measurement.\n", parallelFasterSizes, uncertainSizes)
	} else {
		fmt.Fprintf(os.Stdout, "Parallel was fastest and stable at %d/3 sizes.\n", parallelFasterSizes)
	}
}

func runComparisonSample(t *testing.T, workload differentialWorkload, mode comparisonMode) comparisonSample {
	t.Helper()
	var executor evm.Executor
	switch {
	case mode.sequential:
		executor = evm.NewSequentialExecutor(newChainConfig(), chainID)
	case mode.automatic:
		executor = evm.NewAdaptiveParallelExecutor(newChainConfig(), chainID, 0)
	default:
		executor = evm.NewParallelExecutor(newChainConfig(), chainID, mode.workers)
	}

	balances := workloadBalances(workload)
	var totalDuration time.Duration
	var totalBytes, totalAllocs uint64
	var workerMin, workerMax int
	var workerAvg, aborts, retries float64
	var observation differentialObservation
	for i := 0; i < comparisonBlocksPerSample; i++ {
		db, err := store.NewPebbleDB("comparison-report", t.TempDir())
		if err != nil {
			t.Fatalf("create comparison state: %v", err)
		}
		seed := evm.NewPebbleStateDB(db)
		for _, address := range workload.accounts {
			if balance := balances[address]; balance != nil {
				seed.AddBalanceBig(address, balance)
			}
		}
		for address, code := range workload.initialCode {
			seed.SetCode(address, code, tracing.CodeChangeUnspecified)
		}
		if err := seed.Commit(); err != nil {
			_ = db.Close()
			t.Fatalf("seed comparison state: %v", err)
		}
		stateDB := evm.NewPebbleStateDB(db)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		result, executeErr := executor.ExecuteBlock(stateDB, workload.env, workload.transactions)
		duration := time.Since(started)
		runtime.ReadMemStats(&after)
		if executeErr != nil {
			_ = db.Close()
			t.Fatalf("execute %s: %v", mode.name, executeErr)
		}
		totalDuration += duration
		totalBytes += after.TotalAlloc - before.TotalAlloc
		totalAllocs += after.Mallocs - before.Mallocs
		if !mode.sequential {
			metrics := result.Metrics
			min := int(metrics["worker_active_min"])
			max := int(metrics["worker_active_max"])
			if min > 0 && (workerMin == 0 || min < workerMin) {
				workerMin = min
			}
			if max > workerMax {
				workerMax = max
			}
			workerAvg += metrics["worker_active_avg"]
			aborts += metrics["abort_total"]
			retryCount, hasRetryMetric := metrics["retry_total"]
			if !hasRetryMetric {
				// In the specialized transfer path each abort reruns that transaction once.
				retryCount = metrics["abort_total"]
			}
			retries += retryCount
		}
		current := capturePlutoObservation(stateDB, result, workload)
		current.stateRoot = common.BytesToHash(result.StateRoot[:])
		current.hasStateRoot = true
		current.metrics = result.Metrics
		if i == 0 {
			observation = current
		} else if mismatches := differentialMismatches(workload, mode.name, current, "same-mode sample", observation); len(mismatches) > 0 {
			_ = db.Close()
			t.Fatalf("repeated block %d is not deterministic: %s", i+1, mismatches[0])
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close comparison state: %v", err)
		}
	}
	return comparisonSample{
		duration:  totalDuration / comparisonBlocksPerSample,
		bytes:     totalBytes / comparisonBlocksPerSample,
		allocs:    totalAllocs / comparisonBlocksPerSample,
		workerMin: workerMin,
		workerMax: workerMax,
		workerAvg: workerAvg / comparisonBlocksPerSample,
		aborts:    aborts / comparisonBlocksPerSample,
		retries:   retries / comparisonBlocksPerSample,
		result:    observation,
	}
}

func summarizeComparisonSamples(samples []comparisonSample) comparisonSummary {
	durations := make([]int64, len(samples))
	bytes := make([]uint64, len(samples))
	allocs := make([]uint64, len(samples))
	workers := make([]float64, len(samples))
	aborts := make([]float64, len(samples))
	retries := make([]float64, len(samples))
	mean := 0.0
	workerMin, workerMax := 0, 0
	for i, sample := range samples {
		durations[i] = int64(sample.duration)
		bytes[i] = sample.bytes
		allocs[i] = sample.allocs
		workers[i] = sample.workerAvg
		aborts[i] = sample.aborts
		retries[i] = sample.retries
		mean += float64(sample.duration)
		if sample.workerMin > 0 && (workerMin == 0 || sample.workerMin < workerMin) {
			workerMin = sample.workerMin
		}
		if sample.workerMax > workerMax {
			workerMax = sample.workerMax
		}
	}
	mean /= float64(len(samples))
	variance := 0.0
	for _, sample := range samples {
		delta := float64(sample.duration) - mean
		variance += delta * delta
	}
	variance /= float64(len(samples))
	cv := 0.0
	if mean > 0 {
		cv = math.Sqrt(variance) / mean
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(bytes, func(i, j int) bool { return bytes[i] < bytes[j] })
	sort.Slice(allocs, func(i, j int) bool { return allocs[i] < allocs[j] })
	sort.Float64s(workers)
	sort.Float64s(aborts)
	sort.Float64s(retries)
	middle := len(samples) / 2
	return comparisonSummary{
		medianDuration: time.Duration(durations[middle]),
		medianBytes:    bytes[middle],
		medianAllocs:   allocs[middle],
		workerMin:      workerMin,
		workerMax:      workerMax,
		workerAvg:      workers[middle],
		medianAborts:   aborts[middle],
		medianRetries:  retries[middle],
		coefficientVar: cv,
		samples:        samples,
	}
}

func formatThroughput(transactions int, duration time.Duration) string {
	if duration <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", float64(transactions)/duration.Seconds())
}

func formatCount(count int) string {
	if count >= 1000 {
		return fmt.Sprintf("%s,%03d", formatCount(count/1000), count%1000)
	}
	return fmt.Sprintf("%d", count)
}

func formatCount64(count uint64) string {
	return formatCount(int(count))
}

func formatMiB(bytes uint64) string {
	return fmt.Sprintf("%.2f", float64(bytes)/(1024*1024))
}

func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "n/a"
	}
	if duration < time.Millisecond {
		return fmt.Sprintf("%.1f us", float64(duration)/float64(time.Microsecond))
	}
	return fmt.Sprintf("%.2f ms", float64(duration)/float64(time.Millisecond))
}

func formatTimeComparison(sequential, parallel time.Duration) string {
	if sequential <= 0 || parallel <= 0 {
		return "n/a"
	}
	speedup := float64(sequential) / float64(parallel)
	delta := (float64(parallel)/float64(sequential) - 1) * 100
	if delta < -0.05 {
		return fmt.Sprintf("%.2fx faster", speedup)
	}
	if delta > 0.05 {
		return fmt.Sprintf("%.2fx slower", 1/speedup)
	}
	return fmt.Sprintf("%.2fx equal", speedup)
}
