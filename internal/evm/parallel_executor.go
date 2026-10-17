package evm

import (
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
	"github.com/holiman/uint256"
)

// ExecutionClass is the deterministic P/O/S routing decision. Parametric is
// reserved for certified P artifacts; the current conservative analyzer does
// not produce such artifacts yet.
type ExecutionClass uint8

const (
	ExecutionStatic     ExecutionClass = iota // S: statically bounded access set
	ExecutionParametric                       // P: certified parameterized artifact
	ExecutionOpaque                           // O: ordinary EVM execution in order
)

func (c ExecutionClass) String() string {
	switch c {
	case ExecutionStatic:
		return "S"
	case ExecutionParametric:
		return "P"
	default:
		return "O"
	}
}

// MVKey identifies a versioned state component. The first implementation's
// static transfer analyzer tracks account balance and nonce keys only.
type MVKey struct {
	Address common.Address
	Kind    MVKeyKind
	Slot    common.Hash
}

type MVKeyKind uint8

const (
	MVBalance MVKeyKind = iota
	MVNonce
	MVCode
	MVStorage
)

type versionShard struct {
	sync.RWMutex
	versions map[MVKey]uint64
}

// The current S analyzer supports transfers, which touch at most three keys.
// Keeping these small sets inline avoids per-transaction maps and slices.
const maxStaticTransferKeys = 3

type MVKeySet struct {
	keys [maxStaticTransferKeys]MVKey
	len  uint8
}

func (s *MVKeySet) add(key MVKey) {
	if int(s.len) >= len(s.keys) {
		panic("static transfer MV key capacity exceeded")
	}
	s.keys[s.len] = key
	s.len++
}

type MVVersionSnapshot struct {
	keys     [maxStaticTransferKeys]MVKey
	versions [maxStaticTransferKeys]uint64
	len      uint8
}

// MVMemory is the accepted-version table. It is sharded so parallel
// validation and ordered resolver updates do not contend on one global lock.
type MVMemory struct{ shards [32]versionShard }

func NewMVMemory() *MVMemory {
	m := &MVMemory{}
	for i := range m.shards {
		m.shards[i].versions = make(map[MVKey]uint64)
	}
	return m
}

func (m *MVMemory) shard(key MVKey) *versionShard {
	return &m.shards[key.Address[0]%byte(len(m.shards))]
}

func (m *MVMemory) Version(key MVKey) uint64 {
	shard := m.shard(key)
	shard.RLock()
	version := shard.versions[key]
	shard.RUnlock()
	return version
}

func (m *MVMemory) SnapshotSet(keys MVKeySet) MVVersionSnapshot {
	versions := MVVersionSnapshot{keys: keys.keys, len: keys.len}
	for i := 0; i < int(keys.len); i++ {
		versions.versions[i] = m.Version(keys.keys[i])
	}
	return versions
}

func (m *MVMemory) ValidateSet(readVersions MVVersionSnapshot) bool {
	for i := 0; i < int(readVersions.len); i++ {
		if m.Version(readVersions.keys[i]) != readVersions.versions[i] {
			return false
		}
	}
	return true
}

func (m *MVMemory) CommitSet(writes MVKeySet) {
	for i := 0; i < int(writes.len); i++ {
		key := writes.keys[i]
		shard := m.shard(key)
		shard.Lock()
		shard.versions[key]++
		shard.Unlock()
	}
}

// Snapshot, Validate, and Commit retain the general slice-based API for tests
// and future analyzers with larger access sets. The transfer hot path uses the
// fixed-size variants above to avoid per-transaction allocations.
func (m *MVMemory) Snapshot(keys []MVKey) map[MVKey]uint64 {
	versions := make(map[MVKey]uint64, len(keys))
	for _, key := range keys {
		versions[key] = m.Version(key)
	}
	return versions
}

func (m *MVMemory) Validate(readVersions map[MVKey]uint64) bool {
	for key, version := range readVersions {
		if m.Version(key) != version {
			return false
		}
	}
	return true
}

func (m *MVMemory) Commit(writes []MVKey) {
	for _, key := range writes {
		shard := m.shard(key)
		shard.Lock()
		shard.versions[key]++
		shard.Unlock()
	}
}

// TxState is a private, journaled state overlay forked from a block state.
// The embedded PebbleStateDB keeps the exact existing geth vm.StateDB
// semantics; it reads through to immutable Pebble data and keeps all mutation
// in per-transaction maps until the ordered resolver accepts its write set.
type TxState struct {
	*PebbleStateDB
	TxIndex     int
	Incarnation uint64
	ReadSet     []MVKey
	WriteSet    []MVKey
}

var _ vm.StateDB = (*TxState)(nil)

