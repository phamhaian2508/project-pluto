package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// OCCHooks cung cấp điểm đồng bộ tất định cho kiểm thử song song (không dùng sleep).
type OCCHooks struct {
	OnAttemptStart         func(ctx context.Context, txIndex, attempt int)
	OnStorageRead          func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64)
	OnAttemptBeforeDeliver func(ctx context.Context, txIndex, attempt int)
}

// OCCBlockStats tổng hợp số liệu retry, fallback và thời gian rỗi worker của một block OCC.
type OCCBlockStats struct {
	Workers           int
	RetriedTxs        int
	TotalRetries      int
	FallbackTxs       int
	WaitWorkerNanos   int64
	WorkerIdleNanos   int64
	CommittedAttempts map[int]int
}

// ConflictDetail ghi nhận khóa xung đột đầu tiên khiến một lần chạy thử bị hủy.
type ConflictDetail struct {
	ReadOrder      int
	Key            MVKey
	ReadVersion    uint64
	CurrentVersion uint64
}

// AttemptTrace lưu dấu vết một lần chạy thử / chạy lại của từng giao dịch.
type AttemptTrace struct {
	TxIndex        int
	TxHash         common.Hash
	Attempt        int
	PrefixIndex    int
	PrefixGen      uint64
	Status         string // "committed", "aborted", "fallback_committed", "rejected"
	AbortReason    string
	FirstConflict  *ConflictDetail
	ReadCount      int
	WriteCount     int
	UsedGas        uint64
	EVMStatus      uint64
	CreatedReceipt bool
	LogCount       int
}

// OCCBlockTrace chứa toàn bộ dấu vết thực thi OCC của một block.
type OCCBlockTrace struct {
	BlockNumber uint64
	Stats       OCCBlockStats
	Attempts    []AttemptTrace
}

