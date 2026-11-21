package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// VerifyOptions cấu hình một lượt verify đối chiếu Pluto với `evm t8n`.
type VerifyOptions struct {
	Mode       ExecutionMode
	Workers    int
	Checkpoint CheckpointOptions
	EVMBin     string
	Timeout    time.Duration
}

// VerifiedBlock tóm tắt kết quả đã đối chiếu của một block.
type VerifiedBlock struct {
	BlockNumber    uint64
	AppHash        common.Hash
	GasUsed        uint64
	Receipts       []*TxReceipt
	PostState      *evm.LogicalState
	OCCStats       *evm.OCCBlockStats
	Phase2Metrics  *evm.Phase2CheckpointMetrics
	Phase2Trace    []evm.Phase2TraceEvent
	DecisionTrace  []evm.AdaptiveDecisionEvent
	GroupedStats   *evm.GroupedBlockStats
	GroupedTrace   *evm.GroupedBlockTrace
	PredictedStats *evm.PredictedOCCBlockStats
	PredictedTrace *evm.PredictedOCCBlockTrace
}

// VerifyResult trả về kết quả toàn bộ ca thử sau khi khớp hoàn toàn với Geth.
type VerifyResult struct {
	CaseID            string
	Mode              ExecutionMode
	CheckpointPolicy  string
	Workers           int
	GenesisAppHash    common.Hash
	Blocks            []VerifiedBlock
	ReferenceChecked  bool
	SeqAppHashMatched bool
}

// WritePhase2TraceJSONL xuất toàn bộ sự kiện trace Phase 2 sang JSONL (tạo thư mục cha nếu chưa có).
func WritePhase2TraceJSONL(path string, events []evm.Phase2TraceEvent) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create trace dir %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create trace file %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("encode trace event: %w", err)
		}
	}
	return nil
}

// WriteDecisionTraceJSONL xuất toàn bộ sự kiện quyết định adaptive sang JSONL (tạo thư mục cha nếu chưa có).
func WriteDecisionTraceJSONL(path string, events []evm.AdaptiveDecisionEvent) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create decision trace dir %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create decision trace file %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("encode decision event: %w", err)
		}
	}
	return nil
}