func newTxState(base *PebbleStateDB, txIndex int, incarnation uint64, readSet, writeSet []MVKey) *TxState {
	return &TxState{
		PebbleStateDB: base.Fork(),
		TxIndex:       txIndex,
		Incarnation:   incarnation,
		ReadSet:       append([]MVKey(nil), readSet...),
		WriteSet:      append([]MVKey(nil), writeSet...),
	}
}

// ParallelExecutor speculates only on the sound S subset currently supported
// by the P/O/S analyzer. O transactions are delegated to the sequential
// reference at their original position. P remains disabled until certified
// artifact generation/evaluation is implemented.
type ParallelExecutor struct {
	ChainConfig *params.ChainConfig
	ChainID     *big.Int
	Workers     int
}

func NewParallelExecutor(config *params.ChainConfig, chainID *big.Int, workers int) *ParallelExecutor {
	if workers < 1 {
		workers = runtime.GOMAXPROCS(0)
	}
	return &ParallelExecutor{ChainConfig: config, ChainID: new(big.Int).Set(chainID), Workers: workers}
}

func (e *ParallelExecutor) ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error) {
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
	workers := e.Workers
	if workers < 1 {
		workers = 1
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}
	result := &BlockResult{Results: make([]TxResult, len(txs)), Writes: make(map[string][]byte), Metrics: make(map[string]float64)}
	versions := NewMVMemory()
	sequential := NewSequentialExecutor(e.ChainConfig, e.ChainID)
	var jobs chan func()
	var workerGroup sync.WaitGroup
	startWorkers := func() {
		if jobs != nil {
			return
		}
		jobs = make(chan func(), workers)
		for worker := 0; worker < workers; worker++ {
			workerGroup.Add(1)
			go func() {
				defer workerGroup.Done()
				for job := range jobs {
					job()
				}
			}()
		}
	}
	defer func() {
		if jobs != nil {
			close(jobs)
			workerGroup.Wait()
		}
	}()
	remainingGas := env.GasLimit
	started := time.Now()
	var analyzeNanos, executeNanos, validateNanos, resolveNanos int64
	var incarnationTotal, abortTotal, staticCount, parallelCount, opaqueCount float64

	for i := 0; i < len(txs); {
		analyzeStart := time.Now()
		access, isStatic := e.staticTransferAccess(base, txs[i], env.BaseFee)
		analyzeNanos += time.Since(analyzeStart).Nanoseconds()
		if !isStatic {
			// Keep opaque transactions exactly at their consensus position.
			end := i + 1
			for end < len(txs) {
				analyzeStart = time.Now()
				if _, nextStatic := e.staticTransferAccess(base, txs[end], env.BaseFee); nextStatic {
					analyzeNanos += time.Since(analyzeStart).Nanoseconds()
					break
				}
				analyzeNanos += time.Since(analyzeStart).Nanoseconds()
				end++
			}
			opaqueCount += float64(end - i)
			segmentEnv := env
			segmentEnv.GasLimit = remainingGas
			execStart := time.Now()
			segment, err := sequential.ExecuteBlock(stateDB, segmentEnv, txs[i:end])
			executeNanos += time.Since(execStart).Nanoseconds()
			if err != nil {
				return nil, fmt.Errorf("sequential O segment [%d,%d): %w", i, end, err)
			}
			for j, txResult := range segment.Results {
				txResult.Index = i + j
				result.Results[i+j] = txResult
			}
			result.GasUsed += segment.GasUsed
			if segment.GasUsed > remainingGas {
				return nil, fmt.Errorf("sequential segment consumed %d gas with %d remaining", segment.GasUsed, remainingGas)
			}
			remainingGas -= segment.GasUsed
			i = end
			continue
		}

		// Form a deterministic, contiguous, conflict-free S wave. The static
		// access list contains sender/recipient balances and sender nonce.
		wave := []int{i}
		waveAccesses := []staticTransfer{access}
		used := map[common.Address]bool{access.sender: true, access.recipient: true}
		reservedGas := txs[i].Gas()
		// Transfers are cheap to evaluate. Making each wave only as large as
		// the worker count caused a barrier after every handful of transactions,
		// so amortize scheduling and validation across several tasks per worker.
		// The address conflict check remains the safety boundary for a wave.
		waveLimit := workers * 32
		if waveLimit < workers { // protect against integer overflow
			waveLimit = workers
		}
		for j := i + 1; j < len(txs) && len(wave) < waveLimit; j++ {
			analyzeStart = time.Now()
			next, nextStatic := e.staticTransferAccess(base, txs[j], env.BaseFee)
			analyzeNanos += time.Since(analyzeStart).Nanoseconds()
			if !nextStatic || reservedGas > remainingGas || txs[j].Gas() > remainingGas-reservedGas || used[next.sender] || used[next.recipient] {
				break
			}
			wave = append(wave, j)
			waveAccesses = append(waveAccesses, next)
			used[next.sender], used[next.recipient] = true, true
			reservedGas += txs[j].Gas()
		}
		staticCount += float64(len(wave))
		if len(wave) == 1 {
			segmentEnv := env
			segmentEnv.GasLimit = remainingGas
			execStart := time.Now()
			segment, err := sequential.ExecuteBlock(stateDB, segmentEnv, txs[i:i+1])
			executeNanos += time.Since(execStart).Nanoseconds()
			if err != nil {
				return nil, fmt.Errorf("sequential S fallback at tx %d: %w", i, err)
			}
			segment.Results[0].Index = i
			result.Results[i] = segment.Results[0]
			result.GasUsed += segment.GasUsed
			if segment.GasUsed > remainingGas {
				return nil, fmt.Errorf("transaction %d consumed %d gas with %d remaining", i, segment.GasUsed, remainingGas)
			}
			remainingGas -= segment.GasUsed
			i++
			continue
		}

		type attempt struct {
			index       int
			access      staticTransfer
			sender      *Account
			recipient   *Account
			result      TxResult
			value       *uint256.Int
			valid       bool
			versions    MVVersionSnapshot
			incarnation uint64
		}
		attempts := make([]attempt, len(wave))
		for n, txIndex := range wave {
			access := waveAccesses[n]
			sender, _ := stateDB.Account(access.sender)
			recipient, _ := stateDB.Account(access.recipient)
			attempts[n] = attempt{index: txIndex, access: access, sender: sender, recipient: recipient, versions: versions.SnapshotSet(access.reads), incarnation: 1}
		}
		execStart := time.Now()
		var wg sync.WaitGroup
		startWorkers()
		workerCount := workers
		if workerCount > len(attempts) {
			workerCount = len(attempts)
		}
		for worker := 0; worker < workerCount; worker++ {
			worker := worker
			wg.Add(1)
			jobs <- func() {
				defer wg.Done()
				// One scheduled job evaluates a batch, reducing channel and
				// closure overhead for the deliberately small S transfer operation.
				for n := worker; n < len(attempts); n += workerCount {
					txIndex := attempts[n].index
					attempts[n].result, attempts[n].value, attempts[n].valid = evaluateStaticTransfer(txs[txIndex], attempts[n].sender, attempts[n].recipient)
				}
			}
		}
		wg.Wait()
		executeNanos += time.Since(execStart).Nanoseconds()
		parallelCount += float64(len(wave))

		// Validate and resolve strictly by original TxIndex, independent of
		// worker completion order.
		for n := range attempts {
			attempt := &attempts[n]
			validateStart := time.Now()
			valid := versions.ValidateSet(attempt.versions)
			validateNanos += time.Since(validateStart).Nanoseconds()
			if !valid {
				abortTotal++
				incarnationTotal++
				sender, _ := stateDB.Account(attempt.access.sender)
				recipient, _ := stateDB.Account(attempt.access.recipient)
				attempt.sender, attempt.recipient = sender, recipient
				attempt.result, attempt.value, attempt.valid = evaluateStaticTransfer(txs[attempt.index], sender, recipient)
				attempt.incarnation++
			} else {
				incarnationTotal++
			}
			resolved := attempt.result
			resolved.Index = attempt.index
			result.Results[attempt.index] = resolved
			if attempt.valid {
				if resolved.GasUsed > remainingGas {
					return nil, fmt.Errorf("tx %d consumed %d gas with %d remaining", attempt.index, resolved.GasUsed, remainingGas)
				}
				remainingGas -= resolved.GasUsed
				result.GasUsed += resolved.GasUsed
				resolveStart := time.Now()
				if err := applyStaticTransfer(stateDB, txs[attempt.index], attempt.access, attempt.value, e.ChainConfig, env); err != nil {
					return nil, fmt.Errorf("resolve S tx %d: %w", attempt.index, err)
				}
				access := attempt.access
				versions.CommitSet(access.writes)
				resolveNanos += time.Since(resolveStart).Nanoseconds()
			}
		}
		i += len(wave)
	}

	result.Metrics["tx_total"] = float64(len(txs))
	result.Metrics["tx_class_s"] = staticCount
	result.Metrics["tx_class_p"] = 0
	result.Metrics["tx_class_o"] = opaqueCount
	result.Metrics["worker_count"] = float64(workers)
	result.Metrics["tx_parallel"] = parallelCount
	result.Metrics["incarnation_total"] = incarnationTotal
	result.Metrics["abort_total"] = abortTotal
	result.Metrics["phase_analyze_ns"] = float64(analyzeNanos)
	result.Metrics["phase_execute_ns"] = float64(executeNanos)
	result.Metrics["phase_validate_ns"] = float64(validateNanos)
	result.Metrics["phase_resolve_ns"] = float64(resolveNanos)
	prepareStart := time.Now()
	copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect parallel state writes: %w", err)
	}
	result.Writes = writes
	result.Metrics["phase_commit_prepare_ns"] = float64(time.Since(prepareStart).Nanoseconds())
	result.Metrics["phase_total_ns"] = float64(time.Since(started).Nanoseconds())
	return result, nil
}

