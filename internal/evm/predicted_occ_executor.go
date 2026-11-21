package evm

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var (
	// DefaultSwapRouterAddr và DefaultSwapPoolAddrs ghim đúng địa chỉ mẫu của fixture swap-pools.
	DefaultSwapRouterAddr = common.HexToAddress("0x000000000000000000000000000000000000b001")
	DefaultSwapPoolAddrs  = [4]common.Address{
		common.HexToAddress("0x000000000000000000000000000000000000c101"),
		common.HexToAddress("0x000000000000000000000000000000000000c102"),
		common.HexToAddress("0x000000000000000000000000000000000000c103"),
		common.HexToAddress("0x000000000000000000000000000000000000c104"),
	}
	DefaultSwapToken0Addr = common.HexToAddress("0x000000000000000000000000000000000000a001")
	DefaultSwapToken1Addr = common.HexToAddress("0x000000000000000000000000000000000000a002")

	// Hash runtime bytecode ghim từ bản build SwapAMM.sol (solc 0.8.24, Shanghai, optimizer off, bytecodeHash none).
	DefaultSwapRouterRuntimeCodeHash = common.HexToHash("0xeb2434663870948069133ab8ecf76daf9e6b0b309e8b15c610d1221dd4474ba0")
	DefaultSwapPoolRuntimeCodeHash   = common.HexToHash("0x21c4708d021be0b249f23e5e8585b50905bd4fe9949966d53de22621b4877ad1")
	DefaultSwapTokenRuntimeCodeHash  = common.HexToHash("0xa78dfa9373d661f184caea9f87e8a71536053b95a69ca100ca08eb63f100cc97")

	DefaultSwapPoolReserve0Slot = common.BigToHash(big.NewInt(2))
	DefaultSwapPoolReserve1Slot = common.BigToHash(big.NewInt(3))
	swapExactInSelector         = [4]byte{0x4d, 0xe6, 0xf7, 0xd2}
)

// SwapPredictorConfig quy định mẫu router/pool và vị trí slot dự trữ để nhận diện & học pool nóng xuyên block.
type SwapPredictorConfig struct {
	AllowedRouters map[common.Address]struct{}
	AllowedPools   map[common.Address]struct{}
	RouterCodeHash common.Hash
	PoolCodeHash   common.Hash
	Reserve0Slot   common.Hash
	Reserve1Slot   common.Hash
}

// DefaultSwapPredictorConfig trả về cấu hình mặc định cho router và 4 pool của fixture swap-pools.
func DefaultSwapPredictorConfig() SwapPredictorConfig {
	pools := make(map[common.Address]struct{}, len(DefaultSwapPoolAddrs))
	for _, p := range DefaultSwapPoolAddrs {
		pools[p] = struct{}{}
	}
	return SwapPredictorConfig{
		AllowedRouters: map[common.Address]struct{}{DefaultSwapRouterAddr: {}},
		AllowedPools:   pools,
		RouterCodeHash: DefaultSwapRouterRuntimeCodeHash,
		PoolCodeHash:   DefaultSwapPoolRuntimeCodeHash,
		Reserve0Slot:   DefaultSwapPoolReserve0Slot,
		Reserve1Slot:   DefaultSwapPoolReserve1Slot,
	}
}

