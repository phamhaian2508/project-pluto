package evm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// AdaptiveModelFormatVersion định danh schema model.json của Phase 3.
	AdaptiveModelFormatVersion = 1
)

// SizeBucket ánh xạ số byte checkpoint vào khoảng lũy thừa 2: [2^k, 2^(k+1)).
// Với bytes == 0 trả về 0 ([0, 1)).
func SizeBucket(bytes uint64) uint32 {
	if bytes == 0 {
		return 0
	}
	return uint32(bits.Len64(bytes) - 1)
}

// SizeBucketBounds trả cận dưới bao gồm và cận trên loại trừ [2^k, 2^(k+1)) của sizeBucket.
func SizeBucketBounds(bucket uint32) (minBytes, maxBytes uint64) {
	if bucket >= 63 {
		return uint64(1) << 63, math.MaxUint64
	}
	return uint64(1) << bucket, uint64(1) << (bucket + 1)
}

// ExtractSelector lấy 4 byte selector đầu tiên của calldata (đệm 0 nếu calldata < 4 byte).
func ExtractSelector(input []byte) [4]byte {
	var sel [4]byte
	copy(sel[:], input)
	return sel
}

// FormatSelectorHex định dạng selector 4 byte dạng 0x21252a06.
func FormatSelectorHex(sel [4]byte) string {
	return "0x" + hex.EncodeToString(sel[:])
}

// ParseSelectorHex giải mã chuỗi hex 4 byte ("0x21252a06" hoặc "21252a06").
func ParseSelectorHex(raw string) ([4]byte, error) {
	var out [4]byte
	trimmed := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(raw), "0x"), "0X")
	if len(trimmed) != 8 {
		return out, fmt.Errorf("selector hex %q must be 8 hex chars", raw)
	}
	b, err := hex.DecodeString(trimmed)
	if err != nil {
		return out, fmt.Errorf("invalid selector hex %q: %w", raw, err)
	}
	copy(out[:], b)
	return out, nil
}

// AdaptiveCalibrationKey là khóa tra bảng hiệu chuẩn model.json: (codeHash, selector, pc, sizeBucket).
type AdaptiveCalibrationKey struct {
	CodeHash   common.Hash
	Selector   [4]byte
	PC         uint64
	SizeBucket uint32
}

// AdaptiveCalibrationEntry lưu trung vị ghép cặp (retry_ns - resume_ns) và capture_ns cho một nhóm vị trí + cỡ.
type AdaptiveCalibrationEntry struct {
	CodeHash       string  `json:"codeHash"`
	Selector       string  `json:"selector"`
	PC             uint64  `json:"pc"`
	SizeBucket     uint32  `json:"sizeBucket"`
	SizeMinBytes   uint64  `json:"sizeMinBytes"`
	SizeMaxBytes   uint64  `json:"sizeMaxBytes"`
	Samples        int     `json:"samples"`
	GainNs         float64 `json:"gainNs"`
	CaptureNs      float64 `json:"captureNs"`
	MedianRetryNs  float64 `json:"medianRetryNs,omitempty"`
	MedianResumeNs float64 `json:"medianResumeNs,omitempty"`
}

// AdaptiveNSelectionRecord lưu kết quả thăm dò chu kỳ N trên seed phát triển.
type AdaptiveNSelectionRecord struct {
	Interval         uint64  `json:"interval"`
	Runs             int     `json:"runs"`
	MedianNs         float64 `json:"medianNs"`
	IQRNs            float64 `json:"iqrNs"`
	MedianCreated    float64 `json:"medianCaptureCreated"`
	MedianResumed    float64 `json:"medianResumeCommitted"`
	MedianSkippedBud float64 `json:"medianSkippedBudget"`
}