func (t *OCCBlockTrace) Format() string {
	if t == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "block=%d workers=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
		t.BlockNumber, t.Stats.Workers, t.Stats.RetriedTxs, t.Stats.TotalRetries, t.Stats.FallbackTxs)
	for _, a := range t.Attempts {
		fmt.Fprintf(&b, "  tx[%d] attempt=%d prefix=%d gen=%d status=%s createdReceipt=%v gasUsed=%d logs=%d",
			a.TxIndex, a.Attempt, a.PrefixIndex, a.PrefixGen, a.Status, a.CreatedReceipt, a.UsedGas, a.LogCount)
		if a.AbortReason != "" {
			fmt.Fprintf(&b, " abortReason=%s", a.AbortReason)
		}
		if a.FirstConflict != nil {
			fc := a.FirstConflict
			fmt.Fprintf(&b, " conflict={order=%d key=%s readVer=%d curVer=%d}",
				fc.ReadOrder, fc.Key.String(), fc.ReadVersion, fc.CurrentVersion)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// prefixSnapshot là ảnh chụp bất biến của trạng thái đã chốt tới giao dịch committedIndex-1.
type prefixSnapshot struct {
	state          *PebbleStateDB
	committedIndex int
	generation     uint64
	versions       map[MVKey]uint64
	remainingGas   uint64
}

func newPrefixSnapshot(committed *PebbleStateDB, versions *MVMemory, committedIndex int, remainingGas uint64) *prefixSnapshot {
	return &prefixSnapshot{
		state:          committed.ForkImmutable(),
		committedIndex: committedIndex,
		generation:     versions.Generation(),
		versions:       versions.CloneVersions(),
		remainingGas:   remainingGas,
	}
}

type windowCaptureSpec struct {
	policyName          string
	effectivePolicy     string
	interval            uint64
	hotSlots            map[MVKey]struct{}
	historyMaxPerTx     int
	observeAdaptive     bool
	calibrateFirstState bool
	adaptiveHistory     *AdaptiveHistorySnapshot
	adaptiveModel       *AdaptiveModel
	store               *CheckpointStore
	countOpcodes        bool
	enableCapture       bool
	recordTrace         bool
}

type windowAttemptOutcome struct {
	txIndex                int
	attempt                int
	txHash                 common.Hash
	prefix                 *prefixSnapshot
	state                  *TxState
	result                 TxResult
	delta                  map[string][]byte
	applyErr               error
	internalErr            error
	opcodeSteps            uint64
	captureRequested       uint64
	captureCreated         uint64
	captureSkippedBudget   uint64
	captureSkippedLowScore uint64
	adaptiveCandidates     uint64
	adaptiveCalibHits      uint64
	adaptiveCalibMisses    uint64
	adaptiveHistoryMisses  uint64
	captureNs              uint64
	scoreNs                uint64
	observedCandidate      *AdaptiveCandidateObservation
	sawUnsupportedOpcode   bool
	calibratedCP           *PurePrefixCheckpoint
	p2Trace                []Phase2TraceEvent
	decisionTrace          []AdaptiveDecisionEvent
}

func runWindowAttempt(
	ctx context.Context,
	txIndex int,
	attempt int,
	tx *types.Transaction,
	prefix *prefixSnapshot,
	env BlockEnv,
	config *params.ChainConfig,
	signer types.Signer,
	hooks *OCCHooks,
) *windowAttemptOutcome {
	return runWindowAttemptWithCapture(ctx, txIndex, attempt, tx, prefix, env, config, signer, hooks, nil)
}

func runWindowAttemptWithCapture(
	ctx context.Context,
	txIndex int,
	attempt int,
	tx *types.Transaction,
	prefix *prefixSnapshot,
	env BlockEnv,
	config *params.ChainConfig,
	signer types.Signer,
	hooks *OCCHooks,
	capSpec *windowCaptureSpec,
) *windowAttemptOutcome {
	out := &windowAttemptOutcome{
		txIndex: txIndex,
		attempt: attempt,
		prefix:  prefix,
		result:  TxResult{Index: txIndex},
	}
	if tx == nil {
		out.applyErr = fmt.Errorf("nil transaction")
		out.result.Invalid = true
		out.result.ErrMsg = "nil transaction"
		return out
	}
	out.txHash = tx.Hash()

	if hooks != nil && hooks.OnAttemptStart != nil {
		hooks.OnAttemptStart(ctx, txIndex, attempt)
	}
	if err := ctx.Err(); err != nil {
		out.internalErr = err
		return out
	}

	state := newTxState(prefix.state, txIndex, uint64(attempt), nil, nil)
	if hooks != nil && hooks.OnStorageRead != nil {
		state.onStorageRead = func(addr common.Address, slot, val common.Hash) {
			key := MVKey{Address: addr, Kind: MVStorage, Slot: slot}
			hooks.OnStorageRead(ctx, txIndex, attempt, addr, slot, val, prefix.versions[key])
		}
	}
	out.state = state

	msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
	if err != nil {
		out.applyErr = err
		out.result.Invalid = true
		out.result.ErrMsg = err.Error()
		return out
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

	vmCfg := vm.Config{}
	var capture *phase2CheckpointCapture
	needCapture := capSpec != nil && ((capSpec.enableCapture && capSpec.store != nil && capSpec.store.BudgetBytes() > 0) || capSpec.observeAdaptive || capSpec.calibrateFirstState)
	if needCapture {
		capture = &phase2CheckpointCapture{
			policyName:          capSpec.policyName,
			effectivePolicy:     capSpec.effectivePolicy,
			interval:            capSpec.interval,
			hotSlots:            capSpec.hotSlots,
			historyMaxPerTx:     capSpec.historyMaxPerTx,
			observeAdaptive:     capSpec.observeAdaptive,
			calibrateFirstState: capSpec.calibrateFirstState,
			adaptiveHistory:     capSpec.adaptiveHistory,
			adaptiveModel:       capSpec.adaptiveModel,
			store:               capSpec.store,
			state:               state,
			prefix:              prefix,
			blockNumber:         uint64(env.Height),
			txIndex:             txIndex,
			txHash:              tx.Hash(),
			attempt:             uint64(attempt),
			recordTrace:         capSpec.recordTrace,
		}
		vmCfg.Tracer = &tracing.Hooks{
			OnEnter: func(depth int, typ byte, from, to common.Address, input []byte, gas uint64, value *big.Int) {
				capture.onEnter(depth, typ, from, to, input, gas, value)
			},
			OnExit: func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
				capture.onExit(depth)
			},
			OnOpcode: func(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
				capture.onOpcode(pc, op, gas, cost, scope, rData, depth, err)
			},
		}
	} else if capSpec != nil && capSpec.countOpcodes {
		vmCfg.Tracer = &tracing.Hooks{
			OnOpcode: func(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
				out.opcodeSteps++
			},
		}
	}

	evmEnv := vm.NewEVM(blockContext, state, config, vmCfg)
	if capture != nil {
		capture.evm = evmEnv
		stateOpsOnly := capSpec.effectivePolicy != "fixed"
		releaseHooks, err := checkoutPreGasCheckpointHooksWithMode(evmEnv, capture, stateOpsOnly)
		if err != nil {
			out.internalErr = fmt.Errorf("tx %d install phase2 pre-gas hooks: %w", txIndex, err)
			return out
		}
		defer releaseHooks()
		defer capture.finalize()
	}
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))

	snap := state.Snapshot()
	gasPool := new(core.GasPool).AddGas(prefix.remainingGas)
	execRes, applyErr := core.ApplyMessage(evmEnv, msg, gasPool)
	if capture != nil {
		capture.finalize()
		out.opcodeSteps = capture.opcodeSteps
		out.captureRequested = capture.captureRequested
		out.captureCreated = capture.captureCreated
		out.captureSkippedBudget = capture.captureSkippedBudget
		out.captureSkippedLowScore = capture.captureSkippedLowScore
		out.adaptiveCandidates = capture.adaptiveCandidates
		out.adaptiveCalibHits = capture.adaptiveCalibHits
		out.adaptiveCalibMisses = capture.adaptiveCalibMisses
		out.adaptiveHistoryMisses = capture.adaptiveHistoryMisses
		out.captureNs = capture.captureNs
		out.scoreNs = capture.scoreNs
		out.observedCandidate = capture.observedCandidate
		out.sawUnsupportedOpcode = capture.sawUnsupportedOpcode
		out.calibratedCP = capture.calibratedCP
		out.p2Trace = capture.traceEvents
		out.decisionTrace = capture.decisionEvents
		if capture.err != nil {
			out.internalErr = fmt.Errorf("tx %d checkpoint capture: %w", txIndex, capture.err)
			return out
		}
	}
	if dbErr := state.Err(); dbErr != nil {
		out.internalErr = fmt.Errorf("tx %d state db error: %w", txIndex, dbErr)
		return out
	}

	if applyErr != nil {
		state.RevertToSnapshot(snap)
		state.Finalise(true)
		out.applyErr = applyErr
		out.result.Invalid = true
		out.result.ErrMsg = applyErr.Error()
	} else {
		state.Finalise(true)
		if dbErr := state.Err(); dbErr != nil {
			out.internalErr = fmt.Errorf("tx %d state db error after finalise: %w", txIndex, dbErr)
			return out
		}
		out.result.GasUsed = execRes.UsedGas
		out.result.Failed = execRes.Err != nil
		if execRes.Err != nil {
			out.result.ErrMsg = execRes.Err.Error()
		}
		out.result.ReturnData = append([]byte(nil), execRes.ReturnData...)
		if tx.To() == nil {
			out.result.ContractAddress = crypto.CreateAddress(msg.From, tx.Nonce())
		}
		for _, entry := range state.GetLogs() {
			copyLog := *entry
			copyLog.TxHash = tx.Hash()
			copyLog.TxIndex = uint(txIndex)
			copyLog.BlockNumber = uint64(env.Height)
			copyLog.BlockTimestamp = env.Time
			topics := make([]common.Hash, len(entry.Topics))
			copy(topics, entry.Topics)
			copyLog.Topics = topics
			copyLog.Data = append([]byte(nil), entry.Data...)
			out.result.Logs = append(out.result.Logs, &copyLog)
		}
		// overlay mới chỉ chứa khóa dirty của chính giao dịch này
		delta, err := state.PendingWrites()
		if err != nil {
			out.internalErr = fmt.Errorf("tx %d collect pending writes: %w", txIndex, err)
			return out
		}
		out.delta = delta
	}

	if hooks != nil && hooks.OnAttemptBeforeDeliver != nil {
		hooks.OnAttemptBeforeDeliver(ctx, txIndex, attempt)
	}
	return out
}

