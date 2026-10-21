package evm

import (
	"bytes"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// optimisticAttempt is an isolated execution of one transaction. It contains
// no writes to the block's canonical StateDB until the ordered resolver accepts
// its delta.
type optimisticAttempt struct {
	result      TxResult
	state       *TxState
	readVersion map[MVKey]uint64
	delta       map[string][]byte
	checkpoints []attemptCheckpoint
	status      optimisticAttemptStatus
}

type gethAttemptRecoveryRunner struct {
	base     *PebbleStateDB
	versions *MVMemory
	config   *params.ChainConfig
	chainID  *big.Int
	env      BlockEnv
	tx       *types.Transaction
	txIndex  int
	gasLimit uint64
}

func (r gethAttemptRecoveryRunner) Resume(*attemptCheckpoint, uint64) (*optimisticAttempt, replayWork, error) {
	// Upstream geth does not expose restoration of its private interpreter
	// state. The focused geth-fork adapter selected by M5.1 will implement this
	// capability; until then every real EVM attempt safely falls back to P0.
	return nil, replayWork{}, errResumeUnsupported
}

func (r gethAttemptRecoveryRunner) FullReplay(incarnation uint64) (*optimisticAttempt, replayWork, error) {
	meter := new(attemptReplayMeter)
	attempt, err := executeOptimisticAttemptMeasured(r.base, r.versions, r.config, r.chainID, r.env, r.tx, r.txIndex, incarnation, r.gasLimit, meter)
	work := replayWork{opcodes: meter.opcodes}
	if attempt != nil {
		work.gas = attempt.result.GasUsed
	}
	return attempt, work, err
}

type attemptReplayMeter struct {
	opcodes uint64
}

type optimisticAttemptStatus uint8

const (
	attemptEstimate optimisticAttemptStatus = iota
	attemptValidated
	attemptAborted
)

// ExecuteBlock runs independent transaction attempts concurrently and accepts
// their state effects in canonical transaction order. A stale attempt is
// discarded and rerun against the latest accepted state before it can commit.
func (e *ParallelExecutor) ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error) {
	totalStarted := time.Now()
	stateDB, ok := base.(*PebbleStateDB)
	if !ok {
		return nil, fmt.Errorf("parallel executor requires *PebbleStateDB, got %T", base)
	}
	if e.ChainConfig == nil || e.ChainID == nil {
		return nil, fmt.Errorf("parallel executor requires chain config and chain ID")
	}
	if env.GasLimit == 0 {
		return nil, fmt.Errorf("block gas limit must be greater than zero")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}
	// Keep the certified, allocation-light transfer path for blocks consisting
	// entirely of conflict-free S candidates. General EVM transactions use the
	// isolated optimistic attempts below.
	analyzeStart := time.Now()
	allStatic := len(txs) > 0
	staticAccesses := make([]staticTransfer, len(txs))
	for i, tx := range txs {
		access, ok := e.staticTransferAccess(base, tx, env.BaseFee)
		if !ok {
			allStatic = false
			break
		}
		staticAccesses[i] = access
	}
	preAnalyzeNS := time.Since(analyzeStart).Nanoseconds()
	if allStatic {
		result, err := e.executeStaticBlock(base, env, txs, staticAccesses)
		if err == nil {
			result.Metrics["phase_analyze_ns"] = float64(preAnalyzeNS)
			result.Metrics["phase_total_ns"] += float64(preAnalyzeNS)
		}
		return result, err
	}
	workers := e.Workers
	if workers < 1 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers < 1 {
		workers = 1
	}
	result := &BlockResult{Results: make([]TxResult, len(txs)), Writes: make(map[string][]byte), Metrics: make(map[string]float64)}
	var checkpointMetrics CheckpointMetrics
	versions := NewMVMemory()
	remainingGas := env.GasLimit
	analyzeNS, executeNS, validateNS, resolveNS := preAnalyzeNS, int64(0), int64(0), int64(0)
	var aborts, retries, parallelTxs, staticTxs, opaqueTxs float64

	// Bounded waves limit speculative memory while keeping enough independent
	// transactions in flight to amortize worker scheduling overhead.
	waveSize := workers * 8
	if waveSize < workers || waveSize < 1 { // multiplication overflow guard
		waveSize = workers
	}
	for start := 0; start < len(txs); start += waveSize {
		end := start + waveSize
		if end > len(txs) {
			end = len(txs)
		}
		wave := txs[start:end]
		attempts := make([]*optimisticAttempt, len(wave))
		for _, tx := range wave {
			analyzeStart = time.Now()
			if _, isStatic := e.staticTransferAccess(stateDB, tx, env.BaseFee); isStatic {
				staticTxs++
			} else {
				opaqueTxs++
			}
			analyzeNS += time.Since(analyzeStart).Nanoseconds()
		}

		workerCount := workers
		if workerCount > len(wave) {
			workerCount = len(wave)
		}
		type job struct{ index int }
		jobs := make(chan job)
		var wg sync.WaitGroup
		for worker := 0; worker < workerCount; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					attempt, err := executeOptimisticAttempt(stateDB, versions, e.ChainConfig, e.ChainID, env, wave[item.index], start+item.index, 1, env.GasLimit)
					if err != nil {
						// Store the error as an invalid result; the resolver reports it
						// deterministically in transaction order.
						attempt = &optimisticAttempt{result: TxResult{Index: start + item.index, Invalid: true, ErrMsg: err.Error()}}
					}
					attempts[item.index] = attempt
				}
			}()
		}
		executeStart := time.Now()
		for i := range wave {
			jobs <- job{index: i}
		}
		close(jobs)
		wg.Wait()
		executeNS += time.Since(executeStart).Nanoseconds()
		if workerCount > 1 && len(wave) > 1 {
			parallelTxs += float64(len(wave))
		}

		// The resolver is the only writer to the canonical StateDB and always
		// advances in original block order, regardless of worker completion.
		for i, speculative := range attempts {
			tx := wave[i]
			if speculative == nil {
				return nil, fmt.Errorf("transaction %d produced no speculative result", start+i)
			}
			recordAttemptCheckpoints(&checkpointMetrics, speculative)
			mustRetry := false
			if tx != nil && tx.Gas() > remainingGas {
				mustRetry = true
			}
			validateStart := time.Now()
			if !versions.Validate(speculative.readVersion) {
				mustRetry = true
			}
			validateNS += time.Since(validateStart).Nanoseconds()
			accepted := speculative
			if mustRetry {
				speculative.status = attemptAborted
				aborts++
				retryStart := time.Now()
				failedRead := firstInvalidRead(speculative, versions)
				runner := gethAttemptRecoveryRunner{
					base: stateDB, versions: versions, config: e.ChainConfig, chainID: e.ChainID,
					env: env, tx: tx, txIndex: start + i, gasLimit: remainingGas,
				}
				recovery, retryErr := recoverOptimisticAttempt(versions, speculative, failedRead, 2, runner)
				executeNS += time.Since(retryStart).Nanoseconds()
				if retryErr != nil {
					return nil, fmt.Errorf("retry transaction %d: %w", start+i, retryErr)
				}
				accepted = recovery.attempt
				recordAttemptCheckpoints(&checkpointMetrics, accepted)
				retries += float64(recovery.retries)
				checkpointMetrics.ReplayedOpcodes += recovery.replayedOpcodes
				checkpointMetrics.ReplayedGas += recovery.replayedGas
				checkpointMetrics.ReplayNanoseconds += recovery.replayNanoseconds
				checkpointMetrics.Retries += uint64(recovery.retries)
				checkpointMetrics.Resumes += recovery.resumes
				checkpointMetrics.FullFallbacks += recovery.fullFallbacks
				checkpointMetrics.CheckpointsReused += uint64(len(recovery.reusedPositions))
				checkpointMetrics.ReusedPositions = append(checkpointMetrics.ReusedPositions, recovery.reusedPositions...)
			}
			accepted.status = attemptValidated
			accepted.result.Index = start + i
			result.Results[start+i] = accepted.result
			if accepted.result.Invalid {
				continue
			}
			if accepted.result.GasUsed > remainingGas {
				return nil, fmt.Errorf("transaction %d consumed %d gas with %d remaining", start+i, accepted.result.GasUsed, remainingGas)
			}
			resolveStart := time.Now()
			if err := stateDB.ApplyPendingWrites(accepted.delta); err != nil {
				return nil, fmt.Errorf("apply transaction %d delta: %w", start+i, err)
			}
			versions.CommitTransaction(start+i, accepted.state.Incarnation, accepted.state.VersionedWrites())
			resolveNS += time.Since(resolveStart).Nanoseconds()
			remainingGas -= accepted.result.GasUsed
			result.GasUsed += accepted.result.GasUsed
		}
	}

	prepareStart := time.Now()
	copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect parallel state writes: %w", err)
	}
	result.Writes = writes
	result.Metrics["phase_commit_prepare_ns"] = float64(time.Since(prepareStart).Nanoseconds())
	result.Metrics["tx_total"] = float64(len(txs))
	result.Metrics["tx_class_s"] = staticTxs
	result.Metrics["tx_class_p"] = 0
	result.Metrics["tx_class_o"] = opaqueTxs
	result.Metrics["worker_count"] = float64(workers)
	result.Metrics["tx_parallel"] = parallelTxs
	result.Metrics["incarnation_total"] = float64(len(txs)) + retries
	result.Metrics["abort_total"] = aborts
	result.Metrics["retry_total"] = retries
	result.Metrics["phase_analyze_ns"] = float64(analyzeNS)
	result.Metrics["phase_execute_ns"] = float64(executeNS)
	result.Metrics["phase_validate_ns"] = float64(validateNS)
	result.Metrics["phase_resolve_ns"] = float64(resolveNS)
	result.Metrics["phase_total_ns"] = float64(time.Since(totalStarted).Nanoseconds())
	checkpointMetrics.Aborts = uint64(aborts)
	result.Metrics["checkpoint_replayed_opcodes"] = float64(checkpointMetrics.ReplayedOpcodes)
	result.Metrics["checkpoint_replayed_gas"] = float64(checkpointMetrics.ReplayedGas)
	result.Metrics["checkpoint_replay_ns"] = float64(checkpointMetrics.ReplayNanoseconds)
	result.Metrics["checkpoint_created"] = float64(checkpointMetrics.CheckpointsCreated)
	result.Metrics["checkpoint_reused"] = float64(checkpointMetrics.CheckpointsReused)
	result.Metrics["checkpoint_memory_bytes"] = float64(checkpointMetrics.CheckpointMemoryBytes)
	result.Metrics["checkpoint_resume_total"] = float64(checkpointMetrics.Resumes)
	result.Metrics["checkpoint_full_fallback_total"] = float64(checkpointMetrics.FullFallbacks)
	result.CheckpointMetrics = checkpointMetrics
	return result, nil
}

