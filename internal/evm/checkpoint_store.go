package evm

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

const (
	// PureCheckpointFormatVersion định danh định dạng bản ghi checkpoint Phase 2.
	PureCheckpointFormatVersion uint32 = 1

	// DefaultCheckpointInterval là chu kỳ bước opcode mặc định cho chính sách fixed (và block 1 của history).
	DefaultCheckpointInterval uint64 = 64

	// DefaultCheckpointBudgetBytes là trần ngân sách RAM mặc định (64 MiB) cho CheckpointStore.
	DefaultCheckpointBudgetBytes uint64 = 64 << 20

	// MaxHistoryHotSlots là số khóa (contract, slot) tối đa mà chính sách history giữ từ block H-1 sang H.
	MaxHistoryHotSlots = 32

	// Quy tắc hạch toán bộ nhớ xác định cho mỗi PurePrefixCheckpoint trong CheckpointStore:
	// - structOverhead: kích thước struct PurePrefixCheckpoint làm tròn lên bội 8 byte.
	// - storeEntryOverhead: chi phí quản lý cố định của kho (mục slice/map theo tx và reservation).
	// - stackBytes: len(Stack) * 32 byte (mỗi uint256.Int chiếm đúng 32 byte, cấp phát len == cap).
	// - memoryBytes: align8(len(Memory)) (cấp phát đúng len == cap, làm tròn lên bội 8 byte).
	// - returnDataBytes: align8(len(ReturnData)) (cấp phát đúng len == cap, làm tròn lên bội 8 byte).
	// - preReadsSliceBytes: len(PreExecReads) * sizeof(MVKey).
	// - preReadsMapBytes: len(PreExecReads) * (sizeof(MVKey) + 8 + 16) (key + version + overhead bucket Go map).
	checkpointStoreEntryOverheadBytes uint64 = 64
	checkpointUint256Bytes            uint64 = uint64(unsafe.Sizeof(uint256.Int{}))
	checkpointMVKeyBytes              uint64 = uint64(unsafe.Sizeof(MVKey{}))
	checkpointMVVersionEntryBytes     uint64 = uint64(unsafe.Sizeof(MVKey{})) + 8 + 16
)

var (
	// ErrCheckpointSizeOverflow báo phép tính kích thước checkpoint bị tràn uint64.
	ErrCheckpointSizeOverflow = errors.New("checkpoint accounted bytes uint64 overflow")
)

// CheckpointAccountingBreakdown tách rõ từng thành phần tạo nên accounted_bytes để kiểm thử và kiểm toán.
type CheckpointAccountingBreakdown struct {
	StructOverheadBytes uint64
	StoreOverheadBytes  uint64
	StackBytes          uint64
	MemoryBytes         uint64
	ReturnDataBytes     uint64
	PreReadsSliceBytes  uint64
	PreReadsMapBytes    uint64
	TotalBytes          uint64
}

func safeAddUint64(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, false
	}
	return a + b, true
}

func safeMulUint64(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxUint64/b {
		return 0, false
	}
	return a * b, true
}

func align8Uint64(n uint64) (uint64, bool) {
	if n == 0 {
		return 0, true
	}
	rem := n % 8
	if rem == 0 {
		return n, true
	}
	return safeAddUint64(n, 8-rem)
}

