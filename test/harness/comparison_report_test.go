package harness_test

import (
	"fmt"
	"math"
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
}

type comparisonSample struct {
	duration time.Duration
	bytes    uint64
	allocs   uint64
	result   differentialObservation
}

type comparisonSummary struct {
	medianDuration time.Duration
	medianBytes    uint64
	medianAllocs   uint64
	coefficientVar float64
	samples        []comparisonSample
}

// TestSequentialParallelPerformanceReport in báo cáo dễ đọc, có thể tái lập để
// so sánh thực thi khối. Trước khi xác nhận kết quả đúng, hàm đối chiếu từng
// lượt đo với cả bộ thực thi tuần tự và geth.
func TestSequentialParallelPerformanceReport(t *testing.T) {
	const seed = 0x4d345f
	modes := []comparisonMode{
		{name: "Tuần tự", sequential: true},
		{name: "Song song, 1 worker", workers: 1},
		{name: "Song song, 2 workers", workers: 2},
		{name: "Song song, 4 workers", workers: 4},
		{name: "Song song, 8 workers", workers: 8},
	}

	t.Log("============================================================")
	t.Log("BÁO CÁO SO SÁNH EXECUTOR TUẦN TỰ VÀ SONG SONG")
	t.Logf("Môi trường: Go %s | %s/%s | CPU logic: %d | GOMAXPROCS: %d", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	t.Logf("Số lần đo: %d mẫu × %d block/mẫu | Seed workload: 0x%x | Loại tải: chuyển khoản độc lập, mỗi ví gửi một giao dịch", comparisonReportRepetitions, comparisonBlocksPerSample, seed)
	t.Log("Phạm vi: chỉ đo ExecuteBlock; không tính khởi động node, RPC, tạo Pebble DB hoặc nạp state ban đầu.")
	t.Log("Bộ nhớ là tổng byte/số lần cấp phát trong ExecuteBlock, không phải RAM đỉnh của tiến trình.")
	t.Log("============================================================")

	parallelFasterSizes := 0
	uncertainSizes := 0
	for _, txCount := range []int{128, 512, 1024} {
		t.Run(fmt.Sprintf("transactions-%d", txCount), func(t *testing.T) {
			workload := newIndependentTransferComparisonWorkload(t, txCount)
			geth := runGethDifferentialWorkload(t, workload)
			stats := make(map[string]comparisonSummary, len(modes))

			// Chạy làm nóng mỗi bộ thực thi một lần. Lượt này được kiểm tra tính đúng
			// nhưng không tính vào các mẫu báo cáo.
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
				if mismatches := differentialMismatches(workload, "Tuần tự", sequential.result, "geth", geth); len(mismatches) > 0 {
					t.Fatalf("repetition %d sequential/geth mismatch: %s", repetition+1, mismatches[0])
				}
				seqSummary := stats[modes[0].name]
				seqSummary.samples = append(seqSummary.samples, sequential)
				stats[modes[0].name] = seqSummary

				// Đổi thứ tự đo các cấu hình song song để giảm thiên lệch do luôn đo
				// cùng số worker trước tiên hoặc cuối cùng.
				for offset := 0; offset < len(modes)-1; offset++ {
					mode := modes[1+(offset+repetition)%(len(modes)-1)]
					sample := runComparisonSample(t, workload, mode)
					if mismatches := differentialMismatches(workload, mode.name, sample.result, "Tuần tự", sequential.result); len(mismatches) > 0 {
						t.Fatalf("repetition %d %s differs from sequential: %s", repetition+1, mode.name, mismatches[0])
					}
					if mismatches := differentialMismatches(workload, mode.name, sample.result, "geth", geth); len(mismatches) > 0 {
						t.Fatalf("repetition %d %s differs from geth: %s", repetition+1, mode.name, mismatches[0])
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

			t.Log("")
			t.Logf("Giao dịch: %s | workers song song: 1, 2, 4, 8 | đo: %d mẫu × %d block", formatCount(txCount), comparisonReportRepetitions, comparisonBlocksPerSample)
			t.Log("Chế độ                 Trung vị/block   So với tuần tự       Cấp phát vs tuần tự       Số lần cấp phát   Đúng đắn   Độ ổn định")
			for _, mode := range modes {
				current := stats[mode.name]
				speedup := float64(seq.medianDuration) / float64(current.medianDuration)
				delta := (float64(current.medianDuration)/float64(seq.medianDuration) - 1) * 100
				comparison := "mốc chuẩn"
				memoryComparison := "mốc chuẩn"
				if !mode.sequential {
					if delta < 0 {
						comparison = fmt.Sprintf("nhanh hơn %.1f%% (%.2fx)", -delta, speedup)
					} else if delta > 0 {
						comparison = fmt.Sprintf("chậm hơn %.1f%% (%.2fx)", delta, speedup)
					} else {
						comparison = "xấp xỉ ngang (1.00x)"
					}
					memoryDelta := (float64(current.medianBytes)/float64(seq.medianBytes) - 1) * 100
					memoryComparison = fmt.Sprintf("%s MiB (%+.1f%%)", formatMiB(current.medianBytes), memoryDelta)
				}
				stability := "ổn định"
				if current.coefficientVar > 0.10 {
					stability = fmt.Sprintf("dao động %.1f%% (cẩn trọng)", current.coefficientVar*100)
				}
				if mode.sequential {
					memoryComparison = fmt.Sprintf("%s MiB (mốc)", formatMiB(current.medianBytes))
				}
				t.Logf("%-22s %9.3f ms   %-20s %-24s %14s   Đạt       %s", mode.name, float64(current.medianDuration)/float64(time.Millisecond), comparison, memoryComparison, formatCount64(current.medianAllocs), stability)
				if current.medianDuration < fastestTime {
					fastestName, fastestTime = mode.name, current.medianDuration
				}
			}

			fastest := stats[fastestName]
			if seq.coefficientVar > 0.10 || fastest.coefficientVar > 0.10 {
				uncertainSizes++
				t.Logf("Nhận xét: trung vị thấp nhất là %s, nhưng số đo dao động lớn; chạy lại trên máy ít tải hoặc tăng số mẫu trước khi kết luận.", fastestName)
			} else if fastestName == modes[0].name {
				t.Log("Nhận xét: tuần tự nhanh nhất ở quy mô này; các cấu hình song song chưa đem lại lợi ích đo được.")
			} else {
				parallelFasterSizes++
				t.Logf("Nhận xét: %s nhanh nhất, nhanh hơn tuần tự %.2fx ở workload này.", fastestName, float64(seq.medianDuration)/float64(fastest.medianDuration))
			}
		})
	}

	t.Log("")
	if uncertainSizes == 0 {
		t.Logf("KẾT LUẬN: song song nhanh hơn tuần tự một cách ổn định ở %d/3 quy mô được đo. Kết luận chỉ áp dụng cho workload chuyển khoản độc lập và cấu hình/máy ghi ở trên.", parallelFasterSizes)
	} else {
		t.Logf("KẾT LUẬN: %d/3 quy mô có trung vị song song tốt hơn và ổn định; %d/3 quy mô còn dao động lớn nên chưa đủ cơ sở kết luận. Kết quả chỉ áp dụng cho workload chuyển khoản độc lập và cấu hình/máy ghi ở trên.", parallelFasterSizes, uncertainSizes)
	}
}

func runComparisonSample(t *testing.T, workload differentialWorkload, mode comparisonMode) comparisonSample {
	t.Helper()
	var executor evm.Executor
	if mode.sequential {
		executor = evm.NewSequentialExecutor(newChainConfig(), chainID)
	} else {
		executor = evm.NewParallelExecutor(newChainConfig(), chainID, mode.workers)
	}

	balances := workloadBalances(workload)
	var totalDuration time.Duration
	var totalBytes, totalAllocs uint64
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
		current := capturePlutoObservation(stateDB, result, workload)
		current.stateRoot = common.BytesToHash(result.StateRoot[:])
		current.hasStateRoot = true
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
		duration: totalDuration / comparisonBlocksPerSample,
		bytes:    totalBytes / comparisonBlocksPerSample,
		allocs:   totalAllocs / comparisonBlocksPerSample,
		result:   observation,
	}
}

func summarizeComparisonSamples(samples []comparisonSample) comparisonSummary {
	durations := make([]int64, len(samples))
	bytes := make([]uint64, len(samples))
	allocs := make([]uint64, len(samples))
	mean := 0.0
	for i, sample := range samples {
		durations[i] = int64(sample.duration)
		bytes[i] = sample.bytes
		allocs[i] = sample.allocs
		mean += float64(sample.duration)
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
	middle := len(samples) / 2
	return comparisonSummary{
		medianDuration: time.Duration(durations[middle]),
		medianBytes:    bytes[middle],
		medianAllocs:   allocs[middle],
		coefficientVar: cv,
		samples:        samples,
	}
}

func formatCount(count int) string {
	if count >= 1000 {
		return fmt.Sprintf("%s.%03d", formatCount(count/1000), count%1000)
	}
	return fmt.Sprintf("%d", count)
}

func formatCount64(count uint64) string {
	return formatCount(int(count))
}

func formatMiB(bytes uint64) string {
	return fmt.Sprintf("%.2f", float64(bytes)/(1024*1024))
}
