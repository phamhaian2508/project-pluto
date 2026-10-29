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

// ExecutionClass là quyết định phân luồng P/O/S có tính tất định. P chỉ dành
// cho mô tả thực thi đã được chứng nhận; bộ phân tích thận trọng hiện chưa tạo
// ra loại mô tả này.
type ExecutionClass uint8

const (
	ExecutionStatic     ExecutionClass = iota // S: tập truy cập được giới hạn tĩnh.
	ExecutionParametric                       // P: mô tả thực thi tham số hóa đã chứng nhận.
	ExecutionOpaque                           // O: thực thi EVM thông thường theo thứ tự.
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

// MVKey định danh một thành phần trạng thái có phiên bản. Bộ phân tích chuyển
// khoản tĩnh ở phiên bản đầu chỉ theo dõi khóa số dư và nonce của tài khoản.
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
	// MVAccount là quan hệ phụ thuộc thận trọng trên toàn tài khoản. Các thao tác
	// StateDB có thể tạo, xóa hoặc làm tài khoản rỗng cũng ghi nhận khóa này để
	// bước xác thực lạc quan không bỏ sót ảnh hưởng.
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

// Bộ phân tích S hiện hỗ trợ chuyển khoản, mỗi giao dịch chạm tối đa ba khóa.
// Lưu các tập nhỏ này trực tiếp giúp tránh tạo map và slice cho từng giao dịch.
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

// MVMemory là bảng lưu các phiên bản đã chấp nhận. Bảng được chia thành nhiều
// phần để việc xác thực song song và cập nhật theo thứ tự không tranh một khóa chung.
type MVMemory struct{ shards [32]versionShard }

func NewMVMemory() *MVMemory {
	m := &MVMemory{}
	for i := range m.shards {
		m.shards[i].versions = make(map[MVKey]uint64)
		m.shards[i].history = make(map[MVKey][]MVVersionRecord)
	}
	return m
}

// ReadBefore trả về phiên bản đã chấp nhận mới nhất do giao dịch trước txIndex
// tạo ra. Giá trị được sao chép để bên gọi không thể sửa MVMemory.
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

// Snapshot, Validate và Commit giữ giao diện dùng slice tổng quát cho kiểm thử
// và các bộ phân tích tương lai có tập truy cập lớn hơn. Đường chuyển khoản
// thường dùng các phiên bản kích thước cố định ở trên để tránh cấp phát lặp lại.
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

// TxState là lớp phủ trạng thái riêng có nhật ký, được tách từ trạng thái khối.
// PebbleStateDB nhúng bên trong giữ nguyên ngữ nghĩa vm.StateDB của geth; nó
// đọc từ dữ liệu Pebble bất biến và giữ mọi thay đổi trong map riêng đến khi bộ
// phân giải có thứ tự chấp nhận tập ghi.
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
	// Cập nhật một ô lưu trữ không làm thay đổi sự tồn tại, nonce, số dư hoặc mã
	// của tài khoản. Tăng phiên bản MVAccount cho mọi SSTORE sẽ tạo xung đột giả
	// với phần hợp đồng/mã đã xác thực và ngăn tái sử dụng checkpoint an toàn ngay
	// trước SLOAD.
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
			} else {
				// Include an explicit tombstone so a transition to zero still
				// advances the MVMemory version and invalidates stale readers.
				values[key] = nil
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

// Lớp bọc ghi nhận các quan hệ phụ thuộc trạng thái bền vững, còn trạng thái EVM
// tạm thời (hoàn gas, danh sách truy cập và vùng lưu trữ tạm) vẫn thuộc riêng giao dịch.
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

// ParallelExecutor thực thi đầu cơ trên các lớp phủ EVM cô lập rồi phân giải
// kết quả theo thứ tự giao dịch chuẩn. Đường S trực tiếp hẹp hơn xử lý các khối
// chỉ gồm chuyển khoản EOA không phí đã được chứng nhận. P vẫn tắt cho đến khi
// có thể tạo và đánh giá mô tả thực thi đã chứng nhận.
type ParallelExecutor struct {
	ChainConfig     *params.ChainConfig
	ChainID         *big.Int
	Workers         int
	AutoWorkers     bool
	Checkpoints     CheckpointConfig
	workerMu        sync.Mutex
	adaptiveWorkers int
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

// NewAdaptiveParallelExecutor dùng GOMAXPROCS làm giới hạn trên và điều chỉnh
// số worker giữa các đợt dựa trên xung đột đọc/ghi quan sát được.
func NewAdaptiveParallelExecutor(config *params.ChainConfig, chainID *big.Int, maxWorkers int) *ParallelExecutor {
	executor := NewParallelExecutor(config, chainID, maxWorkers)
	executor.AutoWorkers = true
	return executor
}

func (e *ParallelExecutor) startingWorkerCount(limit int) int {
	if e == nil || !e.AutoWorkers {
		return limit
	}
	e.workerMu.Lock()
	defer e.workerMu.Unlock()
	if e.adaptiveWorkers < 1 || e.adaptiveWorkers > limit {
		e.adaptiveWorkers = limit
	}
	return e.adaptiveWorkers
}

func (e *ParallelExecutor) saveAdaptiveWorkerCount(count int) {
	if e == nil || !e.AutoWorkers || count < 1 {
		return
	}
	e.workerMu.Lock()
	e.adaptiveWorkers = count
	e.workerMu.Unlock()
}

// ConfigureCheckpoints bật chụp/khôi phục có tính tất định theo chính sách đã
// chọn. Hàm khởi tạo mặc định vẫn dùng P0.
func (e *ParallelExecutor) ConfigureCheckpoints(config CheckpointConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	e.Checkpoints = config
	return nil
}

// NewCheckpointSelector tạo trạng thái chính sách tất định, chỉ dùng trong một
// lần thực thi. Hàm không gắn cơ chế khôi phục và không sửa trạng thái EVM.
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
	var workerStats workerUsage

	for i := 0; i < len(txs); {
		access := accesses[i]
		// Tạo một đợt S liên tiếp, tất định và không xung đột. Danh sách truy cập
		// tĩnh gồm số dư người gửi/người nhận và nonce người gửi.
		wave := []int{i}
		waveAccesses := []staticTransfer{access}
		used := map[common.Address]bool{access.sender: true, access.recipient: true}
		reservedGas := txs[i].Gas()
		// Chuyển khoản có chi phí đánh giá thấp. Nếu mỗi đợt chỉ lớn bằng số worker
		// thì cứ vài giao dịch lại phải chờ đồng bộ; vì vậy gom nhiều tác vụ cho
		// mỗi worker để giảm chi phí điều phối và xác thực. Kiểm tra xung đột địa
		// chỉ vẫn là ranh giới an toàn của mỗi đợt.
		waveLimit := workers * 128
		if waveLimit < workers { // Ngăn phép nhân gây tràn số.
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
			workerStats.add(1)
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
		workerStats.add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			worker := worker
			wg.Add(1)
			jobs <- func() {
				defer wg.Done()
				// Mỗi tác vụ được xếp lịch đánh giá một nhóm giao dịch, giảm chi phí
				// channel và tạo hàm đóng cho thao tác chuyển khoản S vốn rất nhỏ.
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

		// Xác thực và phân giải nghiêm ngặt theo TxIndex ban đầu, không phụ thuộc
		// thứ tự hoàn tất của worker.
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
	workerStats.record(result.Metrics, e.AutoWorkers)
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

type workerUsage struct {
	min     int
	max     int
	total   int
	samples int
}

func (usage *workerUsage) add(count int) {
	if usage == nil || count < 1 {
		return
	}
	if usage.samples == 0 || count < usage.min {
		usage.min = count
	}
	if count > usage.max {
		usage.max = count
	}
	usage.total += count
	usage.samples++
}

func (usage workerUsage) record(metrics map[string]float64, automatic bool) {
	if metrics == nil {
		return
	}
	if automatic {
		metrics["worker_auto"] = 1
	} else {
		metrics["worker_auto"] = 0
	}
	if usage.samples == 0 {
		return
	}
	metrics["worker_active_min"] = float64(usage.min)
	metrics["worker_active_max"] = float64(usage.max)
	metrics["worker_active_avg"] = float64(usage.total) / float64(usage.samples)
}

// adaptWorkerCount giữ mức song song khi ít xung đột và hạ worker khi nhiều
// giao dịch phải chạy lại vì đã đọc phiên bản trạng thái cũ.
func adaptWorkerCount(current, limit, conflicts, txCount int) int {
	if current < 1 {
		current = 1
	}
	if limit < current {
		limit = current
	}
	if conflicts > 0 {
		if conflicts*4 >= txCount {
			current /= 2
		} else {
			current--
		}
		if current < 1 {
			current = 1
		}
		return current
	}
	if current < limit && txCount >= current {
		current++
	}
	return current
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

// applyStaticTransfer phân giải một giao dịch S đã xác thực với ảnh hưởng giao
// thức giống lời gọi EOA tốn 21.000 gas của geth. Đường nhanh này chỉ áp dụng
// cho giao dịch legacy không phí, calldata rỗng, gửi đến địa chỉ không phải hợp
// đồng hoặc precompile; các giao dịch khác đều dùng ApplyMessage.
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
