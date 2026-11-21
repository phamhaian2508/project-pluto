package lab

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// BenchExecConfig xác định một cấu hình (mode, workers, checkpoint) trong phép đo Phase 1–2.
type BenchExecConfig struct {
	Mode       ExecutionMode     `json:"mode"`
	Workers    int               `json:"workers"`
	Checkpoint CheckpointOptions `json:"checkpoint,omitempty"`
}

func (c BenchExecConfig) EffectiveCheckpointPolicy() string {
	if c.Checkpoint.Policy == "" {
		return CheckpointOff
	}
	return c.Checkpoint.Policy
}

func (c BenchExecConfig) Label() string {
	if c.Checkpoint.BudgetTier != "" {
		return fmt.Sprintf("%s/%d/%s/%s", c.Mode, c.Workers, c.EffectiveCheckpointPolicy(), c.Checkpoint.BudgetTier)
	}
	if c.Checkpoint.Phase2Mode || (c.Checkpoint.Policy != "" && c.Checkpoint.Policy != CheckpointOff) {
		return fmt.Sprintf("%s/%d/%s", c.Mode, c.Workers, c.EffectiveCheckpointPolicy())
	}
	return fmt.Sprintf("%s/%d", c.Mode, c.Workers)
}

// DefaultBenchConfigs trả về đúng 7 cấu hình đo hiệu năng của Phase 1.
func DefaultBenchConfigs() []BenchExecConfig {
	return []BenchExecConfig{
		{Mode: ModeSequential, Workers: 1},
		{Mode: ModeOCC, Workers: 1},
		{Mode: ModeOCC, Workers: 4},
		{Mode: ModeGrouped, Workers: 1},
		{Mode: ModeGrouped, Workers: 4},
		{Mode: ModePredictedOCC, Workers: 1},
		{Mode: ModePredictedOCC, Workers: 4},
	}
}

// BenchOptions cấu hình một chiến dịch benchmark `pluto-lab bench`.
type BenchOptions struct {
	Suite       string
	Cases       []string
	Configs     []BenchExecConfig
	Runs        int
	Seed        uint64
	Matrix      bool
	ModelPath   string
	Model       *evm.AdaptiveModel
	TestdataDir string
	OutDir      string
	EVMBin      string
	Timeout     time.Duration
	GOMAXPROCS  int
}

// BenchRow biểu diễn đúng một hàng CSV thô ứng với một (case, mode, workers, checkpoint, run, block).
type BenchRow struct {
	Case                   string
	Variant                string
	InputHash              string
	Seed                   string
	Mode                   ExecutionMode
	Workers                int
	Run                    int
	Block                  uint64
	TxCount                int
	FailedTxs              int
	GasUsed                uint64
	BlockElapsedNs         int64
	RetriedTxs             *int
	TotalRetries           *int
	FallbackTxs            *int
	GroupedTxs             *int
	MaxGroupWidth          *int
	UnknownSerialTxs       *int
	HotPools               *string
	WaitedTxs              *int
	WaitPredecessorNs      *int64
	WaitWorkerNs           *int64
	WorkerIdleNs           *int64
	AppHash                common.Hash
	CheckpointPolicy       string
	CheckpointEvery        *uint64
	CheckpointBudgetBytes  *uint64
	BudgetTier             string
	ModelSHA256            string
	CaptureRequested       *uint64
	CaptureCreated         *uint64
	CaptureSkippedBudget   *uint64
	CaptureSkippedLowScore *uint64
	AdaptiveCandidates     *uint64
	AdaptiveCalibHits      *uint64
	AdaptiveCalibMisses    *uint64
	AdaptiveHistoryMisses  *uint64
	ResumeAttempted        *uint64
	ResumeCommitted        *uint64
	ResumeCancelled        *uint64
	RestoreRejectedPrefix  *uint64
	AccountedPeakBytes     *uint64
	CaptureNs              *uint64
	SelectNs               *uint64
	RestoreNs              *uint64
	ScoreNs                *uint64
	HistoryUpdateNs        *uint64
	HistoryMemoryBytes     *uint64
	OpcodeSteps            *uint64
	PrefixStepsSaved       *uint64
	FallbackReasons        *string
}

// BenchConfigSummary tóm tắt trung vị, Q1, Q3, IQR và chênh lệch theo cặp qua các lượt.
type BenchConfigSummary struct {
	Case                  string   `json:"case"`
	Variant               string   `json:"variant,omitempty"`
	Mode                  string   `json:"mode"`
	CheckpointPolicy      string   `json:"checkpointPolicy,omitempty"`
	BudgetTier            string   `json:"budgetTier,omitempty"`
	BudgetBytes           uint64   `json:"budgetBytes,omitempty"`
	Workers               int      `json:"workers"`
	Runs                  int      `json:"runs"`
	Blocks                int      `json:"blocks"`
	TotalTxsPerRun        int      `json:"totalTxsPerRun"`
	MedianNs              float64  `json:"medianNs"`
	Q1Ns                  float64  `json:"q1Ns"`
	Q3Ns                  float64  `json:"q3Ns"`
	IQRNs                 float64  `json:"iqrNs"`
	MedianDeltaVsSeqNs    float64  `json:"medianDeltaVsSeqNs"`
	IQRDeltaVsSeqNs       float64  `json:"iqrDeltaVsSeqNs"`
	MedianDeltaVsOCC4Ns   *float64 `json:"medianDeltaVsOcc4Ns,omitempty"`
	IQRDeltaVsOCC4Ns      *float64 `json:"iqrDeltaVsOcc4Ns,omitempty"`
	MedianDeltaVsOffNs    *float64 `json:"medianDeltaVsOffNs,omitempty"`
	Q1DeltaVsOffNs        *float64 `json:"q1DeltaVsOffNs,omitempty"`
	Q3DeltaVsOffNs        *float64 `json:"q3DeltaVsOffNs,omitempty"`
	IQRDeltaVsOffNs       *float64 `json:"iqrDeltaVsOffNs,omitempty"`
	UnstableVsOff         bool     `json:"unstableVsOff,omitempty"`
	NoiseExceedsDiffVsSeq bool     `json:"noiseExceedsDiffVsSeq"`
	MedianRetriedTxs      float64  `json:"medianRetriedTxs,omitempty"`
	MedianGroupedTxs      float64  `json:"medianGroupedTxs,omitempty"`
	MedianWaitedTxs       float64  `json:"medianWaitedTxs,omitempty"`
	MedianWaitPredNs      float64  `json:"medianWaitPredNs,omitempty"`
	MedianWaitWorkerNs    float64  `json:"medianWaitWorkerNs,omitempty"`
	MedianWorkerIdleNs    float64  `json:"medianWorkerIdleNs,omitempty"`
	MedianCaptureCreated  float64  `json:"medianCaptureCreated,omitempty"`
	MedianSkippedBudget   float64  `json:"medianSkippedBudget,omitempty"`
	MedianSkippedLowScore float64  `json:"medianSkippedLowScore,omitempty"`
	MedianAdaptiveCands   float64  `json:"medianAdaptiveCandidates,omitempty"`
	MedianCalibHits       float64  `json:"medianAdaptiveCalibHits,omitempty"`
	MedianCalibMisses     float64  `json:"medianAdaptiveCalibMisses,omitempty"`
	MedianResumeAttempted float64  `json:"medianResumeAttempted,omitempty"`
	MedianResumeCommitted float64  `json:"medianResumeCommitted,omitempty"`
	MedianResumeCancelled float64  `json:"medianResumeCancelled,omitempty"`
	MedianPeakBytes       float64  `json:"medianPeakBytes,omitempty"`
	MedianOpcodeSteps     float64  `json:"medianOpcodeSteps,omitempty"`
	MedianPrefixSaved     float64  `json:"medianPrefixStepsSaved,omitempty"`
	MedianCaptureNs       float64  `json:"medianCaptureNs,omitempty"`
	MedianSelectNs        float64  `json:"medianSelectNs,omitempty"`
	MedianRestoreNs       float64  `json:"medianRestoreNs,omitempty"`
	MedianScoreNs         float64  `json:"medianScoreNs,omitempty"`
	MedianHistoryUpdateNs float64  `json:"medianHistoryUpdateNs,omitempty"`
	MedianHistoryMemBytes float64  `json:"medianHistoryMemoryBytes,omitempty"`
}

// BenchBlockComparison so sánh trực tiếp một block giữa predicted-occ/4 và occ/4 (ví dụ block 7 và block 8).
type BenchBlockComparison struct {
	Case                string  `json:"case"`
	Block               uint64  `json:"block"`
	OCC4MedianNs        float64 `json:"occ4MedianNs"`
	OCC4IQRNs           float64 `json:"occ4IqrNs"`
	Pred4MedianNs       float64 `json:"pred4MedianNs"`
	Pred4IQRNs          float64 `json:"pred4IqrNs"`
	MedianDeltaPredOCC4 float64 `json:"medianDeltaPredMinusOcc4Ns"`
	IQRDeltaPredOCC4    float64 `json:"iqrDeltaPredMinusOcc4Ns"`
	OCC4MedianRetries   float64 `json:"occ4MedianRetries"`
	Pred4MedianRetries  float64 `json:"pred4MedianRetries"`
	Pred4MedianWaited   float64 `json:"pred4MedianWaitedTxs"`
	Pred4MedianWaitNs   float64 `json:"pred4MedianWaitPredecessorNs"`
	Pred4MedianIdleNs   float64 `json:"pred4MedianWorkerIdleNs"`
	Pred4HotPools       string  `json:"pred4HotPools"`
}