// EstimatePureCheckpointBytes ước lượng chính xác số byte hạch toán TRƯỚC khi cấp phát và sao chép dữ liệu.
func EstimatePureCheckpointBytes(stackLen, memLen, retDataLen, preReadsLen int) (uint64, CheckpointAccountingBreakdown, error) {
	if stackLen < 0 || memLen < 0 || retDataLen < 0 || preReadsLen < 0 {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	return EstimatePureCheckpointBytesUint64(uint64(stackLen), uint64(memLen), uint64(retDataLen), uint64(preReadsLen))
}

// EstimatePureCheckpointBytesUint64 phiên bản uint64 phục vụ kiểm thử tràn số biên.
func EstimatePureCheckpointBytesUint64(stackLen, memLen, retDataLen, preReadsLen uint64) (uint64, CheckpointAccountingBreakdown, error) {
	var b CheckpointAccountingBreakdown
	var ok bool

	b.StructOverheadBytes, ok = align8Uint64(uint64(unsafe.Sizeof(PurePrefixCheckpoint{})))
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	b.StoreOverheadBytes = checkpointStoreEntryOverheadBytes

	b.StackBytes, ok = safeMulUint64(stackLen, checkpointUint256Bytes)
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	b.MemoryBytes, ok = align8Uint64(memLen)
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	b.ReturnDataBytes, ok = align8Uint64(retDataLen)
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	b.PreReadsSliceBytes, ok = safeMulUint64(preReadsLen, checkpointMVKeyBytes)
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}
	b.PreReadsMapBytes, ok = safeMulUint64(preReadsLen, checkpointMVVersionEntryBytes)
	if !ok {
		return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
	}

	total := b.StructOverheadBytes
	for _, part := range []uint64{
		b.StoreOverheadBytes,
		b.StackBytes,
		b.MemoryBytes,
		b.ReturnDataBytes,
		b.PreReadsSliceBytes,
		b.PreReadsMapBytes,
	} {
		total, ok = safeAddUint64(total, part)
		if !ok {
			return 0, CheckpointAccountingBreakdown{}, ErrCheckpointSizeOverflow
		}
	}
	b.TotalBytes = total
	return total, b, nil
}

// PurePrefixCheckpoint là ảnh chụp bất biến trong RAM chỉ dành cho prefix thuần tính toán ở frame gốc (Phase 2).
// Mọi buffer bên trong đều được sở hữu riêng (len == cap) và không bao giờ bị sửa sau khi chốt vào kho.
type PurePrefixCheckpoint struct {
	// Định danh & ngữ cảnh lời gọi
	FormatVersion   uint32
	Fork            string
	BlockNumber     uint64
	TxIndex         int
	TxHash          common.Hash
	Attempt         uint64
	Caller          common.Address
	Callee          common.Address
	CodeHash        common.Hash
	InitialFrameGas uint64
	InputHash       common.Hash
	InputLen        int

	// Vị trí ranh giới trước gas
	PC          uint64
	Opcode      byte
	NextStep    uint64
	Reason      string // "fixed" | "history"
	TriggerSlot *MVKey

	// Dữ liệu frame (bản sao sở hữu hoàn toàn)
	Stack             []uint256.Int
	Memory            []byte
	MemoryLen         uint64
	MemoryLastGasCost uint64
	Gas               uint64
	ReturnData        []byte
	CallGasTemp       uint64

	// Phụ thuộc đọc ở bước chuẩn bị giao dịch (step < 0)
	PreExecReads        []MVKey
	PreExecReadVersions map[MVKey]uint64
	BeforeRead          int

	// Dung lượng đã được đặt chỗ trong CheckpointStore
	AccountedBytes uint64
}

// CheckpointStore quản lý ngân sách RAM cứng M byte dùng chung giữa mọi worker trong một block.
// Luôn thực thi quy tắc reserve-before-copy và không bao giờ chặn goroutine khi hết chỗ.
type CheckpointStore struct {
	mu            sync.Mutex
	budgetBytes   uint64
	usedBytes     uint64
	peakBytes     uint64
	byTx          map[int][]*PurePrefixCheckpoint
	reserved      map[uint64]uint64
	nextReserveID uint64
	skippedBudget uint64
}

// NewCheckpointStore khởi tạo kho checkpoint với trần cứng budgetBytes (M = 0 tương đương tắt lưu).
func NewCheckpointStore(budgetBytes uint64) *CheckpointStore {
	return &CheckpointStore{
		budgetBytes: budgetBytes,
		byTx:        make(map[int][]*PurePrefixCheckpoint),
		reserved:    make(map[uint64]uint64),
	}
}

// BudgetBytes trả về trần ngân sách M bất biến của kho (không cần khóa mutex trên mỗi opcode).
func (s *CheckpointStore) BudgetBytes() uint64 {
	if s == nil {
		return 0
	}
	return s.budgetBytes
}