// RecognizePinnedSwap giải mã lệnh swapExactIn(pool, zeroForOne, amountIn, minAmountOut, recipient) đúng mẫu đã ghim.
func RecognizePinnedSwap(state *PebbleStateDB, tx *types.Transaction, cfg SwapPredictorConfig) (common.Address, bool, error) {
	if tx == nil || tx.To() == nil {
		return common.Address{}, false, nil
	}
	if tx.Value().Sign() != 0 {
		return common.Address{}, false, nil
	}
	to := *tx.To()
	if len(cfg.AllowedRouters) > 0 {
		if _, ok := cfg.AllowedRouters[to]; !ok {
			return common.Address{}, false, nil
		}
	}

	data := tx.Data()
	if len(data) != 164 || !bytes.Equal(data[:4], swapExactInSelector[:]) {
		return common.Address{}, false, nil
	}
	if !bytesAllZero(data[4:16]) || !bytesAllZero(data[36:67]) || data[67] > 1 || !bytesAllZero(data[132:144]) {
		return common.Address{}, false, nil
	}

	pool := common.BytesToAddress(data[16:36])
	if len(cfg.AllowedPools) > 0 {
		if _, ok := cfg.AllowedPools[pool]; !ok {
			return common.Address{}, false, nil
		}
	}

	routerAcc, routerExists, err := state.accountSnapshot(to)
	if err != nil {
		return common.Address{}, false, err
	}
	if !routerExists || routerAcc == nil || len(routerAcc.CodeHash) == 0 {
		return common.Address{}, false, nil
	}
	routerHash := common.BytesToHash(routerAcc.CodeHash)
	if cfg.RouterCodeHash != (common.Hash{}) && routerHash != cfg.RouterCodeHash {
		return common.Address{}, false, nil
	}

	poolAcc, poolExists, err := state.accountSnapshot(pool)
	if err != nil {
		return common.Address{}, false, err
	}
	if !poolExists || poolAcc == nil || len(poolAcc.CodeHash) == 0 {
		return common.Address{}, false, nil
	}
	poolHash := common.BytesToHash(poolAcc.CodeHash)
	if cfg.PoolCodeHash != (common.Hash{}) && poolHash != cfg.PoolCodeHash {
		return common.Address{}, false, nil
	}
	return pool, true, nil
}

func bytesAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// PredictedOCCHooks cung cấp điểm đồng bộ và quan sát lập lịch chờ tiền nhiệm trong test (không dùng sleep).
type PredictedOCCHooks struct {
	OnTxWaitPredecessor    func(ctx context.Context, txIndex, predTxIndex int, pool common.Address)
	OnTxReleasedFromWait   func(ctx context.Context, txIndex, predTxIndex int, pool common.Address)
	OnAttemptStart         func(ctx context.Context, txIndex, attempt int)
	OnStorageRead          func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64)
	OnAttemptBeforeDeliver func(ctx context.Context, txIndex, attempt int)
	OnResultBuffered       func(txIndex, attempt int)
	OnTxCommitted          func(ctx context.Context, txIndex int, res TxResult)
}

func (h *PredictedOCCHooks) toOCCHooks() *OCCHooks {
	if h == nil {
		return nil
	}
	if h.OnAttemptStart == nil && h.OnStorageRead == nil && h.OnAttemptBeforeDeliver == nil {
		return nil
	}
	return &OCCHooks{
		OnAttemptStart:         h.OnAttemptStart,
		OnStorageRead:          h.OnStorageRead,
		OnAttemptBeforeDeliver: h.OnAttemptBeforeDeliver,
	}
}

// PredictedOCCBlockStats tổng hợp số liệu dự đoán, chờ tiền nhiệm, rỗi worker và retry OCC của một block.
type PredictedOCCBlockStats struct {
	Workers              int
	HotPools             []common.Address
	NextHotPools         []common.Address
	EligibleSwapsByPool  map[common.Address]int
	TotalEligibleSwaps   int
	RecognizedSwaps      int
	WaitedTxs            int
	WaitPredecessorNanos int64
	WaitWorkerNanos      int64
	WorkerIdleNanos      int64
	RetriedTxs           int
	TotalRetries         int
	FallbackTxs          int
	CommittedAttempts    map[int]int
}

// PredictedTxScheduleTrace lưu dấu vết lập lịch, chờ tiền nhiệm và học cuối block theo từng giao dịch.
type PredictedTxScheduleTrace struct {
	TxIndex             int
	TxHash              common.Hash
	Recognized          bool
	Pool                common.Address
	HotPool             bool
	WaitedForPred       bool
	PredTxIndex         int
	WaitPredNanos       int64
	WaitWorkerNanos     int64
	DispatchPrefixIndex int
	UpdatedReserves     bool
	CountedInLearning   bool
}

// PredictedOCCBlockTrace kết hợp dấu vết lập lịch dự đoán với dấu vết attempt của OCC.
type PredictedOCCBlockTrace struct {
	BlockNumber uint64
	Stats       PredictedOCCBlockStats
	Schedules   []PredictedTxScheduleTrace
	Attempts    []AttemptTrace
}

