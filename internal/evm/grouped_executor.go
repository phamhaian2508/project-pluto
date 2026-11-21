package evm

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// DefaultApproveTokenAllowanceSlot lấy từ storage-layout.json của bản build OpenZeppelin 4.9.6.
	DefaultApproveTokenAllowanceSlot uint64 = 1
)

var (
	// DefaultApproveTokenRuntimeCodeHash ghim đúng runtime bytecode hash của ApproveToken (OZ 4.9.6, solc 0.8.24, Shanghai).
	DefaultApproveTokenRuntimeCodeHash = common.HexToHash("0xa78dfa9373d661f184caea9f87e8a71536053b95a69ca100ca08eb63f100cc97")
	erc20ApproveSelector               = [4]byte{0x09, 0x5e, 0xa7, 0xb3}
)

// ApprovedERC20Spec mô tả một hợp đồng ERC-20 được bộ xếp nhóm nhận diện bằng runtime code hash và slot mapping `_allowances`.
type ApprovedERC20Spec struct {
	RuntimeCodeHash common.Hash
	AllowanceSlot   uint64
}

// DefaultApprovedERC20Specs trả về registry mặc định gồm mẫu ApproveToken OpenZeppelin 4.9.6.
func DefaultApprovedERC20Specs() map[common.Hash]ApprovedERC20Spec {
	return map[common.Hash]ApprovedERC20Spec{
		DefaultApproveTokenRuntimeCodeHash: {
			RuntimeCodeHash: DefaultApproveTokenRuntimeCodeHash,
			AllowanceSlot:   DefaultApproveTokenAllowanceSlot,
		},
	}
}

// ComputeERC20AllowanceSlot tính slot EVM của mapping `_allowances[owner][spender]` trong Solidity.
func ComputeERC20AllowanceSlot(owner, spender common.Address, mappingSlot uint64) common.Hash {
	var baseSlot [32]byte
	new(big.Int).SetUint64(mappingSlot).FillBytes(baseSlot[:])

	var outerInput [64]byte
	copy(outerInput[12:32], owner.Bytes())
	copy(outerInput[32:64], baseSlot[:])
	outerHash := crypto.Keccak256Hash(outerInput[:])

	var innerInput [64]byte
	copy(innerInput[12:32], spender.Bytes())
	copy(innerInput[32:64], outerHash.Bytes())
	return crypto.Keccak256Hash(innerInput[:])
}

// TxPattern định danh mẫu giao dịch nhận diện được trước khi thực thi.
type TxPattern string

const (
	PatternETHTransfer  TxPattern = "eth_transfer"
	PatternERC20Approve TxPattern = "erc20_approve"
	PatternUnknown      TxPattern = "unknown"
)

// DeclaredAccess giữ kết quả nhận diện và tập khóa đọc/ghi dự kiến tại trạng thái đầu nhóm.
type DeclaredAccess struct {
	Pattern TxPattern
	Reason  string
	Reads   map[MVKey]struct{}
	Writes  map[MVKey]struct{}
}

func newDeclaredAccess(pattern TxPattern) DeclaredAccess {
	return DeclaredAccess{
		Pattern: pattern,
		Reads:   make(map[MVKey]struct{}),
		Writes:  make(map[MVKey]struct{}),
	}
}

// GroupedHooks cho phép test đồng bộ worker đang chạy hoặc cố ý tiêm thiếu khóa khai báo.
type GroupedHooks struct {
	OnAttemptStart         func(ctx context.Context, txIndex, attempt int)
	OnStorageRead          func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64)
	OnAttemptBeforeDeliver func(ctx context.Context, txIndex, attempt int)
	OverrideDeclaredAccess func(txIndex int, declared *DeclaredAccess)
}