func validateWindowReadsDetailed(outcome *windowAttemptOutcome, versions *MVMemory) (firstConflict *ConflictDetail, abortReason string, earliestInvalidStep int64, staleStorageKeys []MVKey) {
	if outcome.prefix.generation != versions.Generation() {
		return nil, "prefix_generation_changed", -1, nil
	}
	if outcome.state == nil {
		return nil, "", -1, nil
	}
	hasInvalid := false
	earliestInvalidStep = -1
	for order, key := range outcome.state.ReadSet {
		readVer := outcome.prefix.versions[key]
		curVer := versions.Version(key)
		if curVer != readVer {
			step := outcome.state.ReadStep(key)
			if !hasInvalid {
				hasInvalid = true
				earliestInvalidStep = step
				firstConflict = &ConflictDetail{
					ReadOrder:      order,
					Key:            key,
					ReadVersion:    readVer,
					CurrentVersion: curVer,
				}
				abortReason = "stale_read"
			} else if step < earliestInvalidStep {
				earliestInvalidStep = step
			}
			if key.Kind == MVStorage {
				staleStorageKeys = append(staleStorageKeys, key)
			}
		}
	}
	return firstConflict, abortReason, earliestInvalidStep, staleStorageKeys
}

func validateWindowReads(outcome *windowAttemptOutcome, versions *MVMemory) (*ConflictDetail, string) {
	conflict, reason, _, _ := validateWindowReadsDetailed(outcome, versions)
	return conflict, reason
}

func assignSequentialLogIndices(logs []*types.Log, startIndex uint) uint {
	idx := startIndex
	for _, l := range logs {
		if l != nil {
			l.Index = idx
			idx++
		}
	}
	return idx
}

func buildWindowAttemptTrace(out *windowAttemptOutcome, status, abortReason string, conflict *ConflictDetail, createdReceipt bool) AttemptTrace {
	var evmStatus uint64
	if createdReceipt && !out.result.Failed && !out.result.Invalid {
		evmStatus = types.ReceiptStatusSuccessful
	}
	var readCnt, writeCnt int
	if out.state != nil {
		readCnt = len(out.state.ReadSet)
		writeCnt = len(out.state.WriteSet)
	}
	return AttemptTrace{
		TxIndex:        out.txIndex,
		TxHash:         out.txHash,
		Attempt:        out.attempt,
		PrefixIndex:    out.prefix.committedIndex,
		PrefixGen:      out.prefix.generation,
		Status:         status,
		AbortReason:    abortReason,
		FirstConflict:  conflict,
		ReadCount:      readCnt,
		WriteCount:     writeCnt,
		UsedGas:        out.result.GasUsed,
		EVMStatus:      evmStatus,
		CreatedReceipt: createdReceipt,
		LogCount:       len(out.result.Logs),
	}
}

func executeSerialTxOnCommitted(
	stateDB *PebbleStateDB,
	env BlockEnv,
	config *params.ChainConfig,
	signer types.Signer,
	tx *types.Transaction,
	txIndex int,
	remainingGas uint64,
	blockLogIndex uint,
) (TxResult, uint, error, error) {
	tr := TxResult{Index: txIndex}
	if tx == nil {
		err := fmt.Errorf("nil transaction")
		tr.Invalid, tr.ErrMsg = true, err.Error()
		return tr, blockLogIndex, err, nil
	}
	msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
	if err != nil {
		tr.Invalid, tr.ErrMsg = true, err.Error()
		return tr, blockLogIndex, err, nil
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
	evmEnv := vm.NewEVM(blockContext, stateDB, config, vm.Config{})
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))

	logStart := len(stateDB.GetLogs())
	snap := stateDB.Snapshot()
	gasPool := new(core.GasPool).AddGas(remainingGas)
	res, applyErr := core.ApplyMessage(evmEnv, msg, gasPool)
	if dbErr := stateDB.Err(); dbErr != nil {
		return tr, blockLogIndex, nil, fmt.Errorf("tx %d state db error: %w", txIndex, dbErr)
	}
	if applyErr != nil {
		stateDB.RevertToSnapshot(snap)
		stateDB.Finalise(true)
		tr.Invalid, tr.ErrMsg = true, applyErr.Error()
		return tr, blockLogIndex, applyErr, nil
	}
	stateDB.Finalise(true)
	if dbErr := stateDB.Err(); dbErr != nil {
		return tr, blockLogIndex, nil, fmt.Errorf("tx %d state db error after finalise: %w", txIndex, dbErr)
	}
	tr.GasUsed = res.UsedGas
	tr.Failed = res.Err != nil
	if res.Err != nil {
		tr.ErrMsg = res.Err.Error()
	}
	tr.ReturnData = append([]byte(nil), res.ReturnData...)
	if tx.To() == nil {
		tr.ContractAddress = crypto.CreateAddress(msg.From, tx.Nonce())
	}
	for _, entry := range stateDB.GetLogs()[logStart:] {
		logCopy := *entry
		logCopy.TxHash = tx.Hash()
		logCopy.TxIndex = uint(txIndex)
		logCopy.Index = blockLogIndex
		logCopy.BlockNumber = uint64(env.Height)
		logCopy.BlockTimestamp = env.Time
		logCopy.Topics = append([]common.Hash(nil), entry.Topics...)
		logCopy.Data = append([]byte(nil), entry.Data...)
		blockLogIndex++
		tr.Logs = append(tr.Logs, &logCopy)
	}
	return tr, blockLogIndex, nil, nil
}