// BenchMetadata lưu cấu hình máy, phiên bản phần mềm, GOMAXPROCS và lịch luân phiên đã chạy.
type BenchMetadata struct {
	TimestampUTC          string            `json:"timestampUtc"`
	Suite                 string            `json:"suite,omitempty"`
	Seed                  uint64            `json:"seed,omitempty"`
	ModelPath             string            `json:"modelPath,omitempty"`
	ModelSHA256           string            `json:"modelSha256,omitempty"`
	FixtureManifestSHA256 string            `json:"fixtureManifestSha256,omitempty"`
	FixtureInputHashes    map[string]string `json:"fixtureInputHashes,omitempty"`
	CheckpointEvery       uint64            `json:"checkpointEvery,omitempty"`
	TightBudgetBytes      uint64            `json:"tightBudgetBytes,omitempty"`
	WideBudgetBytes       uint64            `json:"wideBudgetBytes,omitempty"`
	ScoreThreshold        float64           `json:"scoreThresholdNsPerByte,omitempty"`
	DecisionTracePath     string            `json:"decisionTracePath,omitempty"`
	DecisionTraceEvents   int               `json:"decisionTraceEvents,omitempty"`
	GOOS                  string            `json:"goos"`
	GOARCH                string            `json:"goarch"`
	GoVersion             string            `json:"goVersion"`
	CPUModel              string            `json:"cpuModel,omitempty"`
	NumCPU                int               `json:"numCpu"`
	GOMAXPROCS            int               `json:"gomaxprocs"`
	EVMBin                string            `json:"evmBin"`
	EVMBinVersion         string            `json:"evmVersion"`
	BinaryPath            string            `json:"binaryPath,omitempty"`
	BinarySHA256          string            `json:"binarySha256,omitempty"`
	Runs                  int               `json:"runs"`
	Cases                 []string          `json:"cases"`
	Configs               []BenchExecConfig `json:"configs"`
	RunOrders             [][]string        `json:"runOrders"`
	TimingScope           string            `json:"timingScope"`
	BlankColumnsDoc       map[string]string `json:"blankColumnsExplanation"`
}

// BenchReport chứa toàn bộ kết quả thô, bảng tổng hợp và phân tích block 7/8.
type BenchReport struct {
	Suite           string                      `json:"suite,omitempty"`
	Metadata        BenchMetadata               `json:"metadata"`
	Model           *evm.AdaptiveModel          `json:"-"`
	DecisionTrace   []evm.AdaptiveDecisionEvent `json:"-"`
	Rows            []BenchRow                  `json:"-"`
	Summaries       []BenchConfigSummary        `json:"summaries"`
	Block7Vs8       []BenchBlockComparison      `json:"block7And8Comparisons,omitempty"`
	Phase4Selection *Phase4Selection            `json:"phase4Selection,omitempty"`
	SwapRowCnt      int                         `json:"swapPoolsRows"`
}

// executeAndVerifyBenchChain chạy một cấu hình trên chuỗi block, đối chiếu tuần tự & Geth t8n và trả các hàng đo.
func executeAndVerifyBenchChain(
	ctx context.Context,
	suite, caseID string,
	chainCase *ChainCase,
	seedStr string,
	cfg BenchExecConfig,
	runIdx int,
	seqChain *ChainResult,
	gethChain []*T8NBlockResult,
) ([]BenchRow, error) {
	dbDir, err := os.MkdirTemp("", "pluto-lab-bench-run-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dbDir)

	db, err := store.NewPebbleDB("bench", dbDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	runner, err := NewChainRunnerWithCheckpoints(db, cfg.Mode, cfg.Workers, false, cfg.Checkpoint, chainCase.GenesisAlloc)
	if err != nil {
		return nil, fmt.Errorf("bench case=%s cfg=%s run=%d init genesis: %w", caseID, cfg.Label(), runIdx, err)
	}
	if rep := CompareAppHashes(0, string(ModeSequential), seqChain.GenesisAppHash, string(cfg.Mode), runner.LatestAppHash); !rep.OK() {
		return nil, fmt.Errorf("bench case=%s cfg=%s run=%d genesis: %v", caseID, cfg.Label(), runIdx, rep.Error())
	}

	rows := make([]BenchRow, 0, len(chainCase.Blocks))
	for bIdx, blk := range chainCase.Blocks {
		blkRes, elapsedNs, execErr := runner.ExecuteBlock(ctx, blk.Env, blk.Txs)
		if execErr != nil {
			return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d execute error: %w",
				caseID, cfg.Label(), runIdx, blk.Env.Number, execErr)
		}

		// đối chiếu ngoài khoảng đo với AppHash tuần tự, ngân sách cứng và Geth t8n chuẩn của đúng input
		if blkRes.AppHash != seqChain.Blocks[bIdx].AppHash {
			return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d AppHash mismatch: got=%s want=%s",
				caseID, cfg.Label(), runIdx, blk.Env.Number, blkRes.AppHash.Hex(), seqChain.Blocks[bIdx].AppHash.Hex())
		}
		if blkRes.Phase2Metrics != nil && cfg.EffectiveCheckpointPolicy() != CheckpointOff {
			m := blkRes.Phase2Metrics
			if m.AccountedPeakBytes > m.BudgetBytes {
				return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d peak bytes %d > budget %d",
					caseID, cfg.Label(), runIdx, blk.Env.Number, m.AccountedPeakBytes, m.BudgetBytes)
			}
			if m.AccountedRemainingBytes != 0 {
				return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d remaining bytes %d != 0",
					caseID, cfg.Label(), runIdx, blk.Env.Number, m.AccountedRemainingBytes)
			}
		}
		postState, expErr := runner.ExportLogicalState()
		if expErr != nil {
			return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d export post-state: %w",
				caseID, cfg.Label(), runIdx, blk.Env.Number, expErr)
		}
		if rep := CompareBlockWithGeth(blk.Env.Number, len(blk.Txs), blkRes, postState, gethChain[bIdx]); !rep.OK() {
			return nil, fmt.Errorf("bench case=%s cfg=%s run=%d block=%d diverged from geth: %v",
				caseID, cfg.Label(), runIdx, blk.Env.Number, rep.Error())
		}
		rows = append(rows, buildBenchRow(suite, caseID, chainCase.InputHash, seedStr, cfg, runIdx, blk, blkRes, elapsedNs))
	}
	return rows, nil
}

