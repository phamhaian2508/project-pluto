package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const (
	DefaultPhase3CalibrateSeed uint64 = 31
	DefaultPhase3EvalSeed      uint64 = 73
	DefaultPhase3TargetCase           = "merkle-mixed"
)

// CalibrateOptions cấu hình lệnh `pluto-lab calibrate --suite phase3`.
type CalibrateOptions struct {
	Suite       string
	Seed        uint64
	TestdataDir string
	OutPath     string
	Runs        int
	GOMAXPROCS  int
}

type probeChainOutcome struct {
	totalNs          int64
	peakAllBytes     uint64
	peakSteadyBytes  uint64
	createdAll       uint64
	resumedAll       uint64
	skippedBudAll    uint64
	skippedBudSteady uint64
	skippedLowAll    uint64
	decisionEvents   []evm.AdaptiveDecisionEvent
}

func runSinglePhase3Probe(
	ctx context.Context,
	chainCase *ChainCase,
	workers int,
	cpOpts CheckpointOptions,
	recordTrace bool,
) (*probeChainOutcome, error) {
	dbDir, err := os.MkdirTemp("", "pluto-lab-cal-probe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dbDir)

	db, err := store.NewPebbleDB("cal-probe", dbDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	runner, err := NewChainRunnerWithCheckpoints(db, ModeOCC, workers, recordTrace, cpOpts, chainCase.GenesisAlloc)
	if err != nil {
		return nil, err
	}

	out := &probeChainOutcome{}
	for bIdx, blk := range chainCase.Blocks {
		res, elapsedNs, execErr := runner.ExecuteBlock(ctx, blk.Env, blk.Txs)
		if execErr != nil {
			return nil, fmt.Errorf("probe block %d: %w", blk.Env.Number, execErr)
		}
		out.totalNs += elapsedNs
		if res.Phase2Metrics != nil {
			m := res.Phase2Metrics
			if m.AccountedPeakBytes > out.peakAllBytes {
				out.peakAllBytes = m.AccountedPeakBytes
			}
			if bIdx > 0 && m.AccountedPeakBytes > out.peakSteadyBytes {
				out.peakSteadyBytes = m.AccountedPeakBytes
			}
			out.createdAll += m.CaptureCreated
			out.resumedAll += m.ResumeCommitted
			out.skippedBudAll += m.CaptureSkippedBudget
			if bIdx > 0 {
				out.skippedBudSteady += m.CaptureSkippedBudget
			}
			out.skippedLowAll += m.CaptureSkippedLowScore
		}
		if recordTrace && len(res.DecisionTrace) > 0 {
			out.decisionEvents = append(out.decisionEvents, res.DecisionTrace...)
		}
	}
	return out, nil
}

func runPairedCalibrationOnChain(ctx context.Context, chainCase *ChainCase) ([]evm.AdaptiveCalibrationEntry, uint64, error) {
	dbDir, err := os.MkdirTemp("", "pluto-lab-cal-pairs-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dbDir)

	db, err := store.NewPebbleDB("cal-pairs", dbDir)
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()

	seedDB := evm.NewPebbleStateDB(db)
	if err := seedDB.SeedLogicalState(chainCase.GenesisAlloc); err != nil {
		return nil, 0, fmt.Errorf("seed calibration genesis: %w", err)
	}
	if err := seedDB.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit calibration genesis: %w", err)
	}
	prevAppHash := common.BytesToHash(seedDB.ComputeAppHash())
	blockHashes := map[uint64]common.Hash{0: prevAppHash}
	chainCfg := ShanghaiChainConfig(DefaultChainID)

	type groupBucket struct {
		key       evm.AdaptiveCalibrationKey
		gains     []float64
		captures  []float64
		retries   []float64
		resumes   []float64
	}
	groups := make(map[evm.AdaptiveCalibrationKey]*groupBucket)
	var maxSingleBytes uint64

	for _, blk := range chainCase.Blocks {
		rawTxs := make([]*types.Transaction, len(blk.Txs))
		for i, stx := range blk.Txs {
			rawTxs[i] = stx.Tx
		}
		stateDB := evm.NewPebbleStateDB(db)
		env := evm.BlockEnv{
			Height:       int64(blk.Env.Number),
			Time:         blk.Env.Timestamp,
			Coinbase:     blk.Env.Coinbase,
			GasLimit:     blk.Env.GasLimit,
			Random:       blk.Env.Random,
			BaseFee:      new(big.Int).Set(blk.Env.BaseFee),
			PreviousRoot: prevAppHash.Bytes(),
			GetHash: func(n uint64) common.Hash {
				if n >= blk.Env.Number || blk.Env.Number-n > 256 {
					return common.Hash{}
				}
				if blk.Env.BlockHashes != nil {
					if h, ok := blk.Env.BlockHashes[n]; ok {
						return h
					}
				}
				return blockHashes[n]
			},
		}
		samples, err := evm.CalibrateBlockFirstStatePairs(ctx, stateDB, chainCfg, env, rawTxs)
		if err != nil {
			return nil, 0, fmt.Errorf("calibrate paired block %d: %w", blk.Env.Number, err)
		}
		if err := stateDB.Commit(); err != nil {
			return nil, 0, fmt.Errorf("commit calibrated block %d: %w", blk.Env.Number, err)
		}
		prevAppHash = common.BytesToHash(stateDB.ComputeAppHash(prevAppHash.Bytes()))
		blockHashes[blk.Env.Number] = prevAppHash

		for _, s := range samples {
			if s.AccountedBytes > maxSingleBytes {
				maxSingleBytes = s.AccountedBytes
			}
			k := evm.AdaptiveCalibrationKey{
				CodeHash:   s.CodeHash,
				Selector:   s.Selector,
				PC:         s.PC,
				SizeBucket: s.SizeBucket,
			}
			g := groups[k]
			if g == nil {
				g = &groupBucket{key: k}
				groups[k] = g
			}
			g.gains = append(g.gains, float64(s.GainNs))
			g.captures = append(g.captures, float64(s.CaptureNs))
			g.retries = append(g.retries, float64(s.RetryNs))
			g.resumes = append(g.resumes, float64(s.ResumeNs))
		}
	}

	entries := make([]evm.AdaptiveCalibrationEntry, 0, len(groups))
	for _, g := range groups {
		medGain, _, _, _ := medianAndIQR(g.gains)
		medCap, _, _, _ := medianAndIQR(g.captures)
		medRetry, _, _, _ := medianAndIQR(g.retries)
		medResume, _, _, _ := medianAndIQR(g.resumes)
		minB, maxB := evm.SizeBucketBounds(g.key.SizeBucket)
		entries = append(entries, evm.AdaptiveCalibrationEntry{
			CodeHash:       g.key.CodeHash.Hex(),
			Selector:       evm.FormatSelectorHex(g.key.Selector),
			PC:             g.key.PC,
			SizeBucket:     g.key.SizeBucket,
			SizeMinBytes:   minB,
			SizeMaxBytes:   maxB,
			Samples:        len(g.gains),
			GainNs:         medGain,
			CaptureNs:      medCap,
			MedianRetryNs:  medRetry,
			MedianResumeNs: medResume,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CodeHash != entries[j].CodeHash {
			return entries[i].CodeHash < entries[j].CodeHash
		}
		if entries[i].Selector != entries[j].Selector {
			return entries[i].Selector < entries[j].Selector
		}
		if entries[i].PC != entries[j].PC {
			return entries[i].PC < entries[j].PC
		}
		return entries[i].SizeBucket < entries[j].SizeBucket
	})
	return entries, maxSingleBytes, nil
}

// RunPhase3Calibrate hiệu chuẩn toàn bộ tham số Phase 3 trên seed phát triển (mặc định seed 31) và khóa vào model.json.
func RunPhase3Calibrate(ctx context.Context, opts CalibrateOptions) (*evm.AdaptiveModel, string, error) {
	started := time.Now()
	suite, err := ParseSuite(opts.Suite)
	if err != nil {
		return nil, "", err
	}
	if suite != SuitePhase3 {
		return nil, "", fmt.Errorf("calibrate only supports --suite %q (got %q)", SuitePhase3, suite)
	}
	if opts.Seed == 0 {
		opts.Seed = DefaultPhase3CalibrateSeed
	}
	if opts.TestdataDir == "" {
		opts.TestdataDir = filepath.Join("testdata", SuitePhase3)
	}
	if opts.OutPath == "" {
		opts.OutPath = filepath.Join("results", SuitePhase3, "model.json")
	}
	if opts.Runs <= 0 {
		opts.Runs = 5
	}
	if opts.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(opts.GOMAXPROCS)
	}

	seedManifestPath := filepath.Join(opts.TestdataDir, fmt.Sprintf("seed-%d", opts.Seed), "manifest.json")
	manifestBytes, err := os.ReadFile(seedManifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("read phase3 seed manifest %q: %w", seedManifestPath, err)
	}
	manifestSum := sha256.Sum256(manifestBytes)
	manifestSHA256 := "0x" + hex.EncodeToString(manifestSum[:])

	var seedManifestMeta struct {
		RuntimeCodeHash string `json:"runtimeCodeHash"`
	}
	if err := json.Unmarshal(manifestBytes, &seedManifestMeta); err != nil {
		return nil, "", fmt.Errorf("parse phase3 seed manifest %q: %w", seedManifestPath, err)
	}

	chainCase, err := LoadSuiteFixtureCase(SuitePhase3, opts.TestdataDir, opts.Seed, DefaultPhase3TargetCase)
	if err != nil {
		return nil, "", fmt.Errorf("load %s for seed %d: %w", DefaultPhase3TargetCase, opts.Seed, err)
	}

	// Bước 1: đo ghép cặp retry_ns - resume_ns và capture_ns trên toàn bộ 6 block của merkle-mixed seed phát triển
	entries, maxSingleCPBytes, err := runPairedCalibrationOnChain(ctx, chainCase)
	if err != nil {
		return nil, "", err
	}
	if len(entries) == 0 {
		return nil, "", errors.New("calibration produced zero paired entries")
	}

	// Bước 2: xác định 2 mức RAM chặt và rộng (thử 1 MiB & 64 MiB; nếu 1 MiB chưa gây từ chối thì tinh chỉnh theo đỉnh history)
	const initialTight uint64 = 1 << 20
	const initialWide uint64 = 64 << 20

	histWideProbe, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
		Policy:      CheckpointHistory,
		Interval:    DefaultCheckpointInterval,
		BudgetBytes: initialWide,
		Phase2Mode:  true,
	}, false)
	if err != nil {
		return nil, "", fmt.Errorf("probe history wide: %w", err)
	}
	histInitTightProbe, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
		Policy:      CheckpointHistory,
		Interval:    DefaultCheckpointInterval,
		BudgetBytes: initialTight,
		Phase2Mode:  true,
	}, false)
	if err != nil {
		return nil, "", fmt.Errorf("probe history initial tight: %w", err)
	}

	budgetCal := &evm.AdaptiveBudgetCalibration{
		InitialTightBytes:      initialTight,
		InitialWideBytes:       initialWide,
		InitialTightSkipped:    histInitTightProbe.skippedBudAll,
		HistoryWidePeakBytes:   histWideProbe.peakAllBytes,
		HistorySteadyPeakBytes: histWideProbe.peakSteadyBytes,
		SelectedTightBytes:     initialTight,
		SelectedWideBytes:      initialWide,
		TightVerifiedSkipped:   histInitTightProbe.skippedBudAll,
		WideVerifiedSkipped:    histWideProbe.skippedBudAll,
		TightSteadySkipped:     histInitTightProbe.skippedBudSteady,
	}

	if histInitTightProbe.skippedBudAll == 0 || histInitTightProbe.skippedBudSteady == 0 {
		budgetCal.Adjusted = true
		candTight := histWideProbe.peakAllBytes / 4
		candWide := histWideProbe.peakAllBytes * 2
		if candWide < initialTight {
			candWide = initialTight
		}
		if candTight < maxSingleCPBytes {
			candTight = maxSingleCPBytes
		}
		probeTight, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
			Policy:      CheckpointHistory,
			Interval:    DefaultCheckpointInterval,
			BudgetBytes: candTight,
			Phase2Mode:  true,
		}, false)
		if err != nil {
			return nil, "", fmt.Errorf("probe adjusted tight: %w", err)
		}
		// nếu 1/4 đỉnh tổng (do block 1 fixed) vẫn chưa gây ép RAM trên các block steady (2..6) của history,
		// thu hẹp theo đỉnh steady (tối thiểu đủ chứa vừa 1 điểm claimLate để không vô hiệu hóa resume)
		if probeTight.skippedBudSteady == 0 && histWideProbe.peakSteadyBytes > 0 {
			steadyTight := histWideProbe.peakSteadyBytes / 4
			if steadyTight < maxSingleCPBytes {
				steadyTight = maxSingleCPBytes + 256
			}
			if steadyTight < candTight {
				probeSteady, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
					Policy:      CheckpointHistory,
					Interval:    DefaultCheckpointInterval,
					BudgetBytes: steadyTight,
					Phase2Mode:  true,
				}, false)
				if err != nil {
					return nil, "", fmt.Errorf("probe steady tight: %w", err)
				}
				if probeSteady.skippedBudAll > 0 {
					candTight = steadyTight
					probeTight = probeSteady
				}
			}
		}
		probeWide, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
			Policy:      CheckpointHistory,
			Interval:    DefaultCheckpointInterval,
			BudgetBytes: candWide,
			Phase2Mode:  true,
		}, false)
		if err != nil {
			return nil, "", fmt.Errorf("probe adjusted wide: %w", err)
		}
		budgetCal.SelectedTightBytes = candTight
		budgetCal.SelectedWideBytes = candWide
		budgetCal.TightVerifiedSkipped = probeTight.skippedBudAll
		budgetCal.TightSteadySkipped = probeTight.skippedBudSteady
		budgetCal.WideVerifiedSkipped = probeWide.skippedBudAll
	}
	if budgetCal.TightVerifiedSkipped == 0 {
		return nil, "", fmt.Errorf("tight budget %d bytes failed to trigger any budget skip on %s seed %d",
			budgetCal.SelectedTightBytes, DefaultPhase3TargetCase, opts.Seed)
	}

	// Bước 3: chọn chu kỳ N trong {16, 64, 256} bằng fixed trên merkle-mixed seed phát triển, 4 worker, RAM chặt
	intervals := []uint64{16, 64, 256}
	nTimes := make([][]float64, len(intervals))
	nCreated := make([][]float64, len(intervals))
	nResumed := make([][]float64, len(intervals))
	nSkipped := make([][]float64, len(intervals))
	for runIdx := 0; runIdx < opts.Runs; runIdx++ {
		for pos := range intervals {
			idx := (runIdx + pos) % len(intervals)
			nVal := intervals[idx]
			p, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
				Policy:      CheckpointFixed,
				Interval:    nVal,
				BudgetBytes: budgetCal.SelectedTightBytes,
				Phase2Mode:  true,
			}, false)
			if err != nil {
				return nil, "", fmt.Errorf("calibrate N=%d run=%d: %w", nVal, runIdx+1, err)
			}
			nTimes[idx] = append(nTimes[idx], float64(p.totalNs))
			nCreated[idx] = append(nCreated[idx], float64(p.createdAll))
			nResumed[idx] = append(nResumed[idx], float64(p.resumedAll))
			nSkipped[idx] = append(nSkipped[idx], float64(p.skippedBudAll))
		}
	}

	nRecords := make([]evm.AdaptiveNSelectionRecord, len(intervals))
	bestNIdx := 0
	for i, nVal := range intervals {
		med, _, _, iqr := medianAndIQR(nTimes[i])
		medC, _, _, _ := medianAndIQR(nCreated[i])
		medR, _, _, _ := medianAndIQR(nResumed[i])
		medS, _, _, _ := medianAndIQR(nSkipped[i])
		nRecords[i] = evm.AdaptiveNSelectionRecord{
			Interval:         nVal,
			Runs:             opts.Runs,
			MedianNs:         med,
			IQRNs:            iqr,
			MedianCreated:    medC,
			MedianResumed:    medR,
			MedianSkippedBud: medS,
		}
		if med < nRecords[bestNIdx].MedianNs {
			bestNIdx = i
		}
	}
	selectedN := intervals[bestNIdx]

	// xác nhận lại mức RAM rộng đủ không gây từ chối cho cả fixed & history với N đã chọn
	wideVerifyN, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
		Policy:      CheckpointFixed,
		Interval:    selectedN,
		BudgetBytes: initialWide,
		Phase2Mode:  true,
	}, false)
	if err != nil {
		return nil, "", fmt.Errorf("verify wide peak with selected N=%d: %w", selectedN, err)
	}
	if wideVerifyN.peakAllBytes*2 > budgetCal.SelectedWideBytes {
		budgetCal.SelectedWideBytes = wideVerifyN.peakAllBytes * 2
	}
	wideCheckFinal, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
		Policy:      CheckpointHistory,
		Interval:    selectedN,
		BudgetBytes: budgetCal.SelectedWideBytes,
		Phase2Mode:  true,
	}, false)
	if err != nil {
		return nil, "", fmt.Errorf("final wide budget check: %w", err)
	}
	budgetCal.WideVerifiedSkipped = wideCheckFinal.skippedBudAll

	// Bước 4: thăm dò score dương ở lượt threshold=0 rồi so sánh ngưỡng 0 vs trung vị các score dương
	baseModel := &evm.AdaptiveModel{
		FormatVersion:         evm.AdaptiveModelFormatVersion,
		Suite:                 SuitePhase3,
		Seed:                  opts.Seed,
		TargetCase:            DefaultPhase3TargetCase,
		FixtureManifestSHA256: manifestSHA256,
		FixtureCaseInputHash:  chainCase.InputHash,
		RuntimeCodeHash:       seedManifestMeta.RuntimeCodeHash,
		CheckpointEvery:       selectedN,
		TightBudgetBytes:      budgetCal.SelectedTightBytes,
		WideBudgetBytes:       budgetCal.SelectedWideBytes,
		ScoreThreshold:        0,
		BudgetCalibration:     budgetCal,
		NSelection:            nRecords,
		Entries:               entries,
	}
	if err := baseModel.BuildIndex(); err != nil {
		return nil, "", err
	}

	explore, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
		Policy:        CheckpointAdaptive,
		Interval:      selectedN,
		BudgetBytes:   budgetCal.SelectedTightBytes,
		Phase2Mode:    true,
		AdaptiveModel: baseModel,
	}, true)
	if err != nil {
		return nil, "", fmt.Errorf("adaptive score exploration: %w", err)
	}
	var posScores []float64
	for _, ev := range explore.decisionEvents {
		if ev.ScoreNsPerByte > 0 {
			posScores = append(posScores, ev.ScoreNsPerByte)
		}
	}
	posMed := 0.0
	if len(posScores) > 0 {
		posMed, _, _, _ = medianAndIQR(posScores)
	}
	baseModel.PositiveScoreCount = len(posScores)
	baseModel.PositiveScoreMedian = posMed

	type threshCand struct {
		label string
		val   float64
	}
	cands := []threshCand{{label: "zero", val: 0}}
	if len(posScores) > 0 && posMed > 0 {
		cands = append(cands, threshCand{label: "median_positive", val: posMed})
	}

	thTimes := make([][]float64, len(cands))
	thCreated := make([][]float64, len(cands))
	thResumed := make([][]float64, len(cands))
	thSkipLow := make([][]float64, len(cands))
	thSkipBud := make([][]float64, len(cands))

	for runIdx := 0; runIdx < opts.Runs; runIdx++ {
		for pos := range cands {
			idx := (runIdx + pos) % len(cands)
			candModel := *baseModel
			candModel.ScoreThreshold = cands[idx].val
			if err := candModel.BuildIndex(); err != nil {
				return nil, "", err
			}
			p, err := runSinglePhase3Probe(ctx, chainCase, 4, CheckpointOptions{
				Policy:        CheckpointAdaptive,
				Interval:      selectedN,
				BudgetBytes:   budgetCal.SelectedTightBytes,
				Phase2Mode:    true,
				AdaptiveModel: &candModel,
			}, false)
			if err != nil {
				return nil, "", fmt.Errorf("calibrate threshold=%s run=%d: %w", cands[idx].label, runIdx+1, err)
			}
			thTimes[idx] = append(thTimes[idx], float64(p.totalNs))
			thCreated[idx] = append(thCreated[idx], float64(p.createdAll))
			thResumed[idx] = append(thResumed[idx], float64(p.resumedAll))
			thSkipLow[idx] = append(thSkipLow[idx], float64(p.skippedLowAll))
			thSkipBud[idx] = append(thSkipBud[idx], float64(p.skippedBudAll))
		}
	}

	thRecords := make([]evm.AdaptiveThresholdRecord, len(cands))
	bestThIdx := 0
	for i, c := range cands {
		med, _, _, iqr := medianAndIQR(thTimes[i])
		medC, _, _, _ := medianAndIQR(thCreated[i])
		medR, _, _, _ := medianAndIQR(thResumed[i])
		medSL, _, _, _ := medianAndIQR(thSkipLow[i])
		medSB, _, _, _ := medianAndIQR(thSkipBud[i])
		thRecords[i] = evm.AdaptiveThresholdRecord{
			Label:            c.label,
			Threshold:        c.val,
			Runs:             opts.Runs,
			MedianNs:         med,
			IQRNs:            iqr,
			MedianCreated:    medC,
			MedianResumed:    medR,
			MedianSkippedLow: medSL,
			MedianSkippedBud: medSB,
		}
		if med < thRecords[bestThIdx].MedianNs {
			bestThIdx = i
		}
	}

	baseModel.ScoreThreshold = cands[bestThIdx].val
	baseModel.ThresholdSelection = thRecords
	baseModel.TimestampUTC = time.Now().UTC().Format(time.RFC3339)
	baseModel.CalibrationDurationNs = time.Since(started).Nanoseconds()

	shaHex, err := evm.SaveAdaptiveModel(opts.OutPath, baseModel)
	if err != nil {
		return nil, "", err
	}
	return baseModel, shaHex, nil
}