// AdaptiveThresholdRecord lưu kết quả thăm dò ngưỡng score trên seed phát triển.
type AdaptiveThresholdRecord struct {
	Label            string  `json:"label"`
	Threshold        float64 `json:"thresholdNsPerByte"`
	Runs             int     `json:"runs"`
	MedianNs         float64 `json:"medianNs"`
	IQRNs            float64 `json:"iqrNs"`
	MedianCreated    float64 `json:"medianCaptureCreated"`
	MedianResumed    float64 `json:"medianResumeCommitted"`
	MedianSkippedLow float64 `json:"medianSkippedLowScore"`
	MedianSkippedBud float64 `json:"medianSkippedBudget"`
}

// AdaptiveBudgetCalibration lưu bằng chứng chọn 2 mức RAM chặt và rộng trên seed phát triển.
type AdaptiveBudgetCalibration struct {
	InitialTightBytes       uint64 `json:"initialTightBytes"`
	InitialWideBytes        uint64 `json:"initialWideBytes"`
	InitialTightSkipped     uint64 `json:"initialTightSkippedBudget"`
	HistoryWidePeakBytes    uint64 `json:"historyWidePeakBytes"`
	HistorySteadyPeakBytes  uint64 `json:"historySteadyPeakBytes"`
	Adjusted                bool   `json:"adjusted"`
	SelectedTightBytes      uint64 `json:"selectedTightBytes"`
	SelectedWideBytes       uint64 `json:"selectedWideBytes"`
	TightVerifiedSkipped    uint64 `json:"tightVerifiedSkippedBudget"`
	WideVerifiedSkipped     uint64 `json:"wideVerifiedSkippedBudget"`
	TightSteadySkipped      uint64 `json:"tightSteadySkippedBudget"`
}

// AdaptiveModel là bảng hiệu chuẩn bất biến khóa từ seed phát triển (results/phase3/model.json).
type AdaptiveModel struct {
	FormatVersion         int                        `json:"formatVersion"`
	Suite                 string                     `json:"suite"`
	Seed                  uint64                     `json:"seed"`
	TargetCase            string                     `json:"targetCase"`
	TimestampUTC          string                     `json:"timestampUtc"`
	CalibrationDurationNs int64                      `json:"calibrationDurationNs"`
	FixtureManifestSHA256 string                     `json:"fixtureManifestSha256,omitempty"`
	FixtureCaseInputHash  string                     `json:"fixtureCaseInputHash,omitempty"`
	RuntimeCodeHash       string                     `json:"runtimeCodeHash,omitempty"`
	CheckpointEvery       uint64                     `json:"checkpointEvery"`
	TightBudgetBytes      uint64                     `json:"tightBudgetBytes"`
	WideBudgetBytes       uint64                     `json:"wideBudgetBytes"`
	ScoreThreshold        float64                    `json:"scoreThresholdNsPerByte"`
	PositiveScoreCount    int                        `json:"positiveScoreCount"`
	PositiveScoreMedian   float64                    `json:"positiveScoreMedianNsPerByte"`
	BudgetCalibration     *AdaptiveBudgetCalibration `json:"budgetCalibration,omitempty"`
	NSelection            []AdaptiveNSelectionRecord `json:"nSelection,omitempty"`
	ThresholdSelection    []AdaptiveThresholdRecord  `json:"thresholdSelection,omitempty"`
	Entries               []AdaptiveCalibrationEntry `json:"entries"`

	// chỉ mục tra cứu bất biến trong RAM và SHA-256 của file JSON đã nạp
	index     map[AdaptiveCalibrationKey]AdaptiveCalibrationEntry
	sha256Hex string
}

