package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/lab"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 1
	}

	switch args[0] {
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "bench":
		return runBench(args[1:], stdout, stderr)
	case "calibrate":
		return runCalibrate(args[1:], stdout, stderr)
	case "gen-swap-pools":
		return runGenSwapPools(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  pluto-lab calibrate --suite phase3 [--seed 31] [--runs 5] [--out results/phase3/model.json] [--testdata dir] [--gomaxprocs 0]")
	fmt.Fprintln(w, "  pluto-lab verify --case <case|all> [--suite phase1|phase2|phase3] [--seed 73] [--model path] [--mode sequential|occ|grouped|predicted-occ|all] [--checkpoint off|fixed|history|adaptive|all] [--checkpoint-n 64] [--checkpoint-budget-bytes 67108864] [--trace path.jsonl] [--decision-trace path.jsonl] [--workers 1] [--testdata dir] [--evm-bin path] [--timeout 30s]")
	fmt.Fprintln(w, "  pluto-lab bench [--suite phase1|phase2|phase3] [--matrix] [--seed 73] [--model path] [--case <case|all>] [--mode <mode|all>] [--checkpoint off|fixed|history|adaptive|all] [--checkpoint-n 64] [--checkpoint-budget-bytes 67108864] [--workers 4] [--runs 10] [--out dir] [--gomaxprocs 0] [--testdata dir] [--evm-bin path] [--timeout 30s]")
	fmt.Fprintln(w, "  pluto-lab gen-swap-pools --out <dir> [--artifacts testdata/phase1/swap-pools] [--seed 20260401] [--verify-manifest path]")
}

func resolveCheckpointCLIOptions(
	setFlags map[string]bool,
	checkpointStr string,
	cpN uint64,
	cpEvery uint64,
	budgetBytes uint64,
	budgetMiB int64,
	suite string,
	model *evm.AdaptiveModel,
) (policies []string, interval uint64, resolvedBudget uint64, err error) {
	policies, err = lab.ResolveCheckpointPoliciesForSuite(checkpointStr, suite)
	if err != nil {
		return nil, 0, 0, err
	}

	if setFlags["checkpoint-n"] && setFlags["checkpoint-every"] && cpN != cpEvery {
		return nil, 0, 0, fmt.Errorf("conflicting flags --checkpoint-n=%d and --checkpoint-every=%d", cpN, cpEvery)
	}
	interval = lab.DefaultCheckpointInterval
	if model != nil && model.CheckpointEvery > 0 {
		interval = model.CheckpointEvery
	}
	if setFlags["checkpoint-n"] {
		interval = cpN
	} else if setFlags["checkpoint-every"] {
		interval = cpEvery
	}
	if interval == 0 {
		return nil, 0, 0, fmt.Errorf("--checkpoint-n / --checkpoint-every must be > 0")
	}

	if setFlags["checkpoint-budget-bytes"] && setFlags["checkpoint-budget-mib"] {
		return nil, 0, 0, fmt.Errorf("cannot specify both --checkpoint-budget-bytes and --checkpoint-budget-mib")
	}
	resolvedBudget = lab.DefaultCheckpointBudgetBytes
	if model != nil && model.TightBudgetBytes > 0 {
		resolvedBudget = model.TightBudgetBytes
	}
	if setFlags["checkpoint-budget-bytes"] {
		resolvedBudget = budgetBytes
	} else if setFlags["checkpoint-budget-mib"] {
		if budgetMiB < 0 {
			return nil, 0, 0, fmt.Errorf("--checkpoint-budget-mib must be >= 0, got %d", budgetMiB)
		}
		if uint64(budgetMiB) > math.MaxUint64/(1<<20) {
			return nil, 0, 0, fmt.Errorf("--checkpoint-budget-mib=%d overflows uint64 bytes", budgetMiB)
		}
		resolvedBudget = uint64(budgetMiB) << 20
	}
	return policies, interval, resolvedBudget, nil
}

func runCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	suiteStr := fs.String("suite", lab.SuitePhase3, "calibration suite (phase3)")
	seed := fs.Uint64("seed", lab.DefaultPhase3CalibrateSeed, "development seed (default 31)")
	runs := fs.Int("runs", 5, "probe runs per N and threshold candidate")
	outPath := fs.String("out", filepath.Join("results", "phase3", "model.json"), "output model.json path")
	testdataRoot := fs.String("testdata", "", "path to suite testdata directory (defaults to testdata/<suite>)")
	gomaxprocs := fs.Int("gomaxprocs", 0, "lock GOMAXPROCS (0 preserves env GOMAXPROCS or locks to min(4, NumCPU))")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", fs.Args())
		return 1
	}
	suite, err := lab.ParseSuite(*suiteStr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *testdataRoot == "" {
		*testdataRoot = filepath.Join("testdata", suite)
	}
	procs := *gomaxprocs
	if procs <= 0 && os.Getenv("GOMAXPROCS") == "" {
		procs = runtime.NumCPU()
		if procs > 4 {
			procs = 4
		}
	}

	model, shaHex, err := lab.RunPhase3Calibrate(context.Background(), lab.CalibrateOptions{
		Suite:       suite,
		Seed:        *seed,
		TestdataDir: *testdataRoot,
		OutPath:     *outPath,
		Runs:        *runs,
		GOMAXPROCS:  procs,
	})
	if err != nil {
		fmt.Fprintf(stderr, "calibrate failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "calibrate complete: suite=%s seed=%d out=%s sha256=%s duration=%.2fs\n",
		model.Suite, model.Seed, *outPath, shaHex, float64(model.CalibrationDurationNs)/1e9)
	fmt.Fprintf(stdout, "  selected: N=%d tightBudgetBytes=%d wideBudgetBytes=%d scoreThreshold=%.6f ns/byte entries=%d\n",
		model.CheckpointEvery, model.TightBudgetBytes, model.WideBudgetBytes, model.ScoreThreshold, len(model.Entries))
	for _, nr := range model.NSelection {
		fmt.Fprintf(stdout, "  N-probe: N=%d medianMs=%.3f iqrMs=%.3f created=%.1f resumed=%.1f skippedBudget=%.1f\n",
			nr.Interval, nr.MedianNs/1e6, nr.IQRNs/1e6, nr.MedianCreated, nr.MedianResumed, nr.MedianSkippedBud)
	}
	for _, tr := range model.ThresholdSelection {
		fmt.Fprintf(stdout, "  threshold-probe: %s (%.6f) medianMs=%.3f iqrMs=%.3f created=%.1f resumed=%.1f skippedLow=%.1f skippedBud=%.1f\n",
			tr.Label, tr.Threshold, tr.MedianNs/1e6, tr.IQRNs/1e6, tr.MedianCreated, tr.MedianResumed, tr.MedianSkippedLow, tr.MedianSkippedBud)
	}
	for _, e := range model.Entries {
		fmt.Fprintf(stdout, "  entry: selector=%s pc=%d bucket=%d [%d,%d) samples=%d gainNs=%.1f captureNs=%.1f retryNs=%.1f resumeNs=%.1f\n",
			e.Selector, e.PC, e.SizeBucket, e.SizeMinBytes, e.SizeMaxBytes, e.Samples, e.GainNs, e.CaptureNs, e.MedianRetryNs, e.MedianResumeNs)
	}
	return 0
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)

	suiteStr := fs.String("suite", lab.SuitePhase1, "fixture suite: phase1, phase2, phase3 or phase4")
	matrix := fs.Bool("matrix", false, "verify full Phase 4 10-config matrix (requires --model)")
	caseID := fs.String("case", "", "fixture case ID or 'all' under testdata dir (required unless --suite phase4 --matrix)")
	modeStr := fs.String("mode", string(lab.ModeSequential), "execution mode or 'all'")
	checkpointStr := fs.String("checkpoint", lab.CheckpointOff, "checkpoint policy: off, fixed, history, adaptive or all")
	cpN := fs.Uint64("checkpoint-n", lab.DefaultCheckpointInterval, "opcode interval N for fixed policy (must be > 0)")
	cpEvery := fs.Uint64("checkpoint-every", lab.DefaultCheckpointInterval, "alias of --checkpoint-n (must be > 0)")
	budgetBytes := fs.Uint64("checkpoint-budget-bytes", lab.DefaultCheckpointBudgetBytes, "hard RAM budget M in exact bytes (0 disables capture)")
	budgetMiB := fs.Int64("checkpoint-budget-mib", 64, "hard RAM budget M in MiB (mutually exclusive with --checkpoint-budget-bytes)")
	seed := fs.Uint64("seed", lab.DefaultPhase3EvalSeed, "fixture seed for --suite phase3 (default 73)")
	modelPath := fs.String("model", "", "path to locked Phase 3 model.json (required for adaptive)")
	tracePath := fs.String("trace", "", "optional JSONL output path for Phase 2/3 checkpoint trace events")
	decisionTracePath := fs.String("decision-trace", "", "optional JSONL output path for Phase 3 adaptive decision trace events")
	workers := fs.Int("workers", 1, "worker count (must be >= 1; defaults to 4 for --suite phase2/phase3)")
	testdataRoot := fs.String("testdata", "", "path to suite testdata directory (defaults to testdata/<suite>)")
	evmBinFlag := fs.String("evm-bin", "", "path to geth evm binary (defaults to EVM_BIN or PATH)")
	timeout := fs.Duration("timeout", 30*time.Second, "timeout per evm t8n invocation")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", fs.Args())
		return 1
	}
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	suite, err := lab.ParseSuite(*suiteStr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !setFlags["testdata"] || *testdataRoot == "" {
		if suite == lab.SuitePhase4 {
			*testdataRoot = "testdata"
		} else {
			*testdataRoot = filepath.Join("testdata", suite)
		}
	}
	if suite == lab.SuitePhase2 || suite == lab.SuitePhase3 {
		if !setFlags["workers"] {
			*workers = 4
		}
		if !setFlags["mode"] {
			*modeStr = string(lab.ModeOCC)
		}
	}
	if suite == lab.SuitePhase4 && *matrix && *caseID == "" {
		*caseID = "all"
	}
	if *caseID == "" {
		fmt.Fprintln(stderr, "error: --case is required")
		return 1
	}
	if *workers < 1 {
		fmt.Fprintf(stderr, "error: --workers must be >= 1, got %d\n", *workers)
		return 1
	}

	var loadedModel *evm.AdaptiveModel
	var modelSHA string
	if *modelPath != "" {
		loadedModel, err = evm.LoadAdaptiveModel(*modelPath)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		modelSHA = loadedModel.SHA256()
	}

	policies, interval, resolvedBudget, err := resolveCheckpointCLIOptions(
		setFlags, *checkpointStr, *cpN, *cpEvery, *budgetBytes, *budgetMiB, suite, loadedModel,
	)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, pol := range policies {
		if pol == lab.CheckpointAdaptive && loadedModel == nil {
			fmt.Fprintln(stderr, "error: checkpoint policy \"adaptive\" requires --model <path/to/model.json>")
			return 1
		}
	}

	evmBin, err := lab.ResolveEVMBin(*evmBinFlag)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	var modes []lab.BenchExecConfig
	if suite == lab.SuitePhase4 && *matrix {
		if loadedModel == nil {
			fmt.Fprintln(stderr, "error: --suite phase4 --matrix requires --model <path/to/model.json>")
			return 1
		}
		modes = lab.Phase4MatrixConfigs(loadedModel, *modelPath, modelSHA)
	} else if *modeStr == "all" {
		if suite == lab.SuitePhase2 || suite == lab.SuitePhase3 {
			fmt.Fprintf(stderr, "error: --suite %s requires --mode occ or predicted-occ\n", suite)
			return 1
		}
		if len(policies) > 1 || policies[0] != lab.CheckpointOff {
			fmt.Fprintln(stderr, "error: --checkpoint != off requires --mode occ or predicted-occ")
			return 1
		}
		modes = []lab.BenchExecConfig{
			{Mode: lab.ModeSequential, Workers: 1},
			{Mode: lab.ModeOCC, Workers: *workers},
			{Mode: lab.ModeGrouped, Workers: *workers},
			{Mode: lab.ModePredictedOCC, Workers: *workers},
		}
	} else {
		mode, err := lab.ParseMode(*modeStr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if suite == lab.SuitePhase2 && mode != lab.ModeOCC {
			fmt.Fprintf(stderr, "error: --suite phase2 only supports --mode occ (got %q)\n", mode)
			return 1
		}
		if suite == lab.SuitePhase3 && mode != lab.ModeOCC && mode != lab.ModePredictedOCC {
			fmt.Fprintf(stderr, "error: --suite phase3 only supports --mode occ or predicted-occ (got %q)\n", mode)
			return 1
		}
		if mode != lab.ModeOCC && mode != lab.ModePredictedOCC && (len(policies) > 1 || policies[0] != lab.CheckpointOff) {
			fmt.Fprintf(stderr, "error: checkpoint policy %q requires --mode occ or predicted-occ (got %q)\n", *checkpointStr, mode)
			return 1
		}
		for _, pol := range policies {
			modes = append(modes, lab.BenchExecConfig{
				Mode:    mode,
				Workers: *workers,
				Checkpoint: lab.CheckpointOptions{
					Policy:        pol,
					Interval:      interval,
					BudgetBytes:   resolvedBudget,
					Phase2Mode:    suite == lab.SuitePhase2 || suite == lab.SuitePhase3 || pol != lab.CheckpointOff,
					ModelPath:     *modelPath,
					ModelSHA256:   modelSHA,
					AdaptiveModel: loadedModel,
				},
			})
		}
	}

	var cases []string
	if *caseID == "all" {
		switch suite {
		case lab.SuitePhase2:
			cases = lab.AllPhase2VerifyCases()
		case lab.SuitePhase3:
			cases = lab.AllPhase3VerifyCases()
		case lab.SuitePhase4:
			cases = lab.AllPhase4Cases()
		default:
			cases = lab.AllVerifyCases()
		}
	} else {
		cases = []string{*caseID}
	}

	var allTraceEvents []evm.Phase2TraceEvent
	var allDecisionEvents []evm.AdaptiveDecisionEvent

	for _, cid := range cases {
		chainCase, err := lab.LoadSuiteFixtureCase(suite, *testdataRoot, *seed, cid)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		for _, cfg := range modes {
			if chainCase.ExpectedReject != nil {
				invRes, err := lab.VerifyInvalidChain(context.Background(), chainCase, lab.VerifyOptions{
					Mode:       cfg.Mode,
					Workers:    cfg.Workers,
					Checkpoint: cfg.Checkpoint,
					EVMBin:     evmBin,
					Timeout:    *timeout,
				})
				if err != nil {
					fmt.Fprintf(stderr, "verify failed (case=%s mode=%s/%s workers=%d): %v\n",
						cid, cfg.Mode, cfg.EffectiveCheckpointPolicy(), cfg.Workers, err)
					return 1
				}
				fmt.Fprintf(stdout, "PASS invalid: case=%s mode=%s/%s workers=%d rejectedHeight=%d rejectedTx=%d category=%s committedHeight=%d\n",
					invRes.CaseID, invRes.Mode, cfg.EffectiveCheckpointPolicy(), invRes.Workers,
					invRes.RejectedHeight, invRes.RejectedTxIndex, invRes.Category, invRes.CommittedHeight)
				continue
			}

			res, err := lab.VerifyChain(context.Background(), chainCase, lab.VerifyOptions{
				Mode:       cfg.Mode,
				Workers:    cfg.Workers,
				Checkpoint: cfg.Checkpoint,
				EVMBin:     evmBin,
				Timeout:    *timeout,
			})
			if err != nil {
				fmt.Fprintf(stderr, "verify failed (case=%s mode=%s/%s workers=%d): %v\n",
					cid, cfg.Mode, cfg.EffectiveCheckpointPolicy(), cfg.Workers, err)
				return 1
			}
			if *tracePath != "" {
				for _, blk := range res.Blocks {
					allTraceEvents = append(allTraceEvents, blk.Phase2Trace...)
				}
			}
			if *decisionTracePath != "" {
				for _, blk := range res.Blocks {
					allDecisionEvents = append(allDecisionEvents, blk.DecisionTrace...)
				}
			}
			printVerifySummary(stdout, res, evmBin)
		}
	}
	if *tracePath != "" {
		if err := lab.WritePhase2TraceJSONL(*tracePath, allTraceEvents); err != nil {
			fmt.Fprintf(stderr, "error writing trace: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "trace written: %s (%d events)\n", *tracePath, len(allTraceEvents))
	}
	if *decisionTracePath != "" {
		if err := lab.WriteDecisionTraceJSONL(*decisionTracePath, allDecisionEvents); err != nil {
			fmt.Fprintf(stderr, "error writing decision trace: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "decision trace written: %s (%d events)\n", *decisionTracePath, len(allDecisionEvents))
	}
	return 0
}

func runBench(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)

	suiteStr := fs.String("suite", lab.SuitePhase1, "fixture suite: phase1, phase2 or phase3")
	matrix := fs.Bool("matrix", false, "run full Phase 3 benchmark matrix (5 chains x 4 policies x 1/4 workers x tight/wide RAM)")
	seed := fs.Uint64("seed", lab.DefaultPhase3EvalSeed, "fixture seed for --suite phase3 (default 73)")
	modelPath := fs.String("model", "", "path to locked Phase 3 model.json (required for adaptive or --suite phase3 --matrix)")
	caseID := fs.String("case", "all", "fixture case ID or 'all'")
	modeStr := fs.String("mode", "all", "execution mode or 'all'")
	checkpointStr := fs.String("checkpoint", lab.CheckpointOff, "checkpoint policy: off, fixed, history, adaptive or all")
	cpN := fs.Uint64("checkpoint-n", lab.DefaultCheckpointInterval, "opcode interval N for fixed policy (must be > 0)")
	cpEvery := fs.Uint64("checkpoint-every", lab.DefaultCheckpointInterval, "alias of --checkpoint-n (must be > 0)")
	budgetBytes := fs.Uint64("checkpoint-budget-bytes", lab.DefaultCheckpointBudgetBytes, "hard RAM budget M in exact bytes (0 disables capture)")
	budgetMiB := fs.Int64("checkpoint-budget-mib", 64, "hard RAM budget M in MiB (mutually exclusive with --checkpoint-budget-bytes)")
	workers := fs.Int("workers", 4, "worker count when --mode selects one worker-based executor")
	runs := fs.Int("runs", 10, "measured runs per configuration")
	outDir := fs.String("out", "", "output directory for CSV and summary (defaults to results/<suite>)")
	testdataRoot := fs.String("testdata", "", "path to suite testdata directory (defaults to testdata/<suite>)")
	evmBinFlag := fs.String("evm-bin", "", "path to geth evm binary (defaults to EVM_BIN or PATH)")
	timeout := fs.Duration("timeout", 30*time.Second, "timeout per evm t8n invocation")
	gomaxprocs := fs.Int("gomaxprocs", 0, "lock GOMAXPROCS (0 preserves env GOMAXPROCS or locks to min(4, NumCPU))")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", fs.Args())
		return 1
	}
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	suite, err := lab.ParseSuite(*suiteStr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !setFlags["testdata"] || *testdataRoot == "" {
		if suite == lab.SuitePhase4 {
			*testdataRoot = "testdata"
		} else {
			*testdataRoot = filepath.Join("testdata", suite)
		}
	}
	if !setFlags["out"] || *outDir == "" {
		*outDir = filepath.Join("results", suite)
	}
	if (suite == lab.SuitePhase2 || suite == lab.SuitePhase3) && !setFlags["mode"] {
		*modeStr = string(lab.ModeOCC)
	}
	if *runs < 1 {
		fmt.Fprintf(stderr, "error: --runs must be >= 1, got %d\n", *runs)
		return 1
	}
	if *workers < 1 {
		fmt.Fprintf(stderr, "error: --workers must be >= 1, got %d\n", *workers)
		return 1
	}

	var loadedModel *evm.AdaptiveModel
	var modelSHA string
	if *modelPath != "" {
		loadedModel, err = evm.LoadAdaptiveModel(*modelPath)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		modelSHA = loadedModel.SHA256()
	}

	var cases []string
	if *caseID == "all" {
		switch suite {
		case lab.SuitePhase2:
			cases = lab.AllPhase2BenchCases()
		case lab.SuitePhase3:
			cases = lab.AllPhase3BenchCases()
		case lab.SuitePhase4:
			cases = lab.AllPhase4Cases()
		default:
			cases = lab.AllBenchCases()
		}
	} else {
		cases = []string{*caseID}
	}

	var configs []lab.BenchExecConfig
	if (suite == lab.SuitePhase3 || suite == lab.SuitePhase4) && *matrix {
		if loadedModel == nil {
			fmt.Fprintf(stderr, "error: --suite %s --matrix requires --model <path/to/model.json>\n", suite)
			return 1
		}
	} else {
		policies, interval, resolvedBudget, err := resolveCheckpointCLIOptions(
			setFlags, *checkpointStr, *cpN, *cpEvery, *budgetBytes, *budgetMiB, suite, loadedModel,
		)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		for _, pol := range policies {
			if pol == lab.CheckpointAdaptive && loadedModel == nil {
				fmt.Fprintln(stderr, "error: checkpoint policy \"adaptive\" requires --model <path/to/model.json>")
				return 1
			}
		}

		if *modeStr == "all" {
			if suite == lab.SuitePhase2 || suite == lab.SuitePhase3 {
				fmt.Fprintf(stderr, "error: --suite %s only supports --mode occ or predicted-occ\n", suite)
				return 1
			}
			if len(policies) > 1 || policies[0] != lab.CheckpointOff {
				fmt.Fprintln(stderr, "error: --checkpoint != off requires --mode occ or predicted-occ")
				return 1
			}
			configs = lab.DefaultBenchConfigs()
		} else {
			m, err := lab.ParseMode(*modeStr)
			if err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
			if suite == lab.SuitePhase2 && m != lab.ModeOCC {
				fmt.Fprintf(stderr, "error: --suite phase2 only supports --mode occ (got %q)\n", m)
				return 1
			}
			if suite == lab.SuitePhase3 && m != lab.ModeOCC && m != lab.ModePredictedOCC {
				fmt.Fprintf(stderr, "error: --suite phase3 only supports --mode occ or predicted-occ (got %q)\n", m)
				return 1
			}
			if m != lab.ModeOCC && m != lab.ModePredictedOCC && (len(policies) > 1 || policies[0] != lab.CheckpointOff) {
				fmt.Fprintf(stderr, "error: checkpoint policy %q requires --mode occ or predicted-occ (got %q)\n", *checkpointStr, m)
				return 1
			}
			w := *workers
			if m == lab.ModeSequential {
				w = 1
			}
			for _, pol := range policies {
				configs = append(configs, lab.BenchExecConfig{
					Mode:    m,
					Workers: w,
					Checkpoint: lab.CheckpointOptions{
						Policy:        pol,
						Interval:      interval,
						BudgetBytes:   resolvedBudget,
						Phase2Mode:    suite == lab.SuitePhase2 || suite == lab.SuitePhase3 || pol != lab.CheckpointOff,
						ModelPath:     *modelPath,
						ModelSHA256:   modelSHA,
						AdaptiveModel: loadedModel,
					},
				})
			}
		}
	}

	procs := *gomaxprocs
	if procs <= 0 && os.Getenv("GOMAXPROCS") == "" {
		procs = runtime.NumCPU()
		if procs > 4 {
			procs = 4
		}
	}

	rep, err := lab.RunBenchmark(context.Background(), lab.BenchOptions{
		Suite:       suite,
		Cases:       cases,
		Configs:     configs,
		Runs:        *runs,
		Seed:        *seed,
		Matrix:      *matrix,
		ModelPath:   *modelPath,
		Model:       loadedModel,
		TestdataDir: *testdataRoot,
		OutDir:      *outDir,
		EVMBin:      *evmBinFlag,
		Timeout:     *timeout,
		GOMAXPROCS:  procs,
	})
	if err != nil {
		fmt.Fprintf(stderr, "bench failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "bench complete: suite=%s rows=%d swapPoolsRows=%d gomaxprocs=%d runs=%d out=%s\n",
		suite, len(rep.Rows), rep.SwapRowCnt, rep.Metadata.GOMAXPROCS, rep.Metadata.Runs, *outDir)
	if rep.Phase4Selection != nil {
		sel := rep.Phase4Selection
		fmt.Fprintf(stdout, "phase4 selection: winner=%s/%s workers=%d budgetBytes=%d N=%d medianMs=%.3f iqrMs=%.3f (fastest=%s/%s %.3f ms)\n",
			sel.Mode, sel.CheckpointPolicy, sel.Workers, sel.BudgetBytes, sel.CheckpointInterval,
			sel.WinnerMedianMs, sel.WinnerIQRMs, sel.FastestMode, sel.FastestPolicy, sel.FastestMedianMs)
	}
	for _, s := range rep.Summaries {
		if suite == lab.SuitePhase4 {
			fmt.Fprintf(stdout, "  case=%s mode=%s/%s w=%d medianMs=%.3f iqrMs=%.3f retried=%.1f grouped=%.1f waited=%.1f created=%.1f resumed=%.1f peakBytes=%.0f saved=%.0f\n",
				s.Case, s.Mode, s.CheckpointPolicy, s.Workers, s.MedianNs/1e6, s.IQRNs/1e6,
				s.MedianRetriedTxs, s.MedianGroupedTxs, s.MedianWaitedTxs,
				s.MedianCaptureCreated, s.MedianResumeCommitted, s.MedianPeakBytes, s.MedianPrefixSaved)
		} else if suite == lab.SuitePhase3 {
			dOff := 0.0
			if s.MedianDeltaVsOffNs != nil {
				dOff = *s.MedianDeltaVsOffNs / 1e6
			}
			fmt.Fprintf(stdout, "  case=%s w=%d ram=%s policy=%s medianMs=%.3f iqrMs=%.3f deltaOffMs=%+.3f unstable=%v retried=%.1f created=%.1f skipBud=%.1f skipLow=%.1f resumed=%.1f peakBytes=%.0f saved=%.0f\n",
				s.Case, s.Workers, s.BudgetTier, s.CheckpointPolicy, s.MedianNs/1e6, s.IQRNs/1e6,
				dOff, s.UnstableVsOff, s.MedianRetriedTxs, s.MedianCaptureCreated,
				s.MedianSkippedBudget, s.MedianSkippedLowScore, s.MedianResumeCommitted,
				s.MedianPeakBytes, s.MedianPrefixSaved)
		} else if suite == lab.SuitePhase2 {
			fmt.Fprintf(stdout, "  case=%s mode=%s/%d/%s medianMs=%.3f iqrMs=%.3f retried=%.1f created=%.1f resumed=%.1f peakBytes=%.0f opSteps=%.0f saved=%.0f\n",
				s.Case, s.Mode, s.Workers, s.CheckpointPolicy, s.MedianNs/1e6, s.IQRNs/1e6,
				s.MedianRetriedTxs, s.MedianCaptureCreated, s.MedianResumeCommitted,
				s.MedianPeakBytes, s.MedianOpcodeSteps, s.MedianPrefixSaved)
		} else {
			fmt.Fprintf(stdout, "  case=%s mode=%s/%d medianMs=%.3f iqrMs=%.3f deltaVsSeqMs=%+.3f retried=%.1f grouped=%.1f waited=%.1f noiseGtDiff=%v\n",
				s.Case, s.Mode, s.Workers, s.MedianNs/1e6, s.IQRNs/1e6, s.MedianDeltaVsSeqNs/1e6,
				s.MedianRetriedTxs, s.MedianGroupedTxs, s.MedianWaitedTxs, s.NoiseExceedsDiffVsSeq)
		}
	}
	return 0
}

func runGenSwapPools(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-swap-pools", flag.ContinueOnError)
	fs.SetOutput(stderr)

	outDir := fs.String("out", "", "target output directory for regenerated swap-pools fixture (required)")
	artifactsDir := fs.String("artifacts", filepath.Join("testdata", "phase1", "swap-pools"), "directory containing compiled token/pool/router runtime bytecode hex")
	seed := fs.Uint64("seed", lab.DefaultSwapPoolsSeed, "PRNG seed for swap-pools generation")
	verifyManifest := fs.String("verify-manifest", "", "optional reference manifest.json to verify all 19 input SHA-256 hashes")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", fs.Args())
		return 1
	}
	if *outDir == "" {
		fmt.Fprintln(stderr, "error: --out is required")
		return 1
	}

	man, err := lab.GenerateSwapPoolsFixture(*artifactsDir, *outDir, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "gen-swap-pools failed: %v\n", err)
		return 1
	}
	if *verifyManifest != "" {
		if err := lab.VerifyGeneratedHashesAgainstManifest(man.InputSHA256, *verifyManifest); err != nil {
			fmt.Fprintf(stderr, "manifest hash check failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "PASS: regenerated swap-pools (seed=%d) matches all %d input SHA-256 hashes in %s\n",
			*seed, len(man.InputSHA256), *verifyManifest)
		return 0
	}
	fmt.Fprintf(stdout, "generated swap-pools (seed=%d) into %s (%d input SHA-256 hashes)\n",
		*seed, *outDir, len(man.InputSHA256))
	return 0
}

func printVerifySummary(w io.Writer, res *lab.VerifyResult, evmBin string) {
	cpPol := res.CheckpointPolicy
	if cpPol == "" {
		cpPol = lab.CheckpointOff
	}
	fmt.Fprintf(w, "case=%s mode=%s/%s fork=%s chainId=%d evmBin=%s\n",
		res.CaseID, res.Mode, cpPol, lab.ForkName, lab.DefaultChainID, evmBin)
	if res.Mode != lab.ModeSequential {
		fmt.Fprintf(w, "genesisAppHash=%s workers=%d seqAppHashMatch=%v referenceChecked=%v\n",
			res.GenesisAppHash.Hex(), res.Workers, res.SeqAppHashMatched, res.ReferenceChecked)
	} else {
		fmt.Fprintf(w, "genesisAppHash=%s referenceChecked=%v\n", res.GenesisAppHash.Hex(), res.ReferenceChecked)
	}

	for _, blk := range res.Blocks {
		if blk.OCCStats != nil && blk.Phase2Metrics != nil && cpPol != lab.CheckpointOff {
			m := blk.Phase2Metrics
			fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d retriedTxs=%d totalRetries=%d cpCreated=%d cpSkipBud=%d cpSkipLow=%d resumed=%d/%d peakBytes=%d opSteps=%d saved=%d\n",
				blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts),
				blk.OCCStats.RetriedTxs, blk.OCCStats.TotalRetries,
				m.CaptureCreated, m.CaptureSkippedBudget, m.CaptureSkippedLowScore,
				m.ResumeCommitted, m.ResumeAttempted,
				m.AccountedPeakBytes, m.OpcodeSteps, m.PrefixStepsSaved)
		} else if blk.OCCStats != nil {
			fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
				blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts),
				blk.OCCStats.RetriedTxs, blk.OCCStats.TotalRetries, blk.OCCStats.FallbackTxs)
		} else if blk.GroupedStats != nil {
			fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d groupedTxs=%d maxGroupWidth=%d groupWidths=%v abortedGroups=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
				blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts),
				blk.GroupedStats.GroupedTxs, blk.GroupedStats.MaxGroupWidth, blk.GroupedStats.GroupWidths,
				blk.GroupedStats.AbortedGroups, blk.GroupedStats.RetriedTxs, blk.GroupedStats.TotalRetries, blk.GroupedStats.FallbackTxs)
		} else if blk.PredictedStats != nil {
			ps := blk.PredictedStats
			if blk.Phase2Metrics != nil && cpPol != lab.CheckpointOff {
				m := blk.Phase2Metrics
				fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d hotPools=%s waitedTxs=%d retriedTxs=%d cpCreated=%d cpSkipBud=%d cpSkipLow=%d resumed=%d/%d peakBytes=%d saved=%d\n",
					blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts),
					formatPoolList(ps.HotPools), ps.WaitedTxs, ps.RetriedTxs,
					m.CaptureCreated, m.CaptureSkippedBudget, m.CaptureSkippedLowScore,
					m.ResumeCommitted, m.ResumeAttempted, m.AccountedPeakBytes, m.PrefixStepsSaved)
			} else {
				fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d hotPools=%s eligible={%s} waitedTxs=%d waitPredUs=%d waitWorkerUs=%d workerIdleUs=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
					blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts),
					formatPoolList(ps.HotPools), formatEligibleByPool(ps.EligibleSwapsByPool),
					ps.WaitedTxs, ps.WaitPredecessorNanos/1000, ps.WaitWorkerNanos/1000, ps.WorkerIdleNanos/1000,
					ps.RetriedTxs, ps.TotalRetries, ps.FallbackTxs)
			}
		} else {
			fmt.Fprintf(w, "block=%d appHash=%s gasUsed=%d receipts=%d\n",
				blk.BlockNumber, blk.AppHash.Hex(), blk.GasUsed, len(blk.Receipts))
		}

		if len(res.Blocks) == 1 && len(blk.Receipts) <= 8 {
			for _, rec := range blk.Receipts {
				fmt.Fprintf(w, "  tx[%d] hash=%s status=%d gasUsed=%d cumulativeGas=%d logs=%d\n",
					rec.TransactionIndex, rec.TxHash.Hex(), rec.Status, rec.GasUsed, rec.CumulativeGasUsed, len(rec.Logs))
				for j, l := range rec.Logs {
					dataHint := "0x" + hex.EncodeToString(l.Data)
					if len(l.Data) == common.HashLength {
						val := new(big.Int).SetBytes(l.Data)
						dataHint = fmt.Sprintf("%s (uint=%s)", dataHint, val.String())
					}
					fmt.Fprintf(w, "    log[%d] addr=%s data=%s\n", j, l.Address.Hex(), dataHint)
				}
			}
		} else {
			okCount := 0
			logCount := 0
			for _, rec := range blk.Receipts {
				if rec.Status == 1 {
					okCount++
				}
				logCount += len(rec.Logs)
			}
			acctCount := 0
			if blk.PostState != nil {
				acctCount = len(blk.PostState.Accounts)
			}
			fmt.Fprintf(w, "  summary okReceipts=%d/%d logs=%d postAccounts=%d\n",
				okCount, len(blk.Receipts), logCount, acctCount)
		}
	}
	fmt.Fprintln(w, "PASS: pluto matches geth t8n (post-alloc, gas, receipts, logs)")
}