// Reserve xin đặt chỗ đồng bộ bytes trước khi cấp phát bản sao stack/memory.
// Nếu hết chỗ hoặc M == 0, trả ok=false ngay lập tức (không block -> tránh deadlock).
func (s *CheckpointStore) Reserve(bytes uint64) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.budgetBytes == 0 || bytes == 0 || bytes > s.budgetBytes || s.usedBytes > s.budgetBytes-bytes {
		s.skippedBudget++
		return 0, false
	}
	s.usedBytes += bytes
	if s.usedBytes > s.peakBytes {
		s.peakBytes = s.usedBytes
	}
	s.nextReserveID++
	id := s.nextReserveID
	s.reserved[id] = bytes
	return id, true
}

// CommitReservation chuyển chỗ đã đặt sang checkpoint bất biến gắn với txIndex.
func (s *CheckpointStore) CommitReservation(resID uint64, txIndex int, cp *PurePrefixCheckpoint) error {
	if s == nil {
		return errors.New("nil checkpoint store")
	}
	if cp == nil {
		s.CancelReservation(resID)
		return errors.New("nil checkpoint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	reservedBytes, ok := s.reserved[resID]
	if !ok {
		return fmt.Errorf("unknown checkpoint reservation %d", resID)
	}
	if cp.AccountedBytes != reservedBytes {
		// nếu sai lệch kích thước -> hủy chỗ đã đặt để không lệch kế toán
		delete(s.reserved, resID)
		s.usedBytes -= reservedBytes
		return fmt.Errorf("checkpoint accounted bytes %d != reserved %d", cp.AccountedBytes, reservedBytes)
	}
	delete(s.reserved, resID)
	s.byTx[txIndex] = append(s.byTx[txIndex], cp)
	return nil
}

// CancelReservation hoàn lại chỗ đã đặt nếu thao tác chụp bị hủy trước khi commit.
func (s *CheckpointStore) CancelReservation(resID uint64) {
	if s == nil || resID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes, ok := s.reserved[resID]; ok {
		delete(s.reserved, resID)
		if bytes >= s.usedBytes {
			s.usedBytes = 0
		} else {
			s.usedBytes -= bytes
		}
	}
}

// CheckpointsForTx trả danh sách con trỏ tới các checkpoint bất biến của txIndex theo thứ tự tạo.
func (s *CheckpointStore) CheckpointsForTx(txIndex int) []*PurePrefixCheckpoint {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byTx[txIndex]
	if len(list) == 0 {
		return nil
	}
	out := make([]*PurePrefixCheckpoint, len(list))
	copy(out, list)
	return out
}

// ReleaseTx giải phóng toàn bộ checkpoint thuộc txIndex và trừ khỏi usedBytes.
func (s *CheckpointStore) ReleaseTx(txIndex int) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byTx[txIndex]
	if len(list) == 0 {
		return 0
	}
	var freed uint64
	for _, cp := range list {
		if cp != nil {
			freed += cp.AccountedBytes
		}
	}
	delete(s.byTx, txIndex)
	if freed >= s.usedBytes {
		s.usedBytes = 0
	} else {
		s.usedBytes -= freed
	}
	return freed
}

// ReleaseAll giải phóng toàn bộ checkpoint và reservation còn lại (dùng ở cuối block hoặc khi hủy tác vụ).
func (s *CheckpointStore) ReleaseAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.byTx)
	clear(s.reserved)
	s.usedBytes = 0
}