func (h *GroupedHooks) toOCCHooks() *OCCHooks {
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

// GroupedBlockStats tổng hợp số liệu xếp nhóm theo block.
type GroupedBlockStats struct {
	Workers           int
	GroupCount        int
	GroupWidths       []int
	MaxGroupWidth     int
	GroupedTxs        int
	SerialTxs         int
	UnknownTxs        int
	AbortedGroups     int
	RetriedTxs        int
	TotalRetries      int
	FallbackTxs       int
	CommittedAttempts map[int]int
}

// GroupedTxAttemptTrace ghi dấu vết một lần chạy thử hoặc chạy lại của từng giao dịch trong nhóm.
type GroupedTxAttemptTrace struct {
	TxIndex        int
	TxHash         common.Hash
	Attempt        int
	Status         string // "committed", "aborted", "fallback_committed", "rejected"
	Reason         string
	UndeclaredKey  *MVKey
	DeclaredReads  int
	DeclaredWrites int
	ActualReads    int
	ActualWrites   int
	UsedGas        uint64
	EVMStatus      uint64
	CreatedReceipt bool
	LogCount       int
}

// GroupTrace mô tả đầy đủ một nhóm từ lúc nhận diện, lý do tách nhóm tới khi chốt hoặc hủy chạy lại.
type GroupTrace struct {
	GroupID             int
	PrefixIndex         int
	PrefixGen           uint64
	TxIndices           []int
	Patterns            []TxPattern
	AttemptedWidth      int
	CommittedWidth      int
	Status              string // "committed_parallel", "committed_single", "committed_serial_unknown", "aborted_reexecuted_serial", "rejected"
	CommittedOnFirstTry bool
	SplitReason         string
	SplitKey            *MVKey
	AbortReason         string
	AbortedTxIndex      int
	UndeclaredKey       *MVKey
	Attempts            []GroupedTxAttemptTrace
}

// GroupedBlockTrace chứa toàn bộ dấu vết xếp nhóm của một block.
type GroupedBlockTrace struct {
	BlockNumber uint64
	Stats       GroupedBlockStats
	Groups      []GroupTrace
}

func (t *GroupedBlockTrace) Format() string {
	if t == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "block=%d workers=%d committedBatches=%d widths=%v maxWidth=%d groupedTxs=%d serialTxs=%d unknownTxs=%d abortedGroups=%d retriedTxs=%d totalRetries=%d fallbackTxs=%d\n",
		t.BlockNumber, t.Stats.Workers, t.Stats.GroupCount, t.Stats.GroupWidths, t.Stats.MaxGroupWidth,
		t.Stats.GroupedTxs, t.Stats.SerialTxs, t.Stats.UnknownTxs,
		t.Stats.AbortedGroups, t.Stats.RetriedTxs, t.Stats.TotalRetries, t.Stats.FallbackTxs)
	for _, g := range t.Groups {
		fmt.Fprintf(&b, "  group[%d] prefix=%d gen=%d txs=%v patterns=%v attemptedWidth=%d committedWidth=%d status=%s firstTry=%v",
			g.GroupID, g.PrefixIndex, g.PrefixGen, g.TxIndices, g.Patterns, g.AttemptedWidth, g.CommittedWidth, g.Status, g.CommittedOnFirstTry)
		if g.SplitReason != "" {
			fmt.Fprintf(&b, " splitReason=%s", g.SplitReason)
		}
		if g.SplitKey != nil {
			fmt.Fprintf(&b, " splitKey=%s", g.SplitKey.String())
		}
		if g.AbortReason != "" {
			fmt.Fprintf(&b, " abortReason=%s abortedTx=%d", g.AbortReason, g.AbortedTxIndex)
		}
		if g.UndeclaredKey != nil {
			fmt.Fprintf(&b, " undeclaredKey=%s", g.UndeclaredKey.String())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// GroupedExecutor mở rộng từ phân luồng tĩnh ExecutionStatic: nhận diện EOA transfer + OpenZeppelin ERC-20 approve,
// gom đợt độc lập chạy đồng thời và kiểm tra lại tập truy cập thực tế trước khi chốt.
type GroupedExecutor struct {
	ChainConfig     *params.ChainConfig
	ChainID         *big.Int
	Workers         int
	StrictInvalidTx bool
	RecordTrace     bool
	ApprovedTokens  map[common.Hash]ApprovedERC20Spec
	Hooks           *GroupedHooks
}

func NewGroupedExecutor(config *params.ChainConfig, chainID *big.Int, workers int) *GroupedExecutor {
	if workers < 1 {
		workers = 1
	}
	return &GroupedExecutor{
		ChainConfig:    config,
		ChainID:        new(big.Int).Set(chainID),
		Workers:        workers,
		RecordTrace:    true,
		ApprovedTokens: DefaultApprovedERC20Specs(),
	}
}

func (e *GroupedExecutor) approvedTokensMap() map[common.Hash]ApprovedERC20Spec {
	if len(e.ApprovedTokens) > 0 {
		return e.ApprovedTokens
	}
	return DefaultApprovedERC20Specs()
}

type classifiedMember struct {
	txIndex  int
	tx       *types.Transaction
	sender   common.Address
	declared DeclaredAccess
}

func (e *GroupedExecutor) ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error) {
	totalStarted := time.Now()
	stateDB, ok := base.(*PebbleStateDB)
	if !ok {
		return nil, fmt.Errorf("grouped executor requires *PebbleStateDB, got %T", base)
	}
	if e.ChainConfig == nil || e.ChainID == nil {
		return nil, fmt.Errorf("grouped executor requires chain config and chain ID")
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

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()

	signer := types.MakeSigner(e.ChainConfig, big.NewInt(env.Height), env.Time)
	rules := e.ChainConfig.Rules(big.NewInt(env.Height), true, env.Time)
	precompiles := make(map[common.Address]struct{})
	for _, p := range vm.ActivePrecompiles(rules) {
		precompiles[p] = struct{}{}
	}
	approvedTokens := e.approvedTokensMap()
	occHooks := e.Hooks.toOCCHooks()

	versions := NewMVMemory()
	remainingGas := env.GasLimit
	var blockLogIndex uint

	stats := GroupedBlockStats{
		Workers:           workers,
		CommittedAttempts: make(map[int]int, len(txs)),
	}
	var groupsTrace []GroupTrace
	if e.RecordTrace {
		groupsTrace = make([]GroupTrace, 0, len(txs))
	}

	result := &BlockResult{
		Results: make([]TxResult, len(txs)),
		Writes:  make(map[string][]byte),
		Metrics: make(map[string]float64),
	}

	recordCommittedGroup := func(width int) {
		stats.GroupCount++
		stats.GroupWidths = append(stats.GroupWidths, width)
		if width > stats.MaxGroupWidth {
			stats.MaxGroupWidth = width
		}
		if width >= 2 {
			stats.GroupedTxs += width
		} else {
			stats.SerialTxs += width
		}
	}

	groupID := 0
	for idx := 0; idx < len(txs); {
		curPrefix := newPrefixSnapshot(stateDB, versions, idx, remainingGas)
		headTx := txs[idx]
		headSender, headDecl, err := classifyTxForGroup(curPrefix.state, signer, env, headTx, precompiles, approvedTokens)
		if err != nil {
			return nil, fmt.Errorf("classify tx %d: %w", idx, err)
		}
		if headDecl.Pattern != PatternUnknown && e.Hooks != nil && e.Hooks.OverrideDeclaredAccess != nil {
			e.Hooks.OverrideDeclaredAccess(idx, &headDecl)
		}

		// mẫu chưa nhận diện chạy tuần tự đúng lượt ngay trên stateDB
		if headDecl.Pattern == PatternUnknown {
			tr, nextLogIdx, applyErr, internalErr := executeSerialTxOnCommitted(stateDB, env, e.ChainConfig, signer, headTx, idx, remainingGas, blockLogIndex)
			if internalErr != nil {
				return nil, internalErr
			}
			if applyErr != nil {
				if e.RecordTrace {
					groupsTrace = append(groupsTrace, GroupTrace{
						GroupID:        groupID,
						PrefixIndex:    curPrefix.committedIndex,
						PrefixGen:      curPrefix.generation,
						TxIndices:      []int{idx},
						Patterns:       []TxPattern{PatternUnknown},
						AttemptedWidth: 1,
						Status:         "rejected",
						SplitReason:    headDecl.Reason,
						AbortReason:    applyErr.Error(),
						AbortedTxIndex: idx,
					})
				}
				if e.StrictInvalidTx {
					var h common.Hash
					if headTx != nil {
						h = headTx.Hash()
					}
					return nil, &TxRejectedError{Index: idx, TxHash: h, Category: ClassifyRejectionError(applyErr.Error()), Err: applyErr}
				}
				result.Results[idx] = tr
				groupID++
				idx++
				continue
			}

			versions.BumpGeneration()
			blockLogIndex = nextLogIdx
			remainingGas -= tr.GasUsed
			result.GasUsed += tr.GasUsed
			result.Results[idx] = tr
			stats.CommittedAttempts[idx] = 1
			stats.UnknownTxs++
			recordCommittedGroup(1)

			if e.RecordTrace {
				evmStatus := uint64(types.ReceiptStatusSuccessful)
				if tr.Failed {
					evmStatus = types.ReceiptStatusFailed
				}
				groupsTrace = append(groupsTrace, GroupTrace{
					GroupID:             groupID,
					PrefixIndex:         curPrefix.committedIndex,
					PrefixGen:           curPrefix.generation,
					TxIndices:           []int{idx},
					Patterns:            []TxPattern{PatternUnknown},
					AttemptedWidth:      1,
					CommittedWidth:      1,
					Status:              "committed_serial_unknown",
					CommittedOnFirstTry: true,
					SplitReason:         headDecl.Reason,
					Attempts: []GroupedTxAttemptTrace{
						{
							TxIndex:        idx,
							TxHash:         headTx.Hash(),
							Attempt:        1,
							Status:         "committed",
							Reason:         headDecl.Reason,
							UsedGas:        tr.GasUsed,
							EVMStatus:      evmStatus,
							CreatedReceipt: true,
							LogCount:       len(tr.Logs),
						},
					},
				})
			}
			groupID++
			idx++
			continue
		}

		// gom tối đa W giao dịch liền nhau có tập truy cập dự kiến độc lập
		members := make([]classifiedMember, 0, workers)
		members = append(members, classifiedMember{txIndex: idx, tx: headTx, sender: headSender, declared: headDecl})
		groupReads := copyMVKeySet(headDecl.Reads)
		groupWrites := copyMVKeySet(headDecl.Writes)

		var splitReason string
		var splitKey *MVKey
		for next := idx + 1; next < len(txs) && len(members) < workers; next++ {
			candTx := txs[next]
			candSender, candDecl, err := classifyTxForGroup(curPrefix.state, signer, env, candTx, precompiles, approvedTokens)
			if err != nil {
				return nil, fmt.Errorf("classify tx %d: %w", next, err)
			}
			if candDecl.Pattern != PatternUnknown && e.Hooks != nil && e.Hooks.OverrideDeclaredAccess != nil {
				e.Hooks.OverrideDeclaredAccess(next, &candDecl)
			}
			if candDecl.Pattern == PatternUnknown {
				splitReason = "next_tx_unknown:" + candDecl.Reason
				break
			}
			conflictReason, conflictKey := detectDeclaredMVConflict(groupReads, groupWrites, candDecl)
			if conflictReason != "" {
				splitReason = conflictReason
				splitKey = conflictKey
				break
			}
			members = append(members, classifiedMember{txIndex: next, tx: candTx, sender: candSender, declared: candDecl})
			mergeMVKeySet(groupReads, candDecl.Reads)
			mergeMVKeySet(groupWrites, candDecl.Writes)
		}
		if splitReason == "" && idx+len(members) < len(txs) && len(members) >= workers {
			splitReason = "worker_limit"
		}

		// thực thi nhóm: từ 2 giao dịch trở lên chạy đồng thời trên goroutine riêng
		groupResults := make([]*windowAttemptOutcome, len(members))
		if len(members) == 1 {
			m := members[0]
			groupResults[0] = runWindowAttempt(runCtx, m.txIndex, 1, m.tx, curPrefix, env, e.ChainConfig, signer, occHooks)
		} else {
			var groupWg sync.WaitGroup
			for mi, m := range members {
				mi := mi
				m := m
				groupWg.Add(1)
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer groupWg.Done()
					groupResults[mi] = runWindowAttempt(runCtx, m.txIndex, 1, m.tx, curPrefix, env, e.ChainConfig, signer, occHooks)
				}()
			}
			groupWg.Wait()
		}

		for _, sr := range groupResults {
			if sr == nil {
				return nil, fmt.Errorf("missing speculative group result")
			}
			if sr.internalErr != nil {
				return nil, sr.internalErr
			}
		}

		// đối chiếu tập truy cập thực tế của cả nhóm và ranh giới gas block trước khi chốt tx đầu tiên
		valErr := validateGroupExecution(members, groupResults, remainingGas)

		txIndices := make([]int, len(members))
		patterns := make([]TxPattern, len(members))
		for mi, m := range members {
			txIndices[mi] = m.txIndex
			patterns[mi] = m.declared.Pattern
		}

		if valErr == nil {
			var attemptsTrace []GroupedTxAttemptTrace
			if e.RecordTrace {
				attemptsTrace = make([]GroupedTxAttemptTrace, 0, len(members))
			}
			for mi, m := range members {
				sr := groupResults[mi]
				if err := stateDB.ApplyPendingWrites(sr.delta); err != nil {
					return nil, fmt.Errorf("apply grouped tx %d delta: %w", m.txIndex, err)
				}
				versions.CommitTransaction(m.txIndex, uint64(sr.attempt), sr.state.VersionedWrites())
				blockLogIndex = assignSequentialLogIndices(sr.result.Logs, blockLogIndex)
				remainingGas -= sr.result.GasUsed
				result.GasUsed += sr.result.GasUsed
				sr.result.Index = m.txIndex
				result.Results[m.txIndex] = sr.result
				stats.CommittedAttempts[m.txIndex] = 1

				if e.RecordTrace {
					evmStatus := uint64(types.ReceiptStatusSuccessful)
					if sr.result.Failed {
						evmStatus = types.ReceiptStatusFailed
					}
					attemptsTrace = append(attemptsTrace, GroupedTxAttemptTrace{
						TxIndex:        m.txIndex,
						TxHash:         sr.txHash,
						Attempt:        1,
						Status:         "committed",
						DeclaredReads:  len(m.declared.Reads),
						DeclaredWrites: len(m.declared.Writes),
						ActualReads:    len(sr.state.ReadSet),
						ActualWrites:   len(sr.state.WriteSet),
						UsedGas:        sr.result.GasUsed,
						EVMStatus:      evmStatus,
						CreatedReceipt: true,
						LogCount:       len(sr.result.Logs),
					})
				}
			}

			recordCommittedGroup(len(members))
			status := "committed_single"
			if len(members) >= 2 {
				status = "committed_parallel"
			}
			if e.RecordTrace {
				groupsTrace = append(groupsTrace, GroupTrace{
					GroupID:             groupID,
					PrefixIndex:         curPrefix.committedIndex,
					PrefixGen:           curPrefix.generation,
					TxIndices:           txIndices,
					Patterns:            patterns,
					AttemptedWidth:      len(members),
					CommittedWidth:      len(members),
					Status:              status,
					CommittedOnFirstTry: true,
					SplitReason:         splitReason,
					SplitKey:            splitKey,
					Attempts:            attemptsTrace,
				})
			}
			groupID++
			idx += len(members)
			continue
		}

		// nhóm 1 tx bị apply_error (không thiếu khóa): từ chối ngay mà không cần re-run vô ích
		if len(members) == 1 && valErr.reason == "apply_error" {
			if e.RecordTrace {
				groupsTrace = append(groupsTrace, GroupTrace{
					GroupID:        groupID,
					PrefixIndex:    curPrefix.committedIndex,
					PrefixGen:      curPrefix.generation,
					TxIndices:      txIndices,
					Patterns:       patterns,
					AttemptedWidth: 1,
					Status:         "rejected",
					SplitReason:    splitReason,
					SplitKey:       splitKey,
					AbortReason:    valErr.reason,
					AbortedTxIndex: valErr.txIndex,
				})
			}
			if e.StrictInvalidTx {
				return nil, &TxRejectedError{
					Index:    members[0].txIndex,
					TxHash:   groupResults[0].txHash,
					Category: ClassifyRejectionError(groupResults[0].applyErr.Error()),
					Err:      groupResults[0].applyErr,
				}
			}
			result.Results[members[0].txIndex] = groupResults[0].result
			groupID++
			idx++
			continue
		}

		// hủy toàn bộ kết quả tạm của cả nhóm chưa chốt và chạy lại tuần tự đúng thứ tự
		var attemptsTrace []GroupedTxAttemptTrace
		if e.RecordTrace {
			attemptsTrace = make([]GroupedTxAttemptTrace, 0, len(members)*2)
			for mi, m := range members {
				sr := groupResults[mi]
				reason := fmt.Sprintf("group_aborted_by_tx[%d]", valErr.txIndex)
				var ukey *MVKey
				if m.txIndex == valErr.txIndex {
					reason = valErr.reason
					ukey = valErr.undeclaredKey
				}
				var actR, actW int
				if sr.state != nil {
					actR = len(sr.state.ReadSet)
					actW = len(sr.state.WriteSet)
				}
				attemptsTrace = append(attemptsTrace, GroupedTxAttemptTrace{
					TxIndex:        m.txIndex,
					TxHash:         sr.txHash,
					Attempt:        1,
					Status:         "aborted",
					Reason:         reason,
					UndeclaredKey:  ukey,
					DeclaredReads:  len(m.declared.Reads),
					DeclaredWrites: len(m.declared.Writes),
					ActualReads:    actR,
					ActualWrites:   actW,
					UsedGas:        sr.result.GasUsed,
					CreatedReceipt: false,
					LogCount:       len(sr.result.Logs),
				})
			}
		}

		stats.AbortedGroups++
		stats.RetriedTxs += len(members)
		stats.TotalRetries += len(members)
		stats.FallbackTxs += len(members)

		for _, m := range members {
			tr, nextLogIdx, applyErr, internalErr := executeSerialTxOnCommitted(stateDB, env, e.ChainConfig, signer, m.tx, m.txIndex, remainingGas, blockLogIndex)
			if internalErr != nil {
				return nil, internalErr
			}
			if applyErr != nil {
				if e.RecordTrace {
					attemptsTrace = append(attemptsTrace, GroupedTxAttemptTrace{
						TxIndex: m.txIndex,
						TxHash:  m.tx.Hash(),
						Attempt: 2,
						Status:  "rejected",
						Reason:  applyErr.Error(),
					})
					groupsTrace = append(groupsTrace, GroupTrace{
						GroupID:        groupID,
						PrefixIndex:    curPrefix.committedIndex,
						PrefixGen:      curPrefix.generation,
						TxIndices:      txIndices,
						Patterns:       patterns,
						AttemptedWidth: len(members),
						Status:         "rejected",
						SplitReason:    splitReason,
						SplitKey:       splitKey,
						AbortReason:    valErr.reason,
						AbortedTxIndex: valErr.txIndex,
						UndeclaredKey:  valErr.undeclaredKey,
						Attempts:       attemptsTrace,
					})
				}
				if e.StrictInvalidTx {
					return nil, &TxRejectedError{
						Index:    m.txIndex,
						TxHash:   m.tx.Hash(),
						Category: ClassifyRejectionError(applyErr.Error()),
						Err:      applyErr,
					}
				}
				result.Results[m.txIndex] = tr
				continue
			}

			versions.BumpGeneration()
			blockLogIndex = nextLogIdx
			remainingGas -= tr.GasUsed
			result.GasUsed += tr.GasUsed
			result.Results[m.txIndex] = tr
			stats.CommittedAttempts[m.txIndex] = 2
			recordCommittedGroup(1)

			if e.RecordTrace {
				evmStatus := uint64(types.ReceiptStatusSuccessful)
				if tr.Failed {
					evmStatus = types.ReceiptStatusFailed
				}
				attemptsTrace = append(attemptsTrace, GroupedTxAttemptTrace{
					TxIndex:        m.txIndex,
					TxHash:         m.tx.Hash(),
					Attempt:        2,
					Status:         "fallback_committed",
					DeclaredReads:  len(m.declared.Reads),
					DeclaredWrites: len(m.declared.Writes),
					UsedGas:        tr.GasUsed,
					EVMStatus:      evmStatus,
					CreatedReceipt: true,
					LogCount:       len(tr.Logs),
				})
			}
		}

		if e.RecordTrace {
			groupsTrace = append(groupsTrace, GroupTrace{
				GroupID:             groupID,
				PrefixIndex:         curPrefix.committedIndex,
				PrefixGen:           curPrefix.generation,
				TxIndices:           txIndices,
				Patterns:            patterns,
				AttemptedWidth:      len(members),
				CommittedWidth:      1,
				Status:              "aborted_reexecuted_serial",
				CommittedOnFirstTry: false,
				SplitReason:         splitReason,
				SplitKey:            splitKey,
				AbortReason:         valErr.reason,
				AbortedTxIndex:      valErr.txIndex,
				UndeclaredKey:       valErr.undeclaredKey,
				Attempts:            attemptsTrace,
			})
		}
		groupID++
		idx += len(members)
	}

	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect grouped state writes: %w", err)
	}
	copy(result.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
	result.Writes = writes
	result.GroupedStats = &stats
	if e.RecordTrace {
		result.GroupedTrace = &GroupedBlockTrace{
			BlockNumber: uint64(env.Height),
			Stats:       stats,
			Groups:      groupsTrace,
		}
	}
	result.Metrics["tx_total"] = float64(len(txs))
	result.Metrics["worker_count"] = float64(workers)
	result.Metrics["grouped_txs"] = float64(stats.GroupedTxs)
	result.Metrics["max_group_width"] = float64(stats.MaxGroupWidth)
	result.Metrics["unknown_serial_txs"] = float64(stats.UnknownTxs)
	result.Metrics["aborted_groups"] = float64(stats.AbortedGroups)
	result.Metrics["retried_txs"] = float64(stats.RetriedTxs)
	result.Metrics["total_retries"] = float64(stats.TotalRetries)
	result.Metrics["fallback_txs"] = float64(stats.FallbackTxs)
	result.Metrics["phase_total_ns"] = float64(time.Since(totalStarted).Nanoseconds())
	return result, nil
}

func classifyTxForGroup(
	state *PebbleStateDB,
	signer types.Signer,
	env BlockEnv,
	tx *types.Transaction,
	precompiles map[common.Address]struct{},
	approvedTokens map[common.Hash]ApprovedERC20Spec,
) (common.Address, DeclaredAccess, error) {
	unknown := func(reason string) (common.Address, DeclaredAccess, error) {
		return common.Address{}, DeclaredAccess{Pattern: PatternUnknown, Reason: reason}, nil
	}
	if tx == nil {
		return unknown("nil_tx")
	}
	if tx.To() == nil {
		return unknown("contract_creation")
	}
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.DynamicFeeTxType:
	default:
		return unknown("unsupported_tx_type")
	}
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return unknown("invalid_signature")
	}
	if env.BaseFee == nil {
		return unknown("missing_base_fee")
	}
	feeCap := tx.GasFeeCap()
	tipCap := tx.GasTipCap()
	if feeCap.Cmp(env.BaseFee) < 0 || feeCap.Cmp(tipCap) < 0 {
		return unknown("invalid_fee_caps")
	}
	effectiveTip := new(big.Int).Sub(feeCap, env.BaseFee)
	if tipCap.Cmp(effectiveTip) < 0 {
		effectiveTip = new(big.Int).Set(tipCap)
	}

	if _, isPre := precompiles[sender]; isPre {
		return unknown("precompile_sender")
	}
	senderAcc, senderExists, err := state.accountSnapshot(sender)
	if err != nil {
		return common.Address{}, DeclaredAccess{}, err
	}
	if !senderExists || senderAcc == nil {
		return unknown("sender_absent")
	}
	if len(senderAcc.CodeHash) > 0 {
		return unknown("sender_has_code")
	}

	// kiểm tra coinbase ở đầu nhóm: tip=0 chỉ bỏ qua write-key coinbase khi coinbase tồn tại và không rỗng
	cbAcc, cbExists, err := state.accountSnapshot(env.Coinbase)
	if err != nil {
		return common.Address{}, DeclaredAccess{}, err
	}
	cbExistsNonEmpty := cbExists && !isEmptyAccount(cbAcc)

	to := *tx.To()
	if _, isPre := precompiles[to]; isPre {
		return unknown("precompile_target")
	}
	toAcc, toExists, err := state.accountSnapshot(to)
	if err != nil {
		return common.Address{}, DeclaredAccess{}, err
	}
	toHasCode := toExists && toAcc != nil && len(toAcc.CodeHash) > 0

	addCommonReadsWrites := func(d *DeclaredAccess) {
		d.Reads[MVKey{Address: sender, Kind: MVAccount}] = struct{}{}
		d.Reads[MVKey{Address: sender, Kind: MVNonce}] = struct{}{}
		d.Reads[MVKey{Address: sender, Kind: MVCode}] = struct{}{}
		d.Reads[MVKey{Address: sender, Kind: MVBalance}] = struct{}{}
		d.Writes[MVKey{Address: sender, Kind: MVAccount}] = struct{}{}
		d.Writes[MVKey{Address: sender, Kind: MVNonce}] = struct{}{}
		d.Writes[MVKey{Address: sender, Kind: MVBalance}] = struct{}{}

		// luôn đọc coinbase để mọi tx ghi vào coinbase trong cùng nhóm đều gây tách nhóm
		d.Reads[MVKey{Address: env.Coinbase, Kind: MVAccount}] = struct{}{}
		d.Reads[MVKey{Address: env.Coinbase, Kind: MVNonce}] = struct{}{}
		d.Reads[MVKey{Address: env.Coinbase, Kind: MVCode}] = struct{}{}
		d.Reads[MVKey{Address: env.Coinbase, Kind: MVBalance}] = struct{}{}

		if effectiveTip.Sign() > 0 || !cbExistsNonEmpty {
			d.Writes[MVKey{Address: env.Coinbase, Kind: MVAccount}] = struct{}{}
			d.Writes[MVKey{Address: env.Coinbase, Kind: MVBalance}] = struct{}{}
		}
	}

	data := tx.Data()
	if len(data) == 0 {
		if toHasCode {
			return unknown("recipient_has_code")
		}
		d := newDeclaredAccess(PatternETHTransfer)
		addCommonReadsWrites(&d)

		d.Reads[MVKey{Address: to, Kind: MVAccount}] = struct{}{}
		d.Reads[MVKey{Address: to, Kind: MVNonce}] = struct{}{}
		d.Reads[MVKey{Address: to, Kind: MVCode}] = struct{}{}
		d.Reads[MVKey{Address: to, Kind: MVBalance}] = struct{}{}

		toEmpty := !toExists || isEmptyAccount(toAcc)
		if tx.Value().Sign() > 0 || toEmpty {
			d.Writes[MVKey{Address: to, Kind: MVAccount}] = struct{}{}
			d.Writes[MVKey{Address: to, Kind: MVBalance}] = struct{}{}
		}
		return sender, d, nil
	}

	if len(data) < 4 || !bytes.Equal(data[:4], erc20ApproveSelector[:]) {
		return unknown("unrecognized_selector")
	}
	if !toHasCode {
		return unknown("target_has_no_code")
	}
	codeHash := common.BytesToHash(toAcc.CodeHash)
	spec, ok := approvedTokens[codeHash]
	if !ok {
		return unknown("unrecognized_code_hash")
	}
	if tx.Value().Sign() != 0 {
		return unknown("non_zero_value")
	}
	if len(data) != 68 {
		return unknown("invalid_approve_calldata_len")
	}
	for _, b := range data[4:16] {
		if b != 0 {
			return unknown("invalid_spender_padding")
		}
	}
	spender := common.BytesToAddress(data[16:36])
	if spender == (common.Address{}) {
		return unknown("zero_spender")
	}

	slot := ComputeERC20AllowanceSlot(sender, spender, spec.AllowanceSlot)
	slotKey := MVKey{Address: to, Kind: MVStorage, Slot: slot}

	d := newDeclaredAccess(PatternERC20Approve)
	addCommonReadsWrites(&d)
	d.Reads[MVKey{Address: to, Kind: MVAccount}] = struct{}{}
	d.Reads[MVKey{Address: to, Kind: MVNonce}] = struct{}{}
	d.Reads[MVKey{Address: to, Kind: MVCode}] = struct{}{}
	d.Reads[slotKey] = struct{}{}
	d.Writes[slotKey] = struct{}{}
	return sender, d, nil
}