func formatPoolLabel(addr common.Address) string {
	for i, p := range evm.DefaultSwapPoolAddrs {
		if addr == p {
			return fmt.Sprintf("pool%c", 'A'+i)
		}
	}
	return addr.Hex()
}

func formatPoolList(pools []common.Address) string {
	if len(pools) == 0 {
		return "[]"
	}
	labels := make([]string, len(pools))
	for i, p := range pools {
		labels[i] = formatPoolLabel(p)
	}
	return "[" + strings.Join(labels, ",") + "]"
}

func formatEligibleByPool(counts map[common.Address]int) string {
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(evm.DefaultSwapPoolAddrs))
	seen := make(map[common.Address]bool, len(evm.DefaultSwapPoolAddrs))
	for i, p := range evm.DefaultSwapPoolAddrs {
		if c, ok := counts[p]; ok {
			parts = append(parts, fmt.Sprintf("pool%c:%d", 'A'+i, c))
			seen[p] = true
		}
	}
	extra := make([]common.Address, 0)
	for p := range counts {
		if !seen[p] {
			extra = append(extra, p)
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		return extra[i].Hex() < extra[j].Hex()
	})
	for _, p := range extra {
		parts = append(parts, fmt.Sprintf("%s:%d", p.Hex(), counts[p]))
	}
	return strings.Join(parts, ",")
}