// Stats trả về (usedBytes, peakBytes, budgetBytes, skippedBudget) tại thời điểm gọi.
func (s *CheckpointStore) Stats() (used, peak, budget, skipped uint64) {
	if s == nil {
		return 0, 0, 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usedBytes, s.peakBytes, s.budgetBytes, s.skippedBudget
}

// CheckpointHistoryTracker học top-32 khóa (contract, slot) xung đột từ block H-1 sang block H.
// Block đầu tiên (hasHistory == false) dùng fixed với cùng N; từ block thứ hai trở đi chỉ chụp
// trước SLOAD/SSTORE thuộc danh sách nóng đã chốt của block trước.
type CheckpointHistoryTracker struct {
	mu         sync.RWMutex
	maxHot     int
	hasHistory bool
	hotList    []MVKey
	hotSet     map[MVKey]struct{}
}

// NewCheckpointHistoryTracker khởi tạo bộ theo dõi lịch sử xung đột xuyên block (mặc định top 32).
func NewCheckpointHistoryTracker(maxHot int) *CheckpointHistoryTracker {
	if maxHot <= 0 {
		maxHot = MaxHistoryHotSlots
	}
	return &CheckpointHistoryTracker{
		maxHot: maxHot,
		hotSet: make(map[MVKey]struct{}),
	}
}

// SnapshotForBlock trả bản sao bất biến dùng cố định trong suốt một block:
// hasHistory=false nghĩa là block đầu tiên (history fallback về fixed với cùng N).
func (t *CheckpointHistoryTracker) SnapshotForBlock() (hasHistory bool, hotSet map[MVKey]struct{}, hotList []MVKey) {
	if t == nil {
		return false, nil, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.hasHistory {
		return false, nil, nil
	}
	setCopy := make(map[MVKey]struct{}, len(t.hotSet))
	for k := range t.hotSet {
		setCopy[k] = struct{}{}
	}
	listCopy := make([]MVKey, len(t.hotList))
	copy(listCopy, t.hotList)
	return true, setCopy, listCopy
}

// HotSlots trả danh sách khóa nóng hiện hành theo thứ tự tất định.
func (t *CheckpointHistoryTracker) HotSlots() []MVKey {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]MVKey, len(t.hotList))
	copy(out, t.hotList)
	return out
}

// EstimatedMemoryBytes ước lượng xác định bộ nhớ bảng top-32 slot nóng hiện giữ trong RAM.
func (t *CheckpointHistoryTracker) EstimatedMemoryBytes() uint64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	n := uint64(len(t.hotList))
	t.mu.RUnlock()
	if n == 0 {
		return 0
	}
	return n*checkpointMVKeyBytes + n*(checkpointMVKeyBytes+16)
}

// HasHistory cho biết tracker đã chốt ít nhất một block trước đó hay chưa.
func (t *CheckpointHistoryTracker) HasHistory() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.hasHistory
}

// Reset xóa toàn bộ lịch sử đã học (dùng giữa các lượt chạy/chính sách độc lập).
func (t *CheckpointHistoryTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hasHistory = false
	t.hotList = nil
	t.hotSet = make(map[MVKey]struct{})
}

// RecordBlockConflicts chốt top-32 (contract, slot) từ tần suất xung đột lần thử đầu tiên của block vừa xong.
// Thứ tự tất định: count giảm dần -> Address tăng dần -> Slot tăng dần. Nếu 0 xung đột -> danh sách rỗng.
func (t *CheckpointHistoryTracker) RecordBlockConflicts(conflictCounts map[MVKey]uint64) {
	if t == nil {
		return
	}
	type entry struct {
		key   MVKey
		count uint64
	}
	entries := make([]entry, 0, len(conflictCounts))
	for key, cnt := range conflictCounts {
		if key.Kind != MVStorage || cnt == 0 {
			continue
		}
		entries = append(entries, entry{key: key, count: cnt})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		if cmp := bytes.Compare(entries[i].key.Address[:], entries[j].key.Address[:]); cmp != 0 {
			return cmp < 0
		}
		return bytes.Compare(entries[i].key.Slot[:], entries[j].key.Slot[:]) < 0
	})
	limit := t.maxHot
	if limit <= 0 {
		limit = MaxHistoryHotSlots
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}

	nextList := make([]MVKey, len(entries))
	nextSet := make(map[MVKey]struct{}, len(entries))
	for i, e := range entries {
		nextList[i] = e.key
		nextSet[e.key] = struct{}{}
	}

	t.mu.Lock()
	t.hasHistory = true
	t.hotList = nextList
	t.hotSet = nextSet
	t.mu.Unlock()
}