// VerifyChain chạy cả Pluto (internal/evm + store.PebbleDB) và `evm t8n` độc lập rồi đối chiếu mọi block.
func VerifyChain(ctx context.Context, c *ChainCase, opts VerifyOptions) (*VerifyResult, error) {
	if c == nil {
		return nil, errors.New("chain case must not be nil")
	}
	if c.ExpectedReject != nil {
		return nil, fmt.Errorf("case %q is an intentionally invalid fixture; use VerifyInvalidChain", c.ID)
	}
	if opts.Mode == "" {
		opts.Mode = ModeSequential
	}
	mode, err := ParseMode(string(opts.Mode))
	if err != nil {
		return nil, err
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}

	evmBin, err := ResolveEVMBin(opts.EVMBin)
	if err != nil {
		return nil, err
	}

	dbDir, err := os.MkdirTemp("", "pluto-lab-verify-*")
	if err != nil {
		return nil, fmt.Errorf("create temp pebble dir: %w", err)
	}
	defer os.RemoveAll(dbDir)

	db, err := store.NewPebbleDB("verify", dbDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	cpPolicy, err := ParseCheckpointPolicy(opts.Checkpoint.Policy)
	if err != nil {
		return nil, err
	}
	cpOpts := opts.Checkpoint
	cpOpts.Policy = cpPolicy

	plutoChainRes, err := ExecuteChainWithCheckpoints(ctx, db, mode, opts.Workers, true, cpOpts, c)
	if err != nil {
		return nil, fmt.Errorf("pluto execution failed: %w", err)
	}

	seqMatched := false
	if mode != ModeSequential {
		seqDir, err := os.MkdirTemp("", "pluto-lab-verify-seq-*")
		if err != nil {
			return nil, fmt.Errorf("create temp sequential pebble dir: %w", err)
		}
		defer os.RemoveAll(seqDir)

		seqDB, err := store.NewPebbleDB("verify-seq", seqDir)
		if err != nil {
			return nil, err
		}
		defer seqDB.Close()

		seqChainRes, err := ExecuteChain(ctx, seqDB, ModeSequential, 1, false, c)
		if err != nil {
			return nil, fmt.Errorf("sequential reference execution failed: %w", err)
		}
		if rep := CompareAppHashes(0, string(ModeSequential), seqChainRes.GenesisAppHash, string(mode), plutoChainRes.GenesisAppHash); !rep.OK() {
			return nil, rep.Error()
		}
		if len(seqChainRes.Blocks) != len(plutoChainRes.Blocks) {
			return nil, fmt.Errorf("block count mismatch vs sequential: seq=%d %s=%d", len(seqChainRes.Blocks), mode, len(plutoChainRes.Blocks))
		}
		for i := range c.Blocks {
			seqBlk := seqChainRes.Blocks[i]
			pBlk := plutoChainRes.Blocks[i]
			if rep := CompareAppHashes(c.Blocks[i].Env.Number, string(ModeSequential), seqBlk.AppHash, string(mode), pBlk.AppHash); !rep.OK() {
				return nil, rep.Error()
			}
			// đối chiếu thêm ReturnData và ErrMsg của từng tx giữa sequential và executor song song
			if len(seqBlk.TxReturnData) != len(pBlk.TxReturnData) || len(seqBlk.TxErrMsgs) != len(pBlk.TxErrMsgs) {
				return nil, fmt.Errorf("block %d tx result count mismatch vs sequential: seq=%d %s=%d", c.Blocks[i].Env.Number, len(seqBlk.TxReturnData), mode, len(pBlk.TxReturnData))
			}
			for txIdx := range seqBlk.TxReturnData {
				if string(seqBlk.TxReturnData[txIdx]) != string(pBlk.TxReturnData[txIdx]) {
					return nil, fmt.Errorf("block %d tx[%d] ReturnData mismatch vs sequential: seq=%x %s=%x", c.Blocks[i].Env.Number, txIdx, seqBlk.TxReturnData[txIdx], mode, pBlk.TxReturnData[txIdx])
				}
				if seqBlk.TxErrMsgs[txIdx] != pBlk.TxErrMsgs[txIdx] {
					return nil, fmt.Errorf("block %d tx[%d] ErrMsg mismatch vs sequential: seq=%q %s=%q", c.Blocks[i].Env.Number, txIdx, seqBlk.TxErrMsgs[txIdx], mode, pBlk.TxErrMsgs[txIdx])
				}
			}
		}
		seqMatched = true
	}

	gethChainRes, err := RunT8NChain(ctx, evmBin, c, opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("geth t8n execution failed: %w", err)
	}
	if len(plutoChainRes.Blocks) != len(gethChainRes) {
		return nil, fmt.Errorf("block count mismatch: pluto=%d geth=%d", len(plutoChainRes.Blocks), len(gethChainRes))
	}

	verifiedBlocks := make([]VerifiedBlock, 0, len(plutoChainRes.Blocks))
	for i := range c.Blocks {
		blk := c.Blocks[i]
		pBlk := plutoChainRes.Blocks[i]
		pPost := plutoChainRes.PostStates[i]
		gBlk := gethChainRes[i]

		report := CompareBlockWithGeth(blk.Env.Number, len(blk.Txs), pBlk, pPost, gBlk)
		if !report.OK() {
			return nil, report.Error()
		}
		if pBlk.Phase2Metrics != nil && cpOpts.Policy != CheckpointOff {
			m := pBlk.Phase2Metrics
			if m.AccountedPeakBytes > m.BudgetBytes {
				return nil, fmt.Errorf("block %d checkpoint peak bytes %d exceeds budget %d", blk.Env.Number, m.AccountedPeakBytes, m.BudgetBytes)
			}
			if m.AccountedRemainingBytes != 0 {
				return nil, fmt.Errorf("block %d checkpoint remaining bytes %d != 0", blk.Env.Number, m.AccountedRemainingBytes)
			}
		}
		verifiedBlocks = append(verifiedBlocks, VerifiedBlock{
			BlockNumber:    pBlk.BlockNumber,
			AppHash:        pBlk.AppHash,
			GasUsed:        pBlk.GasUsed,
			Receipts:       pBlk.Receipts,
			PostState:      pPost,
			OCCStats:       pBlk.OCCStats,
			Phase2Metrics:  pBlk.Phase2Metrics,
			Phase2Trace:    pBlk.Phase2Trace,
			DecisionTrace:  pBlk.DecisionTrace,
			GroupedStats:   pBlk.GroupedStats,
			GroupedTrace:   pBlk.GroupedTrace,
			PredictedStats: pBlk.PredictedStats,
			PredictedTrace: pBlk.PredictedTrace,
		})
	}

	refChecked := false
	allBlocksHaveRef := len(c.Blocks) > 0
	for i, blk := range c.Blocks {
		if len(blk.ReferenceResult) == 0 {
			allBlocksHaveRef = false
			continue
		}
		if err := CompareT8NReferenceResult(blk.ReferenceResult, gethChainRes[i].RawResultJSON); err != nil {
			return nil, fmt.Errorf("block %d t8n replay diverged from reference-result.json: %w", blk.Env.Number, err)
		}
	}
	if allBlocksHaveRef {
		refChecked = true
	} else if len(c.ReferenceResult) > 0 && len(gethChainRes) == 1 {
		if err := CompareT8NReferenceResult(c.ReferenceResult, gethChainRes[0].RawResultJSON); err != nil {
			return nil, fmt.Errorf("t8n replay diverged from reference-result.json: %w", err)
		}
		refChecked = true
	}

	return &VerifyResult{
		CaseID:            c.ID,
		Mode:              mode,
		CheckpointPolicy:  cpPolicy,
		Workers:           opts.Workers,
		GenesisAppHash:    plutoChainRes.GenesisAppHash,
		Blocks:            verifiedBlocks,
		ReferenceChecked:  refChecked,
		SeqAppHashMatched: seqMatched,
	}, nil
}

// InvalidVerifyResult tóm tắt kết quả kiểm tra fixture cố ý sai bị từ chối đúng vị trí ở cả Pluto và Geth.
type InvalidVerifyResult struct {
	CaseID          string
	Mode            ExecutionMode
	Workers         int
	RejectedHeight  uint64
	RejectedTxIndex int
	Category        string
	CommittedHeight uint64
}

// VerifyInvalidChain xác nhận block hợp lệ trước đó được chốt và block cố ý sai bị từ chối đúng chỉ số/lý do, giữ height cũ.
func VerifyInvalidChain(ctx context.Context, c *ChainCase, opts VerifyOptions) (*InvalidVerifyResult, error) {
	if c == nil || c.ExpectedReject == nil {
		return nil, errors.New("VerifyInvalidChain requires ChainCase with ExpectedReject")
	}
	if opts.Mode == "" {
		opts.Mode = ModeSequential
	}
	mode, err := ParseMode(string(opts.Mode))
	if err != nil {
		return nil, err
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	evmBin, err := ResolveEVMBin(opts.EVMBin)
	if err != nil {
		return nil, err
	}

	exp := c.ExpectedReject
	if exp.BlockIndex < 0 || exp.BlockIndex >= len(c.Blocks) {
		return nil, fmt.Errorf("expectedReject.blockIndex=%d out of range [0,%d)", exp.BlockIndex, len(c.Blocks))
	}

	dbDir, err := os.MkdirTemp("", "pluto-lab-verify-invalid-*")
	if err != nil {
		return nil, fmt.Errorf("create temp pebble dir: %w", err)
	}
	defer os.RemoveAll(dbDir)

	db, err := store.NewPebbleDB("verify-invalid", dbDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	cpPolicy, err := ParseCheckpointPolicy(opts.Checkpoint.Policy)
	if err != nil {
		return nil, err
	}
	cpOpts := opts.Checkpoint
	cpOpts.Policy = cpPolicy

	runner, err := NewChainRunnerWithCheckpoints(db, mode, opts.Workers, true, cpOpts, c.GenesisAlloc)
	if err != nil {
		return nil, err
	}

	curAlloc := c.GenesisAlloc.Clone()
	for i := 0; i < exp.BlockIndex; i++ {
		blk := c.Blocks[i]
		pBlk, _, err := runner.ExecuteBlock(ctx, blk.Env, blk.Txs)
		if err != nil {
			return nil, fmt.Errorf("prefix block %d unexpectedly failed on %s: %w", i, mode, err)
		}
		pPost, err := runner.ExportLogicalState()
		if err != nil {
			return nil, err
		}
		gBlk, err := RunT8NBlock(ctx, evmBin, curAlloc, blk.Env, blk.RawTxsJSON, opts.Timeout)
		if err != nil {
			return nil, fmt.Errorf("prefix block %d t8n failed: %w", i, err)
		}
		if rep := CompareBlockWithGeth(blk.Env.Number, len(blk.Txs), pBlk, pPost, gBlk); !rep.OK() {
			return nil, rep.Error()
		}
		curAlloc = gBlk.PostState
	}

	heightBefore := runner.CommittedHeight
	appHashBefore := runner.LatestAppHash
	var hotBefore []common.Address
	if predExec, ok := runner.Executor.(*evm.PredictedOCCExecutor); ok {
		hotBefore = predExec.HotPools()
	}
	var hotSlotsBefore []evm.MVKey
	if parExec, ok := runner.Executor.(*evm.ParallelExecutor); ok {
		hotSlotsBefore = parExec.HotSlots()
	}

	badBlk := c.Blocks[exp.BlockIndex]
	if _, _, execErr := runner.ExecuteBlock(ctx, badBlk.Env, badBlk.Txs); execErr == nil {
		return nil, fmt.Errorf("mode %s/%d accepted invalid block %d (expected reject tx=%d category=%s)",
			mode, opts.Workers, badBlk.Env.Number, exp.TxIndex, exp.Category)
	} else {
		var rejErr *evm.TxRejectedError
		if !errors.As(execErr, &rejErr) {
			return nil, fmt.Errorf("mode %s/%d expected TxRejectedError, got %T: %v", mode, opts.Workers, execErr, execErr)
		}
		if rejErr.Index != exp.TxIndex || rejErr.Category != exp.Category {
			return nil, fmt.Errorf("mode %s/%d reject mismatch: got tx=%d cat=%q, want tx=%d cat=%q",
				mode, opts.Workers, rejErr.Index, rejErr.Category, exp.TxIndex, exp.Category)
		}
	}

	if runner.CommittedHeight != heightBefore || runner.CommittedHeight != exp.ExpectedCommittedHeight {
		return nil, fmt.Errorf("height changed after rejected block: before=%d after=%d want=%d",
			heightBefore, runner.CommittedHeight, exp.ExpectedCommittedHeight)
	}
	if runner.LatestAppHash != appHashBefore {
		return nil, fmt.Errorf("AppHash changed after rejected block: before=%s after=%s",
			appHashBefore.Hex(), runner.LatestAppHash.Hex())
	}
	if predExec, ok := runner.Executor.(*evm.PredictedOCCExecutor); ok {
		if !slices.Equal(predExec.HotPools(), hotBefore) {
			return nil, fmt.Errorf("predicted-occ leaked HotPools after rejected block: before=%v after=%v",
				hotBefore, predExec.HotPools())
		}
	}
	if parExec, ok := runner.Executor.(*evm.ParallelExecutor); ok {
		if !slices.Equal(parExec.HotSlots(), hotSlotsBefore) {
			return nil, fmt.Errorf("occ leaked checkpoint HotSlots after rejected block: before=%v after=%v",
				hotSlotsBefore, parExec.HotSlots())
		}
	}

	gBad, err := RunT8NBlock(ctx, evmBin, curAlloc, badBlk.Env, badBlk.RawTxsJSON, opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("t8n failed executing invalid block %d: %w", badBlk.Env.Number, err)
	}
	if len(gBad.Rejected) == 0 {
		return nil, fmt.Errorf("t8n did not reject any tx in invalid block %d", badBlk.Env.Number)
	}
	if gBad.Rejected[0].Index != exp.TxIndex || evm.ClassifyRejectionError(gBad.Rejected[0].Error) != exp.Category {
		return nil, fmt.Errorf("t8n reject mismatch: got %+v, want tx=%d cat=%s", gBad.Rejected[0], exp.TxIndex, exp.Category)
	}

	return &InvalidVerifyResult{
		CaseID:          c.ID,
		Mode:            mode,
		Workers:         opts.Workers,
		RejectedHeight:  badBlk.Env.Number,
		RejectedTxIndex: exp.TxIndex,
		Category:        exp.Category,
		CommittedHeight: runner.CommittedHeight,
	}, nil
}
