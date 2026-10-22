package evm

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
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
	// MVAccount is a conservative account-wide dependency. StateDB operations
	// that can create, delete, or change account emptiness also record this key
	// so those effects cannot be missed by optimistic validation.
	MVAccount
	MVStorageRoot
)

type versionShard struct {
	sync.RWMutex
	versions map[MVKey]uint64
	history  map[MVKey][]MVVersionRecord
}

type MVVersionRecord struct {
	TxIndex     int
	Incarnation uint64
	Version     uint64
	Value       []byte
	Deleted     bool
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
		m.shards[i].history = make(map[MVKey][]MVVersionRecord)
	}
	return m
}

// ReadBefore returns the latest accepted version produced by a transaction
// earlier than txIndex. The value is copied so callers cannot mutate MVMemory.
func (m *MVMemory) ReadBefore(key MVKey, txIndex int) (MVVersionRecord, bool) {
	shard := m.shard(key)
	shard.RLock()
	history := shard.history[key]
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].TxIndex < txIndex {
			record := history[i]
			record.Value = append([]byte(nil), record.Value...)
			shard.RUnlock()
			return record, true
		}
	}
	shard.RUnlock()
	return MVVersionRecord{}, false
}

func (m *MVMemory) CommitTransaction(txIndex int, incarnation uint64, writes map[MVKey][]byte) {
	for key, value := range writes {
		shard := m.shard(key)
		shard.Lock()
		version := shard.versions[key] + 1
		shard.versions[key] = version
		shard.history[key] = append(shard.history[key], MVVersionRecord{
			TxIndex: txIndex, Incarnation: incarnation, Version: version,
			Value: append([]byte(nil), value...), Deleted: value == nil,
		})
		shard.Unlock()
	}
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
	reads       map[MVKey]struct{}
	writes      map[MVKey]struct{}
}

var _ vm.StateDB = (*TxState)(nil)

func newTxState(base *PebbleStateDB, txIndex int, incarnation uint64, readSet, writeSet []MVKey) *TxState {
	txState := &TxState{
		PebbleStateDB: base.NewOverlay(),
		TxIndex:       txIndex,
		Incarnation:   incarnation,
		reads:         make(map[MVKey]struct{}, len(readSet)+8),
		writes:        make(map[MVKey]struct{}, len(writeSet)+8),
	}
	for _, key := range readSet {
		txState.addReadKey(key)
	}
	for _, key := range writeSet {
		txState.addWriteKey(key)
	}
	return txState
}

func (s *TxState) addReadKey(key MVKey) {
	if _, exists := s.reads[key]; exists {
		return
	}
	s.reads[key] = struct{}{}
	s.ReadSet = append(s.ReadSet, key)
}

func (s *TxState) addWriteKey(key MVKey) {
	if _, exists := s.writes[key]; exists {
		return
	}
	s.writes[key] = struct{}{}
	s.WriteSet = append(s.WriteSet, key)
}

func (s *TxState) recordRead(key MVKey) {
	s.addReadKey(key)
	s.addReadKey(MVKey{Address: key.Address, Kind: MVAccount})
}

func (s *TxState) recordWrite(key MVKey) {
	s.addWriteKey(key)
	// A storage-slot update does not change account existence, nonce, balance,
	// or code. Versioning MVAccount for every SSTORE creates a false conflict
	// with the already-validated contract/code prefix and prevents a safe
	// checkpoint immediately before SLOAD from being reused.
	if key.Kind != MVStorage && key.Kind != MVStorageRoot {
		s.addWriteKey(MVKey{Address: key.Address, Kind: MVAccount})
	}
}

func (s *TxState) AccessSets() (reads, writes []MVKey) {
	return append([]MVKey(nil), s.ReadSet...), append([]MVKey(nil), s.WriteSet...)
}

func (s *TxState) VersionedWrites() map[MVKey][]byte {
	_, keys := s.AccessSets()
	return stateVersionedWrites(s.PebbleStateDB, keys)
}