func (t *PredictedOCCBlockTrace) Format() string {
	if t == nil {
		return "<nil>"
	}
	var b strings.Builder
	hotHex := make([]string, len(t.Stats.HotPools))
	for i, p := range t.Stats.HotPools {
		hotHex[i] = p.Hex()
	}
	nextHex := make([]string, len(t.Stats.NextHotPools))
	for i, p := range t.Stats.NextHotPools {
		nextHex[i] = p.Hex()
	}
	fmt.Fprintf(&b, "block=%d workers=%d hotPools=%v nextHotPools=%v eligibleTotal=%d waitedTxs=%d waitPredUs=%d waitWorkerUs=%d workerIdleUs=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
		t.BlockNumber, t.Stats.Workers, hotHex, nextHex, t.Stats.TotalEligibleSwaps,
		t.Stats.WaitedTxs, t.Stats.WaitPredecessorNanos/1000, t.Stats.WaitWorkerNanos/1000, t.Stats.WorkerIdleNanos/1000,
		t.Stats.RetriedTxs, t.Stats.TotalRetries, t.Stats.FallbackTxs)
	for _, s := range t.Schedules {
		poolStr := "-"
		if s.Recognized {
			poolStr = s.Pool.Hex()
		}
		fmt.Fprintf(&b, "  sched tx[%d] recognized=%v pool=%s hot=%v waited=%v pred=%d dispatchPrefix=%d updatedReserves=%v learned=%v\n",
			s.TxIndex, s.Recognized, poolStr, s.HotPool, s.WaitedForPred, s.PredTxIndex,
			s.DispatchPrefixIndex, s.UpdatedReserves, s.CountedInLearning)
	}
	return b.String()
}

// PredictedOCCExecutor thực thi OCC với lập lịch dự đoán pool nóng kiểu Spectrum xuyên block (block H dùng thống kê block H-1),
// dùng chung CheckpointStore ngân sách cứng M byte và chính sách off / fixed / history / adaptive với ParallelExecutor.
type PredictedOCCExecutor struct {
	ChainConfig     *params.ChainConfig
	ChainID         *big.Int
	Workers         int
	StrictInvalidTx bool
	Phase2Mode      bool
	RecordTrace     bool
	Predictor       SwapPredictorConfig
	Hooks           *PredictedOCCHooks
	Checkpoints     CheckpointConfig
	HistoryTracker  *CheckpointHistoryTracker
	AdaptiveTracker *AdaptiveHistoryTracker

	mu       sync.Mutex
	hotPools map[common.Address]struct{}
}

func NewPredictedOCCExecutor(config *params.ChainConfig, chainID *big.Int, workers int) *PredictedOCCExecutor {
	if workers < 1 {
		workers = 1
	}
	return &PredictedOCCExecutor{
		ChainConfig:     config,
		ChainID:         new(big.Int).Set(chainID),
		Workers:         workers,
		RecordTrace:     true,
		Predictor:       DefaultSwapPredictorConfig(),
		Checkpoints:     DefaultCheckpointConfig(),
		HistoryTracker:  NewCheckpointHistoryTracker(MaxHistoryHotSlots),
		AdaptiveTracker: NewAdaptiveHistoryTracker(),
		hotPools:        make(map[common.Address]struct{}),
	}
}

// ConfigureCheckpoints bật chụp/khôi phục Phase 2–3 dùng chung kho ngân sách RAM cứng cho predicted-occ.
func (e *PredictedOCCExecutor) ConfigureCheckpoints(config CheckpointConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Policy == CheckpointPolicyAdaptive && config.AdaptiveModel == nil {
		return fmt.Errorf("checkpoint policy adaptive requires AdaptiveModel")
	}
	e.Checkpoints = config
	if config.Policy != CheckpointPolicyOff || config.UseBudget {
		e.Phase2Mode = true
	}
	if e.HistoryTracker == nil {
		e.HistoryTracker = NewCheckpointHistoryTracker(MaxHistoryHotSlots)
	}
	if e.AdaptiveTracker == nil {
		e.AdaptiveTracker = NewAdaptiveHistoryTracker()
	}
	return nil
}

// ResetHistory xóa lịch sử pool nóng và lịch sử checkpoint xuyên block để bắt đầu chuỗi/lượt đo mới sạch sẽ.
func (e *PredictedOCCExecutor) ResetHistory() {
	e.mu.Lock()
	e.hotPools = make(map[common.Address]struct{})
	e.mu.Unlock()
	e.ResetCheckpointHistory()
}