// RunBenchmark thực thi phép đo đúng phạm vi ExecuteBlock trên mọi tải, cấu hình và lượt, đối chiếu từng block với Geth.
func RunBenchmark(ctx context.Context, opts BenchOptions) (*BenchReport, error) {
	suite, err := ParseSuite(opts.Suite)
	if err != nil {
		return nil, err
	}
	if (suite == SuitePhase3 || suite == SuitePhase4) && opts.Seed == 0 {
		opts.Seed = DefaultPhase3EvalSeed
	}
	if len(opts.Cases) == 0 {
		switch suite {
		case SuitePhase2:
			opts.Cases = AllPhase2BenchCases()
		case SuitePhase3:
			opts.Cases = AllPhase3BenchCases()
		case SuitePhase4:
			opts.Cases = AllPhase4Cases()
		default:
			opts.Cases = AllBenchCases()
		}
	}
	if opts.Runs <= 0 {
		opts.Runs = 10
	}
	if opts.TestdataDir == "" {
		if suite == SuitePhase4 {
			opts.TestdataDir = "testdata"
		} else {
			opts.TestdataDir = filepath.Join("testdata", suite)
		}
	}
	if opts.OutDir == "" {
		opts.OutDir = filepath.Join("results", suite)
	}
	if opts.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(opts.GOMAXPROCS)
	}
	lockedProcs := runtime.GOMAXPROCS(0)

	evmBin, err := ResolveEVMBin(opts.EVMBin)
	if err != nil {
		return nil, err
	}

	model := opts.Model
	if model == nil && opts.ModelPath != "" {
		model, err = evm.LoadAdaptiveModel(opts.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("load adaptive model: %w", err)
		}
	}
	modelSHA := ""
	if model != nil {
		modelSHA = model.SHA256()
	}

	var fixtureManifestSHA string
	if suite == SuitePhase3 {
		seedManPath := filepath.Join(opts.TestdataDir, fmt.Sprintf("seed-%d", opts.Seed), "manifest.json")
		if rawMan, rErr := os.ReadFile(seedManPath); rErr == nil {
			sum := sha256.Sum256(rawMan)
			fixtureManifestSHA = "0x" + hex.EncodeToString(sum[:])
		}
	}

	// dựng danh sách cấu hình chuẩn cho Phase 3 hoặc Phase 4 matrix
	if suite == SuitePhase4 && opts.Matrix {
		if model == nil {
			return nil, fmt.Errorf("phase4 matrix benchmark requires --model (model.json)")
		}
		opts.Configs = Phase4MatrixConfigs(model, opts.ModelPath, modelSHA)
	} else if suite == SuitePhase3 && opts.Matrix {
		if model == nil {
			return nil, fmt.Errorf("phase3 matrix benchmark requires --model (model.json)")
		}
		var matConfigs []BenchExecConfig
		for _, w := range []int{1, 4} {
			for _, tier := range []struct {
				label string
				bytes uint64
			}{
				{label: "tight", bytes: model.TightBudgetBytes},
				{label: "wide", bytes: model.WideBudgetBytes},
			} {
				for _, pol := range []string{CheckpointOff, CheckpointFixed, CheckpointHistory, CheckpointAdaptive} {
					matConfigs = append(matConfigs, BenchExecConfig{
						Mode:    ModeOCC,
						Workers: w,
						Checkpoint: CheckpointOptions{
							Policy:        pol,
							Interval:      model.CheckpointEvery,
							BudgetBytes:   tier.bytes,
							BudgetTier:    tier.label,
							Phase2Mode:    true,
							ModelPath:     opts.ModelPath,
							ModelSHA256:   modelSHA,
							AdaptiveModel: model,
						},
					})
				}
			}
		}
		opts.Configs = matConfigs
	} else if len(opts.Configs) == 0 {
		opts.Configs = DefaultBenchConfigs()
	}

	runOrders := make([][]string, opts.Runs)
	if suite == SuitePhase3 && opts.Matrix {
		pols := []string{CheckpointOff, CheckpointFixed, CheckpointHistory, CheckpointAdaptive}
		for r := 0; r < opts.Runs; r++ {
			order := make([]string, len(pols))
			for pos := range pols {
				order[pos] = pols[(r+pos)%len(pols)]
			}
			runOrders[r] = order
		}
	} else {
		for r := 0; r < opts.Runs; r++ {
			order := make([]string, len(opts.Configs))
			for pos := range opts.Configs {
				c := opts.Configs[(r+pos)%len(opts.Configs)]
				order[pos] = c.Label()
			}
			runOrders[r] = order
		}
	}

	allRows := make([]BenchRow, 0, opts.Runs*len(opts.Configs)*16)
	fixtureInputHashes := make(map[string]string, len(opts.Cases))
	var mixedChainCase *ChainCase

	for _, caseID := range opts.Cases {
		chainCase, err := LoadSuiteFixtureCase(suite, opts.TestdataDir, opts.Seed, caseID)
		if err != nil {
			return nil, fmt.Errorf("bench load case %q: %w", caseID, err)
		}
		if chainCase.ExpectedReject != nil {
			return nil, fmt.Errorf("bench case %q must be valid fixture, not invalid-block", caseID)
		}
		fixtureInputHashes[caseID] = chainCase.InputHash
		if caseID == DefaultPhase3TargetCase {
			mixedChainCase = chainCase
		}

		// chạy trước Geth t8n và sequential tham chiếu ngoài khoảng đo
		gethChain, err := RunT8NChain(ctx, evmBin, chainCase, opts.Timeout)
		if err != nil {
			return nil, fmt.Errorf("bench precompute geth %q: %w", caseID, err)
		}
		if len(gethChain) != len(chainCase.Blocks) {
			return nil, fmt.Errorf("bench %q geth block count mismatch: got=%d want=%d", caseID, len(gethChain), len(chainCase.Blocks))
		}
		for i, blk := range chainCase.Blocks {
			if len(blk.ReferenceResult) > 0 {
				if err := CompareT8NReferenceResult(blk.ReferenceResult, gethChain[i].RawResultJSON); err != nil {
					return nil, fmt.Errorf("bench %q block %d geth != reference-result.json: %w", caseID, blk.Env.Number, err)
				}
			}
		}

		seqRefDir, err := os.MkdirTemp("", "pluto-lab-bench-seqref-*")
		if err != nil {
			return nil, err
		}
		seqRefDB, err := store.NewPebbleDB("seqref", seqRefDir)
		if err != nil {
			_ = os.RemoveAll(seqRefDir)
			return nil, err
		}
		seqChain, seqErr := ExecuteChain(ctx, seqRefDB, ModeSequential, 1, false, chainCase)
		_ = seqRefDB.Close()
		_ = os.RemoveAll(seqRefDir)
		if seqErr != nil {
			return nil, fmt.Errorf("bench precompute sequential %q: %w", caseID, seqErr)
		}
		for i, blk := range chainCase.Blocks {
			rep := CompareBlockWithGeth(blk.Env.Number, len(blk.Txs), seqChain.Blocks[i], seqChain.PostStates[i], gethChain[i])
			if !rep.OK() {
				return nil, fmt.Errorf("bench precompute sequential %q block %d vs geth: %v", caseID, blk.Env.Number, rep.Error())
			}
		}

		seedStr := ""
		if chainCase.Seed != nil {
			seedStr = strconv.FormatUint(*chainCase.Seed, 10)
		} else if suite == SuitePhase3 && opts.Seed > 0 {
			seedStr = strconv.FormatUint(opts.Seed, 10)
		}

		if suite == SuitePhase3 && opts.Matrix {
			// chạy ma trận Phase 3: xoay vòng 4 chính sách mỗi lượt, tái sử dụng kết quả off cho cả 2 mức RAM tight/wide
			pols := []string{CheckpointOff, CheckpointFixed, CheckpointHistory, CheckpointAdaptive}
			for _, w := range []int{1, 4} {
				for runIdx := 1; runIdx <= opts.Runs; runIdx++ {
					for pos := range pols {
						pol := pols[(runIdx-1+pos)%len(pols)]
						if pol == CheckpointOff {
							tightCfg := BenchExecConfig{
								Mode:    ModeOCC,
								Workers: w,
								Checkpoint: CheckpointOptions{
									Policy:      CheckpointOff,
									Interval:    model.CheckpointEvery,
									BudgetBytes: model.TightBudgetBytes,
									BudgetTier:  "tight",
									Phase2Mode:  true,
									ModelPath:   opts.ModelPath,
									ModelSHA256: modelSHA,
								},
							}
							offRows, err := executeAndVerifyBenchChain(ctx, suite, caseID, chainCase, seedStr, tightCfg, runIdx, seqChain, gethChain)
							if err != nil {
								return nil, err
							}
							allRows = append(allRows, offRows...)
							// tái sử dụng đúng kết quả off cho mức RAM wide cùng lượt
							for _, r := range offRows {
								cloned := r
								cloned.BudgetTier = "wide"
								wideB := model.WideBudgetBytes
								cloned.CheckpointBudgetBytes = &wideB
								allRows = append(allRows, cloned)
							}
							continue
						}

						tiers := []struct {
							label string
							bytes uint64
						}{
							{label: "tight", bytes: model.TightBudgetBytes},
							{label: "wide", bytes: model.WideBudgetBytes},
						}
						if runIdx%2 == 0 {
							tiers[0], tiers[1] = tiers[1], tiers[0]
						}
						for _, t := range tiers {
							cfg := BenchExecConfig{
								Mode:    ModeOCC,
								Workers: w,
								Checkpoint: CheckpointOptions{
									Policy:        pol,
									Interval:      model.CheckpointEvery,
									BudgetBytes:   t.bytes,
									BudgetTier:    t.label,
									Phase2Mode:    true,
									ModelPath:     opts.ModelPath,
									ModelSHA256:   modelSHA,
									AdaptiveModel: model,
								},
							}
							rows, err := executeAndVerifyBenchChain(ctx, suite, caseID, chainCase, seedStr, cfg, runIdx, seqChain, gethChain)
							if err != nil {
								return nil, err
							}
							allRows = append(allRows, rows...)
						}
					}
				}
			}
		} else {
			for runIdx := 1; runIdx <= opts.Runs; runIdx++ {
				for pos := range opts.Configs {
					cfg := opts.Configs[(runIdx-1+pos)%len(opts.Configs)]
					rows, err := executeAndVerifyBenchChain(ctx, suite, caseID, chainCase, seedStr, cfg, runIdx, seqChain, gethChain)
					if err != nil {
						return nil, err
					}
					allRows = append(allRows, rows...)
				}
			}
		}
	}

	// ngoài vùng đo: chạy 1 lượt minh họa adaptive (4 worker, RAM chặt trên merkle-mixed) để xuất decision-trace.jsonl
	var decisionTrace []evm.AdaptiveDecisionEvent
	var decisionTracePath string
	if suite == SuitePhase3 && model != nil && mixedChainCase != nil {
		probe, err := runSinglePhase3Probe(ctx, mixedChainCase, 4, CheckpointOptions{
			Policy:        CheckpointAdaptive,
			Interval:      model.CheckpointEvery,
			BudgetBytes:   model.TightBudgetBytes,
			BudgetTier:    "tight",
			Phase2Mode:    true,
			ModelPath:     opts.ModelPath,
			ModelSHA256:   modelSHA,
			AdaptiveModel: model,
		}, true)
		if err != nil {
			return nil, fmt.Errorf("record illustrative phase3 decision trace: %w", err)
		}
		decisionTrace = probe.decisionEvents
		decisionTracePath = filepath.Join(opts.OutDir, "decision-trace.jsonl")
	}

	summaries, comparisons := summarizeBenchRows(allRows, opts.Cases, opts.Configs, opts.Runs)
	swapCnt := 0
	for _, r := range allRows {
		if r.Case == "swap-pools" {
			swapCnt++
		}
	}

	binPath, binHash := resolveSelfBinaryInfo()
	meta := BenchMetadata{
		TimestampUTC:          time.Now().UTC().Format(time.RFC3339),
		Suite:                 suite,
		Seed:                  opts.Seed,
		ModelPath:             opts.ModelPath,
		ModelSHA256:           modelSHA,
		FixtureManifestSHA256: fixtureManifestSHA,
		FixtureInputHashes:    fixtureInputHashes,
		DecisionTracePath:     decisionTracePath,
		DecisionTraceEvents:   len(decisionTrace),
		GOOS:                  runtime.GOOS,
		GOARCH:                runtime.GOARCH,
		GoVersion:             runtime.Version(),
		CPUModel:              readCPUModelLinux(),
		NumCPU:                runtime.NumCPU(),
		GOMAXPROCS:            lockedProcs,
		EVMBin:                evmBin,
		EVMBinVersion:         "geth v1.16.8 (t8n Shanghai)",
		BinaryPath:            binPath,
		BinarySHA256:          binHash,
		Runs:                  opts.Runs,
		Cases:                 slices.Clone(opts.Cases),
		Configs:               slices.Clone(opts.Configs),
		RunOrders:             runOrders,
		TimingScope:           "time.Since(t0) enclosing internal/evm Executor.ExecuteBlock (scheduling, execution, retries, checkpoint capture/scoring/history/resume, ComputeAppHash, PendingWrites); excludes fixture decode/signing, Genesis init, PebbleDB fsync Commit, ExportLogicalState, Geth t8n, comparisons, and CSV writing; RecordTrace=false and Hooks=nil.",
		BlankColumnsDoc: map[string]string{
			"retried_txs,total_retries,fallback_txs":         "N/A (blank) for sequential because transactions execute strictly one by one.",
			"grouped_txs,max_group_width,unknown_serial_txs": "Populated only for grouped; N/A (blank) for sequential, occ, and predicted-occ.",
			"hot_pools,waited_txs,wait_predecessor_ns":       "Populated only for predicted-occ (hot_pools='none' when empty); N/A (blank) for other modes.",
			"wait_worker_ns":                                 "Always 0 for occ (dispatches directly inside sliding window W); for predicted-occ measures readyQueue wait from right after RecognizePinnedSwap (or predecessor release) until worker dispatch; N/A (blank) for sequential and grouped.",
			"worker_idle_ns":                                 "Integrated (workers - activeWorkers) * dt sampled across worker dispatch and completion transitions in occ and predicted-occ; N/A (blank) for sequential and grouped.",
			"seed":                                           "Present when fixture manifest specifies a deterministic PRNG seed; N/A (blank) for static fixtures.",
		},
	}
	if model != nil {
		meta.CheckpointEvery = model.CheckpointEvery
		meta.TightBudgetBytes = model.TightBudgetBytes
		meta.WideBudgetBytes = model.WideBudgetBytes
		meta.ScoreThreshold = model.ScoreThreshold
	}

	report := &BenchReport{
		Suite:         suite,
		Metadata:      meta,
		Model:         model,
		DecisionTrace: decisionTrace,
		Rows:          allRows,
		Summaries:     summaries,
		Block7Vs8:     comparisons,
		SwapRowCnt:    swapCnt,
	}

	if err := writeBenchArtifacts(opts.OutDir, report); err != nil {
		return nil, err
	}
	return report, nil
}

