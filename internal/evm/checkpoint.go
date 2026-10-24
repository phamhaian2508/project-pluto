package evm

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// CheckpointPolicy xác định thời điểm bộ thực thi song song chụp checkpoint
// trong bộ nhớ. Việc chọn chỉ dựa trên vị trí opcode, sự kiện đọc trạng thái và
// dấu xung đột theo thứ tự; không dùng thời gian thực.
type CheckpointPolicy uint8

const (
	CheckpointPolicyP0 CheckpointPolicy = iota // không tạo checkpoint
	CheckpointPolicyP1                         // trước mỗi lần đọc trạng thái
	CheckpointPolicyP2                         // sau mỗi chu kỳ opcode đã cấu hình
	CheckpointPolicyP3                         // trước khi đọc khóa thường gây xung đột
)

// CheckpointConfig cấu hình cách chọn checkpoint có tính tất định. Chỉ P2 cần
// Interval; giá trị này cho biết số opcode thực thi giữa hai checkpoint.
type CheckpointConfig struct {
	Policy   CheckpointPolicy
	Interval uint64
}

func DefaultCheckpointConfig() CheckpointConfig {
	return CheckpointConfig{Policy: CheckpointPolicyP0}
}

func (c CheckpointConfig) Validate() error {
	if c.Policy > CheckpointPolicyP3 {
		return errors.New("unknown checkpoint policy")
	}
	if c.Policy == CheckpointPolicyP2 && c.Interval == 0 {
		return errors.New("P2 checkpoint interval must be greater than zero")
	}
	return nil
}

type CheckpointReason uint8

const (
	CheckpointBeforeStateRead CheckpointReason = iota
	CheckpointOpcodeInterval
	CheckpointBeforeHotRead
)

// CheckpointDecision xác định ranh giới ngay trước sự kiện tại OpcodePosition.
// OpcodePosition là số opcode đã thực thi trong giao dịch, bắt đầu từ 0.
type CheckpointDecision struct {
	OpcodePosition uint64
	Reason         CheckpointReason
	ReadKey        MVKey
}

// CheckpointSelector chỉ tồn tại trong một lần thực thi. Các khóa xung đột phải
// được đánh dấu theo thứ tự xác thực chuẩn để P3 không phụ thuộc lịch chạy goroutine.
type CheckpointSelector struct {
	config  CheckpointConfig
	hotKeys map[MVKey]struct{}
}

func newCheckpointSelector(config CheckpointConfig) (*CheckpointSelector, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &CheckpointSelector{config: config, hotKeys: make(map[MVKey]struct{})}, nil
}

// BeforeOpcode chọn checkpoint P2 sau khi đã thực thi đúng N, 2N, ... opcode.
// Vị trí 0 là ranh giới đầu giao dịch, không phải điểm checkpoint P2.
func (s *CheckpointSelector) BeforeOpcode(position uint64) (CheckpointDecision, bool) {
	if s == nil || s.config.Policy != CheckpointPolicyP2 || position == 0 || position%s.config.Interval != 0 {
		return CheckpointDecision{}, false
	}
	return CheckpointDecision{OpcodePosition: position, Reason: CheckpointOpcodeInterval}, true
}

// BeforeStateRead chọn checkpoint P1 cho mọi lần đọc và P3 chỉ cho khóa từng
// được đánh dấu thường xung đột trong lần xác thực có thứ tự.
func (s *CheckpointSelector) BeforeStateRead(position uint64, key MVKey) (CheckpointDecision, bool) {
	if s == nil {
		return CheckpointDecision{}, false
	}
	switch s.config.Policy {
	case CheckpointPolicyP1:
		return CheckpointDecision{OpcodePosition: position, Reason: CheckpointBeforeStateRead, ReadKey: key}, true
	case CheckpointPolicyP3:
		if _, hot := s.hotKeys[key]; hot {
			return CheckpointDecision{OpcodePosition: position, Reason: CheckpointBeforeHotRead, ReadKey: key}, true
		}
	}
	return CheckpointDecision{}, false
}

// MarkReadConflict đánh dấu một khóa là thường xung đột theo cách tất định. Bên
// gọi phải cung cấp xung đột theo thứ tự xác thực giao dịch chuẩn.
func (s *CheckpointSelector) MarkReadConflict(key MVKey) {
	if s != nil && s.config.Policy == CheckpointPolicyP3 {
		s.hotKeys[key] = struct{}{}
	}
}

// InterpreterState chứa trạng thái không phụ thuộc bản rẽ nhánh geth, cần thiết
// để tiếp tục giữa các opcode. Bộ kết nối geth của chương trình điền dữ liệu này.
type InterpreterState struct {
	PC                uint64
	Stack             []uint256.Int
	Memory            []byte
	MemoryLastGasCost uint64
	ReturnData        []byte
	Gas               uint64
	CallGasTemp       uint64
	CallFrames        []CallFrameState
}