// BuildIndex dựng bảng băm tra cứu bất biến từ Entries và kiểm tra hợp lệ dữ liệu số.
func (m *AdaptiveModel) BuildIndex() error {
	if m == nil {
		return errors.New("nil adaptive model")
	}
	if math.IsNaN(m.ScoreThreshold) || math.IsInf(m.ScoreThreshold, 0) {
		return fmt.Errorf("adaptive model has non-finite scoreThreshold: %v", m.ScoreThreshold)
	}
	idx := make(map[AdaptiveCalibrationKey]AdaptiveCalibrationEntry, len(m.Entries))
	for i, e := range m.Entries {
		if e.Samples <= 0 {
			return fmt.Errorf("adaptive model entry %d has non-positive samples=%d", i, e.Samples)
		}
		if math.IsNaN(e.GainNs) || math.IsInf(e.GainNs, 0) || math.IsNaN(e.CaptureNs) || math.IsInf(e.CaptureNs, 0) {
			return fmt.Errorf("adaptive model entry %d has non-finite gain/capture", i)
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.CodeHash)), "0x") || len(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e.CodeHash)), "0x")) != 64 {
			return fmt.Errorf("adaptive model entry %d has invalid codeHash %q", i, e.CodeHash)
		}
		sel, err := ParseSelectorHex(e.Selector)
		if err != nil {
			return fmt.Errorf("adaptive model entry %d: %w", i, err)
		}
		key := AdaptiveCalibrationKey{
			CodeHash:   common.HexToHash(e.CodeHash),
			Selector:   sel,
			PC:         e.PC,
			SizeBucket: e.SizeBucket,
		}
		idx[key] = e
	}
	m.index = idx
	return nil
}

// Lookup tra mẫu hiệu chuẩn theo (codeHash, selector, pc, sizeBucket).
func (m *AdaptiveModel) Lookup(key AdaptiveCalibrationKey) (AdaptiveCalibrationEntry, bool) {
	if m == nil || len(m.index) == 0 {
		return AdaptiveCalibrationEntry{}, false
	}
	e, ok := m.index[key]
	if !ok || e.Samples <= 0 {
		return AdaptiveCalibrationEntry{}, false
	}
	return e, true
}

