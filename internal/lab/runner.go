package lab

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// ChainRunner điều phối thực thi chuỗi block trực tiếp trên internal/evm và internal/store.PebbleDB.
type ChainRunner struct {
	DB              *store.PebbleDB
	Executor        evm.Executor
	Mode            ExecutionMode
	Workers         int
	CommittedHeight uint64
	LatestAppHash   common.Hash
	BlockHashes     map[uint64]common.Hash
}

// NewChainRunner khởi tạo executor của internal/evm theo đúng mode/workers và nạp genesis vào store.PebbleDB.
func NewChainRunner(db *store.PebbleDB, mode ExecutionMode, workers int, recordTrace bool, genesis *evm.LogicalState) (*ChainRunner, error) {
	return NewChainRunnerWithCheckpoints(db, mode, workers, recordTrace, CheckpointOptions{Policy: CheckpointOff}, genesis)
}

// NewChainRunnerWithCheckpoints khởi tạo executor có cấu hình checkpoint Phase 2 và lịch sử sạch đầu lượt.
func NewChainRunnerWithCheckpoints(db *store.PebbleDB, mode ExecutionMode, workers int, recordTrace bool, cpOpts CheckpointOptions, genesis *evm.LogicalState) (*ChainRunner, error) {
	if db == nil {
		return nil, errors.New("pebble db must not be nil")
	}
	if workers < 1 {
		workers = 1
	}
	polName, err := ParseCheckpointPolicy(cpOpts.Policy)
	if err != nil {
		return nil, err
	}
	if polName != CheckpointOff && mode != ModeOCC && mode != ModePredictedOCC {
		return nil, fmt.Errorf("checkpoint policy %q is only supported with mode %q or %q (got %q)", polName, ModeOCC, ModePredictedOCC, mode)
	}
	if polName == CheckpointAdaptive && cpOpts.AdaptiveModel == nil {
		return nil, fmt.Errorf("checkpoint policy %q requires a loaded AdaptiveModel (--model)", CheckpointAdaptive)
	}
	chainCfg := ShanghaiChainConfig(DefaultChainID)
	chainID := new(big.Int).SetUint64(DefaultChainID)

	buildEVMCheckpointCfg := func() (evm.CheckpointConfig, error) {
		evmPol, parseErr := evm.ParseCheckpointPolicy(polName)
		if parseErr != nil {
			return evm.CheckpointConfig{}, parseErr
		}
		interval := cpOpts.Interval
		if (evmPol == evm.CheckpointPolicyFixed || evmPol == evm.CheckpointPolicyHistory || evmPol == evm.CheckpointPolicyAdaptive) && interval == 0 {
			interval = DefaultCheckpointInterval
		}
		return evm.CheckpointConfig{
			Policy:             evmPol,
			Interval:           interval,
			BudgetBytes:        cpOpts.BudgetBytes,
			UseBudget:          true,
			AdaptiveModel:      cpOpts.AdaptiveModel,
			DisableOffObserver: cpOpts.DisableOffObserver,
			HistoryMaxPerTx:    cpOpts.HistoryMaxPerTx,
		}, nil
	}

	var exec evm.Executor
	switch mode {
	case ModeSequential:
		seq := evm.NewSequentialExecutor(chainCfg, chainID)
		seq.StrictInvalidTx = true
		exec = seq
	case ModeOCC:
		par := evm.NewParallelExecutor(chainCfg, chainID, workers)
		par.SlidingWindow = true
		par.StrictInvalidTx = true
		par.RecordTrace = recordTrace
		par.Phase2Mode = cpOpts.Phase2Mode || polName != CheckpointOff
		if par.Phase2Mode {
			cfg, cfgErr := buildEVMCheckpointCfg()
			if cfgErr != nil {
				return nil, cfgErr
			}
			par.Checkpoints = cfg
			par.HistoryTracker = evm.NewCheckpointHistoryTracker(evm.MaxHistoryHotSlots)
			par.AdaptiveTracker = evm.NewAdaptiveHistoryTracker()
		}
		exec = par
	case ModeGrouped:
		grp := evm.NewGroupedExecutor(chainCfg, chainID, workers)
		grp.StrictInvalidTx = true
		grp.RecordTrace = recordTrace
		exec = grp
	case ModePredictedOCC:
		pred := evm.NewPredictedOCCExecutor(chainCfg, chainID, workers)
		pred.StrictInvalidTx = true
		pred.RecordTrace = recordTrace
		pred.Phase2Mode = cpOpts.Phase2Mode || polName != CheckpointOff
		if pred.Phase2Mode {
			cfg, cfgErr := buildEVMCheckpointCfg()
			if cfgErr != nil {
				return nil, cfgErr
			}
			pred.Checkpoints = cfg
			pred.HistoryTracker = evm.NewCheckpointHistoryTracker(evm.MaxHistoryHotSlots)
			pred.AdaptiveTracker = evm.NewAdaptiveHistoryTracker()
		}
		exec = pred
	default:
		return nil, fmt.Errorf("unsupported mode %q", mode)
	}

	seed := evm.NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(genesis); err != nil {
		return nil, fmt.Errorf("seed genesis logical state: %w", err)
	}
	if err := seed.Commit(); err != nil {
		return nil, fmt.Errorf("commit genesis state: %w", err)
	}
	genHash := common.BytesToHash(seed.ComputeAppHash())

	return &ChainRunner{
		DB:              db,
		Executor:        exec,
		Mode:            mode,
		Workers:         workers,
		CommittedHeight: 0,
		LatestAppHash:   genHash,
		BlockHashes:     map[uint64]common.Hash{0: genHash},
	}, nil
}