type groupValidationError struct {
	txIndex       int
	reason        string
	undeclaredKey *MVKey
}

func validateGroupExecution(
	members []classifiedMember,
	results []*windowAttemptOutcome,
	remainingGas uint64,
) *groupValidationError {
	for i, m := range members {
		sr := results[i]
		if sr.applyErr != nil {
			return &groupValidationError{txIndex: m.txIndex, reason: "apply_error"}
		}
		if sr.state != nil {
			for _, r := range sr.state.ReadSet {
				if _, ok := m.declared.Reads[r]; !ok {
					k := r
					return &groupValidationError{txIndex: m.txIndex, reason: "undeclared_read", undeclaredKey: &k}
				}
			}
			for _, w := range sr.state.WriteSet {
				if _, ok := m.declared.Writes[w]; !ok {
					k := w
					return &groupValidationError{txIndex: m.txIndex, reason: "undeclared_write", undeclaredKey: &k}
				}
			}
		}
	}

	// kiểm tra chéo cặp đọc/ghi thực tế giữa các thành viên trước khi chốt bất kỳ tx nào
	if len(members) >= 2 {
		actualReads := make([]map[MVKey]struct{}, len(members))
		actualWrites := make([]map[MVKey]struct{}, len(members))
		for i, sr := range results {
			rset := make(map[MVKey]struct{})
			wset := make(map[MVKey]struct{})
			if sr.state != nil {
				for _, r := range sr.state.ReadSet {
					rset[r] = struct{}{}
				}
				for _, w := range sr.state.WriteSet {
					wset[w] = struct{}{}
				}
			}
			actualReads[i] = rset
			actualWrites[i] = wset
		}
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				if k := firstIntersectingMVKey(actualWrites[i], actualWrites[j]); k != nil {
					return &groupValidationError{txIndex: members[j].txIndex, reason: "actual_write_write_conflict", undeclaredKey: k}
				}
				if k := firstIntersectingMVKey(actualWrites[i], actualReads[j]); k != nil {
					return &groupValidationError{txIndex: members[j].txIndex, reason: "actual_write_read_conflict", undeclaredKey: k}
				}
				if k := firstIntersectingMVKey(actualReads[i], actualWrites[j]); k != nil {
					return &groupValidationError{txIndex: members[j].txIndex, reason: "actual_read_write_conflict", undeclaredKey: k}
				}
			}
		}
	}

	// kiểm tra ranh giới gas block tuần tự qua cả nhóm trước khi áp bất kỳ kết quả nào
	simRemaining := remainingGas
	for i, m := range members {
		if m.tx.Gas() > simRemaining {
			return &groupValidationError{txIndex: m.txIndex, reason: "block_gas_limit_boundary"}
		}
		simRemaining -= results[i].result.GasUsed
	}
	return nil
}