// ResetCheckpointHistory xóa lịch sử xung đột và thống kê adaptive xuyên block.
func (e *PredictedOCCExecutor) ResetCheckpointHistory() {
	if e == nil {
		return
	}
	if e.HistoryTracker != nil {
		e.HistoryTracker.Reset()
	}
	if e.AdaptiveTracker != nil {
		e.AdaptiveTracker.Reset()
	}
}

// HotSlots trả danh sách khóa nóng (contract, slot) học được cho chính sách history/adaptive.
func (e *PredictedOCCExecutor) HotSlots() []MVKey {
	if e == nil || e.HistoryTracker == nil {
		return nil
	}
	return e.HistoryTracker.HotSlots()
}

// HotPools trả về danh sách pool đang được đánh dấu nóng cho block tiếp theo (đã sắp xếp).
func (e *PredictedOCCExecutor) HotPools() []common.Address {
	e.mu.Lock()
	defer e.mu.Unlock()
	return sortedAddresses(e.hotPools)
}

// SetHotPools cho phép test khởi tạo sẵn lịch sử pool nóng cho một block đơn.
func (e *PredictedOCCExecutor) SetHotPools(pools []common.Address) {
	e.mu.Lock()
	e.hotPools = make(map[common.Address]struct{}, len(pools))
	for _, p := range pools {
		e.hotPools[p] = struct{}{}
	}
	e.mu.Unlock()
}