type staticTransfer struct {
	sender    common.Address
	recipient common.Address
	reads     MVKeySet
	writes    MVKeySet
}

func (e *ParallelExecutor) staticTransferAccess(base StateReader, tx *types.Transaction, baseFee *big.Int) (staticTransfer, bool) {
	if tx == nil || tx.Type() != types.LegacyTxType || tx.To() == nil || len(tx.Data()) != 0 || tx.Gas() != 21000 || tx.GasPrice().Sign() != 0 || !tx.Protected() || tx.ChainId().Cmp(e.ChainID) != 0 || baseFee == nil || baseFee.Sign() != 0 {
		return staticTransfer{}, false
	}
	signer := types.LatestSignerForChainID(e.ChainID)
	sender, err := types.Sender(signer, tx)
	if err != nil || len(base.Code(sender)) != 0 || len(base.Code(*tx.To())) != 0 || isPrecompileAddress(*tx.To()) {
		return staticTransfer{}, false
	}
	access := staticTransfer{sender: sender, recipient: *tx.To()}
	access.reads.add(MVKey{Address: sender, Kind: MVBalance})
	access.reads.add(MVKey{Address: sender, Kind: MVNonce})
	if sender == *tx.To() {
		access.writes = access.reads
		return access, true
	}
	access.reads.add(MVKey{Address: *tx.To(), Kind: MVBalance})
	access.writes = access.reads
	return access, true
}

