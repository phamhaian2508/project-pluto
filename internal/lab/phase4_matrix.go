package lab

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

const (
	// Phase4SelectionFormatVersion định danh schema selection.json của Phase 4.
	Phase4SelectionFormatVersion = 1
)

// Phase4MatrixConfigs trả về đúng 10 cấu hình đối chiếu của Phase 4 (Mục 1 phase4/worker-handoff.md):
// sequential/off (1w), grouped/off (4w), occ/{off,fixed,history,adaptive} (4w), predicted-occ/{off,fixed,history,adaptive} (4w).
func Phase4MatrixConfigs(model *evm.AdaptiveModel, modelPath, modelSHA string) []BenchExecConfig {
	interval := DefaultCheckpointInterval
	budget := DefaultCheckpointBudgetBytes
	if model != nil {
		if model.CheckpointEvery > 0 {
			interval = model.CheckpointEvery
		}
		if model.TightBudgetBytes > 0 {
			budget = model.TightBudgetBytes
		}
	}
	makeOff := func(mode ExecutionMode, workers int) BenchExecConfig {
		return BenchExecConfig{
			Mode:    mode,
			Workers: workers,
			Checkpoint: CheckpointOptions{
				Policy:             CheckpointOff,
				Interval:           interval,
				BudgetBytes:        budget,
				BudgetTier:         "tight",
				Phase2Mode:         mode == ModeOCC || mode == ModePredictedOCC,
				DisableOffObserver: true,
				ModelPath:          modelPath,
				ModelSHA256:        modelSHA,
			},
		}
	}
	makeEnabled := func(mode ExecutionMode, policy string) BenchExecConfig {
		cfg := BenchExecConfig{
			Mode:    mode,
			Workers: 4,
			Checkpoint: CheckpointOptions{
				Policy:        policy,
				Interval:      interval,
				BudgetBytes:   budget,
				BudgetTier:    "tight",
				Phase2Mode:    true,
				ModelPath:     modelPath,
				ModelSHA256:   modelSHA,
				AdaptiveModel: model,
			},
		}
		if policy != CheckpointAdaptive {
			cfg.Checkpoint.AdaptiveModel = nil
		}
		return cfg
	}

	return []BenchExecConfig{
		makeOff(ModeSequential, 1),
		makeOff(ModeGrouped, 4),
		makeOff(ModeOCC, 4),
		makeEnabled(ModeOCC, CheckpointFixed),
		makeEnabled(ModeOCC, CheckpointHistory),
		makeEnabled(ModeOCC, CheckpointAdaptive),
		makeOff(ModePredictedOCC, 4),
		makeEnabled(ModePredictedOCC, CheckpointFixed),
		makeEnabled(ModePredictedOCC, CheckpointHistory),
		makeEnabled(ModePredictedOCC, CheckpointAdaptive),
	}
}

// phase4SimplicityRank quy ước thứ tự ít thành phần hơn khi ưu thế chưa ổn định (IQR chênh lệch chứa 0).
func phase4SimplicityRank(mode ExecutionMode, policy string) int {
	switch mode {
	case ModeSequential:
		return 1
	case ModeGrouped:
		return 2
	case ModeOCC:
		switch policy {
		case CheckpointOff, "":
			return 3
		case CheckpointFixed:
			return 4
		case CheckpointHistory:
			return 5
		case CheckpointAdaptive:
			return 6
		}
	case ModePredictedOCC:
		switch policy {
		case CheckpointOff, "":
			return 7
		case CheckpointFixed:
			return 8
		case CheckpointHistory:
			return 9
		case CheckpointAdaptive:
			return 10
		}
	}
	return 99
}