func buildBenchRow(
	suite, caseID, inputHash, seedStr string,
	cfg BenchExecConfig,
	runIdx int,
	blk BlockSpec,
	res *BlockResult,
	elapsedNs int64,
) BenchRow {
	failedTxs := 0
	for _, rec := range res.Receipts {
		if rec.Status == 0 {
			failedTxs++
		}
	}
	variant := ""
	if suite == SuitePhase3 {
		_, variant = SplitPhase3CaseVariant(caseID)
	}
	row := BenchRow{
		Case:             caseID,
		Variant:          variant,
		InputHash:        inputHash,
		Seed:             seedStr,
		Mode:             cfg.Mode,
		Workers:          cfg.Workers,
		Run:              runIdx,
		Block:            res.BlockNumber,
		TxCount:          len(blk.Txs),
		FailedTxs:        failedTxs,
		GasUsed:          res.GasUsed,
		BlockElapsedNs:   elapsedNs,
		AppHash:          res.AppHash,
		CheckpointPolicy: cfg.EffectiveCheckpointPolicy(),
		BudgetTier:       cfg.Checkpoint.BudgetTier,
		ModelSHA256:      cfg.Checkpoint.ModelSHA256,
	}

	if res.OCCStats != nil {
		s := res.OCCStats
		row.RetriedTxs = intPtr(s.RetriedTxs)
		row.TotalRetries = intPtr(s.TotalRetries)
		row.FallbackTxs = intPtr(s.FallbackTxs)
		row.WaitWorkerNs = int64Ptr(s.WaitWorkerNanos)
		row.WorkerIdleNs = int64Ptr(s.WorkerIdleNanos)
	} else if res.GroupedStats != nil {
		s := res.GroupedStats
		row.RetriedTxs = intPtr(s.RetriedTxs)
		row.TotalRetries = intPtr(s.TotalRetries)
		row.FallbackTxs = intPtr(s.FallbackTxs)
		row.GroupedTxs = intPtr(s.GroupedTxs)
		row.MaxGroupWidth = intPtr(s.MaxGroupWidth)
		row.UnknownSerialTxs = intPtr(s.UnknownTxs)
	} else if res.PredictedStats != nil {
		s := res.PredictedStats
		row.RetriedTxs = intPtr(s.RetriedTxs)
		row.TotalRetries = intPtr(s.TotalRetries)
		row.FallbackTxs = intPtr(s.FallbackTxs)
		hp := formatHotPoolsCSV(s.HotPools)
		row.HotPools = &hp
		row.WaitedTxs = intPtr(s.WaitedTxs)
		row.WaitPredecessorNs = int64Ptr(s.WaitPredecessorNanos)
		row.WaitWorkerNs = int64Ptr(s.WaitWorkerNanos)
		row.WorkerIdleNs = int64Ptr(s.WorkerIdleNanos)
	}
	if res.Phase2Metrics != nil {
		m := res.Phase2Metrics
		row.CheckpointPolicy = m.Policy
		row.CheckpointEvery = uint64Ptr(m.Interval)
		row.CheckpointBudgetBytes = uint64Ptr(m.BudgetBytes)
		row.CaptureRequested = uint64Ptr(m.CaptureRequested)
		row.CaptureCreated = uint64Ptr(m.CaptureCreated)
		row.CaptureSkippedBudget = uint64Ptr(m.CaptureSkippedBudget)
		row.CaptureSkippedLowScore = uint64Ptr(m.CaptureSkippedLowScore)
		row.AdaptiveCandidates = uint64Ptr(m.AdaptiveCandidates)
		row.AdaptiveCalibHits = uint64Ptr(m.AdaptiveCalibHits)
		row.AdaptiveCalibMisses = uint64Ptr(m.AdaptiveCalibMisses)
		row.AdaptiveHistoryMisses = uint64Ptr(m.AdaptiveHistoryMisses)
		row.ResumeAttempted = uint64Ptr(m.ResumeAttempted)
		row.ResumeCommitted = uint64Ptr(m.ResumeCommitted)
		row.ResumeCancelled = uint64Ptr(m.ResumeCancelled)
		row.RestoreRejectedPrefix = uint64Ptr(m.RestoreRejectedPrefix)
		row.AccountedPeakBytes = uint64Ptr(m.AccountedPeakBytes)
		row.CaptureNs = uint64Ptr(m.CaptureNs)
		row.SelectNs = uint64Ptr(m.SelectNs)
		row.RestoreNs = uint64Ptr(m.RestoreNs)
		row.ScoreNs = uint64Ptr(m.ScoreNs)
		row.HistoryUpdateNs = uint64Ptr(m.HistoryUpdateNs)
		row.HistoryMemoryBytes = uint64Ptr(m.HistoryMemoryBytes)
		row.OpcodeSteps = uint64Ptr(m.OpcodeSteps)
		row.PrefixStepsSaved = uint64Ptr(m.PrefixStepsSaved)
		fbStr := formatFallbackReasonsCSV(m.FallbackReasons)
		row.FallbackReasons = &fbStr
	}
	return row
}

func formatFallbackReasonsCSV(reasons map[string]uint64) string {
	if len(reasons) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, reasons[k]))
	}
	return strings.Join(parts, ";")
}

func uint64Ptr(v uint64) *uint64 { return &v }