// occCheckpointRuntime gom cấu hình, kho byte, bộ học lịch sử và thống kê Phase 2–3
// dùng chung cho cả ParallelExecutor (occ) lẫn PredictedOCCExecutor (predicted-occ).
type occCheckpointRuntime struct {
	policy          CheckpointPolicy
	policyName      string
	effectivePolicy string
	interval        uint64
	budgetBytes     uint64
	observeAdaptive bool
	store           *CheckpointStore
	p2Metrics       *Phase2CheckpointMetrics
	specCapSpec     *windowCaptureSpec
	replayCapSpec   *windowCaptureSpec
	blockConflicts  map[MVKey]uint64
	adaptiveObs     map[AdaptiveLocationKey]AdaptiveSampleCounts
	p2Trace         []Phase2TraceEvent
	decisionTrace   []AdaptiveDecisionEvent
}

func prepareOCCCheckpointRuntime(
	cfg CheckpointConfig,
	phase2Mode bool,
	recordTrace bool,
	txCount int,
	histTracker *CheckpointHistoryTracker,
	adaptiveTracker *AdaptiveHistoryTracker,
) (*occCheckpointRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	policy := cfg.Policy
	policyName := policy.Phase2Name()
	interval := cfg.Interval
	if (policy == CheckpointPolicyFixed || policy == CheckpointPolicyHistory || policy == CheckpointPolicyAdaptive) && interval == 0 {
		interval = DefaultCheckpointInterval
	}
	budgetBytes := cfg.BudgetBytes
	if !cfg.UseBudget && policy != CheckpointPolicyOff {
		budgetBytes = DefaultCheckpointBudgetBytes
	}

	hasHistory, hotSlots, hotSlotList := histTracker.SnapshotForBlock()
	var adaptiveSnap *AdaptiveHistorySnapshot
	if adaptiveTracker != nil {
		adaptiveSnap = adaptiveTracker.SnapshotForBlock()
	}

	effectivePolicy := "off"
	switch policy {
	case CheckpointPolicyOff:
		effectivePolicy = "off"
	case CheckpointPolicyFixed:
		if !cfg.UseBudget || budgetBytes > 0 {
			effectivePolicy = "fixed"
		}
	case CheckpointPolicyHistory:
		if !cfg.UseBudget || budgetBytes > 0 {
			if !hasHistory {
				// block đầu chưa có lịch sử -> dùng fixed với cùng N
				effectivePolicy = "fixed"
			} else {
				effectivePolicy = "history"
			}
		}
	case CheckpointPolicyAdaptive:
		if !cfg.UseBudget || budgetBytes > 0 {
			if !hasHistory {
				// block đầu chưa có lịch sử -> dùng fixed với cùng N và ngân sách
				effectivePolicy = "fixed"
			} else {
				effectivePolicy = "adaptive"
			}
		}
	default:
		return nil, fmt.Errorf("sliding-window OCC does not support checkpoint policy %q", policy.String())
	}

	var store *CheckpointStore
	if effectivePolicy == "fixed" || effectivePolicy == "history" || effectivePolicy == "adaptive" {
		store = NewCheckpointStore(budgetBytes)
	}
	// M = 0 hành xử hoàn toàn như off (không gắn tracer quan sát adaptive)
	observeAdaptive := policy == CheckpointPolicyAdaptive && (!cfg.UseBudget || budgetBytes > 0)
	offObserverDisabled := policy == CheckpointPolicyOff && cfg.DisableOffObserver
	countOpcodes := !offObserverDisabled && (phase2Mode || policy != CheckpointPolicyOff || cfg.UseBudget || recordTrace)

	p2Metrics := newPhase2CheckpointMetrics(policyName, effectivePolicy, interval, budgetBytes, hotSlotList)
	p2Metrics.OffObserverDisabled = offObserverDisabled

	rt := &occCheckpointRuntime{
		policy:          policy,
		policyName:      policyName,
		effectivePolicy: effectivePolicy,
		interval:        interval,
		budgetBytes:     budgetBytes,
		observeAdaptive: observeAdaptive,
		store:           store,
		p2Metrics:       p2Metrics,
		blockConflicts:  make(map[MVKey]uint64),
		specCapSpec: &windowCaptureSpec{
			policyName:      policyName,
			effectivePolicy: effectivePolicy,
			interval:        interval,
			hotSlots:        hotSlots,
			historyMaxPerTx: cfg.HistoryMaxPerTx,
			observeAdaptive: observeAdaptive,
			adaptiveHistory: adaptiveSnap,
			adaptiveModel:   cfg.AdaptiveModel,
			store:           store,
			countOpcodes:    countOpcodes,
			enableCapture:   store != nil && store.BudgetBytes() > 0,
			recordTrace:     recordTrace,
		},
		replayCapSpec: &windowCaptureSpec{
			policyName:      policyName,
			effectivePolicy: "off",
			// tắt OnOpcode đếm bước trên FullReplay khi DisableOffObserver=true (Phase 4/production) để tránh overhead giả
			countOpcodes:  countOpcodes && !cfg.DisableOffObserver,
			enableCapture: false,
			recordTrace:   false,
		},
	}
	if observeAdaptive {
		rt.adaptiveObs = make(map[AdaptiveLocationKey]AdaptiveSampleCounts)
	}
	if recordTrace {
		rt.p2Trace = make([]Phase2TraceEvent, 0, txCount*4)
		if policy == CheckpointPolicyAdaptive {
			rt.decisionTrace = make([]AdaptiveDecisionEvent, 0, txCount)
		}
	}
	return rt, nil
}