// ExecuteBlock thực thi một block (trả về elapsedNs chỉ gồm ExecuteBlock; Commit đĩa nằm ngoài khoảng đo).
func (r *ChainRunner) ExecuteBlock(ctx context.Context, env BlockEnv, txs []SignedTx) (*BlockResult, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := env.ValidateShanghai(); err != nil {
		return nil, 0, err
	}

	rawTxs := make([]*types.Transaction, len(txs))
	for i, stx := range txs {
		rawTxs[i] = stx.Tx
	}

	prevAppHash := r.LatestAppHash
	stateDB := evm.NewPebbleStateDB(r.DB)
	evmEnv := evm.BlockEnv{
		Height:       int64(env.Number),
		Time:         env.Timestamp,
		Coinbase:     env.Coinbase,
		GasLimit:     env.GasLimit,
		Random:       env.Random,
		BaseFee:      new(big.Int).Set(env.BaseFee),
		PreviousRoot: prevAppHash.Bytes(),
		GetHash: func(n uint64) common.Hash {
			if n >= env.Number {
				return common.Hash{}
			}
			if env.Number-n > 256 {
				return common.Hash{}
			}
			if env.BlockHashes != nil {
				if h, ok := env.BlockHashes[n]; ok {
					return h
				}
			}
			if h, ok := r.BlockHashes[n]; ok {
				return h
			}
			return common.Hash{}
		},
	}

	t0 := time.Now()
	evmRes, err := r.Executor.ExecuteBlock(stateDB, evmEnv, rawTxs)
	elapsedNs := time.Since(t0).Nanoseconds()
	if err != nil {
		// không commit lên PebbleDB nếu block bị từ chối hoặc lỗi nội bộ
		return nil, elapsedNs, err
	}

	appHash := common.Hash(evmRes.StateRoot)
	receipts, normErr := NormalizeReceipts(txs, evmRes, env.Number, appHash, env.Timestamp)
	if normErr != nil {
		return nil, elapsedNs, fmt.Errorf("normalize receipts block %d: %w", env.Number, normErr)
	}
	txReturnData := make([][]byte, len(evmRes.Results))
	txErrMsgs := make([]string, len(evmRes.Results))
	for i, tr := range evmRes.Results {
		txReturnData[i] = append([]byte(nil), tr.ReturnData...)
		txErrMsgs[i] = tr.ErrMsg
	}

	// ghi bền vững xuống store.PebbleDB (ngoài khoảng đo ExecuteBlock)
	if err := stateDB.Commit(); err != nil {
		return nil, elapsedNs, fmt.Errorf("commit block %d to pebble: %w", env.Number, err)
	}
	r.CommittedHeight = env.Number
	r.LatestAppHash = appHash
	r.BlockHashes[env.Number] = appHash

	return &BlockResult{
		BlockNumber:    env.Number,
		PrevAppHash:    prevAppHash,
		AppHash:        appHash,
		GasUsed:        evmRes.GasUsed,
		Receipts:       receipts,
		TxReturnData:   txReturnData,
		TxErrMsgs:      txErrMsgs,
		OCCStats:       evmRes.OCCStats,
		OCCTrace:       evmRes.OCCTrace,
		Phase2Metrics:  evmRes.Phase2Metrics,
		Phase2Trace:    evmRes.Phase2Trace,
		DecisionTrace:  evmRes.DecisionTrace,
		GroupedStats:   evmRes.GroupedStats,
		GroupedTrace:   evmRes.GroupedTrace,
		PredictedStats: evmRes.PredictedStats,
		PredictedTrace: evmRes.PredictedTrace,
	}, elapsedNs, nil
}