func formatHotPoolsCSV(pools []common.Address) string {
	if len(pools) == 0 {
		return "none"
	}
	names := make([]string, len(pools))
	for i, p := range pools {
		lbl := p.Hex()
		for pi, def := range evm.DefaultSwapPoolAddrs {
			if p == def {
				lbl = fmt.Sprintf("pool%c", 'A'+pi)
				break
			}
		}
		names[i] = lbl
	}
	return strings.Join(names, ";")
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

func quantileSorted(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func medianAndIQR(vals []float64) (med, q1, q3, iqr float64) {
	if len(vals) == 0 {
		return 0, 0, 0, 0
	}
	cp := slices.Clone(vals)
	sort.Float64s(cp)
	q1 = quantileSorted(cp, 0.25)
	med = quantileSorted(cp, 0.50)
	q3 = quantileSorted(cp, 0.75)
	iqr = q3 - q1
	return med, q1, q3, iqr
}

func summarizeBenchRows(
	rows []BenchRow,
	cases []string,
	configs []BenchExecConfig,
	runs int,
) ([]BenchConfigSummary, []BenchBlockComparison) {
	type key struct {
		caseID     string
		mode       ExecutionMode
		workers    int
		cpPolicy   string
		budgetTier string
		run        int
	}
	type agg struct {
		totalNs         int64
		retried         int
		grouped         int
		waited          int
		waitPredNs      int64
		waitWorkNs      int64
		idleNs          int64
		captureCreated  uint64
		skippedBudget   uint64
		skippedLowScore uint64
		adaptiveCands   uint64
		calibHits       uint64
		calibMisses     uint64
		resumeAttempted uint64
		resumeCommitted uint64
		resumeCancelled uint64
		peakBytes       uint64
		opcodeSteps     uint64
		prefixSaved     uint64
		captureNs       uint64
		selectNs        uint64
		restoreNs       uint64
		scoreNs         uint64
		histUpdateNs    uint64
		histMemBytes    uint64
		blocks          int
		txs             int
	}
	byRun := make(map[key]*agg)
	for _, r := range rows {
		pol := r.CheckpointPolicy
		if pol == "" {
			pol = CheckpointOff
		}
		k := key{r.Case, r.Mode, r.Workers, pol, r.BudgetTier, r.Run}
		a := byRun[k]
		if a == nil {
			a = &agg{}
			byRun[k] = a
		}
		a.totalNs += r.BlockElapsedNs
		a.blocks++
		a.txs += r.TxCount
		if r.RetriedTxs != nil {
			a.retried += *r.RetriedTxs
		}
		if r.GroupedTxs != nil {
			a.grouped += *r.GroupedTxs
		}
		if r.WaitedTxs != nil {
			a.waited += *r.WaitedTxs
		}
		if r.WaitPredecessorNs != nil {
			a.waitPredNs += *r.WaitPredecessorNs
		}
		if r.WaitWorkerNs != nil {
			a.waitWorkNs += *r.WaitWorkerNs
		}
		if r.WorkerIdleNs != nil {
			a.idleNs += *r.WorkerIdleNs
		}
		if r.CaptureCreated != nil {
			a.captureCreated += *r.CaptureCreated
		}
		if r.CaptureSkippedBudget != nil {
			a.skippedBudget += *r.CaptureSkippedBudget
		}
		if r.CaptureSkippedLowScore != nil {
			a.skippedLowScore += *r.CaptureSkippedLowScore
		}
		if r.AdaptiveCandidates != nil {
			a.adaptiveCands += *r.AdaptiveCandidates
		}
		if r.AdaptiveCalibHits != nil {
			a.calibHits += *r.AdaptiveCalibHits
		}
		if r.AdaptiveCalibMisses != nil {
			a.calibMisses += *r.AdaptiveCalibMisses
		}
		if r.ResumeAttempted != nil {
			a.resumeAttempted += *r.ResumeAttempted
		}
		if r.ResumeCommitted != nil {
			a.resumeCommitted += *r.ResumeCommitted
		}
		if r.ResumeCancelled != nil {
			a.resumeCancelled += *r.ResumeCancelled
		}
		if r.AccountedPeakBytes != nil && *r.AccountedPeakBytes > a.peakBytes {
			a.peakBytes = *r.AccountedPeakBytes
		}
		if r.OpcodeSteps != nil {
			a.opcodeSteps += *r.OpcodeSteps
		}
		if r.PrefixStepsSaved != nil {
			a.prefixSaved += *r.PrefixStepsSaved
		}
		if r.CaptureNs != nil {
			a.captureNs += *r.CaptureNs
		}
		if r.SelectNs != nil {
			a.selectNs += *r.SelectNs
		}
		if r.RestoreNs != nil {
			a.restoreNs += *r.RestoreNs
		}
		if r.ScoreNs != nil {
			a.scoreNs += *r.ScoreNs
		}
		if r.HistoryUpdateNs != nil {
			a.histUpdateNs += *r.HistoryUpdateNs
		}
		if r.HistoryMemoryBytes != nil && *r.HistoryMemoryBytes > a.histMemBytes {
			a.histMemBytes = *r.HistoryMemoryBytes
		}
	}

	summaries := make([]BenchConfigSummary, 0, len(cases)*len(configs))
	findBaseline := func(cid string, mode ExecutionMode, workers int, tier string, r int) *agg {
		if a := byRun[key{cid, mode, workers, CheckpointOff, tier, r}]; a != nil {
			return a
		}
		return byRun[key{cid, mode, workers, CheckpointOff, "", r}]
	}
	for _, cid := range cases {
		for _, cfg := range configs {
			pol := cfg.EffectiveCheckpointPolicy()
			tier := cfg.Checkpoint.BudgetTier
			totals := make([]float64, 0, runs)
			retried := make([]float64, 0, runs)
			grouped := make([]float64, 0, runs)
			waited := make([]float64, 0, runs)
			waitPred := make([]float64, 0, runs)
			waitWork := make([]float64, 0, runs)
			idle := make([]float64, 0, runs)
			capCreated := make([]float64, 0, runs)
			skipBud := make([]float64, 0, runs)
			skipLow := make([]float64, 0, runs)
			adCands := make([]float64, 0, runs)
			calHits := make([]float64, 0, runs)
			calMiss := make([]float64, 0, runs)
			resAtt := make([]float64, 0, runs)
			resCommitted := make([]float64, 0, runs)
			resCanc := make([]float64, 0, runs)
			peakBytes := make([]float64, 0, runs)
			opSteps := make([]float64, 0, runs)
			prefSaved := make([]float64, 0, runs)
			capNs := make([]float64, 0, runs)
			selNs := make([]float64, 0, runs)
			rstNs := make([]float64, 0, runs)
			scrNs := make([]float64, 0, runs)
			hstNs := make([]float64, 0, runs)
			hstMem := make([]float64, 0, runs)
			deltasSeq := make([]float64, 0, runs)
			deltasOCC4 := make([]float64, 0, runs)
			deltasOff := make([]float64, 0, runs)
			var blkCount, txCount int

			for r := 1; r <= runs; r++ {
				a := byRun[key{cid, cfg.Mode, cfg.Workers, pol, tier, r}]
				if a == nil {
					continue
				}
				blkCount = a.blocks
				txCount = a.txs
				v := float64(a.totalNs)
				totals = append(totals, v)
				retried = append(retried, float64(a.retried))
				grouped = append(grouped, float64(a.grouped))
				waited = append(waited, float64(a.waited))
				waitPred = append(waitPred, float64(a.waitPredNs))
				waitWork = append(waitWork, float64(a.waitWorkNs))
				idle = append(idle, float64(a.idleNs))
				capCreated = append(capCreated, float64(a.captureCreated))
				skipBud = append(skipBud, float64(a.skippedBudget))
				skipLow = append(skipLow, float64(a.skippedLowScore))
				adCands = append(adCands, float64(a.adaptiveCands))
				calHits = append(calHits, float64(a.calibHits))
				calMiss = append(calMiss, float64(a.calibMisses))
				resAtt = append(resAtt, float64(a.resumeAttempted))
				resCommitted = append(resCommitted, float64(a.resumeCommitted))
				resCanc = append(resCanc, float64(a.resumeCancelled))
				peakBytes = append(peakBytes, float64(a.peakBytes))
				opSteps = append(opSteps, float64(a.opcodeSteps))
				prefSaved = append(prefSaved, float64(a.prefixSaved))
				capNs = append(capNs, float64(a.captureNs))
				selNs = append(selNs, float64(a.selectNs))
				rstNs = append(rstNs, float64(a.restoreNs))
				scrNs = append(scrNs, float64(a.scoreNs))
				hstNs = append(hstNs, float64(a.histUpdateNs))
				hstMem = append(hstMem, float64(a.histMemBytes))
				if seqA := findBaseline(cid, ModeSequential, 1, tier, r); seqA != nil {
					deltasSeq = append(deltasSeq, v-float64(seqA.totalNs))
				}
				if occ4A := findBaseline(cid, ModeOCC, 4, tier, r); occ4A != nil {
					deltasOCC4 = append(deltasOCC4, v-float64(occ4A.totalNs))
				}
				if offA := byRun[key{cid, cfg.Mode, cfg.Workers, CheckpointOff, tier, r}]; offA != nil {
					deltasOff = append(deltasOff, v-float64(offA.totalNs))
				}
			}
			if len(totals) == 0 {
				continue
			}
			hasSeq := len(deltasSeq) > 0
			hasOCC4 := len(deltasOCC4) > 0

			med, q1, q3, iqr := medianAndIQR(totals)
			medDSeq, _, _, iqrDSeq := medianAndIQR(deltasSeq)
			medRet, _, _, _ := medianAndIQR(retried)
			medGrp, _, _, _ := medianAndIQR(grouped)
			medWait, _, _, _ := medianAndIQR(waited)
			medWP, _, _, _ := medianAndIQR(waitPred)
			medWW, _, _, _ := medianAndIQR(waitWork)
			medIdle, _, _, _ := medianAndIQR(idle)
			medCap, _, _, _ := medianAndIQR(capCreated)
			medSkipBud, _, _, _ := medianAndIQR(skipBud)
			medSkipLow, _, _, _ := medianAndIQR(skipLow)
			medAdCands, _, _, _ := medianAndIQR(adCands)
			medCalHits, _, _, _ := medianAndIQR(calHits)
			medCalMiss, _, _, _ := medianAndIQR(calMiss)
			medResAtt, _, _, _ := medianAndIQR(resAtt)
			medRes, _, _, _ := medianAndIQR(resCommitted)
			medResCanc, _, _, _ := medianAndIQR(resCanc)
			medPeak, _, _, _ := medianAndIQR(peakBytes)
			medOps, _, _, _ := medianAndIQR(opSteps)
			medPref, _, _, _ := medianAndIQR(prefSaved)
			medCapNs, _, _, _ := medianAndIQR(capNs)
			medSelNs, _, _, _ := medianAndIQR(selNs)
			medRstNs, _, _, _ := medianAndIQR(rstNs)
			medScrNs, _, _, _ := medianAndIQR(scrNs)
			medHstNs, _, _, _ := medianAndIQR(hstNs)
			medHstMem, _, _, _ := medianAndIQR(hstMem)

			_, variant := SplitPhase3CaseVariant(cid)
			sum := BenchConfigSummary{
				Case:                  cid,
				Variant:               variant,
				Mode:                  string(cfg.Mode),
				CheckpointPolicy:      pol,
				BudgetTier:            tier,
				BudgetBytes:           cfg.Checkpoint.BudgetBytes,
				Workers:               cfg.Workers,
				Runs:                  len(totals),
				Blocks:                blkCount,
				TotalTxsPerRun:        txCount,
				MedianNs:              med,
				Q1Ns:                  q1,
				Q3Ns:                  q3,
				IQRNs:                 iqr,
				MedianDeltaVsSeqNs:    medDSeq,
				IQRDeltaVsSeqNs:       iqrDSeq,
				NoiseExceedsDiffVsSeq: hasSeq && cfg.Mode != ModeSequential && (iqr > math.Abs(medDSeq) || iqrDSeq > math.Abs(medDSeq)),
				MedianRetriedTxs:      medRet,
				MedianGroupedTxs:      medGrp,
				MedianWaitedTxs:       medWait,
				MedianWaitPredNs:      medWP,
				MedianWaitWorkerNs:    medWW,
				MedianWorkerIdleNs:    medIdle,
				MedianCaptureCreated:  medCap,
				MedianSkippedBudget:   medSkipBud,
				MedianSkippedLowScore: medSkipLow,
				MedianAdaptiveCands:   medAdCands,
				MedianCalibHits:       medCalHits,
				MedianCalibMisses:     medCalMiss,
				MedianResumeAttempted: medResAtt,
				MedianResumeCommitted: medRes,
				MedianResumeCancelled: medResCanc,
				MedianPeakBytes:       medPeak,
				MedianOpcodeSteps:     medOps,
				MedianPrefixSaved:     medPref,
				MedianCaptureNs:       medCapNs,
				MedianSelectNs:        medSelNs,
				MedianRestoreNs:       medRstNs,
				MedianScoreNs:         medScrNs,
				MedianHistoryUpdateNs: medHstNs,
				MedianHistoryMemBytes: medHstMem,
			}
			if hasOCC4 {
				medDOCC4, _, _, iqrDOCC4 := medianAndIQR(deltasOCC4)
				sum.MedianDeltaVsOCC4Ns = &medDOCC4
				sum.IQRDeltaVsOCC4Ns = &iqrDOCC4
			}
			if len(deltasOff) > 0 {
				medDOff, q1DOff, q3DOff, iqrDOff := medianAndIQR(deltasOff)
				sum.MedianDeltaVsOffNs = &medDOff
				sum.Q1DeltaVsOffNs = &q1DOff
				sum.Q3DeltaVsOffNs = &q3DOff
				sum.IQRDeltaVsOffNs = &iqrDOff
				sum.UnstableVsOff = pol != CheckpointOff && q1DOff <= 0 && q3DOff >= 0
			}
			summaries = append(summaries, sum)
		}
	}

	comparisons := make([]BenchBlockComparison, 0, 6)
	for _, cid := range cases {
		if !strings.HasPrefix(cid, "swap-pools") {
			continue
		}
		for _, blkNum := range []uint64{7, 8} {
			occRows := make(map[int]BenchRow)
			predRows := make(map[int]BenchRow)
			for _, r := range rows {
				if r.Case != cid || r.Block != blkNum || r.Workers != 4 {
					continue
				}
				if r.Mode == ModeOCC {
					occRows[r.Run] = r
				} else if r.Mode == ModePredictedOCC {
					predRows[r.Run] = r
				}
			}
			if len(occRows) == 0 || len(predRows) == 0 {
				continue
			}
			occT, predT, deltaT := make([]float64, 0, runs), make([]float64, 0, runs), make([]float64, 0, runs)
			occR, predR, predW, predWN, predIN := make([]float64, 0, runs), make([]float64, 0, runs), make([]float64, 0, runs), make([]float64, 0, runs), make([]float64, 0, runs)
			hotStr := ""
			for run := 1; run <= runs; run++ {
				or, ok1 := occRows[run]
				pr, ok2 := predRows[run]
				if !ok1 || !ok2 {
					continue
				}
				occT = append(occT, float64(or.BlockElapsedNs))
				predT = append(predT, float64(pr.BlockElapsedNs))
				deltaT = append(deltaT, float64(pr.BlockElapsedNs-or.BlockElapsedNs))
				if or.RetriedTxs != nil {
					occR = append(occR, float64(*or.RetriedTxs))
				}
				if pr.RetriedTxs != nil {
					predR = append(predR, float64(*pr.RetriedTxs))
				}
				if pr.WaitedTxs != nil {
					predW = append(predW, float64(*pr.WaitedTxs))
				}
				if pr.WaitPredecessorNs != nil {
					predWN = append(predWN, float64(*pr.WaitPredecessorNs))
				}
				if pr.WorkerIdleNs != nil {
					predIN = append(predIN, float64(*pr.WorkerIdleNs))
				}
				if pr.HotPools != nil {
					hotStr = *pr.HotPools
				}
			}
			oMed, _, _, oIQR := medianAndIQR(occT)
			pMed, _, _, pIQR := medianAndIQR(predT)
			dMed, _, _, dIQR := medianAndIQR(deltaT)
			orMed, _, _, _ := medianAndIQR(occR)
			prMed, _, _, _ := medianAndIQR(predR)
			pwMed, _, _, _ := medianAndIQR(predW)
			pwnMed, _, _, _ := medianAndIQR(predWN)
			pinMed, _, _, _ := medianAndIQR(predIN)
			comparisons = append(comparisons, BenchBlockComparison{
				Case:                cid,
				Block:               blkNum,
				OCC4MedianNs:        oMed,
				OCC4IQRNs:           oIQR,
				Pred4MedianNs:       pMed,
				Pred4IQRNs:          pIQR,
				MedianDeltaPredOCC4: dMed,
				IQRDeltaPredOCC4:    dIQR,
				OCC4MedianRetries:   orMed,
				Pred4MedianRetries:  prMed,
				Pred4MedianWaited:   pwMed,
				Pred4MedianWaitNs:   pwnMed,
				Pred4MedianIdleNs:   pinMed,
				Pred4HotPools:       hotStr,
			})
		}
	}

	return summaries, comparisons
}

var benchCSVHeader = []string{
	"case",
	"input_hash",
	"seed",
	"mode",
	"workers",
	"run",
	"block",
	"tx_count",
	"failed_txs",
	"gas_used",
	"block_elapsed_ns",
	"retried_txs",
	"total_retries",
	"fallback_txs",
	"grouped_txs",
	"max_group_width",
	"unknown_serial_txs",
	"hot_pools",
	"waited_txs",
	"wait_predecessor_ns",
	"wait_worker_ns",
	"worker_idle_ns",
	"app_hash",
}

var benchPhase2ExtraHeader = []string{
	"checkpoint_policy",
	"checkpoint_every",
	"checkpoint_budget_bytes",
	"capture_requested",
	"capture_created",
	"capture_skipped_budget",
	"resume_attempted",
	"resume_committed",
	"resume_cancelled",
	"restore_rejected_prefix",
	"accounted_peak_bytes",
	"capture_ns",
	"select_ns",
	"restore_ns",
	"opcode_steps",
	"prefix_steps_saved",
	"fallback_reasons",
}

var benchPhase3ExtraHeader = []string{
	"variant",
	"budget_tier",
	"model_sha256",
	"capture_skipped_low_score",
	"adaptive_candidates",
	"adaptive_calib_hits",
	"adaptive_calib_misses",
	"adaptive_history_misses",
	"score_ns",
	"history_update_ns",
	"history_memory_bytes",
}

func formatOptInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func formatOptInt64(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func formatOptUint64(p *uint64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatUint(*p, 10)
}

func formatOptStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func writeBenchCSVFile(path string, rows []BenchRow, includePhase2, includePhase3 bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	header := append([]string(nil), benchCSVHeader...)
	if includePhase2 || includePhase3 {
		header = append(header, benchPhase2ExtraHeader...)
	}
	if includePhase3 {
		header = append(header, benchPhase3ExtraHeader...)
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{
			r.Case,
			r.InputHash,
			r.Seed,
			string(r.Mode),
			strconv.Itoa(r.Workers),
			strconv.Itoa(r.Run),
			strconv.FormatUint(r.Block, 10),
			strconv.Itoa(r.TxCount),
			strconv.Itoa(r.FailedTxs),
			strconv.FormatUint(r.GasUsed, 10),
			strconv.FormatInt(r.BlockElapsedNs, 10),
			formatOptInt(r.RetriedTxs),
			formatOptInt(r.TotalRetries),
			formatOptInt(r.FallbackTxs),
			formatOptInt(r.GroupedTxs),
			formatOptInt(r.MaxGroupWidth),
			formatOptInt(r.UnknownSerialTxs),
			formatOptStr(r.HotPools),
			formatOptInt(r.WaitedTxs),
			formatOptInt64(r.WaitPredecessorNs),
			formatOptInt64(r.WaitWorkerNs),
			formatOptInt64(r.WorkerIdleNs),
			r.AppHash.Hex(),
		}
		if includePhase2 || includePhase3 {
			rec = append(rec,
				r.CheckpointPolicy,
				formatOptUint64(r.CheckpointEvery),
				formatOptUint64(r.CheckpointBudgetBytes),
				formatOptUint64(r.CaptureRequested),
				formatOptUint64(r.CaptureCreated),
				formatOptUint64(r.CaptureSkippedBudget),
				formatOptUint64(r.ResumeAttempted),
				formatOptUint64(r.ResumeCommitted),
				formatOptUint64(r.ResumeCancelled),
				formatOptUint64(r.RestoreRejectedPrefix),
				formatOptUint64(r.AccountedPeakBytes),
				formatOptUint64(r.CaptureNs),
				formatOptUint64(r.SelectNs),
				formatOptUint64(r.RestoreNs),
				formatOptUint64(r.OpcodeSteps),
				formatOptUint64(r.PrefixStepsSaved),
				formatOptStr(r.FallbackReasons),
			)
		}
		if includePhase3 {
			rec = append(rec,
				r.Variant,
				r.BudgetTier,
				r.ModelSHA256,
				formatOptUint64(r.CaptureSkippedLowScore),
				formatOptUint64(r.AdaptiveCandidates),
				formatOptUint64(r.AdaptiveCalibHits),
				formatOptUint64(r.AdaptiveCalibMisses),
				formatOptUint64(r.AdaptiveHistoryMisses),
				formatOptUint64(r.ScoreNs),
				formatOptUint64(r.HistoryUpdateNs),
				formatOptUint64(r.HistoryMemoryBytes),
			)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writeBenchSummaryCSV(path string, summaries []BenchConfigSummary, includePhase2, includePhase3 bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	header := []string{
		"case", "mode", "workers", "runs", "blocks", "total_txs_per_run",
		"median_ms", "q1_ms", "q3_ms", "iqr_ms",
		"median_delta_vs_seq_ms", "iqr_delta_vs_seq_ms",
		"median_delta_vs_occ4_ms", "iqr_delta_vs_occ4_ms",
		"noise_exceeds_diff_vs_seq",
		"median_retried_txs", "median_grouped_txs", "median_waited_txs",
		"median_wait_pred_ms", "median_wait_worker_ms", "median_worker_idle_ms",
	}
	if includePhase2 || includePhase3 {
		header = append(header,
			"checkpoint_policy",
			"median_capture_created",
			"median_resume_committed",
			"median_accounted_peak_bytes",
			"median_opcode_steps",
			"median_prefix_steps_saved",
		)
	}
	if includePhase3 {
		header = append(header,
			"variant",
			"budget_tier",
			"budget_bytes",
			"median_delta_vs_off_ms",
			"q1_delta_vs_off_ms",
			"q3_delta_vs_off_ms",
			"iqr_delta_vs_off_ms",
			"unstable_vs_off",
			"median_skipped_budget",
			"median_skipped_low_score",
			"median_adaptive_candidates",
			"median_adaptive_calib_hits",
			"median_adaptive_calib_misses",
			"median_resume_attempted",
			"median_resume_cancelled",
			"median_capture_ms",
			"median_select_ms",
			"median_restore_ms",
			"median_score_ms",
			"median_history_update_ms",
			"median_history_memory_bytes",
		)
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, s := range summaries {
		dOcc, iqrOcc := "", ""
		if s.MedianDeltaVsOCC4Ns != nil {
			dOcc = fmt.Sprintf("%.4f", *s.MedianDeltaVsOCC4Ns/1e6)
		}
		if s.IQRDeltaVsOCC4Ns != nil {
			iqrOcc = fmt.Sprintf("%.4f", *s.IQRDeltaVsOCC4Ns/1e6)
		}
		retStr, grpStr, waitedStr, wpStr, wwStr, idleStr := "", "", "", "", "", ""
		if s.Mode != string(ModeSequential) {
			retStr = fmt.Sprintf("%.1f", s.MedianRetriedTxs)
		}
		if s.Mode == string(ModeGrouped) {
			grpStr = fmt.Sprintf("%.1f", s.MedianGroupedTxs)
		}
		if s.Mode == string(ModePredictedOCC) {
			waitedStr = fmt.Sprintf("%.1f", s.MedianWaitedTxs)
			wpStr = fmt.Sprintf("%.4f", s.MedianWaitPredNs/1e6)
		}
		if s.Mode == string(ModeOCC) || s.Mode == string(ModePredictedOCC) {
			wwStr = fmt.Sprintf("%.4f", s.MedianWaitWorkerNs/1e6)
			idleStr = fmt.Sprintf("%.4f", s.MedianWorkerIdleNs/1e6)
		}
		rec := []string{
			s.Case,
			s.Mode,
			strconv.Itoa(s.Workers),
			strconv.Itoa(s.Runs),
			strconv.Itoa(s.Blocks),
			strconv.Itoa(s.TotalTxsPerRun),
			fmt.Sprintf("%.4f", s.MedianNs/1e6),
			fmt.Sprintf("%.4f", s.Q1Ns/1e6),
			fmt.Sprintf("%.4f", s.Q3Ns/1e6),
			fmt.Sprintf("%.4f", s.IQRNs/1e6),
			fmt.Sprintf("%.4f", s.MedianDeltaVsSeqNs/1e6),
			fmt.Sprintf("%.4f", s.IQRDeltaVsSeqNs/1e6),
			dOcc,
			iqrOcc,
			strconv.FormatBool(s.NoiseExceedsDiffVsSeq),
			retStr,
			grpStr,
			waitedStr,
			wpStr,
			wwStr,
			idleStr,
		}
		if includePhase2 || includePhase3 {
			rec = append(rec,
				s.CheckpointPolicy,
				fmt.Sprintf("%.1f", s.MedianCaptureCreated),
				fmt.Sprintf("%.1f", s.MedianResumeCommitted),
				fmt.Sprintf("%.0f", s.MedianPeakBytes),
				fmt.Sprintf("%.0f", s.MedianOpcodeSteps),
				fmt.Sprintf("%.0f", s.MedianPrefixSaved),
			)
		}
		if includePhase3 {
			dOff, q1Off, q3Off, iqrOff := "", "", "", ""
			if s.MedianDeltaVsOffNs != nil {
				dOff = fmt.Sprintf("%.4f", *s.MedianDeltaVsOffNs/1e6)
			}
			if s.Q1DeltaVsOffNs != nil {
				q1Off = fmt.Sprintf("%.4f", *s.Q1DeltaVsOffNs/1e6)
			}
			if s.Q3DeltaVsOffNs != nil {
				q3Off = fmt.Sprintf("%.4f", *s.Q3DeltaVsOffNs/1e6)
			}
			if s.IQRDeltaVsOffNs != nil {
				iqrOff = fmt.Sprintf("%.4f", *s.IQRDeltaVsOffNs/1e6)
			}
			rec = append(rec,
				s.Variant,
				s.BudgetTier,
				strconv.FormatUint(s.BudgetBytes, 10),
				dOff,
				q1Off,
				q3Off,
				iqrOff,
				strconv.FormatBool(s.UnstableVsOff),
				fmt.Sprintf("%.1f", s.MedianSkippedBudget),
				fmt.Sprintf("%.1f", s.MedianSkippedLowScore),
				fmt.Sprintf("%.1f", s.MedianAdaptiveCands),
				fmt.Sprintf("%.1f", s.MedianCalibHits),
				fmt.Sprintf("%.1f", s.MedianCalibMisses),
				fmt.Sprintf("%.1f", s.MedianResumeAttempted),
				fmt.Sprintf("%.1f", s.MedianResumeCancelled),
				fmt.Sprintf("%.4f", s.MedianCaptureNs/1e6),
				fmt.Sprintf("%.4f", s.MedianSelectNs/1e6),
				fmt.Sprintf("%.4f", s.MedianRestoreNs/1e6),
				fmt.Sprintf("%.4f", s.MedianScoreNs/1e6),
				fmt.Sprintf("%.4f", s.MedianHistoryUpdateNs/1e6),
				fmt.Sprintf("%.0f", s.MedianHistoryMemBytes),
			)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writeBenchArtifacts(outDir string, rep *BenchReport) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	includePhase3 := rep.Suite == SuitePhase3 || rep.Suite == SuitePhase4
	includePhase2 := rep.Suite == SuitePhase2 || includePhase3
	if !includePhase2 {
		for _, r := range rep.Rows {
			if r.CaptureRequested != nil || (r.CheckpointPolicy != "" && r.CheckpointPolicy != CheckpointOff) {
				includePhase2 = true
				break
			}
		}
	}
	if err := writeBenchCSVFile(filepath.Join(outDir, "raw.csv"), rep.Rows, includePhase2, includePhase3); err != nil {
		return err
	}
	var swapRows []BenchRow
	for _, r := range rep.Rows {
		if r.Case == "swap-pools" {
			swapRows = append(swapRows, r)
		}
	}
	if len(swapRows) > 0 {
		if err := writeBenchCSVFile(filepath.Join(outDir, "swap-pools.csv"), swapRows, includePhase2, includePhase3); err != nil {
			return err
		}
	}
	if err := writeBenchSummaryCSV(filepath.Join(outDir, "summary.csv"), rep.Summaries, includePhase2, includePhase3); err != nil {
		return err
	}
	if len(rep.DecisionTrace) > 0 {
		tracePath := rep.Metadata.DecisionTracePath
		if tracePath == "" {
			tracePath = filepath.Join(outDir, "decision-trace.jsonl")
		}
		if err := WriteDecisionTraceJSONL(tracePath, rep.DecisionTrace); err != nil {
			return err
		}
	}
	metaBytes, err := json.MarshalIndent(rep.Metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "metadata.json"), append(metaBytes, '\n'), 0o644); err != nil {
		return err
	}
	if rep.Suite == SuitePhase4 {
		sel, selErr := BuildPhase4Selection(rep)
		if selErr != nil {
			return selErr
		}
		rep.Phase4Selection = sel
		if err := writePhase4SelectionJSON(filepath.Join(outDir, "selection.json"), sel); err != nil {
			return err
		}
		return writePhase4SummaryMD(filepath.Join(outDir, "summary.md"), rep, sel)
	}
	if includePhase3 {
		return writePhase3SummaryMD(filepath.Join(outDir, "summary.md"), rep)
	}
	if rep.Suite == SuitePhase2 {
		return writePhase2SummaryMD(filepath.Join(outDir, "summary.md"), rep)
	}

	var md strings.Builder
	md.WriteString("# Pluto Phase 1 Benchmark Summary (project-pluto)\n\n")
	fmt.Fprintf(&md, "- OS / Arch: `%s/%s`, CPU: `%s` (`numCPU=%d`), locked `GOMAXPROCS=%d`\n",
		rep.Metadata.GOOS, rep.Metadata.GOARCH, rep.Metadata.CPUModel, rep.Metadata.NumCPU, rep.Metadata.GOMAXPROCS)
	fmt.Fprintf(&md, "- Go: `%s`, Geth reference: `%s`, `evmBin=%s`\n",
		rep.Metadata.GoVersion, rep.Metadata.EVMBinVersion, rep.Metadata.EVMBin)
	fmt.Fprintf(&md, "- Binary SHA-256: `%s`\n", rep.Metadata.BinarySHA256)
	fmt.Fprintf(&md, "- Runs per config: `%d`, rotated config order per run\n\n", rep.Metadata.Runs)

	md.WriteString("## 1. Tổng hợp trung vị và IQR theo tải (tổng thời gian mọi block trong 1 lượt)\n\n")
	md.WriteString("| Case | Mode/W | Median (ms) | IQR (ms) | Delta vs seq/1 (ms) | IQR delta (ms) | Noise > abs(Diff) | Retried | Grouped | Waited | WaitPred (ms) | WaitWorker (ms) | WorkerIdle (ms) |\n")
	md.WriteString("| --- | --- | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, s := range rep.Summaries {
		noiseStr := "no"
		if s.NoiseExceedsDiffVsSeq {
			noiseStr = "yes"
		}
		retStr, grpStr, waitStr, wpStr, wwStr, idleStr := "-", "-", "-", "-", "-", "-"
		if s.Mode != string(ModeSequential) {
			retStr = fmt.Sprintf("%.1f", s.MedianRetriedTxs)
		}
		if s.Mode == string(ModeGrouped) {
			grpStr = fmt.Sprintf("%.1f", s.MedianGroupedTxs)
		}
		if s.Mode == string(ModePredictedOCC) {
			waitStr = fmt.Sprintf("%.1f", s.MedianWaitedTxs)
			wpStr = fmt.Sprintf("%.3f", s.MedianWaitPredNs/1e6)
		}
		if s.Mode == string(ModeOCC) || s.Mode == string(ModePredictedOCC) {
			wwStr = fmt.Sprintf("%.4f", s.MedianWaitWorkerNs/1e6)
			idleStr = fmt.Sprintf("%.3f", s.MedianWorkerIdleNs/1e6)
		}
		fmt.Fprintf(&md, "| %s | %s/%d | %.3f | %.3f | %+.3f | %.3f | %s | %s | %s | %s | %s | %s | %s |\n",
			s.Case, s.Mode, s.Workers, s.MedianNs/1e6, s.IQRNs/1e6,
			s.MedianDeltaVsSeqNs/1e6, s.IQRDeltaVsSeqNs/1e6, noiseStr,
			retStr, grpStr, waitStr, wpStr, wwStr, idleStr)
	}

	if len(rep.Block7Vs8) > 0 {
		md.WriteString("\n## 2. Đối chiếu trực tiếp predicted-occ/4 vs occ/4 tại Block 7 và Block 8\n\n")
		md.WriteString("| Case | Block | HotPools (pred/4) | occ/4 Median (ms) | pred/4 Median (ms) | Paired Delta (ms) | Delta IQR (ms) | occ/4 Retries | pred/4 Retries | pred/4 Waited | WaitPred (ms) | WorkerIdle (ms) |\n")
		md.WriteString("| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
		for _, bc := range rep.Block7Vs8 {
			fmt.Fprintf(&md, "| %s | %d | %s | %.3f | %.3f | %+.3f | %.3f | %.1f | %.1f | %.1f | %.3f | %.3f |\n",
				bc.Case, bc.Block, bc.Pred4HotPools, bc.OCC4MedianNs/1e6, bc.Pred4MedianNs/1e6,
				bc.MedianDeltaPredOCC4/1e6, bc.IQRDeltaPredOCC4/1e6,
				bc.OCC4MedianRetries, bc.Pred4MedianRetries, bc.Pred4MedianWaited,
				bc.Pred4MedianWaitNs/1e6, bc.Pred4MedianIdleNs/1e6)
		}
	}

	md.WriteString("\n## 3. Quy ước cột trống và phạm vi đo\n\n")
	md.WriteString("- `block_elapsed_ns`: đo trực tiếp quanh `internal/evm` `Executor.ExecuteBlock` (lập lịch, thực thi, retry, `ComputeAppHash`, `PendingWrites` và cập nhật lịch sử dự đoán); tắt trace & test hooks. Bước ghi đĩa `PebbleDB` (`Commit`), `ExportLogicalState`, `evm t8n` và ghi CSV nằm ngoài khoảng đo.\n")
	md.WriteString("- `wait_worker_ns`: bằng `0` ở `occ`, đo thời gian đợi trong `readyQueue` ở `predicted-occ`, để trống ở `sequential` và `grouped`.\n")
	md.WriteString("- `worker_idle_ns`: tích phân `(workers - activeWorkers) * dt` ở `occ` và `predicted-occ`; để trống ở `sequential` và `grouped`.\n\n")

	md.WriteString("## 4. Lệnh tái lập\n\n```bash\n")
	md.WriteString("EVM_BIN=/tmp/plutof-evm-v1168/evm GOWORK=off go run ./cmd/pluto-lab verify --case all --mode all --workers 4\n")
	md.WriteString("EVM_BIN=/tmp/plutof-evm-v1168/evm GOWORK=off go run ./cmd/pluto-lab bench --case all --mode all --runs 10 --out results/phase1\n")
	md.WriteString("```\n\n")

	md.WriteString("## 5. Kết luận ngắn theo số liệu\n\n")
	writeBenchConclusions(&md, rep)
	return os.WriteFile(filepath.Join(outDir, "summary.md"), []byte(md.String()), 0o644)
}

func writeBenchConclusions(md *strings.Builder, rep *BenchReport) {
	byCase := make(map[string]map[string]BenchConfigSummary)
	var caseOrder []string
	var fasterThanSeq, slowerThanSeq, noiseCases, highRetry []string

	for _, s := range rep.Summaries {
		m := byCase[s.Case]
		if m == nil {
			m = make(map[string]BenchConfigSummary)
			byCase[s.Case] = m
			caseOrder = append(caseOrder, s.Case)
		}
		lbl := fmt.Sprintf("%s/%d", s.Mode, s.Workers)
		m[lbl] = s

		if s.Mode != string(ModeSequential) && s.Workers == 4 {
			if s.MedianDeltaVsSeqNs < 0 {
				fasterThanSeq = append(fasterThanSeq, fmt.Sprintf("`%s` (`%s`: %.3f ms vs seq %.3f ms, delta %+.3f ms)",
					s.Case, lbl, s.MedianNs/1e6, (s.MedianNs-s.MedianDeltaVsSeqNs)/1e6, s.MedianDeltaVsSeqNs/1e6))
			} else {
				slowerThanSeq = append(slowerThanSeq, fmt.Sprintf("`%s` (`%s`: %.3f ms, delta %+.3f ms)",
					s.Case, lbl, s.MedianNs/1e6, s.MedianDeltaVsSeqNs/1e6))
			}
		}
		if s.NoiseExceedsDiffVsSeq {
			noiseCases = append(noiseCases, fmt.Sprintf("`%s` `%s` (IQR=%.3f ms, IQR delta=%.3f ms, |delta|=%.3f ms)",
				s.Case, lbl, s.IQRNs/1e6, s.IQRDeltaVsSeqNs/1e6, math.Abs(s.MedianDeltaVsSeqNs)/1e6))
		}
	}

	for _, cid := range caseOrder {
		m := byCase[cid]
		occ4, hasOCC := m["occ/4"]
		if !hasOCC || occ4.MedianRetriedTxs <= 0 {
			continue
		}
		if ps, ok := m["predicted-occ/4"]; ok {
			highRetry = append(highRetry, fmt.Sprintf("`%s` (`occ/4`: %.1f/%d tx retry, `predicted-occ/4`: %.1f/%d tx retry)",
				cid, occ4.MedianRetriedTxs, occ4.TotalTxsPerRun, ps.MedianRetriedTxs, ps.TotalTxsPerRun))
		} else {
			highRetry = append(highRetry, fmt.Sprintf("`%s` (`occ/4`: %.1f/%d tx retry)",
				cid, occ4.MedianRetriedTxs, occ4.TotalTxsPerRun))
		}
	}

	if len(fasterThanSeq) > 0 {
		fmt.Fprintf(md, "- **Tải giảm thời gian ở 4 worker so với `sequential/1`**: %s.\n", strings.Join(fasterThanSeq, "; "))
	} else {
		md.WriteString("- **Tải giảm thời gian ở 4 worker so với `sequential/1`**: không có cấu hình 4 worker nào nhanh hơn `sequential/1` trên các tải nhẹ hiện tại.\n")
	}
	if len(slowerThanSeq) > 0 {
		fmt.Fprintf(md, "- **Tải tăng thời gian ở 4 worker so với `sequential/1`**: %s.\n", strings.Join(slowerThanSeq, "; "))
	}
	if len(rep.Block7Vs8) > 0 {
		var b78Notes []string
		for _, bc := range rep.Block7Vs8 {
			b78Notes = append(b78Notes, fmt.Sprintf("`%s` B%d (hot=`%s`): `pred/4 - occ/4 = %+.3f ms`, retries %.1f -> %.1f, waited=%.1f",
				bc.Case, bc.Block, bc.Pred4HotPools, bc.MedianDeltaPredOCC4/1e6,
				bc.OCC4MedianRetries, bc.Pred4MedianRetries, bc.Pred4MedianWaited))
		}
		fmt.Fprintf(md, "- **Đối chiếu Block 7 vs Block 8 (`predicted-occ/4` vs `occ/4`)**: %s.\n", strings.Join(b78Notes, "; "))
	}
	if len(noiseCases) > 0 {
		fmt.Fprintf(md, "- **Ca có dao động lớn hơn chênh lệch (`Noise > abs(Diff)`)**: %s.\n", strings.Join(noiseCases, "; "))
	} else {
		md.WriteString("- **Ca có dao động lớn hơn chênh lệch (`Noise > abs(Diff)`)**: không có.\n")
	}
	if len(highRetry) > 0 {
		fmt.Fprintf(md, "- **Tải còn nhiều việc phải chạy lại để khảo sát checkpoint ở Phase 2**: %s.\n", strings.Join(highRetry, "; "))
	}
}

func writePhase2SummaryMD(path string, rep *BenchReport) error {
	var md strings.Builder
	md.WriteString("# Pluto Phase 2 Checkpoint Benchmark Summary (project-pluto)\n\n")
	fmt.Fprintf(&md, "- OS / Arch: `%s/%s`, CPU: `%s` (`numCPU=%d`), locked `GOMAXPROCS=%d`\n",
		rep.Metadata.GOOS, rep.Metadata.GOARCH, rep.Metadata.CPUModel, rep.Metadata.NumCPU, rep.Metadata.GOMAXPROCS)
	fmt.Fprintf(&md, "- Go: `%s`, Geth reference: `%s`, `evmBin=%s`\n",
		rep.Metadata.GoVersion, rep.Metadata.EVMBinVersion, rep.Metadata.EVMBin)
	fmt.Fprintf(&md, "- Runs per config: `%d`, rotated config order per run\n\n", rep.Metadata.Runs)

	md.WriteString("## Tổng hợp Phase 2 (so sánh `off` / `fixed` / `history` dưới ngân sách RAM cứng)\n\n")
	md.WriteString("| Case | Mode/W | Policy | Budget (B) | Median (ms) | IQR (ms) | Delta vs off (ms) | Retried | Captured | Resumed | Peak (B) | Opcode Steps | Prefix Saved |\n")
	md.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, s := range rep.Summaries {
		deltaOff := "-"
		if s.MedianDeltaVsOffNs != nil {
			deltaOff = fmt.Sprintf("%+.3f", *s.MedianDeltaVsOffNs/1e6)
		}
		fmt.Fprintf(&md, "| %s | %s/%d | %s | %d | %.3f | %.3f | %s | %.1f | %.1f | %.1f | %.0f | %.0f | %.0f |\n",
			s.Case, s.Mode, s.Workers, s.CheckpointPolicy, s.BudgetBytes,
			s.MedianNs/1e6, s.IQRNs/1e6, deltaOff,
			s.MedianRetriedTxs, s.MedianCaptureCreated, s.MedianResumeCommitted,
			s.MedianPeakBytes, s.MedianOpcodeSteps, s.MedianPrefixSaved)
	}
	return os.WriteFile(path, []byte(md.String()), 0o644)
}

func resolveSelfBinaryInfo() (string, string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return exe, ""
	}
	sum := sha256.Sum256(data)
	return exe, "0x" + hex.EncodeToString(sum[:])
}

func readCPUModelLinux() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if idx := strings.IndexByte(line, ':'); idx >= 0 {
				return strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return ""
}