// Phase4CandidateComparison lưu kết quả so sánh từng cấu hình trên tải mục tiêu merkle-mixed.
type Phase4CandidateComparison struct {
	Mode              string  `json:"mode"`
	CheckpointPolicy  string  `json:"checkpointPolicy"`
	Workers           int     `json:"workers"`
	SimplicityRank    int     `json:"simplicityRank"`
	MedianMs          float64 `json:"medianMs"`
	IQRMs             float64 `json:"iqrMs"`
	DeltaVsFastestMs  float64 `json:"deltaVsFastestMs"`
	DeltaQ1Ms         float64 `json:"deltaQ1Ms"`
	DeltaQ3Ms         float64 `json:"deltaQ3Ms"`
	Indistinguishable bool    `json:"indistinguishableFromFastest"`
	RetriedTxs        float64 `json:"medianRetriedTxs"`
	WaitedTxs         float64 `json:"medianWaitedTxs"`
	GroupedTxs        float64 `json:"medianGroupedTxs"`
	CaptureCreated    float64 `json:"medianCaptureCreated"`
	ResumeCommitted   float64 `json:"medianResumeCommitted"`
	PrefixStepsSaved  float64 `json:"medianPrefixStepsSaved"`
	PeakBytes         float64 `json:"medianPeakBytes"`
}

// Phase4CaseHighlight ghi lại cấu hình thắng cuộc và cấu hình nhanh nhất trên từng bộ tải phụ.
type Phase4CaseHighlight struct {
	Case                  string  `json:"case"`
	SelectedMode          string  `json:"selectedMode"`
	SelectedPolicy        string  `json:"selectedPolicy"`
	SelectedMedianMs      float64 `json:"selectedMedianMs"`
	SelectedIQRMs         float64 `json:"selectedIqrMs"`
	SelectedWaitedTxs     float64 `json:"selectedMedianWaitedTxs"`
	SelectedRetriedTxs    float64 `json:"selectedMedianRetriedTxs"`
	SelectedGroupedTxs    float64 `json:"selectedMedianGroupedTxs"`
	BestOnCaseMode        string  `json:"bestOnCaseMode"`
	BestOnCasePolicy      string  `json:"bestOnCasePolicy"`
	BestOnCaseWorkers     int     `json:"bestOnCaseWorkers"`
	BestOnCaseMedianMs    float64 `json:"bestOnCaseMedianMs"`
	BestOnCaseIQRMs       float64 `json:"bestOnCaseIqrMs"`
	PredictedWaitedOnCase float64 `json:"predictedOccOffMedianWaitedTxs,omitempty"`
}

// Phase4CheckpointRef lưu cấu hình bật checkpoint tốt nhất làm đối chứng demo khi winner là off/sequential.
type Phase4CheckpointRef struct {
	Mode             string  `json:"mode"`
	CheckpointPolicy string  `json:"checkpointPolicy"`
	Workers          int     `json:"workers"`
	MedianMs         float64 `json:"medianMs"`
	IQRMs            float64 `json:"iqrMs"`
	ResumeCommitted  float64 `json:"medianResumeCommitted"`
	PrefixStepsSaved float64 `json:"medianPrefixStepsSaved"`
	PeakBytes        float64 `json:"medianPeakBytes"`
}

// Phase4Selection là kết quả khóa lựa chọn cấu hình mạng Pluto cho Phase 4 (`results/phase4/selection.json`).
type Phase4Selection struct {
	FormatVersion           int                         `json:"formatVersion"`
	TimestampUTC            string                      `json:"timestampUtc"`
	GitCommit               string                      `json:"gitCommit,omitempty"`
	TargetCase              string                      `json:"targetCase"`
	EvalSeed                uint64                      `json:"evalSeed"`
	Runs                    int                         `json:"runs"`
	Mode                    string                      `json:"mode"`
	CheckpointPolicy        string                      `json:"checkpointPolicy"`
	Workers                 int                         `json:"workers"`
	BudgetBytes             uint64                      `json:"budgetBytes"`
	CheckpointInterval      uint64                      `json:"checkpointInterval"`
	DisableOffObserver      bool                        `json:"disableOffObserver"`
	ModelPath               string                      `json:"modelPath,omitempty"`
	ModelSHA256             string                      `json:"modelSha256,omitempty"`
	FixtureInputHashes      map[string]string           `json:"fixtureInputHashes,omitempty"`
	WinnerMedianMs          float64                     `json:"winnerMedianMs"`
	WinnerIQRMs             float64                     `json:"winnerIqrMs"`
	FastestMode             string                      `json:"fastestMode"`
	FastestPolicy           string                      `json:"fastestPolicy"`
	FastestMedianMs         float64                     `json:"fastestMedianMs"`
	BestEnabledCheckpoint   *Phase4CheckpointRef        `json:"bestEnabledCheckpoint,omitempty"`
	Rationale               string                      `json:"rationale"`
	TargetCaseComparisons   []Phase4CandidateComparison `json:"targetCaseComparisons"`
	SwapPoolsSummary        *Phase4CaseHighlight        `json:"swapPoolsSummary,omitempty"`
	ApproveIndependentStats *Phase4CaseHighlight        `json:"approveIndependentSummary,omitempty"`
}