func sortedAddresses(set map[common.Address]struct{}) []common.Address {
	out := make([]common.Address, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
}

func cloneAddressSet(set map[common.Address]struct{}) map[common.Address]struct{} {
	out := make(map[common.Address]struct{}, len(set))
	for a := range set {
		out[a] = struct{}{}
	}
	return out
}

// ComputeNextHotPools chọn pool có count >= 2 và 2*count >= totalEligible ở block vừa chốt.
func ComputeNextHotPools(counts map[common.Address]int, totalEligible int) map[common.Address]struct{} {
	next := make(map[common.Address]struct{})
	if totalEligible < 2 {
		return next
	}
	for pool, cnt := range counts {
		if cnt >= 2 && 2*cnt >= totalEligible {
			next[pool] = struct{}{}
		}
	}
	return next
}

func (e *PredictedOCCExecutor) ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error) {
	totalStarted := time.Now()
	stateDB, ok := base.(*PebbleStateDB)
	if !ok {
		return nil, fmt.Errorf("predicted-occ executor requires *PebbleStateDB, got %T", base)
	}
	if e.ChainConfig == nil || e.ChainID == nil {
		return nil, fmt.Errorf("predicted-occ executor requires chain config and chain ID")
	}
	if env.GasLimit == 0 {
		return nil, fmt.Errorf("block gas limit must be greater than zero")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}

	workers := e.Workers
	if workers < 1 {
		workers = 1
	}

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

	e.mu.Lock()
	activeHotSet := cloneAddressSet(e.hotPools)
	e.mu.Unlock()

	stats := PredictedOCCBlockStats{
		Workers:             workers,
		HotPools:            sortedAddresses(activeHotSet),
		EligibleSwapsByPool: make(map[common.Address]int),
		CommittedAttempts:   make(map[int]int, len(txs)),
	}

	schedTraces := make([]PredictedTxScheduleTrace, len(txs))
	for i, tx := range txs {
		var h common.Hash
		if tx != nil {
			h = tx.Hash()
		}
		schedTraces[i] = PredictedTxScheduleTrace{
			TxIndex:             i,
			TxHash:              h,
			PredTxIndex:         -1,
			DispatchPrefixIndex: -1,
		}
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
	occHooks := e.Hooks.toOCCHooks()

	// trạng thái điều phối chỉ thuộc goroutine coordinator
	lastPoolTx := make(map[common.Address]int)
	waitingOn := make(map[int][]int)
	readyQueue := make([]int, 0, workers)
	enterTimes := make([]time.Time, len(txs))
	readyTimes := make([]time.Time, len(txs))

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
	nextAdmit := 0

	admitWindow := func() error {
		for nextAdmit < len(txs) && nextAdmit < nextCommit+workers {
			txIdx := nextAdmit
			tx := txs[txIdx]

			pool, recognized, err := RecognizePinnedSwap(curPrefix.state, tx, e.Predictor)
			if err != nil {
				return fmt.Errorf("recognize swap tx %d: %w", txIdx, err)
			}
			sched := &schedTraces[txIdx]
			if recognized {
				sched.Recognized = true
				sched.Pool = pool
				stats.RecognizedSwaps++
				_, isHot := activeHotSet[pool]
				sched.HotPool = isHot
				if isHot {
					if pred, hasPred := lastPoolTx[pool]; hasPred && pred >= nextCommit {
						// giữ giao dịch phụ thuộc ngoài readyQueue để không chiếm worker
						sched.WaitedForPred = true
						sched.PredTxIndex = pred
						stats.WaitedTxs++
						enterTimes[txIdx] = time.Now()
						waitingOn[pred] = append(waitingOn[pred], txIdx)
						if e.Hooks != nil && e.Hooks.OnTxWaitPredecessor != nil {
							e.Hooks.OnTxWaitPredecessor(runCtx, txIdx, pred, pool)
						}
						lastPoolTx[pool] = txIdx
						nextAdmit++
						continue
					}
				}
				lastPoolTx[pool] = txIdx
			}

			// lấy mốc sẵn sàng sau RecognizePinnedSwap để wait_worker_ns không cộng thời gian nhận diện
			readyTimes[txIdx] = time.Now()
			readyQueue = append(readyQueue, txIdx)
			nextAdmit++
		}
		sort.Ints(readyQueue)
		return nil
	}

	dispatchReady := func() {
		for activeWorkers < workers && len(readyQueue) > 0 {
			txIdx := readyQueue[0]
			readyQueue = readyQueue[1:]
			now := time.Now()
			waitW := now.Sub(readyTimes[txIdx]).Nanoseconds()
			if waitW > 0 {
				schedTraces[txIdx].WaitWorkerNanos += waitW
				stats.WaitWorkerNanos += waitW
			}
			schedTraces[txIdx].DispatchPrefixIndex = curPrefix.committedIndex

			sampleWorkerIdle(now)
			activeWorkers++

			tx := txs[txIdx]
			prefix := curPrefix
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := runWindowAttemptWithCapture(runCtx, txIdx, 1, tx, prefix, env, e.ChainConfig, signer, occHooks, cpRT.specCapSpec)
				select {
				case resultsCh <- res:
				case <-runCtx.Done():
				}
			}()
		}
	}

	for nextCommit < len(txs) {
		if err := admitWindow(); err != nil {
			return nil, err
		}
		dispatchReady()

		specRes, ok := completed[nextCommit]
		for !ok {
			incoming := <-resultsCh
			sampleWorkerIdle(time.Now())
			activeWorkers--

			completed[incoming.txIndex] = incoming
			if incoming.txIndex != nextCommit && e.Hooks != nil && e.Hooks.OnResultBuffered != nil {
				e.Hooks.OnResultBuffered(incoming.txIndex, incoming.attempt)
			}
			dispatchReady()
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
		sched := &schedTraces[nextCommit]
		var beforeR0, beforeR1 common.Hash
		if sched.Recognized {
			var err error
			beforeR0, err = curPrefix.state.storageSnapshot(sched.Pool, e.Predictor.Reserve0Slot)
			if err != nil {
				return nil, err
			}
			beforeR1, err = curPrefix.state.storageSnapshot(sched.Pool, e.Predictor.Reserve1Slot)
			if err != nil {
				return nil, err
			}
		}

		conflict, abortReason, earliestInvalidStep, staleStorageKeys := validateWindowReadsDetailed(specRes, versions)
		cpRT.recordValidationLearning(specRes, abortReason, earliestInvalidStep, staleStorageKeys)
		gasExceedsRemaining := tx != nil && tx.Gas() > remainingGas
		if abortReason == "" && gasExceedsRemaining && specRes.applyErr == nil {
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
					return nil, fmt.Errorf("apply predicted-occ tx %d delta: %w", nextCommit, err)
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
				cpRT.store.ReleaseTx(nextCommit)
				retryRes := runWindowAttemptWithCapture(runCtx, nextCommit, retryAttempt, tx, curPrefix, env, e.ChainConfig, signer, occHooks, cpRT.replayCapSpec)
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
						return nil, fmt.Errorf("apply retried predicted-occ tx %d delta: %w", nextCommit, err)
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

		// học pool nóng chỉ từ swap nhận diện được, thành công và thực sự làm đổi dự trữ pool
		if sched.Recognized && !finalRes.Invalid {
			afterR0 := stateDB.GetState(sched.Pool, e.Predictor.Reserve0Slot)
			afterR1 := stateDB.GetState(sched.Pool, e.Predictor.Reserve1Slot)
			updatedReserves := afterR0 != beforeR0 || afterR1 != beforeR1
			sched.UpdatedReserves = updatedReserves
			if !finalRes.Failed && updatedReserves {
				sched.CountedInLearning = true
				stats.EligibleSwapsByPool[sched.Pool]++
				stats.TotalEligibleSwaps++
			}
		}

		finalRes.Index = nextCommit
		result.Results[nextCommit] = finalRes
		if e.Hooks != nil && e.Hooks.OnTxCommitted != nil {
			e.Hooks.OnTxCommitted(runCtx, nextCommit, finalRes)
		}

		// dựng bản nhìn mới từ trạng thái vừa chốt trước khi mở khóa các tx cùng pool đang đợi
		nextIdx := nextCommit + 1
		if nextIdx < len(txs) {
			curPrefix = newPrefixSnapshot(stateDB, versions, nextIdx, remainingGas)
		}
		if waiters := waitingOn[nextCommit]; len(waiters) > 0 {
			now := time.Now()
			for _, wIdx := range waiters {
				readyTimes[wIdx] = now
				if waitP := now.Sub(enterTimes[wIdx]).Nanoseconds(); waitP > 0 {
					schedTraces[wIdx].WaitPredNanos = waitP
					stats.WaitPredecessorNanos += waitP
				}
				readyQueue = append(readyQueue, wIdx)
				if e.Hooks != nil && e.Hooks.OnTxReleasedFromWait != nil {
					e.Hooks.OnTxReleasedFromWait(runCtx, wIdx, nextCommit, schedTraces[wIdx].Pool)
				}
			}
			delete(waitingOn, nextCommit)
			sort.Ints(readyQueue)
		}

		nextCommit = nextIdx
	}

	wg.Wait()
	sampleWorkerIdle(time.Now())

	nextHotSet := ComputeNextHotPools(stats.EligibleSwapsByPool, stats.TotalEligibleSwaps)
	stats.NextHotPools = sortedAddresses(nextHotSet)

	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect predicted-occ state writes: %w", err)
	}
	copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))

	// chỉ công bố lịch sử pool nóng và lịch sử checkpoint sau khi toàn bộ block hoàn tất hợp lệ
	e.mu.Lock()
	e.hotPools = nextHotSet
	e.mu.Unlock()
	cpRT.finalizeAndPublish(result, e.HistoryTracker, e.AdaptiveTracker, stats.RetriedTxs, stats.TotalRetries, e.RecordTrace)

	result.Writes = writes

	result.PredictedStats = &stats
	if e.RecordTrace {
		result.PredictedTrace = &PredictedOCCBlockTrace{
			BlockNumber: uint64(env.Height),
			Stats:       stats,
			Schedules:   schedTraces,
			Attempts:    attempts,
		}
	}
	result.Metrics["tx_total"] = float64(len(txs))
	result.Metrics["worker_count"] = float64(workers)
	result.Metrics["recognized_swaps"] = float64(stats.RecognizedSwaps)
	result.Metrics["waited_txs"] = float64(stats.WaitedTxs)
	result.Metrics["wait_predecessor_ns"] = float64(stats.WaitPredecessorNanos)
	result.Metrics["wait_worker_ns"] = float64(stats.WaitWorkerNanos)
	result.Metrics["worker_idle_ns"] = float64(stats.WorkerIdleNanos)
	result.Metrics["retried_txs"] = float64(stats.RetriedTxs)
	result.Metrics["total_retries"] = float64(stats.TotalRetries)
	result.Metrics["fallback_txs"] = float64(stats.FallbackTxs)
	result.Metrics["phase_total_ns"] = float64(time.Since(totalStarted).Nanoseconds())
	return result, nil
}