func stateVersionedWrites(state *PebbleStateDB, keys []MVKey) map[MVKey][]byte {
	values := make(map[MVKey][]byte, len(keys))
	for _, key := range keys {
		switch key.Kind {
		case MVBalance:
			balance := state.GetBalance(key.Address).ToBig()
			values[key] = balance.FillBytes(make([]byte, 32))
		case MVNonce:
			var nonce [8]byte
			binary.BigEndian.PutUint64(nonce[:], state.GetNonce(key.Address))
			values[key] = nonce[:]
		case MVCode:
			values[key] = state.GetCode(key.Address)
		case MVStorage:
			slot := state.GetState(key.Address, key.Slot)
			if slot != (common.Hash{}) {
				values[key] = slot.Bytes()
			}
		case MVAccount:
			account := state.getAccount(key.Address)
			encoded, err := rlp.EncodeToBytes(account)
			if err == nil {
				values[key] = encoded
			}
		case MVStorageRoot:
			root := state.GetStorageRoot(key.Address)
			values[key] = root.Bytes()
		}
	}
	return values
}

// The wrapper records persistent state dependencies while leaving transient
// EVM state (refunds, access lists and transient storage) transaction-local.
func (s *TxState) CreateAccount(addr common.Address) {
	s.recordWrite(MVKey{Address: addr, Kind: MVAccount})
	s.PebbleStateDB.CreateAccount(addr)
}

func (s *TxState) CreateContract(addr common.Address) {
	s.recordWrite(MVKey{Address: addr, Kind: MVNonce})
	s.PebbleStateDB.CreateContract(addr)
}

func (s *TxState) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if amount != nil && amount.IsZero() {
		return s.PebbleStateDB.SubBalance(addr, amount, reason)
	}
	s.recordRead(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVBalance})
	return s.PebbleStateDB.SubBalance(addr, amount, reason)
}

func (s *TxState) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if amount != nil && amount.IsZero() {
		return s.PebbleStateDB.AddBalance(addr, amount, reason)
	}
	s.recordRead(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVBalance})
	return s.PebbleStateDB.AddBalance(addr, amount, reason)
}

func (s *TxState) GetBalance(addr common.Address) *uint256.Int {
	s.recordRead(MVKey{Address: addr, Kind: MVBalance})
	return s.PebbleStateDB.GetBalance(addr)
}

func (s *TxState) GetNonce(addr common.Address) uint64 {
	s.recordRead(MVKey{Address: addr, Kind: MVNonce})
	return s.PebbleStateDB.GetNonce(addr)
}

func (s *TxState) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	s.recordWrite(MVKey{Address: addr, Kind: MVNonce})
	s.PebbleStateDB.SetNonce(addr, nonce, reason)
}

func (s *TxState) GetCodeHash(addr common.Address) common.Hash {
	s.recordRead(MVKey{Address: addr, Kind: MVCode})
	return s.PebbleStateDB.GetCodeHash(addr)
}

func (s *TxState) GetCode(addr common.Address) []byte {
	s.recordRead(MVKey{Address: addr, Kind: MVCode})
	return s.PebbleStateDB.GetCode(addr)
}

func (s *TxState) GetCodeSize(addr common.Address) int {
	s.recordRead(MVKey{Address: addr, Kind: MVCode})
	return s.PebbleStateDB.GetCodeSize(addr)
}

func (s *TxState) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	s.recordWrite(MVKey{Address: addr, Kind: MVCode})
	return s.PebbleStateDB.SetCode(addr, code, reason)
}

func (s *TxState) GetStateAndCommittedState(addr common.Address, slot common.Hash) (common.Hash, common.Hash) {
	s.recordRead(MVKey{Address: addr, Kind: MVStorage, Slot: slot})
	return s.PebbleStateDB.GetStateAndCommittedState(addr, slot)
}

func (s *TxState) GetState(addr common.Address, slot common.Hash) common.Hash {
	s.recordRead(MVKey{Address: addr, Kind: MVStorage, Slot: slot})
	return s.PebbleStateDB.GetState(addr, slot)
}

func (s *TxState) SetState(addr common.Address, slot, value common.Hash) common.Hash {
	s.recordWrite(MVKey{Address: addr, Kind: MVStorage, Slot: slot})
	return s.PebbleStateDB.SetState(addr, slot, value)
}

func (s *TxState) GetStorageRoot(addr common.Address) common.Hash {
	s.recordRead(MVKey{Address: addr, Kind: MVStorageRoot})
	return s.PebbleStateDB.GetStorageRoot(addr)
}

func (s *TxState) SelfDestruct(addr common.Address) uint256.Int {
	s.recordRead(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVAccount})
	return s.PebbleStateDB.SelfDestruct(addr)
}

