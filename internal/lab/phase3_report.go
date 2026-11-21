package lab

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func writePhase3SummaryMD(path string, rep *BenchReport) error {
	var md strings.Builder
	m := rep.Metadata

	md.WriteString("# Pluto Phase 3 Benchmark Summary (`adaptive` vs `off` / `fixed` / `history`)\n\n")
	fmt.Fprintf(&md, "- OS / Arch: `%s/%s`, CPU: `%s` (`numCPU=%d`), locked `GOMAXPROCS=%d`\n",
		m.GOOS, m.GOARCH, m.CPUModel, m.NumCPU, m.GOMAXPROCS)
	fmt.Fprintf(&md, "- Go: `%s`, Geth reference: `%s`, `evmBin=%s`\n",
		m.GoVersion, m.EVMBinVersion, m.EVMBin)
	fmt.Fprintf(&md, "- Eval seed: `%d`, Fixture manifest SHA-256: `%s`\n", m.Seed, m.FixtureManifestSHA256)
	fmt.Fprintf(&md, "- Model: `%s` (`sha256=%s`), `N=%d`, tight RAM=`%d bytes`, wide RAM=`%d bytes`, score threshold=`%.4f ns/byte`\n",
		m.ModelPath, m.ModelSHA256, m.CheckpointEvery, m.TightBudgetBytes, m.WideBudgetBytes, m.ScoreThreshold)
	if rep.Model != nil {
		fmt.Fprintf(&md, "- Calibration seed: `%d`, one-time calibration duration: `%.2f s`, paired entries: `%d`\n",
			rep.Model.Seed, float64(rep.Model.CalibrationDurationNs)/1e9, len(rep.Model.Entries))
	}
	fmt.Fprintf(&md, "- Runs per config: `%d` (rotated policy order per run; `off` reused across `tight`/`wide` within each run)\n\n", m.Runs)

	// 1. Bảng tải mục tiêu merkle-mixed (4 worker, RAM chặt và RAM rộng)
	md.WriteString("## 1. Tải mục tiêu: `merkle-mixed` (`4` worker, RAM chặt & rộng)\n\n")
	md.WriteString("| RAM Tier | Policy | Median (ms) | IQR (ms) | Delta vs off (ms) | IQR delta (ms) | Unstable (0 in IQR) | Retried | Created | SkipBudget | SkipLowScore | Resumed | Steps Saved | Peak Bytes | Score+Hist (ms) |\n")
	md.WriteString("| --- | --- | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")

	var targetTightSummaries []BenchConfigSummary
	for _, s := range rep.Summaries {
		if s.Case != DefaultPhase3TargetCase || s.Workers != 4 {
			continue
		}
		if s.BudgetTier == "tight" {
			targetTightSummaries = append(targetTightSummaries, s)
		}
		dOff, iqrOff := "0.000", "0.000"
		if s.MedianDeltaVsOffNs != nil {
			dOff = fmt.Sprintf("%+.3f", *s.MedianDeltaVsOffNs/1e6)
		}
		if s.IQRDeltaVsOffNs != nil {
			iqrOff = fmt.Sprintf("%.3f", *s.IQRDeltaVsOffNs/1e6)
		}
		unstableStr := "no"
		if s.UnstableVsOff {
			unstableStr = "yes"
		}
		fmt.Fprintf(&md, "| %s (%d B) | `%s` | %.3f | %.3f | %s | %s | %s | %.1f | %.1f | %.1f | %.1f | %.1f | %.0f | %.0f | %.3f |\n",
			s.BudgetTier, s.BudgetBytes, s.CheckpointPolicy,
			s.MedianNs/1e6, s.IQRNs/1e6, dOff, iqrOff, unstableStr,
			s.MedianRetriedTxs, s.MedianCaptureCreated, s.MedianSkippedBudget,
			s.MedianSkippedLowScore, s.MedianResumeCommitted, s.MedianPrefixSaved,
			s.MedianPeakBytes, (s.MedianScoreNs+s.MedianHistoryUpdateNs)/1e6)
	}

	// 2. Bảng ma trận đầy đủ (5 chuỗi × 2 worker × 2 mức RAM × 4 chính sách)
	md.WriteString("\n## 2. Ma trận đầy đủ (5 chuỗi × 4 chính sách × 1/4 worker × RAM chặt/rộng)\n\n")
	md.WriteString("| Case | Variant | W | RAM Tier | Policy | Median (ms) | IQR (ms) | Delta vs off (ms) | Unstable | Retried | Created | SkipBud | SkipLow | Resumed | Steps Saved | Peak Bytes |\n")
	md.WriteString("| --- | --- | ---: | --- | --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, s := range rep.Summaries {
		dOff := "0.000"
		if s.MedianDeltaVsOffNs != nil {
			dOff = fmt.Sprintf("%+.3f", *s.MedianDeltaVsOffNs/1e6)
		}
		unstableStr := "no"
		if s.UnstableVsOff {
			unstableStr = "yes"
		}
		fmt.Fprintf(&md, "| %s | %s | %d | %s | `%s` | %.3f | %.3f | %s | %s | %.1f | %.1f | %.1f | %.1f | %.1f | %.0f | %.0f |\n",
			s.Case, s.Variant, s.Workers, s.BudgetTier, s.CheckpointPolicy,
			s.MedianNs/1e6, s.IQRNs/1e6, dOff, unstableStr,
			s.MedianRetriedTxs, s.MedianCaptureCreated, s.MedianSkippedBudget,
			s.MedianSkippedLowScore, s.MedianResumeCommitted, s.MedianPrefixSaved, s.MedianPeakBytes)
	}

	// 3. Phân tích từng block của merkle-mixed (4 worker, RAM chặt) và độ trễ 1 block từ decision-trace.jsonl
	md.WriteString("\n## 3. Diễn biến từng block trên `merkle-mixed` (`4` worker, RAM chặt — độ trễ 1 block)\n\n")
	md.WriteString("| Block | Giai đoạn tải | `off` (ms) | `fixed` (ms) | `history` (ms) | `adaptive` (ms) | `adaptive` Created | `adaptive` SkipLow | `adaptive` SkipBud | `adaptive` Resumed |\n")
	md.WriteString("| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")

	phaseLabel := func(b uint64) string {
		switch b {
		case 1:
			return "Block 1 (bootstrap fixed, word 0..15)"
		case 2, 3:
			return "Ổn định 1 (word 0..15: 1 early + 3 late)"
		case 4:
			return "Đổi tải (word 16..31: độ trễ lịch sử 1 block)"
		default:
			return "Ổn định 2 (word 16..31: 3 early + 1 late)"
		}
	}

	for blk := uint64(1); blk <= 6; blk++ {
		byPol := map[string][]float64{}
		var adCreated, adSkipLow, adSkipBud, adResumed []float64
		for _, r := range rep.Rows {
			if r.Case != DefaultPhase3TargetCase || r.Workers != 4 || r.BudgetTier != "tight" || r.Block != blk {
				continue
			}
			byPol[r.CheckpointPolicy] = append(byPol[r.CheckpointPolicy], float64(r.BlockElapsedNs)/1e6)
			if r.CheckpointPolicy == CheckpointAdaptive {
				if r.CaptureCreated != nil {
					adCreated = append(adCreated, float64(*r.CaptureCreated))
				}
				if r.CaptureSkippedLowScore != nil {
					adSkipLow = append(adSkipLow, float64(*r.CaptureSkippedLowScore))
				}
				if r.CaptureSkippedBudget != nil {
					adSkipBud = append(adSkipBud, float64(*r.CaptureSkippedBudget))
				}
				if r.ResumeCommitted != nil {
					adResumed = append(adResumed, float64(*r.ResumeCommitted))
				}
			}
		}
		medOff, _, _, _ := medianAndIQR(byPol[CheckpointOff])
		medFix, _, _, _ := medianAndIQR(byPol[CheckpointFixed])
		medHis, _, _, _ := medianAndIQR(byPol[CheckpointHistory])
		medAd, _, _, _ := medianAndIQR(byPol[CheckpointAdaptive])
		medC, _, _, _ := medianAndIQR(adCreated)
		medSL, _, _, _ := medianAndIQR(adSkipLow)
		medSB, _, _, _ := medianAndIQR(adSkipBud)
		medR, _, _, _ := medianAndIQR(adResumed)
		fmt.Fprintf(&md, "| %d | %s | %.3f | %.3f | %.3f | %.3f | %.1f | %.1f | %.1f | %.1f |\n",
			blk, phaseLabel(blk), medOff, medFix, medHis, medAd, medC, medSL, medSB, medR)
	}

	if len(rep.DecisionTrace) > 0 {
		md.WriteString("\n### Tóm tắt `decision-trace.jsonl` (lượt minh họa `adaptive`, 4 worker, RAM chặt)\n\n")
		byBlkDec := make(map[uint64]map[string]int)
		var blocks []uint64
		seenBlk := make(map[uint64]bool)
		for _, ev := range rep.DecisionTrace {
			if !seenBlk[ev.BlockNumber] {
				seenBlk[ev.BlockNumber] = true
				blocks = append(blocks, ev.BlockNumber)
			}
			if byBlkDec[ev.BlockNumber] == nil {
				byBlkDec[ev.BlockNumber] = make(map[string]int)
			}
			byBlkDec[ev.BlockNumber][ev.Decision]++
		}
		sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
		md.WriteString("| Block | Phân bố quyết định trong `decision-trace.jsonl` |\n")
		md.WriteString("| ---: | --- |\n")
		for _, b := range blocks {
			counts := byBlkDec[b]
			keys := make([]string, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("`%s`: %d", k, counts[k]))
			}
			fmt.Fprintf(&md, "| %d | %s |\n", b, strings.Join(parts, ", "))
		}
	}

	// 4. Quyết định chọn cấu hình mang sang Phase 4
	md.WriteString("\n## 4. Nhận định và khuyến nghị mang sang Phase 4\n\n")
	if len(targetTightSummaries) > 0 {
		bestOverall := targetTightSummaries[0]
		var bestEnabled *BenchConfigSummary
		for i := range targetTightSummaries {
			s := targetTightSummaries[i]
			if s.MedianNs < bestOverall.MedianNs {
				bestOverall = s
			}
			if s.CheckpointPolicy != CheckpointOff {
				if bestEnabled == nil || s.MedianNs < bestEnabled.MedianNs {
					cp := s
					bestEnabled = &cp
				}
			}
		}
		fmt.Fprintf(&md, "- **Cấu hình nhanh nhất trên tải mục tiêu (`merkle-mixed`, 4 worker, RAM chặt):** `%s` (median `%.3f ms`, IQR `%.3f ms`).\n",
			bestOverall.CheckpointPolicy, bestOverall.MedianNs/1e6, bestOverall.IQRNs/1e6)
		if bestOverall.CheckpointPolicy == CheckpointOff && bestEnabled != nil {
			fmt.Fprintf(&md, "- **Chính sách bật checkpoint tốt nhất (mang kèm sang Phase 4 cùng `off` theo quy ước handoff):** `%s` (median `%.3f ms`, delta vs `off` `%+.3f ms`, unstable=`%v`, resumed=`%.1f`, prefix steps saved=`%.0f`).\n",
				bestEnabled.CheckpointPolicy, bestEnabled.MedianNs/1e6,
				derefFloat64(bestEnabled.MedianDeltaVsOffNs)/1e6, bestEnabled.UnstableVsOff,
				bestEnabled.MedianResumeCommitted, bestEnabled.MedianPrefixSaved)
		} else if bestOverall.CheckpointPolicy != CheckpointOff {
			fmt.Fprintf(&md, "- **Đề xuất sang Phase 4:** mang `%s` cùng `off` làm đối chứng (delta vs `off` `%+.3f ms`, unstable=`%v`).\n",
				bestOverall.CheckpointPolicy, derefFloat64(bestOverall.MedianDeltaVsOffNs)/1e6, bestOverall.UnstableVsOff)
		}
		md.WriteString("- **Điều kiện khiến nhóm đổi lựa chọn:** khi tải có prefix thuần tính toán trước điểm xung đột dài hơn chi phí tracer/hook của interpreter (hoặc khi thay tracer Go bằng bản vá interpreter nội tuyến không cần `OnOpcode` trên mỗi bước), `adaptive` có thể chuyển lợi thế tiết kiệm `prefix_steps_saved` thành giảm thời gian thực tế vượt `off` ngay cả dưới RAM chặt.\n")
	}

	md.WriteString("\n## 5. Lệnh tái lập\n\n```bash\n")
	md.WriteString("EVM_BIN=/tmp/plutof-evm-v1168/evm GOWORK=off go run ./cmd/pluto-lab calibrate --suite phase3 --seed 31 --out results/phase3/model.json\n")
	md.WriteString("EVM_BIN=/tmp/plutof-evm-v1168/evm GOWORK=off go run ./cmd/pluto-lab verify --suite phase3 --case all --mode occ --checkpoint all --seed 73 --model results/phase3/model.json\n")
	md.WriteString("EVM_BIN=/tmp/plutof-evm-v1168/evm GOWORK=off go run ./cmd/pluto-lab bench --suite phase3 --matrix --seed 73 --runs 10 --model results/phase3/model.json --out results/phase3\n")
	md.WriteString("```\n")

	return os.WriteFile(path, []byte(md.String()), 0o644)
}

func derefFloat64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