func (rt *occCheckpointRuntime) recordAttemptMetrics(specRes *windowAttemptOutcome, recordTrace bool) {
	if rt == nil || specRes == nil {
		return
	}
	m := rt.p2Metrics
	m.OpcodeSteps += specRes.opcodeSteps
	m.CaptureRequested += specRes.captureRequested
	m.CaptureCreated += specRes.captureCreated
	m.CaptureSkippedBudget += specRes.captureSkippedBudget
	m.CaptureSkippedLowScore += specRes.captureSkippedLowScore
	m.AdaptiveCandidates += specRes.adaptiveCandidates
	m.AdaptiveCalibHits += specRes.adaptiveCalibHits
	m.AdaptiveCalibMisses += specRes.adaptiveCalibMisses
	m.AdaptiveHistoryMisses += specRes.adaptiveHistoryMisses
	m.CaptureNs += specRes.captureNs
	m.ScoreNs += specRes.scoreNs
	if recordTrace {
		if len(specRes.p2Trace) > 0 {
			rt.p2Trace = append(rt.p2Trace, specRes.p2Trace...)
		}
		if len(specRes.decisionTrace) > 0 {
			rt.decisionTrace = append(rt.decisionTrace, specRes.decisionTrace...)
		}
	}
}

func (rt *occCheckpointRuntime) recordValidationLearning(specRes *windowAttemptOutcome, abortReason string, earliestInvalidStep int64, staleStorageKeys []MVKey) {
	if rt == nil {
		return
	}
	measureHist := rt.policy == CheckpointPolicyHistory || rt.policy == CheckpointPolicyAdaptive
	var histStart time.Time
	if measureHist {
		histStart = time.Now()
	}
	// luôn thống kê xung đột storage từ lần thử đầu (mỗi (tx, contract, slot) tối đa 1 lần)
	for _, k := range staleStorageKeys {
		rt.blockConflicts[k]++
	}
	if rt.observeAdaptive && specRes != nil && specRes.observedCandidate != nil {
		obs := specRes.observedCandidate
		cnt := rt.adaptiveObs[obs.Key]
		cnt.Seen++
		if abortReason == "stale_read" && earliestInvalidStep >= 0 && uint64(earliestInvalidStep) == obs.Step && obs.PrefixValid && !specRes.sawUnsupportedOpcode {
			cnt.Useful++
		}
		rt.adaptiveObs[obs.Key] = cnt
	}
	if measureHist {
		if dt := time.Since(histStart).Nanoseconds(); dt > 0 {
			rt.p2Metrics.HistoryUpdateNs += uint64(dt)
		}
	}
}

func (rt *occCheckpointRuntime) tryResumeTx(
	txIndex int,
	tx *types.Transaction,
	specRes *windowAttemptOutcome,
	earliestInvalidStep int64,
	curPrefix *prefixSnapshot,
	versions *MVMemory,
	env BlockEnv,
	chainCfg *params.ChainConfig,
	signer types.Signer,
	recordTrace bool,
	stateDB *PebbleStateDB,
	blockLogIndex *uint,
	remainingGas *uint64,
	result *BlockResult,
	committedAttempts map[int]int,
	attempts *[]AttemptTrace,
) (finalRes TxResult, resumed bool, retryAttempt int, err error) {
	retryAttempt = 2
	if rt.store == nil {
		rt.p2Metrics.addFallback("policy_off")
		return TxResult{}, false, retryAttempt, nil
	}
	selStart := time.Now()
	cps := rt.store.CheckpointsForTx(txIndex)
	selectedCP, rejectedPrefix, selFallback := selectPurePrefixCheckpoint(cps, earliestInvalidStep, versions)
	if dt := time.Since(selStart).Nanoseconds(); dt > 0 {
		rt.p2Metrics.SelectNs += uint64(dt)
	}
	rt.p2Metrics.RestoreRejectedPrefix += rejectedPrefix

	if selectedCP == nil {
		rt.p2Metrics.addFallback(selFallback)
		if recordTrace {
			rt.p2Trace = append(rt.p2Trace, Phase2TraceEvent{
				Policy:      rt.policyName,
				BlockNumber: uint64(env.Height),
				TxIndex:     txIndex,
				TxHash:      specRes.txHash.Hex(),
				Attempt:     1,
				Event:       "fallback",
				Reason:      selFallback,
			})
		}
		return TxResult{}, false, retryAttempt, nil
	}

	if recordTrace {
		selEv := Phase2TraceEvent{
			Policy:       rt.policyName,
			BlockNumber:  uint64(env.Height),
			TxIndex:      txIndex,
			TxHash:       specRes.txHash.Hex(),
			Attempt:      1,
			Event:        "checkpoint_selected",
			Step:         selectedCP.NextStep,
			PC:           selectedCP.PC,
			Opcode:       vm.OpCode(selectedCP.Opcode).String(),
			GasRemaining: selectedCP.Gas,
			Reason:       selectedCP.Reason,
		}
		if selectedCP.TriggerSlot != nil {
			selEv.SlotKey = selectedCP.TriggerSlot.String()
		}
		rt.p2Trace = append(rt.p2Trace, selEv)
		rt.p2Trace = append(rt.p2Trace, Phase2TraceEvent{
			Policy:          rt.policyName,
			BlockNumber:     uint64(env.Height),
			TxIndex:         txIndex,
			TxHash:          specRes.txHash.Hex(),
			Attempt:         2,
			Event:           "resume_start",
			Step:            selectedCP.NextStep,
			PC:              selectedCP.PC,
			Opcode:          vm.OpCode(selectedCP.Opcode).String(),
			GasRemaining:    selectedCP.Gas,
			ResumedFromStep: selectedCP.NextStep,
			ResumedFromPC:   selectedCP.PC,
		})
	}

	rt.p2Metrics.ResumeAttempted++
	resStart := time.Now()
	resumedOut, resFallback, resSteps, resErr := resumePurePrefixAttempt(txIndex, 2, tx, curPrefix, env, chainCfg, signer, selectedCP)
	if dt := time.Since(resStart).Nanoseconds(); dt > 0 {
		rt.p2Metrics.RestoreNs += uint64(dt)
	}
	rt.p2Metrics.OpcodeSteps += resSteps

	if resErr == nil {
		rt.store.ReleaseTx(txIndex)
		if err := stateDB.ApplyPendingWrites(resumedOut.delta); err != nil {
			return TxResult{}, false, retryAttempt, fmt.Errorf("apply resumed tx %d delta: %w", txIndex, err)
		}
		versions.CommitTransaction(txIndex, uint64(resumedOut.attempt), resumedOut.state.VersionedWrites())
		*blockLogIndex = assignSequentialLogIndices(resumedOut.result.Logs, *blockLogIndex)
		*remainingGas -= resumedOut.result.GasUsed
		result.GasUsed += resumedOut.result.GasUsed
		committedAttempts[txIndex] = resumedOut.attempt
		rt.p2Metrics.ResumeCommitted++
		rt.p2Metrics.PrefixStepsSaved += selectedCP.NextStep
		if recordTrace {
			*attempts = append(*attempts, buildWindowAttemptTrace(resumedOut, "committed", "", nil, true))
			rt.p2Trace = append(rt.p2Trace, Phase2TraceEvent{
				Policy:          rt.policyName,
				BlockNumber:     uint64(env.Height),
				TxIndex:         txIndex,
				TxHash:          resumedOut.txHash.Hex(),
				Attempt:         resumedOut.attempt,
				Event:           "commit",
				Resumed:         true,
				ResumedFromStep: selectedCP.NextStep,
				ResumedFromPC:   selectedCP.PC,
				OpcodeSteps:     resSteps,
			})
		}
		return resumedOut.result, true, retryAttempt, nil
	}
	if errors.Is(resErr, errResumeUnsupported) {
		rt.p2Metrics.ResumeCancelled++
		if resFallback == "context_mismatch" || resFallback == "prep_failed" || resFallback == "target_missing" || resFallback == "target_empty_code" {
			rt.p2Metrics.RestoreRejectedPrefix++
		}
		rt.p2Metrics.addFallback(resFallback)
		retryAttempt = 3
		if recordTrace {
			rt.p2Trace = append(rt.p2Trace, Phase2TraceEvent{
				Policy:          rt.policyName,
				BlockNumber:     uint64(env.Height),
				TxIndex:         txIndex,
				TxHash:          specRes.txHash.Hex(),
				Attempt:         2,
				Event:           "fallback",
				Reason:          resFallback,
				ResumedFromStep: selectedCP.NextStep,
				ResumedFromPC:   selectedCP.PC,
				OpcodeSteps:     resSteps,
			})
		}
		return TxResult{}, false, retryAttempt, nil
	}
	rt.store.ReleaseTx(txIndex)
	return TxResult{}, false, retryAttempt, resErr
}