// CallFrameState sở hữu ngữ cảnh thực thi và trạng thái máy có thể thay đổi của
// một khung gọi EVM đang tạm dừng.
type CallFrameState struct {
	PC          uint64
	Stack       []uint256.Int
	Memory      []byte
	ReturnData  []byte
	Gas         uint64
	Depth       int
	CallType    byte
	Caller      common.Address
	Address     common.Address
	CodeAddress common.Address
	CodeHash    common.Hash
	Code        []byte
	Input       []byte
	Value       uint256.Int
	ReadOnly    bool
	Deployment  bool
	SystemCall  bool
}

// ExecutionCheckpoint là ảnh chụp bất biến chỉ lưu trong RAM. Kiểu này không có
// giao diện tuần tự hóa hoặc cơ sở dữ liệu.
type ExecutionCheckpoint struct {
	interpreter InterpreterState
	state       *PebbleStateDB
	txIndex     int
	incarnation uint64
	readSet     []MVKey
	writeSet    []MVKey
	reads       map[MVKey]struct{}
	writes      map[MVKey]struct{}
}

// TakeExecutionCheckpoint chụp thủ công một checkpoint đầy đủ. Các chính sách
// tự động không gọi hàm này khi P0 đang hoạt động.
func TakeExecutionCheckpoint(interpreter *InterpreterState, state *TxState) (*ExecutionCheckpoint, error) {
	if interpreter == nil || state == nil || state.PebbleStateDB == nil {
		return nil, errors.New("checkpoint requires interpreter and transaction state")
	}
	return &ExecutionCheckpoint{
		interpreter: cloneInterpreterState(*interpreter),
		state:       cloneStateForCheckpoint(state.PebbleStateDB),
		txIndex:     state.TxIndex,
		incarnation: state.Incarnation,
		readSet:     append([]MVKey(nil), state.ReadSet...),
		writeSet:    append([]MVKey(nil), state.WriteSet...),
		reads:       cloneMVKeySet(state.reads),
		writes:      cloneMVKeySet(state.writes),
	}, nil
}

// Restore thay toàn bộ trạng thái trình thông dịch, lớp phủ, tập truy cập và lần
// chạy bằng bản sao riêng của dữ liệu đã chụp. Nhờ đó có thể dùng checkpoint
// nhiều lần mà không chia sẻ vùng nhớ có thể thay đổi.
func (c *ExecutionCheckpoint) Restore(interpreter *InterpreterState, state *TxState) error {
	if c == nil || interpreter == nil || state == nil {
		return errors.New("restore requires checkpoint, interpreter and transaction state")
	}
	*interpreter = cloneInterpreterState(c.interpreter)
	state.PebbleStateDB = cloneStateForCheckpoint(c.state)
	state.TxIndex = c.txIndex
	state.Incarnation = c.incarnation
	state.ReadSet = append([]MVKey(nil), c.readSet...)
	state.WriteSet = append([]MVKey(nil), c.writeSet...)
	state.reads = cloneMVKeySet(c.reads)
	state.writes = cloneMVKeySet(c.writes)
	return nil
}

// EstimatedMemoryBytes trả về kích thước dữ liệu xác định mà checkpoint sở hữu.
// Hàm không tính phần phụ trội cấp phát map/slice khi chạy, vì phần này thay đổi
// theo phiên bản Go và làm số liệu benchmark kém ổn định.
func (c *ExecutionCheckpoint) EstimatedMemoryBytes() uint64 {
	if c == nil {
		return 0
	}
	size := interpreterPayloadBytes(c.interpreter)
	size += uint64(len(c.readSet)+len(c.writeSet)+len(c.reads)+len(c.writes)) * 53 // địa chỉ + loại khóa + ô lưu trữ
	if c.state == nil {
		return size
	}
	for _, account := range c.state.state {
		size += 20
		if account != nil {
			size += 8 + uint64(len(account.CodeHash))
			if account.Balance != nil {
				size += uint64(len(account.Balance.Bytes()))
			}
		}
	}
	for _, code := range c.state.code {
		size += 20 + uint64(len(code))
	}
	size += storagePayloadBytes(c.state.storage)
	size += storagePayloadBytes(c.state.transientStorage)
	size += storagePayloadBytes(c.state.originStorage)
	for _, log := range c.state.logs {
		if log != nil {
			size += 20 + uint64(len(log.Topics))*32 + uint64(len(log.Data))
		}
	}
	for _, entry := range c.state.journal {
		size += 20 + 8 + 32 + 32 + uint64(len(entry.prevCode)+len(entry.prevCodeHash))
		if entry.prevBalance != nil {
			size += uint64(len(entry.prevBalance.Bytes()))
		}
	}
	size += uint64(len(c.state.snapshots)) * 32
	if c.state.accessList != nil {
		size += uint64(len(c.state.accessList.addresses)) * 20
		for _, slots := range c.state.accessList.slots {
			size += 20 + uint64(len(slots))*32
		}
	}
	return size
}