func (s *TxState) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	s.recordRead(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVBalance})
	s.recordWrite(MVKey{Address: addr, Kind: MVAccount})
	return s.PebbleStateDB.SelfDestruct6780(addr)
}

func (s *TxState) Exist(addr common.Address) bool {
	s.recordRead(MVKey{Address: addr, Kind: MVAccount})
	return s.PebbleStateDB.Exist(addr)
}

func (s *TxState) Empty(addr common.Address) bool {
	s.recordRead(MVKey{Address: addr, Kind: MVAccount})
	return s.PebbleStateDB.Empty(addr)
}

// ParallelExecutor speculates on isolated EVM state overlays and resolves
// results in canonical transaction order. A narrower direct S path handles
// blocks made entirely of certified zero-fee EOA transfers. P artifacts remain
// disabled until certified artifact generation/evaluation is implemented.
type ParallelExecutor struct {
	ChainConfig *params.ChainConfig
	ChainID     *big.Int
	Workers     int
	Checkpoints CheckpointConfig
}

func NewParallelExecutor(config *params.ChainConfig, chainID *big.Int, workers int) *ParallelExecutor {
	if workers < 1 {
		workers = runtime.GOMAXPROCS(0)
	}
	return &ParallelExecutor{
		ChainConfig: config, ChainID: new(big.Int).Set(chainID), Workers: workers,
		Checkpoints: DefaultCheckpointConfig(),
	}
}

// ConfigureCheckpoints enables deterministic capture/recovery for the selected
// policy. The default constructor continues to use P0.
func (e *ParallelExecutor) ConfigureCheckpoints(config CheckpointConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	e.Checkpoints = config
	return nil
}

// NewCheckpointSelector creates deterministic, execution-local policy state.
// It does not attach recovery or mutate EVM state.
func (e *ParallelExecutor) NewCheckpointSelector() (*CheckpointSelector, error) {
	if e == nil {
		return nil, fmt.Errorf("checkpoint selector requires parallel executor")
	}
	return newCheckpointSelector(e.Checkpoints)
}

func (e *ParallelExecutor) executeStaticBlock(base StateReader, env BlockEnv, txs []*types.Transaction, accesses []staticTransfer) (*BlockResult, error) {
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
		access := accesses[i]
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
		waveLimit := workers * 128
		if waveLimit < workers { // protect against integer overflow
			waveLimit = workers
		}
		for j := i + 1; j < len(txs) && len(wave) < waveLimit; j++ {
			next := accesses[j]
			if (next.sender == (common.Address{}) && next.recipient == (common.Address{})) || reservedGas > remainingGas || txs[j].Gas() > remainingGas-reservedGas || used[next.sender] || used[next.recipient] {
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
		if workerCount > 1 && len(wave) > 1 {
			parallelCount += float64(len(wave))
		}

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
				if err := applyStaticTransfer(stateDB, txs[attempt.index], attempt.access, attempt.value, env.Coinbase); err != nil {
					return nil, fmt.Errorf("resolve S tx %d: %w", attempt.index, err)
				}
				access := attempt.access
				versions.CommitTransaction(attempt.index, attempt.incarnation, stateVersionedWrites(stateDB, access.writes.keys[:access.writes.len]))
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
	result.CheckpointMetrics = CheckpointMetrics{
		Aborts:        uint64(abortTotal),
		Retries:       uint64(abortTotal),
		FullFallbacks: uint64(abortTotal),
	}
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
	stateDB, ok := base.(*PebbleStateDB)
	if err != nil || !ok || len(stateDB.getAccount(sender).CodeHash) != 0 || len(stateDB.getAccount(*tx.To()).CodeHash) != 0 || isPrecompileAddress(*tx.To()) {
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
func applyStaticTransfer(state *PebbleStateDB, tx *types.Transaction, access staticTransfer, value *uint256.Int, coinbase common.Address) error {
	if value == nil {
		return fmt.Errorf("static transfer has no validated value")
	}
	return state.ApplyEOATransfer(access.sender, access.recipient, coinbase, tx.Nonce(), value)
}

func isPrecompileAddress(address common.Address) bool {
	for i := 0; i < common.AddressLength-1; i++ {
		if address[i] != 0 {
			return false
		}
	}
	return address[common.AddressLength-1] > 0 && address[common.AddressLength-1] <= 10
}