func (rt *occCheckpointRuntime) finalizeAndPublish(
	result *BlockResult,
	histTracker *CheckpointHistoryTracker,
	adaptiveTracker *AdaptiveHistoryTracker,
	retriedTxs, totalRetries int,
	recordTrace bool,
) {
	if rt.store != nil {
		_, peakBytes, _, _ := rt.store.Stats()
		rt.p2Metrics.AccountedPeakBytes = peakBytes
		rt.store.ReleaseAll()
		remAfter, _, _, _ := rt.store.Stats()
		rt.p2Metrics.AccountedRemainingBytes = remAfter
	}

	// chỉ chốt lịch sử cho block kế tiếp sau khi block hiện tại hoàn tất hợp lệ
	measureHist := rt.policy == CheckpointPolicyHistory || rt.policy == CheckpointPolicyAdaptive
	var histStart time.Time
	if measureHist {
		histStart = time.Now()
	}
	if histTracker != nil {
		histTracker.RecordBlockConflicts(rt.blockConflicts)
	}
	var adaptiveMem uint64
	if rt.observeAdaptive && adaptiveTracker != nil {
		adaptiveMem = adaptiveTracker.PublishBlock(rt.adaptiveObs)
	}
	if measureHist {
		if dt := time.Since(histStart).Nanoseconds(); dt > 0 {
			rt.p2Metrics.HistoryUpdateNs += uint64(dt)
		}
	}
	if rt.policy == CheckpointPolicyHistory && histTracker != nil {
		rt.p2Metrics.HistoryMemoryBytes = histTracker.EstimatedMemoryBytes()
	} else if rt.policy == CheckpointPolicyAdaptive {
		var hMem uint64
		if histTracker != nil {
			hMem = histTracker.EstimatedMemoryBytes()
		}
		rt.p2Metrics.HistoryMemoryBytes = hMem + adaptiveMem
	}

	result.Phase2Metrics = rt.p2Metrics
	if recordTrace {
		result.Phase2Trace = rt.p2Trace
		result.DecisionTrace = rt.decisionTrace
	}

	var totalFallbacks uint64
	for _, cnt := range rt.p2Metrics.FallbackReasons {
		totalFallbacks += cnt
	}
	result.CheckpointMetrics = CheckpointMetrics{
		CheckpointsCreated:    rt.p2Metrics.CaptureCreated,
		CheckpointsReused:     rt.p2Metrics.ResumeCommitted,
		CheckpointMemoryBytes: rt.p2Metrics.AccountedPeakBytes,
		Aborts:                uint64(retriedTxs),
		Retries:               uint64(totalRetries),
		Resumes:               rt.p2Metrics.ResumeCommitted,
		FullFallbacks:         totalFallbacks,
	}

	result.Metrics["checkpoint_created"] = float64(rt.p2Metrics.CaptureCreated)
	result.Metrics["checkpoint_skipped_budget"] = float64(rt.p2Metrics.CaptureSkippedBudget)
	result.Metrics["checkpoint_skipped_low_score"] = float64(rt.p2Metrics.CaptureSkippedLowScore)
	result.Metrics["checkpoint_adaptive_candidates"] = float64(rt.p2Metrics.AdaptiveCandidates)
	result.Metrics["checkpoint_adaptive_calib_hits"] = float64(rt.p2Metrics.AdaptiveCalibHits)
	result.Metrics["checkpoint_adaptive_calib_misses"] = float64(rt.p2Metrics.AdaptiveCalibMisses)
	result.Metrics["checkpoint_resume_attempted"] = float64(rt.p2Metrics.ResumeAttempted)
	result.Metrics["checkpoint_resume_committed"] = float64(rt.p2Metrics.ResumeCommitted)
	result.Metrics["checkpoint_accounted_peak_bytes"] = float64(rt.p2Metrics.AccountedPeakBytes)
	result.Metrics["checkpoint_opcode_steps"] = float64(rt.p2Metrics.OpcodeSteps)
	result.Metrics["checkpoint_prefix_steps_saved"] = float64(rt.p2Metrics.PrefixStepsSaved)
	result.Metrics["checkpoint_score_ns"] = float64(rt.p2Metrics.ScoreNs)
	result.Metrics["checkpoint_history_update_ns"] = float64(rt.p2Metrics.HistoryUpdateNs)
	result.Metrics["checkpoint_history_memory_bytes"] = float64(rt.p2Metrics.HistoryMemoryBytes)
}