// Phase2CheckpointMetrics tổng hợp đầy đủ số liệu quan sát Phase 2–3 cho một block OCC / predicted-occ.
type Phase2CheckpointMetrics struct {
	Policy                  string            `json:"checkpointPolicy"`
	EffectivePolicy         string            `json:"effectivePolicy"` // "fixed" ở block 1 của history/adaptive
	Interval                uint64            `json:"checkpointEvery"`
	BudgetBytes             uint64            `json:"checkpointBudgetBytes"`
	CaptureRequested        uint64            `json:"captureRequested"`
	CaptureCreated          uint64            `json:"captureCreated"`
	CaptureSkippedBudget    uint64            `json:"captureSkippedBudget"`
	CaptureSkippedLowScore  uint64            `json:"captureSkippedLowScore"`
	AdaptiveCandidates      uint64            `json:"adaptiveCandidates"`
	AdaptiveCalibHits       uint64            `json:"adaptiveCalibHits"`
	AdaptiveCalibMisses     uint64            `json:"adaptiveCalibMisses"`
	AdaptiveHistoryMisses   uint64            `json:"adaptiveHistoryMisses"`
	ResumeAttempted         uint64            `json:"resumeAttempted"`
	ResumeCommitted         uint64            `json:"resumeCommitted"`
	ResumeCancelled         uint64            `json:"resumeCancelled"`
	RestoreRejectedPrefix   uint64            `json:"restoreRejectedPrefix"`
	AccountedPeakBytes      uint64            `json:"accountedPeakBytes"`
	AccountedRemainingBytes uint64            `json:"accountedRemainingBytes"`
	CaptureNs               uint64            `json:"captureNs"`
	SelectNs                uint64            `json:"selectNs"`
	RestoreNs               uint64            `json:"restoreNs"`
	ScoreNs                 uint64            `json:"scoreNs"`
	HistoryUpdateNs         uint64            `json:"historyUpdateNs"`
	HistoryMemoryBytes      uint64            `json:"historyMemoryBytes"`
	OpcodeSteps             uint64            `json:"opcodeSteps"`
	PrefixStepsSaved        uint64            `json:"prefixStepsSaved"`
	OffObserverDisabled     bool              `json:"offObserverDisabled,omitempty"`
	FallbackReasons         map[string]uint64 `json:"fallbackReasons,omitempty"`
	HotSlots                []MVKey           `json:"hotSlots,omitempty"`
}

func newPhase2CheckpointMetrics(policy, effectivePolicy string, interval, budgetBytes uint64, hotSlots []MVKey) *Phase2CheckpointMetrics {
	return &Phase2CheckpointMetrics{
		Policy:          policy,
		EffectivePolicy: effectivePolicy,
		Interval:        interval,
		BudgetBytes:     budgetBytes,
		FallbackReasons: make(map[string]uint64),
		HotSlots:        hotSlots,
	}
}

func (m *Phase2CheckpointMetrics) addFallback(reason string) {
	if m == nil || reason == "" {
		return
	}
	if m.FallbackReasons == nil {
		m.FallbackReasons = make(map[string]uint64)
	}
	m.FallbackReasons[reason]++
}

// Phase2TraceEvent ghi dấu vết chi tiết nối lần đọc cũ -> checkpoint -> bước resume -> lần chạy chốt receipt.
type Phase2TraceEvent struct {
	Policy              string `json:"policy"`
	BlockNumber         uint64 `json:"block"`
	TxIndex             int    `json:"tx_index"`
	TxHash              string `json:"tx_hash"`
	Attempt             int    `json:"attempt"`
	Event               string `json:"event"` // capture_created | capture_skipped_budget | conflict | checkpoint_selected | resume_start | fallback | commit
	Step                uint64 `json:"step,omitempty"`
	PC                  uint64 `json:"pc,omitempty"`
	Opcode              string `json:"opcode,omitempty"`
	GasRemaining        uint64 `json:"gas_remaining,omitempty"`
	Reason              string `json:"reason,omitempty"`
	SlotKey             string `json:"slot_key,omitempty"`
	ConflictKey         string `json:"conflict_key,omitempty"`
	EarliestInvalidStep int64  `json:"earliest_invalid_step,omitempty"`
	ResumedFromStep     uint64 `json:"resumed_from_step,omitempty"`
	ResumedFromPC       uint64 `json:"resumed_from_pc,omitempty"`
	Resumed             bool   `json:"resumed,omitempty"`
	OpcodeSteps         uint64 `json:"opcode_steps,omitempty"`
	AccountedBytes      uint64 `json:"accounted_bytes,omitempty"`
}