func detectDeclaredMVConflict(groupReads, groupWrites map[MVKey]struct{}, cand DeclaredAccess) (string, *MVKey) {
	if k := firstIntersectingMVKey(cand.Writes, groupWrites); k != nil {
		return "write_write_conflict", k
	}
	if k := firstIntersectingMVKey(cand.Writes, groupReads); k != nil {
		return "write_read_conflict", k
	}
	if k := firstIntersectingMVKey(cand.Reads, groupWrites); k != nil {
		return "read_write_conflict", k
	}
	return "", nil
}

func firstIntersectingMVKey(a, b map[MVKey]struct{}) *MVKey {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	var best *MVKey
	for k := range a {
		if _, ok := b[k]; !ok {
			continue
		}
		if best == nil || compareMVKeys(k, *best) < 0 {
			cp := k
			best = &cp
		}
	}
	return best
}

func compareMVKeys(a, b MVKey) int {
	if a.Kind != b.Kind {
		if a.Kind < b.Kind {
			return -1
		}
		return 1
	}
	if c := bytes.Compare(a.Address[:], b.Address[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.Slot[:], b.Slot[:])
}

func copyMVKeySet(src map[MVKey]struct{}) map[MVKey]struct{} {
	out := make(map[MVKey]struct{}, len(src))
	for k := range src {
		out[k] = struct{}{}
	}
	return out
}

func mergeMVKeySet(dst, src map[MVKey]struct{}) {
	for k := range src {
		dst[k] = struct{}{}
	}
}