// executeSlidingWindowOCC chạy OCC cửa sổ trượt rộng W = workers trên nền PebbleStateDB + MVMemory,
// tích hợp đầy đủ CheckpointStore ngân sách cứng M byte và chính sách off / fixed / history / adaptive xuyên block.
func (e *ParallelExecutor) executeSlidingWindowOCC(stateDB *PebbleStateDB, env BlockEnv, txs []*types.Transaction, workers int) (*BlockResult, error) {
	totalStarted := time.Now()
	if e.HistoryTracker == nil {
		e.HistoryTracker = NewCheckpointHistoryTracker(MaxHistoryHotSlots)
	}
	if e.AdaptiveTracker == nil {
		e.AdaptiveTracker = NewAdaptiveHistoryTracker()
	}

	cpRT, err := prepareOCCCheckpointRuntime(e.Checkpoints, e.Phase2Mode, e.RecordTrace, len(txs), e.HistoryTracker, e.AdaptiveTracker)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		if cpRT.store != nil {
			cpRT.store.ReleaseAll()
		}
	}()

	signer := types.MakeSigner(e.ChainConfig, big.NewInt(env.Height), env.Time)
	versions := NewMVMemory()
	remainingGas := env.GasLimit
	var blockLogIndex uint

	stats := OCCBlockStats{
		Workers:           workers,
		CommittedAttempts: make(map[int]int, len(txs)),
	}
	var attempts []AttemptTrace
	if e.RecordTrace {
		attempts = make([]AttemptTrace, 0, len(txs))
	}

	result := &BlockResult{
		Results: make([]TxResult, len(txs)),
		Writes:  make(map[string][]byte),
		Metrics: make(map[string]float64),
	}

	resultsCh := make(chan *windowAttemptOutcome, len(txs)+workers)
	completed := make(map[int]*windowAttemptOutcome, workers)
	curPrefix := newPrefixSnapshot(stateDB, versions, 0, remainingGas)

	activeWorkers := 0
	lastIdleSample := time.Now()
	sampleWorkerIdle := func(now time.Time) {
		if activeWorkers < workers {
			dt := now.Sub(lastIdleSample).Nanoseconds()
			if dt > 0 {
				stats.WorkerIdleNanos += dt * int64(workers-activeWorkers)
			}
		}
		lastIdleSample = now
	}

	nextCommit := 0
	nextDispatch := 0

	dispatchWindow := func() {
		for nextDispatch < len(txs) && nextDispatch < nextCommit+workers {
			txIdx := nextDispatch
			tx := txs[txIdx]
			prefix := curPrefix
			nextDispatch++

			sampleWorkerIdle(time.Now())
			activeWorkers++
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := runWindowAttemptWithCapture(runCtx, txIdx, 1, tx, prefix, env, e.ChainConfig, signer, e.OCCHooks, cpRT.specCapSpec)
				select {
				case resultsCh <- res:
				case <-runCtx.Done():
				}
			}()
		}
	}

	for nextCommit < len(txs) {
		dispatchWindow()

		specRes, ok := completed[nextCommit]
		for !ok {
			incoming := <-resultsCh
			sampleWorkerIdle(time.Now())
			activeWorkers--
			completed[incoming.txIndex] = incoming
			if incoming.txIndex == nextCommit {
				specRes = incoming
				ok = true
			}
		}
		delete(completed, nextCommit)

		cpRT.recordAttemptMetrics(specRes, e.RecordTrace)

		if specRes.internalErr != nil {
			cpRT.store.ReleaseTx(nextCommit)
			return nil, specRes.internalErr
		}

		tx := txs[nextCommit]
		conflict, abortReason, earliestInvalidStep, staleStorageKeys := validateWindowReadsDetailed(specRes, versions)
		cpRT.recordValidationLearning(specRes, abortReason, earliestInvalidStep, staleStorageKeys)
		gasExceedsRemaining := tx != nil && tx.Gas() > remainingGas
		if abortReason == "" && gasExceedsRemaining && specRes.applyErr == nil {
			// không reject sớm gas_limit_reached trước Geth preCheck -> chạy lại trên curPrefix để ra đúng lỗi chuẩn (nonce/funds/gas)
			abortReason = "gas_limit_changed"
		}

		var finalRes TxResult
		if abortReason == "" {
			cpRT.store.ReleaseTx(nextCommit)
			if specRes.applyErr != nil {
				if e.RecordTrace {
					attempts = append(attempts, buildWindowAttemptTrace(specRes, "rejected", "", nil, false))
				}
				if e.StrictInvalidTx {
					return nil, &TxRejectedError{
						Index:    nextCommit,
						TxHash:   specRes.txHash,
						Category: ClassifyRejectionError(specRes.applyErr.Error()),
						Err:      specRes.applyErr,
					}
				}
				finalRes = specRes.result
			} else {
				if err := stateDB.ApplyPendingWrites(specRes.delta); err != nil {
					return nil, fmt.Errorf("apply tx %d delta: %w", nextCommit, err)
				}
				versions.CommitTransaction(nextCommit, uint64(specRes.attempt), specRes.state.VersionedWrites())
				blockLogIndex = assignSequentialLogIndices(specRes.result.Logs, blockLogIndex)
				remainingGas -= specRes.result.GasUsed
				result.GasUsed += specRes.result.GasUsed
				finalRes = specRes.result
				stats.CommittedAttempts[nextCommit] = specRes.attempt
				if e.RecordTrace {
					attempts = append(attempts, buildWindowAttemptTrace(specRes, "committed", "", nil, true))
					cpRT.p2Trace = append(cpRT.p2Trace, Phase2TraceEvent{
						Policy:      cpRT.policyName,
						BlockNumber: uint64(env.Height),
						TxIndex:     nextCommit,
						TxHash:      specRes.txHash.Hex(),
						Attempt:     specRes.attempt,
						Event:       "commit",
						Resumed:     false,
						OpcodeSteps: specRes.opcodeSteps,
					})
				}
			}
		} else {
			if e.RecordTrace {
				attempts = append(attempts, buildWindowAttemptTrace(specRes, "aborted", abortReason, conflict, false))
				conflictKey := ""
				if conflict != nil {
					conflictKey = conflict.Key.String()
				}
				cpRT.p2Trace = append(cpRT.p2Trace, Phase2TraceEvent{
					Policy:              cpRT.policyName,
					BlockNumber:         uint64(env.Height),
					TxIndex:             nextCommit,
					TxHash:              specRes.txHash.Hex(),
					Attempt:             specRes.attempt,
					Event:               "conflict",
					Reason:              abortReason,
					ConflictKey:         conflictKey,
					EarliestInvalidStep: earliestInvalidStep,
				})
			}
			stats.RetriedTxs++
			stats.TotalRetries++

			var resumedRes TxResult
			var resumed bool
			retryAttempt := specRes.attempt + 1
			if !gasExceedsRemaining {
				var resErr error
				resumedRes, resumed, retryAttempt, resErr = cpRT.tryResumeTx(
					nextCommit, tx, specRes, earliestInvalidStep, curPrefix, versions,
					env, e.ChainConfig, signer, e.RecordTrace, stateDB,
					&blockLogIndex, &remainingGas, result, stats.CommittedAttempts, &attempts,
				)
				if resErr != nil {
					return nil, resErr
				}
			}
			if resumed {
				finalRes = resumedRes
			} else {
				// giải phóng toàn bộ checkpoint của tx trước khi chạy FullReplay
				cpRT.store.ReleaseTx(nextCommit)
				retryRes := runWindowAttemptWithCapture(runCtx, nextCommit, retryAttempt, tx, curPrefix, env, e.ChainConfig, signer, e.OCCHooks, cpRT.replayCapSpec)
				cpRT.p2Metrics.OpcodeSteps += retryRes.opcodeSteps
				if retryRes.internalErr != nil {
					return nil, retryRes.internalErr
				}
				if retryRes.applyErr != nil {
					if e.RecordTrace {
						attempts = append(attempts, buildWindowAttemptTrace(retryRes, "rejected", "", nil, false))
					}
					if e.StrictInvalidTx {
						return nil, &TxRejectedError{
							Index:    nextCommit,
							TxHash:   retryRes.txHash,
							Category: ClassifyRejectionError(retryRes.applyErr.Error()),
							Err:      retryRes.applyErr,
						}
					}
					finalRes = retryRes.result
				} else {
					if err := stateDB.ApplyPendingWrites(retryRes.delta); err != nil {
						return nil, fmt.Errorf("apply retried tx %d delta: %w", nextCommit, err)
					}
					versions.CommitTransaction(nextCommit, uint64(retryRes.attempt), retryRes.state.VersionedWrites())
					blockLogIndex = assignSequentialLogIndices(retryRes.result.Logs, blockLogIndex)
					remainingGas -= retryRes.result.GasUsed
					result.GasUsed += retryRes.result.GasUsed
					finalRes = retryRes.result
					stats.CommittedAttempts[nextCommit] = retryRes.attempt
					if e.RecordTrace {
						attempts = append(attempts, buildWindowAttemptTrace(retryRes, "committed", "", nil, true))
						cpRT.p2Trace = append(cpRT.p2Trace, Phase2TraceEvent{
							Policy:      cpRT.policyName,
							BlockNumber: uint64(env.Height),
							TxIndex:     nextCommit,
							TxHash:      retryRes.txHash.Hex(),
							Attempt:     retryRes.attempt,
							Event:       "commit",
							Resumed:     false,
							OpcodeSteps: retryRes.opcodeSteps,
						})
					}
				}
			}
		}

		finalRes.Index = nextCommit
		result.Results[nextCommit] = finalRes
		nextCommit++
		if nextCommit < len(txs) {
			curPrefix = newPrefixSnapshot(stateDB, versions, nextCommit, remainingGas)
		}
	}

	// chờ toàn bộ worker thoát hẳn trước khi chốt thống kê kho checkpoint
	wg.Wait()
	sampleWorkerIdle(time.Now())

	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect occ state writes: %w", err)
	}
	copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))

	cpRT.finalizeAndPublish(result, e.HistoryTracker, e.AdaptiveTracker, stats.RetriedTxs, stats.TotalRetries, e.RecordTrace)

	result.Writes = writes
	result.OCCStats = &stats
	if e.RecordTrace {
		result.OCCTrace = &OCCBlockTrace{
			BlockNumber: uint64(env.Height),
			Stats:       stats,
			Attempts:    attempts,
		}
	}

	result.Metrics["tx_total"] = float64(len(txs))
	result.Metrics["worker_count"] = float64(workers)
	result.Metrics["retried_txs"] = float64(stats.RetriedTxs)
	result.Metrics["total_retries"] = float64(stats.TotalRetries)
	result.Metrics["fallback_txs"] = float64(stats.FallbackTxs)
	result.Metrics["worker_idle_ns"] = float64(stats.WorkerIdleNanos)
	result.Metrics["phase_total_ns"] = float64(time.Since(totalStarted).Nanoseconds())
	return result, nil
}