func recordAttemptCheckpoints(metrics *CheckpointMetrics, attempt *optimisticAttempt) {
	if metrics == nil || attempt == nil {
		return
	}
	metrics.CheckpointsCreated += uint64(len(attempt.checkpoints))
	for i := range attempt.checkpoints {
		metrics.CheckpointMemoryBytes += attempt.checkpoints[i].execution.EstimatedMemoryBytes()
	}
}

func executeOptimisticAttempt(base *PebbleStateDB, versions *MVMemory, config *params.ChainConfig, chainID *big.Int, env BlockEnv, tx *types.Transaction, txIndex int, incarnation uint64, gasLimit uint64) (*optimisticAttempt, error) {
	return executeOptimisticAttemptMeasured(base, versions, config, chainID, env, tx, txIndex, incarnation, gasLimit, nil)
}

func executeOptimisticAttemptMeasured(base *PebbleStateDB, versions *MVMemory, config *params.ChainConfig, chainID *big.Int, env BlockEnv, tx *types.Transaction, txIndex int, incarnation uint64, gasLimit uint64, meter *attemptReplayMeter) (*optimisticAttempt, error) {
	attempt := &optimisticAttempt{result: TxResult{Index: txIndex}, status: attemptEstimate}
	if tx == nil {
		attempt.result.Invalid = true
		attempt.result.ErrMsg = "nil transaction"
		return attempt, nil
	}
	if config == nil || chainID == nil {
		return nil, fmt.Errorf("optimistic attempt requires chain config and chain ID")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}
	before, err := base.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("snapshot pre-transaction writes: %w", err)
	}
	state := newTxState(base, txIndex, incarnation, nil, nil)
	attempt.state = state

	signer := types.LatestSignerForChainID(chainID)
	msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
	if err != nil {
		attempt.result.Invalid = true
		attempt.result.ErrMsg = err.Error()
		return attempt, nil
	}
	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     env.GetHash,
		Coinbase:    env.Coinbase,
		BlockNumber: big.NewInt(env.Height),
		Time:        env.Time,
		Difficulty:  new(big.Int),
		Random:      &env.Random,
		BaseFee:     new(big.Int).Set(env.BaseFee),
		BlobBaseFee: new(big.Int),
		GasLimit:    env.GasLimit,
	}
	vmConfig := vm.Config{}
	if meter != nil {
		vmConfig.Tracer = &tracing.Hooks{
			OnOpcode: func(uint64, byte, uint64, uint64, tracing.OpContext, []byte, int, error) {
				meter.opcodes++
			},
		}
	}
	evmEnv := vm.NewEVM(blockContext, state, config, vmConfig)
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))
	logStart := len(state.GetLogs())
	snapshot := state.Snapshot()
	gasPool := new(core.GasPool).AddGas(gasLimit)
	execution, applyErr := core.ApplyMessage(evmEnv, msg, gasPool)
	if applyErr != nil {
		state.RevertToSnapshot(snapshot)
		state.Finalise(true)
		attempt.result.Invalid = true
		attempt.result.ErrMsg = applyErr.Error()
	} else {
		state.Finalise(true)
		attempt.result.GasUsed = execution.UsedGas
		attempt.result.Failed = execution.Err != nil
		if execution.Err != nil {
			attempt.result.ErrMsg = execution.Err.Error()
		}
		attempt.result.ReturnData = append([]byte(nil), execution.ReturnData...)
		for _, entry := range state.GetLogs()[logStart:] {
			copyLog := *entry
			copyLog.TxHash = tx.Hash()
			copyLog.Topics = append([]common.Hash(nil), entry.Topics...)
			copyLog.Data = append([]byte(nil), entry.Data...)
			attempt.result.Logs = append(attempt.result.Logs, &copyLog)
		}
	}

	reads, _ := state.AccessSets()
	attempt.readVersion = versions.Snapshot(reads)
	after, err := state.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect speculative transaction writes: %w", err)
	}
	attempt.delta = diffPendingWrites(before, after)
	return attempt, nil
}

func diffPendingWrites(before, after map[string][]byte) map[string][]byte {
	delta := make(map[string][]byte)
	for key, value := range after {
		previous, exists := before[key]
		if exists && bytes.Equal(previous, value) {
			continue
		}
		delta[key] = append([]byte(nil), value...)
		if value == nil {
			delta[key] = nil
		}
	}
	return delta
}