// SHA256 trả về chuỗi SHA-256 của file model.json (hoặc JSON chuẩn hóa nếu dựng trong RAM).
func (m *AdaptiveModel) SHA256() string {
	if m == nil {
		return ""
	}
	if m.sha256Hex != "" {
		return m.sha256Hex
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "0x" + hex.EncodeToString(sum[:])
}

// LoadAdaptiveModel đọc, kiểm tra hợp lệ và đánh chỉ mục file model.json.
func LoadAdaptiveModel(path string) (*AdaptiveModel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read adaptive model %q: %w", path, err)
	}
	var m AdaptiveModel
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse adaptive model %q: %w", path, err)
	}
	if m.FormatVersion != AdaptiveModelFormatVersion {
		return nil, fmt.Errorf("unsupported adaptive model formatVersion=%d (want %d)", m.FormatVersion, AdaptiveModelFormatVersion)
	}
	if err := m.BuildIndex(); err != nil {
		return nil, fmt.Errorf("validate adaptive model %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	m.sha256Hex = "0x" + hex.EncodeToString(sum[:])
	return &m, nil
}

// SaveAdaptiveModel tuần tự hóa model.json xác định (sắp xếp entries) và trả về SHA-256 của file vừa ghi.
func SaveAdaptiveModel(path string, m *AdaptiveModel) (string, error) {
	if m == nil {
		return "", errors.New("nil adaptive model")
	}
	if m.FormatVersion == 0 {
		m.FormatVersion = AdaptiveModelFormatVersion
	}
	sort.Slice(m.Entries, func(i, j int) bool {
		a, b := m.Entries[i], m.Entries[j]
		if a.CodeHash != b.CodeHash {
			return a.CodeHash < b.CodeHash
		}
		if a.Selector != b.Selector {
			return a.Selector < b.Selector
		}
		if a.PC != b.PC {
			return a.PC < b.PC
		}
		return a.SizeBucket < b.SizeBucket
	})
	if err := m.BuildIndex(); err != nil {
		return "", err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create model dir %q: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal adaptive model: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("write adaptive model %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	m.sha256Hex = "0x" + hex.EncodeToString(sum[:])
	return m.sha256Hex, nil
}

// AdaptiveScoreInput gom toàn bộ tham số cho hàm tính điểm lợi ích thuần túy (dễ unit-test).
type AdaptiveScoreInput struct {
	Prob           float64
	GainNs         float64
	CaptureNs      float64
	AccountedBytes uint64
	Threshold      float64
}

// AdaptiveScoreResult trả về kết quả chấm điểm và lý do từ chối cụ thể nếu Accept == false.
type AdaptiveScoreResult struct {
	Accept bool
	NetNs  float64
	Score  float64 // ns/byte
	Reason string  // "" | "zero_bytes" | "non_finite" | "non_positive_net" | "below_threshold"
}

// ScoreAdaptiveCandidate tính net_ns = p*gain_ns - capture_ns và score = net_ns / bytes.
// Chỉ nhận khi bytes > 0, mọi giá trị hữu hạn, net_ns > 0 và score >= threshold.
func ScoreAdaptiveCandidate(in AdaptiveScoreInput) AdaptiveScoreResult {
	if in.AccountedBytes == 0 {
		return AdaptiveScoreResult{Accept: false, Reason: "zero_bytes"}
	}
	if math.IsNaN(in.Prob) || math.IsInf(in.Prob, 0) || in.Prob < 0 || in.Prob > 1 ||
		math.IsNaN(in.GainNs) || math.IsInf(in.GainNs, 0) ||
		math.IsNaN(in.CaptureNs) || math.IsInf(in.CaptureNs, 0) ||
		math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0) {
		return AdaptiveScoreResult{Accept: false, Reason: "non_finite"}
	}
	netNs := in.Prob*in.GainNs - in.CaptureNs
	score := netNs / float64(in.AccountedBytes)
	if math.IsNaN(netNs) || math.IsInf(netNs, 0) || math.IsNaN(score) || math.IsInf(score, 0) {
		return AdaptiveScoreResult{Accept: false, Reason: "non_finite"}
	}
	if netNs <= 0 {
		return AdaptiveScoreResult{Accept: false, NetNs: netNs, Score: score, Reason: "non_positive_net"}
	}
	if score < in.Threshold {
		return AdaptiveScoreResult{Accept: false, NetNs: netNs, Score: score, Reason: "below_threshold"}
	}
	return AdaptiveScoreResult{Accept: true, NetNs: netNs, Score: score}
}

// AdaptiveLocationKey nhóm mẫu học xuyên block theo (codeHash, selector, pc, slot).
type AdaptiveLocationKey struct {
	CodeHash common.Hash
	Selector [4]byte
	PC       uint64
	Slot     MVKey
}

// AdaptiveSampleCounts lưu cặp (seen, useful) của một nhóm vị trí hoặc một slot ở block trước.
type AdaptiveSampleCounts struct {
	Seen   uint64
	Useful uint64
}

// AdaptiveCandidateObservation ghi nhận ứng viên ngay trước lần truy cập state đầu tiên trong frame gốc.
type AdaptiveCandidateObservation struct {
	Key         AdaptiveLocationKey
	Step        uint64
	Opcode      byte
	PrefixValid bool
}

// AdaptiveHistorySnapshot là bản chụp chỉ đọc bất biến của bảng block H-1 dùng trong suốt block H.
type AdaptiveHistorySnapshot struct {
	HasHistory bool
	ByLocation map[AdaptiveLocationKey]AdaptiveSampleCounts
	BySlot     map[MVKey]AdaptiveSampleCounts
}

// LookupProb tra p = useful / seen theo vị trí chính xác; nếu mẫu đơn (seen==1, useful==0)
// có thể do rơi vào tx đầu block hoặc vị trí mới nhưng khóa nóng đã có thống kê rộng hơn,
// fallback sang tỷ lệ gộp của khóa sum(useful) / sum(seen).
func (s *AdaptiveHistorySnapshot) LookupProb(loc AdaptiveLocationKey) (p float64, seen, useful uint64, source string, ok bool) {
	if s == nil || !s.HasHistory {
		return 0, 0, 0, "missing", false
	}
	if c, found := s.ByLocation[loc]; found && c.Seen > 0 {
		if c.Seen >= 2 || c.Useful > 0 {
			return float64(c.Useful) / float64(c.Seen), c.Seen, c.Useful, "exact", true
		}
		if sc, sFound := s.BySlot[loc.Slot]; sFound && sc.Seen > c.Seen {
			return float64(sc.Useful) / float64(sc.Seen), sc.Seen, sc.Useful, "slot_fallback", true
		}
		return 0, c.Seen, 0, "exact", true
	}
	if c, found := s.BySlot[loc.Slot]; found && c.Seen > 0 {
		return float64(c.Useful) / float64(c.Seen), c.Seen, c.Useful, "slot_fallback", true
	}
	return 0, 0, 0, "missing", false
}

// EstimatedMemoryBytes ước lượng xác định bộ nhớ bảng lịch sử adaptive đang giữ (chỉ 1 bảng block trước).
func (s *AdaptiveHistorySnapshot) EstimatedMemoryBytes() uint64 {
	if s == nil || !s.HasHistory {
		return 0
	}
	locEntry := uint64(unsafe.Sizeof(AdaptiveLocationKey{}) + unsafe.Sizeof(AdaptiveSampleCounts{}) + 16)
	slotEntry := uint64(unsafe.Sizeof(MVKey{}) + unsafe.Sizeof(AdaptiveSampleCounts{}) + 16)
	return uint64(len(s.ByLocation))*locEntry + uint64(len(s.BySlot))*slotEntry
}

// AdaptiveHistoryTracker giữ duy nhất bảng đã công bố của block trước (không giữ lịch sử vô hạn).
type AdaptiveHistoryTracker struct {
	mu   sync.RWMutex
	prev *AdaptiveHistorySnapshot
}

func NewAdaptiveHistoryTracker() *AdaptiveHistoryTracker {
	return &AdaptiveHistoryTracker{}
}

// SnapshotForBlock trả con trỏ tới snapshot bất biến của block trước (không cần copy lại cho từng worker).
func (t *AdaptiveHistoryTracker) SnapshotForBlock() *AdaptiveHistorySnapshot {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	snap := t.prev
	t.mu.RUnlock()
	return snap
}

// Reset giải phóng hoàn toàn bảng lịch sử (dùng giữa các lượt chạy/chính sách độc lập).
func (t *AdaptiveHistoryTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.prev = nil
	t.mu.Unlock()
}

// SeedForTest nạp sẵn bảng lịch sử dựng trước cho unit test (không làm thay đổi vòng đời chuẩn).
func (t *AdaptiveHistoryTracker) SeedForTest(byLoc map[AdaptiveLocationKey]AdaptiveSampleCounts) {
	if t == nil {
		return
	}
	locCopy := make(map[AdaptiveLocationKey]AdaptiveSampleCounts, len(byLoc))
	bySlot := make(map[MVKey]AdaptiveSampleCounts)
	for k, c := range byLoc {
		if c.Seen == 0 {
			continue
		}
		locCopy[k] = c
		agg := bySlot[k.Slot]
		agg.Seen += c.Seen
		agg.Useful += c.Useful
		bySlot[k.Slot] = agg
	}
	t.mu.Lock()
	t.prev = &AdaptiveHistorySnapshot{
		HasHistory: true,
		ByLocation: locCopy,
		BySlot:     bySlot,
	}
	t.mu.Unlock()
}

// PublishBlock thay thế toàn bộ bảng cũ bằng bảng mới chốt ở cuối block, giải phóng bảng H-2.
func (t *AdaptiveHistoryTracker) PublishBlock(byLoc map[AdaptiveLocationKey]AdaptiveSampleCounts) uint64 {
	if t == nil {
		return 0
	}
	locCopy := make(map[AdaptiveLocationKey]AdaptiveSampleCounts, len(byLoc))
	bySlot := make(map[MVKey]AdaptiveSampleCounts)
	for k, c := range byLoc {
		if c.Seen == 0 {
			continue
		}
		locCopy[k] = c
		agg := bySlot[k.Slot]
		agg.Seen += c.Seen
		agg.Useful += c.Useful
		bySlot[k.Slot] = agg
	}
	snap := &AdaptiveHistorySnapshot{
		HasHistory: true,
		ByLocation: locCopy,
		BySlot:     bySlot,
	}
	t.mu.Lock()
	t.prev = snap
	t.mu.Unlock()
	return snap.EstimatedMemoryBytes()
}

// AdaptiveDecisionEvent ghi lại dấu vết minh họa 1 quyết định chọn/bỏ checkpoint của adaptive/history.
type AdaptiveDecisionEvent struct {
	Policy             string  `json:"policy"`
	EffectivePolicy    string  `json:"effective_policy"`
	BlockNumber        uint64  `json:"block"`
	TxIndex            int     `json:"tx_index"`
	TxHash             string  `json:"tx_hash"`
	CodeHash           string  `json:"code_hash"`
	Selector           string  `json:"selector"`
	PC                 uint64  `json:"pc"`
	Steps              uint64  `json:"steps"`
	Opcode             string  `json:"opcode"`
	SlotKey            string  `json:"slot_key"`
	Prob               float64 `json:"p"`
	Seen               uint64  `json:"seen,omitempty"`
	Useful             uint64  `json:"useful,omitempty"`
	ProbSource         string  `json:"prob_source,omitempty"`
	Bytes              uint64  `json:"bytes"`
	SizeBucket         uint32  `json:"size_bucket"`
	GainNs             float64 `json:"gain_ns"`
	CaptureNs          float64 `json:"capture_ns"`
	NetNs              float64 `json:"net_ns"`
	ScoreNsPerByte     float64 `json:"score_ns_per_byte"`
	ThresholdNsPerByte float64 `json:"threshold_ns_per_byte"`
	Decision           string  `json:"decision"` // captured | skipped_low_score | skipped_budget | skipped_no_calibration | skipped_no_history | skipped_invalid | skipped_tx_limit | skipped_cold_slot | fixed_bootstrap
	Reason             string  `json:"reason,omitempty"`
}

// AdaptiveCalibrationSample lưu 1 cặp đo (retry_ns, resume_ns, capture_ns) tại lần truy cập state đầu tiên của 1 tx.
type AdaptiveCalibrationSample struct {
	TxIndex        int
	CodeHash       common.Hash
	Selector       [4]byte
	PC             uint64
	Step           uint64
	AccountedBytes uint64
	SizeBucket     uint32
	CaptureNs      int64
	RetryNs        int64
	ResumeNs       int64
	GainNs         int64 // RetryNs - ResumeNs
}

// CalibrateBlockFirstStatePairs chạy tuần tự từng tx trên stateDB để:
// 1) chụp đúng checkpoint tại lần truy cập state đầu tiên trong prefix thuần tính toán (đo capture_ns),
// 2) đo ghép cặp trên cùng nền sau các tx tiền nhiệm: 1 lượt full retry và 1 lượt resume từ checkpoint (đảo thứ tự chẵn/lẻ),
// 3) đối chiếu kết quả resume khớp tuyệt đối full retry rồi áp dụng delta lên stateDB cho tx kế tiếp.
func CalibrateBlockFirstStatePairs(
	ctx context.Context,
	stateDB *PebbleStateDB,
	chainCfg *params.ChainConfig,
	env BlockEnv,
	txs []*types.Transaction,
) ([]AdaptiveCalibrationSample, error) {
	if stateDB == nil || chainCfg == nil {
		return nil, errors.New("calibration requires non-nil stateDB and chainConfig")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}
	signer := types.MakeSigner(chainCfg, big.NewInt(env.Height), env.Time)
	versions := NewMVMemory()
	remainingGas := env.GasLimit

	samples := make([]AdaptiveCalibrationSample, 0, len(txs))
	for i, tx := range txs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tx == nil {
			continue
		}
		curPrefix := newPrefixSnapshot(stateDB, versions, i, remainingGas)

		// bước 1: chụp đúng 1 checkpoint tại lần truy cập state đầu tiên (SLOAD/SSTORE) và đo capture_ns
		calStore := NewCheckpointStore(DefaultCheckpointBudgetBytes)
		calSpec := &windowCaptureSpec{
			policyName:          "calibrate",
			effectivePolicy:     "calibrate",
			calibrateFirstState: true,
			store:               calStore,
			countOpcodes:        false,
			enableCapture:       true,
			recordTrace:         false,
		}
		capOut := runWindowAttemptWithCapture(ctx, i, 1, tx, curPrefix, env, chainCfg, signer, nil, calSpec)
		cp := capOut.calibratedCP
		captureNs := int64(capOut.captureNs)
		calStore.ReleaseAll()

		if capOut.internalErr != nil {
			return nil, fmt.Errorf("calibrate capture tx %d: %w", i, capOut.internalErr)
		}
		if capOut.applyErr != nil {
			continue
		}

		// bước 2: đo ghép cặp retry_ns và resume_ns trên cùng curPrefix (đảo thứ tự chẵn/lẻ tránh thiên lệch cache)
		var retryOut *windowAttemptOutcome
		var resumedOut *windowAttemptOutcome
		var retryNs, resumeNs int64
		var resErr error
		var resFallback string

		if cp != nil {
			runRetry := func() {
				t0 := time.Now()
				retryOut = runWindowAttemptWithCapture(ctx, i, 2, tx, curPrefix, env, chainCfg, signer, nil, nil)
				retryNs = time.Since(t0).Nanoseconds()
			}
			runResume := func() {
				t0 := time.Now()
				resumedOut, resFallback, _, resErr = resumePurePrefixAttempt(i, 2, tx, curPrefix, env, chainCfg, signer, cp)
				resumeNs = time.Since(t0).Nanoseconds()
			}
			if i%2 == 0 {
				runRetry()
				runResume()
			} else {
				runResume()
				runRetry()
			}
			if retryOut.internalErr != nil {
				return nil, fmt.Errorf("calibrate retry tx %d: %w", i, retryOut.internalErr)
			}
			if resErr != nil && !errors.Is(resErr, errResumeUnsupported) {
				return nil, fmt.Errorf("calibrate resume tx %d (%s): %w", i, resFallback, resErr)
			}
			if resErr == nil && retryOut.applyErr == nil {
				if resumedOut.result.GasUsed != retryOut.result.GasUsed ||
					resumedOut.result.Failed != retryOut.result.Failed ||
					!bytes.Equal(resumedOut.result.ReturnData, retryOut.result.ReturnData) ||
					len(resumedOut.result.Logs) != len(retryOut.result.Logs) {
					return nil, fmt.Errorf("calibrate tx %d resume mismatch vs retry (gas=%d/%d failed=%v/%v logs=%d/%d)",
						i, resumedOut.result.GasUsed, retryOut.result.GasUsed,
						resumedOut.result.Failed, retryOut.result.Failed,
						len(resumedOut.result.Logs), len(retryOut.result.Logs))
				}
				samples = append(samples, AdaptiveCalibrationSample{
					TxIndex:        i,
					CodeHash:       cp.CodeHash,
					Selector:       ExtractSelector(tx.Data()),
					PC:             cp.PC,
					Step:           cp.NextStep,
					AccountedBytes: cp.AccountedBytes,
					SizeBucket:     SizeBucket(cp.AccountedBytes),
					CaptureNs:      captureNs,
					RetryNs:        retryNs,
					ResumeNs:       resumeNs,
					GainNs:         retryNs - resumeNs,
				})
			}
		}

		// bước 3: áp dụng delta thật lên stateDB để tx kế tiếp chạy đúng nền sau tx i
		commitOut := capOut
		if retryOut != nil && retryOut.applyErr == nil {
			commitOut = retryOut
		}
		if err := stateDB.ApplyPendingWrites(commitOut.delta); err != nil {
			return nil, fmt.Errorf("calibrate apply tx %d delta: %w", i, err)
		}
		versions.CommitTransaction(i, uint64(commitOut.attempt), commitOut.state.VersionedWrites())
		remainingGas -= commitOut.result.GasUsed
	}
	return samples, nil
}