// ExportLogicalState xuất toàn bộ trạng thái logic đã chốt hiện tại từ PebbleDB.
func (r *ChainRunner) ExportLogicalState() (*evm.LogicalState, error) {
	return evm.NewPebbleStateDB(r.DB).ExportLogicalState()
}

// ChainResult lưu đầu ra toàn bộ chuỗi block của một mode thực thi.
type ChainResult struct {
	GenesisAppHash common.Hash
	Blocks         []*BlockResult
	PostStates     []*evm.LogicalState
}

// ExecuteChain chạy toàn bộ ChainCase trên một mode/workers và xuất post-state sau từng block.
func ExecuteChain(ctx context.Context, db *store.PebbleDB, mode ExecutionMode, workers int, recordTrace bool, c *ChainCase) (*ChainResult, error) {
	return ExecuteChainWithCheckpoints(ctx, db, mode, workers, recordTrace, CheckpointOptions{Policy: CheckpointOff}, c)
}

// ExecuteChainWithCheckpoints chạy toàn bộ ChainCase với cấu hình checkpoint Phase 2.
func ExecuteChainWithCheckpoints(ctx context.Context, db *store.PebbleDB, mode ExecutionMode, workers int, recordTrace bool, cpOpts CheckpointOptions, c *ChainCase) (*ChainResult, error) {
	if c == nil {
		return nil, errors.New("chain case must not be nil")
	}
	runner, err := NewChainRunnerWithCheckpoints(db, mode, workers, recordTrace, cpOpts, c.GenesisAlloc)
	if err != nil {
		return nil, err
	}
	out := &ChainResult{
		GenesisAppHash: runner.LatestAppHash,
		Blocks:         make([]*BlockResult, 0, len(c.Blocks)),
		PostStates:     make([]*evm.LogicalState, 0, len(c.Blocks)),
	}
	for i, blk := range c.Blocks {
		res, _, err := runner.ExecuteBlock(ctx, blk.Env, blk.Txs)
		if err != nil {
			return nil, fmt.Errorf("block %d (height=%d): %w", i, blk.Env.Number, err)
		}
		post, err := runner.ExportLogicalState()
		if err != nil {
			return nil, fmt.Errorf("export post-state block %d: %w", i, err)
		}
		out.Blocks = append(out.Blocks, res)
		out.PostStates = append(out.PostStates, post)
	}
	return out, nil
}