func evaluateStaticTransfer(tx *types.Transaction, sender, recipient *Account) (TxResult, *uint256.Int, bool) {
	result := TxResult{Index: 0}
	if tx == nil || sender == nil || recipient == nil {
		result.Invalid = true
		result.ErrMsg = "missing static transaction state"
		return result, nil, false
	}
	if tx.Nonce() != sender.Nonce {
		result.Invalid = true
		if tx.Nonce() < sender.Nonce {
			result.ErrMsg = fmt.Sprintf("nonce too low: tx %d state %d", tx.Nonce(), sender.Nonce)
		} else {
			result.ErrMsg = fmt.Sprintf("nonce too high: tx %d state %d", tx.Nonce(), sender.Nonce)
		}
		return result, nil, false
	}
	if sender.Nonce == ^uint64(0) {
		result.Invalid, result.ErrMsg = true, "sender nonce overflow"
		return result, nil, false
	}
	if sender.Balance == nil || sender.Balance.Cmp(tx.Cost()) < 0 {
		result.Invalid = true
		result.ErrMsg = "insufficient funds for transfer"
		return result, nil, false
	}
	value, overflow := uint256.FromBig(tx.Value())
	if overflow {
		result.Invalid, result.ErrMsg = true, "transfer value exceeds uint256"
		return result, nil, false
	}
	result.GasUsed = 21_000
	return result, value, true
}

// applyStaticTransfer resolves a validated S artifact with the same protocol
// effects as geth's 21,000-gas EOA call. This fast path is only reachable for
// legacy, zero-fee, empty-calldata transfers to non-contract, non-precompile
// addresses; all other transactions use ApplyMessage.
func applyStaticTransfer(state *PebbleStateDB, tx *types.Transaction, access staticTransfer, value *uint256.Int, chainConfig *params.ChainConfig, env BlockEnv) error {
	if value == nil {
		return fmt.Errorf("static transfer has no validated value")
	}
	zero := new(uint256.Int)
	state.SubBalance(access.sender, zero, tracing.BalanceDecreaseGasBuy)
	rules := chainConfig.Rules(big.NewInt(env.Height), true, env.Time)
	state.Prepare(rules, access.sender, env.Coinbase, tx.To(), vm.ActivePrecompiles(rules), tx.AccessList())
	state.SetNonce(access.sender, tx.Nonce()+1, tracing.NonceChangeEoACall)
	core.Transfer(state, access.sender, access.recipient, value)
	state.AddBalance(access.sender, zero, tracing.BalanceIncreaseGasReturn)
	state.AddBalance(env.Coinbase, zero, tracing.BalanceIncreaseRewardTransactionFee)
	state.Finalise(true)
	return nil
}

func isPrecompileAddress(address common.Address) bool {
	for i := 0; i < common.AddressLength-1; i++ {
		if address[i] != 0 {
			return false
		}
	}
	return address[common.AddressLength-1] > 0 && address[common.AddressLength-1] <= 10
}