func interpreterPayloadBytes(state InterpreterState) uint64 {
	size := uint64(16 + len(state.Stack)*32 + len(state.Memory) + len(state.ReturnData))
	for _, frame := range state.CallFrames {
		size += uint64(16 + len(frame.Stack)*32 + len(frame.Memory) + len(frame.ReturnData))
		size += 20*3 + uint64(len(frame.Code)+len(frame.Input)) + 32
	}
	return size
}

func storagePayloadBytes(storage map[common.Address]map[common.Hash]common.Hash) uint64 {
	var size uint64
	for _, slots := range storage {
		size += 20 + uint64(len(slots))*64
	}
	return size
}

func cloneInterpreterState(src InterpreterState) InterpreterState {
	dst := InterpreterState{
		PC: src.PC, Stack: append([]uint256.Int(nil), src.Stack...),
		Memory: append([]byte(nil), src.Memory...), MemoryLastGasCost: src.MemoryLastGasCost,
		ReturnData: append([]byte(nil), src.ReturnData...), Gas: src.Gas, CallGasTemp: src.CallGasTemp,
		CallFrames: make([]CallFrameState, len(src.CallFrames)),
	}
	for i, frame := range src.CallFrames {
		dst.CallFrames[i] = frame
		dst.CallFrames[i].Stack = append([]uint256.Int(nil), frame.Stack...)
		dst.CallFrames[i].Memory = append([]byte(nil), frame.Memory...)
		dst.CallFrames[i].ReturnData = append([]byte(nil), frame.ReturnData...)
		dst.CallFrames[i].Code = append([]byte(nil), frame.Code...)
		dst.CallFrames[i].Input = append([]byte(nil), frame.Input...)
	}
	return dst
}

func cloneStateForCheckpoint(src *PebbleStateDB) *PebbleStateDB {
	dst := NewPebbleStateDB(src.db)
	dst.overlayBase = src.overlayBase
	for addr, account := range src.state {
		if account == nil {
			dst.state[addr] = nil
			continue
		}
		clone := *account
		if account.Balance != nil {
			clone.Balance = new(big.Int).Set(account.Balance)
		}
		clone.CodeHash = append([]byte(nil), account.CodeHash...)
		dst.state[addr] = &clone
	}
	for addr, code := range src.code {
		dst.code[addr] = append([]byte(nil), code...)
	}
	dst.dirtyCode = cloneAddressBoolMap(src.dirtyCode)
	dst.storage = cloneAddressStorage(src.storage)
	dst.dirtyStorage = cloneAddressBoolStorage(src.dirtyStorage)
	dst.selfDestructed = cloneAddressBoolMap(src.selfDestructed)
	dst.transientStorage = cloneAddressStorage(src.transientStorage)
	dst.refund = src.refund
	dst.logs = cloneLogs(src.logs)
	dst.journal = cloneJournal(src.journal)
	dst.snapshots = append([]snapshot(nil), src.snapshots...)
	dst.nextRevID = src.nextRevID
	dst.accessList = cloneAccessList(src.accessList)
	dst.dirtyAccounts = cloneAddressBoolMap(src.dirtyAccounts)
	dst.originStorage = cloneAddressStorage(src.originStorage)
	return dst
}

func cloneJournal(src []journalEntry) []journalEntry {
	dst := make([]journalEntry, len(src))
	for i, entry := range src {
		dst[i] = entry
		if entry.prevBalance != nil {
			dst[i].prevBalance = new(big.Int).Set(entry.prevBalance)
		}
		dst[i].prevCode = append([]byte(nil), entry.prevCode...)
		dst[i].prevCodeHash = append([]byte(nil), entry.prevCodeHash...)
	}
	return dst
}

func cloneAccessList(src *accessList) *accessList {
	if src == nil {
		return nil
	}
	dst := newAccessList()
	dst.addresses = cloneAddressBoolMap(src.addresses)
	dst.slots = cloneAddressBoolStorage(src.slots)
	return dst
}

func cloneAddressStorage(src map[common.Address]map[common.Hash]common.Hash) map[common.Address]map[common.Hash]common.Hash {
	dst := make(map[common.Address]map[common.Hash]common.Hash, len(src))
	for addr, slots := range src {
		dst[addr] = cloneHashMap(slots)
	}
	return dst
}

func cloneAddressBoolStorage(src map[common.Address]map[common.Hash]bool) map[common.Address]map[common.Hash]bool {
	dst := make(map[common.Address]map[common.Hash]bool, len(src))
	for addr, slots := range src {
		cloned := make(map[common.Hash]bool, len(slots))
		for key, value := range slots {
			cloned[key] = value
		}
		dst[addr] = cloned
	}
	return dst
}

func cloneAddressBoolMap(src map[common.Address]bool) map[common.Address]bool {
	dst := make(map[common.Address]bool, len(src))
	for addr, value := range src {
		dst[addr] = value
	}
	return dst
}

func cloneMVKeySet(src map[MVKey]struct{}) map[MVKey]struct{} {
	dst := make(map[MVKey]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}