// LoadPhase4Selection đọc và kiểm tra tính hợp lệ của selection.json.
func LoadPhase4Selection(path string) (*Phase4Selection, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read selection.json %q: %w", path, err)
	}
	var sel Phase4Selection
	if err := json.Unmarshal(raw, &sel); err != nil {
		return nil, fmt.Errorf("parse selection.json %q: %w", path, err)
	}
	if strings.TrimSpace(sel.Mode) == "" {
		return nil, fmt.Errorf("selection.json %q missing mode", path)
	}
	if sel.Workers < 1 {
		return nil, fmt.Errorf("selection.json %q invalid workers %d", path, sel.Workers)
	}
	if strings.TrimSpace(sel.CheckpointPolicy) == "" {
		sel.CheckpointPolicy = CheckpointOff
	}
	return &sel, nil
}

func currentGitCommit() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func phase4Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := p * float64(n-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

func quartileRangeFloat(vals []float64) (q1, med, q3 float64) {
	if len(vals) == 0 {
		return 0, 0, 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	q1 = phase4Percentile(cp, 0.25)
	med = phase4Percentile(cp, 0.50)
	q3 = phase4Percentile(cp, 0.75)
	return q1, med, q3
}

// BuildPhase4Selection áp quy tắc chọn khóa trước trên merkle-mixed (4w, RAM chặt) từ kết quả đo ma trận 10 cấu hình.
func BuildPhase4Selection(rep *BenchReport) (*Phase4Selection, error) {
	if rep == nil {
		return nil, fmt.Errorf("nil bench report")
	}

	targetCase := DefaultPhase3TargetCase
	type cfgKey struct {
		mode   ExecutionMode
		policy string
		w      int
	}

	// gom tổng thời gian cả chuỗi theo từng (cfg, run) trên targetCase
	perCfgRunNs := make(map[cfgKey]map[int]int64)
	for _, r := range rep.Rows {
		if r.Case != targetCase {
			continue
		}
		k := cfgKey{mode: r.Mode, policy: r.CheckpointPolicy, w: r.Workers}
		if perCfgRunNs[k] == nil {
			perCfgRunNs[k] = make(map[int]int64)
		}
		perCfgRunNs[k][r.Run] += r.BlockElapsedNs
	}

	var targetSummaries []BenchConfigSummary
	for _, s := range rep.Summaries {
		if s.Case == targetCase {
			targetSummaries = append(targetSummaries, s)
		}
	}
	if len(targetSummaries) == 0 {
		return nil, fmt.Errorf("phase4 selection requires summaries for target case %q", targetCase)
	}

	// tìm cấu hình có trung vị thấp nhất
	fastest := targetSummaries[0]
	for _, s := range targetSummaries[1:] {
		if s.MedianNs < fastest.MedianNs ||
			(s.MedianNs == fastest.MedianNs && phase4SimplicityRank(ExecutionMode(s.Mode), s.CheckpointPolicy) < phase4SimplicityRank(ExecutionMode(fastest.Mode), fastest.CheckpointPolicy)) {
			fastest = s
		}
	}
	fastestKey := cfgKey{mode: ExecutionMode(fastest.Mode), policy: fastest.CheckpointPolicy, w: fastest.Workers}

	comparisons := make([]Phase4CandidateComparison, 0, len(targetSummaries))
	var eligibleIndist []Phase4CandidateComparison
	var bestEnabled *Phase4CheckpointRef

	for _, s := range targetSummaries {
		k := cfgKey{mode: ExecutionMode(s.Mode), policy: s.CheckpointPolicy, w: s.Workers}
		var deltasMs []float64
		for runIdx := 1; runIdx <= rep.Metadata.Runs; runIdx++ {
			vCand, ok1 := perCfgRunNs[k][runIdx]
			vFast, ok2 := perCfgRunNs[fastestKey][runIdx]
			if ok1 && ok2 {
				deltasMs = append(deltasMs, float64(vCand-vFast)/1e6)
			}
		}
		q1, dMed, q3 := quartileRangeFloat(deltasMs)
		indist := (k == fastestKey) || (len(deltasMs) > 0 && q1 <= 0 && q3 >= 0)

		cand := Phase4CandidateComparison{
			Mode:              s.Mode,
			CheckpointPolicy:  s.CheckpointPolicy,
			Workers:           s.Workers,
			SimplicityRank:    phase4SimplicityRank(ExecutionMode(s.Mode), s.CheckpointPolicy),
			MedianMs:          s.MedianNs / 1e6,
			IQRMs:             s.IQRNs / 1e6,
			DeltaVsFastestMs:  dMed,
			DeltaQ1Ms:         q1,
			DeltaQ3Ms:         q3,
			Indistinguishable: indist,
			RetriedTxs:        s.MedianRetriedTxs,
			WaitedTxs:         s.MedianWaitedTxs,
			GroupedTxs:        s.MedianGroupedTxs,
			CaptureCreated:    s.MedianCaptureCreated,
			ResumeCommitted:   s.MedianResumeCommitted,
			PrefixStepsSaved:  s.MedianPrefixSaved,
			PeakBytes:         s.MedianPeakBytes,
		}
		comparisons = append(comparisons, cand)
		if indist {
			eligibleIndist = append(eligibleIndist, cand)
		}
	}

	// chọn bestEnabledCheckpoint theo cùng quy tắc tie-break IQR chứa 0 (ít thành phần hơn thắng khi chưa ổn định)
	var enabledSummaries []BenchConfigSummary
	for _, s := range targetSummaries {
		if s.CheckpointPolicy != CheckpointOff && s.CheckpointPolicy != "" {
			enabledSummaries = append(enabledSummaries, s)
		}
	}
	if len(enabledSummaries) > 0 {
		fastestEn := enabledSummaries[0]
		for _, s := range enabledSummaries[1:] {
			if s.MedianNs < fastestEn.MedianNs ||
				(s.MedianNs == fastestEn.MedianNs && phase4SimplicityRank(ExecutionMode(s.Mode), s.CheckpointPolicy) < phase4SimplicityRank(ExecutionMode(fastestEn.Mode), fastestEn.CheckpointPolicy)) {
				fastestEn = s
			}
		}
		fastestEnKey := cfgKey{mode: ExecutionMode(fastestEn.Mode), policy: fastestEn.CheckpointPolicy, w: fastestEn.Workers}
		chosenEn := fastestEn
		chosenEnRank := phase4SimplicityRank(ExecutionMode(chosenEn.Mode), chosenEn.CheckpointPolicy)
		for _, s := range enabledSummaries {
			k := cfgKey{mode: ExecutionMode(s.Mode), policy: s.CheckpointPolicy, w: s.Workers}
			var deltasMs []float64
			for runIdx := 1; runIdx <= rep.Metadata.Runs; runIdx++ {
				vCand, ok1 := perCfgRunNs[k][runIdx]
				vFast, ok2 := perCfgRunNs[fastestEnKey][runIdx]
				if ok1 && ok2 {
					deltasMs = append(deltasMs, float64(vCand-vFast)/1e6)
				}
			}
			q1, _, q3 := quartileRangeFloat(deltasMs)
			indistEn := (k == fastestEnKey) || (len(deltasMs) > 0 && q1 <= 0 && q3 >= 0)
			if !indistEn {
				continue
			}
			rank := phase4SimplicityRank(ExecutionMode(s.Mode), s.CheckpointPolicy)
			if rank < chosenEnRank || (rank == chosenEnRank && s.MedianNs < chosenEn.MedianNs) {
				chosenEn = s
				chosenEnRank = rank
			}
		}
		bestEnabled = &Phase4CheckpointRef{
			Mode:             string(chosenEn.Mode),
			CheckpointPolicy: chosenEn.CheckpointPolicy,
			Workers:          chosenEn.Workers,
			MedianMs:         chosenEn.MedianNs / 1e6,
			IQRMs:            chosenEn.IQRNs / 1e6,
			ResumeCommitted:  chosenEn.MedianResumeCommitted,
			PrefixStepsSaved: chosenEn.MedianPrefixSaved,
			PeakBytes:        chosenEn.MedianPeakBytes,
		}
	}

	sort.Slice(comparisons, func(i, j int) bool {
		if comparisons[i].MedianMs != comparisons[j].MedianMs {
			return comparisons[i].MedianMs < comparisons[j].MedianMs
		}
		return comparisons[i].SimplicityRank < comparisons[j].SimplicityRank
	})

	// chọn ứng viên ít thành phần nhất trong nhóm không phân biệt được về thống kê với fastest
	winner := eligibleIndist[0]
	for _, c := range eligibleIndist[1:] {
		if c.SimplicityRank < winner.SimplicityRank ||
			(c.SimplicityRank == winner.SimplicityRank && c.MedianMs < winner.MedianMs) {
			winner = c
		}
	}

	var rationale string
	if winner.Mode == string(fastest.Mode) && winner.CheckpointPolicy == fastest.CheckpointPolicy {
		rationale = fmt.Sprintf(
			"Trên tải mục tiêu %s (seed=%d, RAM chặt=%d B, %d lượt), %s/%s (%d worker) đạt trung vị thấp nhất (%.3f ms, IQR %.3f ms), khớp 100%% Geth evm t8n và nằm trong ngân sách RAM.",
			targetCase, rep.Metadata.Seed, rep.Metadata.TightBudgetBytes, rep.Metadata.Runs,
			winner.Mode, winner.CheckpointPolicy, winner.Workers, winner.MedianMs, winner.IQRMs,
		)
	} else {
		rationale = fmt.Sprintf(
			"Trên tải mục tiêu %s (seed=%d, RAM chặt=%d B, %d lượt), cấu hình nhanh nhất là %s/%s (%.3f ms), nhưng chênh lệch ghép cặp với %s/%s (%.3f ms) có IQR [%.3f, %.3f] ms chứa 0 (ưu thế chưa ổn định). Theo quy tắc khóa trước, chọn cấu hình ít thành phần hơn là %s/%s (%d worker).",
			targetCase, rep.Metadata.Seed, rep.Metadata.TightBudgetBytes, rep.Metadata.Runs,
			fastest.Mode, fastest.CheckpointPolicy, fastest.MedianNs/1e6,
			winner.Mode, winner.CheckpointPolicy, winner.MedianMs,
			winner.DeltaQ1Ms, winner.DeltaQ3Ms,
			winner.Mode, winner.CheckpointPolicy, winner.Workers,
		)
	}

	buildHighlight := func(caseName string) *Phase4CaseHighlight {
		var list []BenchConfigSummary
		for _, s := range rep.Summaries {
			if s.Case == caseName {
				list = append(list, s)
			}
		}
		if len(list) == 0 {
			return nil
		}
		best := list[0]
		var selSum *BenchConfigSummary
		var predWaited float64
		for i := range list {
			s := list[i]
			if s.MedianNs < best.MedianNs {
				best = s
			}
			if string(s.Mode) == winner.Mode && s.CheckpointPolicy == winner.CheckpointPolicy && s.Workers == winner.Workers {
				cp := s
				selSum = &cp
			}
			if ExecutionMode(s.Mode) == ModePredictedOCC && s.CheckpointPolicy == CheckpointOff {
				predWaited = s.MedianWaitedTxs
			}
		}
		h := &Phase4CaseHighlight{
			Case:                  caseName,
			SelectedMode:          winner.Mode,
			SelectedPolicy:        winner.CheckpointPolicy,
			BestOnCaseMode:        string(best.Mode),
			BestOnCasePolicy:      best.CheckpointPolicy,
			BestOnCaseWorkers:     best.Workers,
			BestOnCaseMedianMs:    best.MedianNs / 1e6,
			BestOnCaseIQRMs:       best.IQRNs / 1e6,
			PredictedWaitedOnCase: predWaited,
		}
		if selSum != nil {
			h.SelectedMedianMs = selSum.MedianNs / 1e6
			h.SelectedIQRMs = selSum.IQRNs / 1e6
			h.SelectedWaitedTxs = selSum.MedianWaitedTxs
			h.SelectedRetriedTxs = selSum.MedianRetriedTxs
			h.SelectedGroupedTxs = selSum.MedianGroupedTxs
		}
		return h
	}

	return &Phase4Selection{
		FormatVersion:           Phase4SelectionFormatVersion,
		TimestampUTC:            time.Now().UTC().Format(time.RFC3339),
		GitCommit:               currentGitCommit(),
		TargetCase:              targetCase,
		EvalSeed:                rep.Metadata.Seed,
		Runs:                    rep.Metadata.Runs,
		Mode:                    winner.Mode,
		CheckpointPolicy:        winner.CheckpointPolicy,
		Workers:                 winner.Workers,
		BudgetBytes:             rep.Metadata.TightBudgetBytes,
		CheckpointInterval:      rep.Metadata.CheckpointEvery,
		DisableOffObserver:      winner.CheckpointPolicy == CheckpointOff,
		ModelPath:               rep.Metadata.ModelPath,
		ModelSHA256:             rep.Metadata.ModelSHA256,
		FixtureInputHashes:      rep.Metadata.FixtureInputHashes,
		WinnerMedianMs:          winner.MedianMs,
		WinnerIQRMs:             winner.IQRMs,
		FastestMode:             string(fastest.Mode),
		FastestPolicy:           fastest.CheckpointPolicy,
		FastestMedianMs:         fastest.MedianNs / 1e6,
		BestEnabledCheckpoint:   bestEnabled,
		Rationale:               rationale,
		TargetCaseComparisons:   comparisons,
		SwapPoolsSummary:        buildHighlight("swap-pools"),
		ApproveIndependentStats: buildHighlight("approve-independent"),
	}, nil
}

func writePhase4SelectionJSON(path string, sel *Phase4Selection) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sel, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func writePhase4SummaryMD(path string, rep *BenchReport, sel *Phase4Selection) error {
	var md strings.Builder
	m := rep.Metadata

	md.WriteString("# Pluto Phase 4 — Ma trận 10 cấu hình & Quyết định chọn cấu hình mạng\n\n")
	fmt.Fprintf(&md, "- OS / Arch: `%s/%s`, CPU: `%s` (`numCPU=%d`), locked `GOMAXPROCS=%d`\n",
		m.GOOS, m.GOARCH, m.CPUModel, m.NumCPU, m.GOMAXPROCS)
	fmt.Fprintf(&md, "- Go: `%s`, Geth reference: `%s`, `evmBin=%s`\n", m.GoVersion, m.EVMBinVersion, m.EVMBin)
	fmt.Fprintf(&md, "- Model: `%s` (`sha256=%s`), `N=%d`, RAM chặt=`%d bytes`, ngưỡng=`%.4f ns/byte`\n",
		m.ModelPath, m.ModelSHA256, m.CheckpointEvery, m.TightBudgetBytes, m.ScoreThreshold)
	fmt.Fprintf(&md, "- Runs per config: `%d`, rotated order per run; `off` tắt quan sát checkpoint (`DisableOffObserver=true`).\n\n", m.Runs)

	if sel != nil {
		md.WriteString("## 1. Cấu hình được chọn cho mạng Pluto (`selection.json`)\n\n")
		fmt.Fprintf(&md, "- **Cấu hình thắng cuộc:** `%s / %s` (`workers=%d`, `budget=%d B`, `N=%d`)\n",
			sel.Mode, sel.CheckpointPolicy, sel.Workers, sel.BudgetBytes, sel.CheckpointInterval)
		fmt.Fprintf(&md, "- **Cấu hình trung vị thấp nhất trên `%s`:** `%s / %s` (`%.3f ms`)\n",
			sel.TargetCase, sel.FastestMode, sel.FastestPolicy, sel.FastestMedianMs)
		if sel.BestEnabledCheckpoint != nil {
			fmt.Fprintf(&md, "- **Chính sách bật checkpoint tốt nhất (đối chứng demo):** `%s / %s` (`%.3f ms`, `resumed=%.1f`, `saved=%.0f` opcodes, `peak=%.0f B`)\n",
				sel.BestEnabledCheckpoint.Mode, sel.BestEnabledCheckpoint.CheckpointPolicy,
				sel.BestEnabledCheckpoint.MedianMs, sel.BestEnabledCheckpoint.ResumeCommitted,
				sel.BestEnabledCheckpoint.PrefixStepsSaved, sel.BestEnabledCheckpoint.PeakBytes)
		}
		fmt.Fprintf(&md, "- **Lý do chọn:** %s\n\n", sel.Rationale)

		md.WriteString("## 2. Xếp hạng 10 cấu hình trên tải mục tiêu `merkle-mixed` (`seed-73`, RAM chặt)\n\n")
		md.WriteString("| Mode | Policy | W | Median (ms) | IQR (ms) | Delta vs Fastest (ms) | IQR [Q1, Q3] (ms) | 0 in IQR | Retried | Waited | Created | Resumed | Steps Saved | Peak B |\n")
		md.WriteString("| --- | --- | ---: | ---: | ---: | ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
		for _, c := range sel.TargetCaseComparisons {
			ind := "no"
			if c.Indistinguishable {
				ind = "yes"
			}
			fmt.Fprintf(&md, "| `%s` | `%s` | %d | %.3f | %.3f | %+.3f | [%+.3f, %+.3f] | %s | %.1f | %.1f | %.1f | %.1f | %.0f | %.0f |\n",
				c.Mode, c.CheckpointPolicy, c.Workers, c.MedianMs, c.IQRMs,
				c.DeltaVsFastestMs, c.DeltaQ1Ms, c.DeltaQ3Ms, ind,
				c.RetriedTxs, c.WaitedTxs, c.CaptureCreated, c.ResumeCommitted, c.PrefixStepsSaved, c.PeakBytes)
		}
		md.WriteString("\n")
	}

	md.WriteString("## 3. Kết quả toàn bộ 10 cấu hình trên cả 3 bộ tải (`merkle-mixed`, `swap-pools`, `approve-independent`)\n\n")
	md.WriteString("| Case | Mode | Policy | W | Median (ms) | IQR (ms) | Retried | Grouped | Waited | WaitPred (ms) | Created | Resumed | Steps Saved | Peak B |\n")
	md.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, s := range rep.Summaries {
		fmt.Fprintf(&md, "| `%s` | `%s` | `%s` | %d | %.3f | %.3f | %.1f | %.1f | %.1f | %.3f | %.1f | %.1f | %.0f | %.0f |\n",
			s.Case, s.Mode, s.CheckpointPolicy, s.Workers, s.MedianNs/1e6, s.IQRNs/1e6,
			s.MedianRetriedTxs, s.MedianGroupedTxs, s.MedianWaitedTxs, s.MedianWaitPredNs/1e6,
			s.MedianCaptureCreated, s.MedianResumeCommitted, s.MedianPrefixSaved, s.MedianPeakBytes)
	}

	return os.WriteFile(path, []byte(md.String()), 0o644)
}
