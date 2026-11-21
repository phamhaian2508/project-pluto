package evm

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// BlockEnv chứa các đầu vào tất định được mọi bộ thực thi dùng chung.
type BlockEnv struct {
	Height       int64
	Time         uint64
	Coinbase     common.Address
	GasLimit     uint64
	Random       common.Hash
	GetHash      func(uint64) common.Hash
	BaseFee      *big.Int
	PreviousRoot []byte
}

// StateReader là giao diện chỉ đọc trạng thái dùng chung với các bộ thực thi khác.
type StateReader interface {
	Account(addr common.Address) (*Account, bool)
	Storage(addr common.Address, key common.Hash) common.Hash
	Code(addr common.Address) []byte
}

type TxResult struct {
	Index           int
	Invalid         bool
	Failed          bool
	GasUsed         uint64
	Logs            []*types.Log
	ReturnData      []byte
	ContractAddress common.Address
	ErrMsg          string
}

// TxRejectedError báo giao dịch không hợp lệ khi chế độ StrictInvalidTx bật (lab / verify).
type TxRejectedError struct {
	Index    int
	TxHash   common.Hash
	Category string
	Err      error
}

func (e *TxRejectedError) Error() string {
	return fmt.Sprintf("tx %d (%s) rejected [%s]: %v", e.Index, e.TxHash.Hex(), e.Category, e.Err)
}

func (e *TxRejectedError) Unwrap() error {
	return e.Err
}

// ClassifyRejectionError chuẩn hóa nhóm lỗi từ chối giao dịch giữa Pluto và Geth t8n.
func ClassifyRejectionError(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "nonce too low"):
		return "nonce_too_low"
	case strings.Contains(lower, "nonce too high"):
		return "nonce_too_high"
	case strings.Contains(lower, "gas limit reached"):
		return "gas_limit_reached"
	case strings.Contains(lower, "insufficient funds"):
		return "insufficient_funds"
	case strings.Contains(lower, "intrinsic gas too low"):
		return "intrinsic_gas"
	case strings.Contains(lower, "max fee per gas less than block base fee"):
		return "fee_cap_too_low"
	case strings.Contains(lower, "max priority fee per gas higher than max fee per gas"):
		return "tip_above_fee_cap"
	case strings.Contains(lower, "sender not an eoa"):
		return "sender_not_eoa"
	default:
		return "other"
	}
}

type BlockResult struct {
	Results           []TxResult
	GasUsed           uint64
	StateRoot         [32]byte
	Writes            map[string][]byte
	Metrics           map[string]float64
	CheckpointMetrics CheckpointMetrics
	CheckpointTrace   []CheckpointTraceEvent
	Phase2Metrics     *Phase2CheckpointMetrics
	Phase2Trace       []Phase2TraceEvent
	DecisionTrace     []AdaptiveDecisionEvent
	OCCStats          *OCCBlockStats
	OCCTrace          *OCCBlockTrace
	GroupedStats      *GroupedBlockStats
	GroupedTrace      *GroupedBlockTrace
	PredictedStats    *PredictedOCCBlockStats
	PredictedTrace    *PredictedOCCBlockTrace
}

type Executor interface {
	// ExecuteBlock chạy giao dịch trên state view và trả kết quả cùng các writes; App/ABCI mới commit chúng.
	ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error)
}

// SequentialExecutor là bộ máy tham chiếu. Nó áp dụng giao dịch theo thứ tự,
// dùng logic xác thực, tính gas và chuyển trạng thái EVM của geth.
type SequentialExecutor struct {
	ChainConfig     *params.ChainConfig
	ChainID         *big.Int
	StrictInvalidTx bool // true -> từ chối cả block khi gặp giao dịch invalid (dùng cho pluto-lab)
}

func NewSequentialExecutor(config *params.ChainConfig, chainID *big.Int) *SequentialExecutor {
	return &SequentialExecutor{ChainConfig: config, ChainID: new(big.Int).Set(chainID)}
}

func (e *SequentialExecutor) ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error) {
	stateDB, ok := base.(*PebbleStateDB)
	if !ok {
		return nil, fmt.Errorf("sequential executor requires *PebbleStateDB, got %T", base)
	}
	if e.ChainConfig == nil || e.ChainID == nil {
		return nil, fmt.Errorf("sequential executor requires chain config and chain ID")
	}
	if env.GasLimit == 0 {
		return nil, fmt.Errorf("block gas limit must be greater than zero")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}

	// BlockContext cung cấp cho opcode EVM các dữ liệu cố định của block đang xử lý.
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
	evmEnv := vm.NewEVM(blockContext, stateDB, e.ChainConfig, vm.Config{})
	signer := types.MakeSigner(e.ChainConfig, big.NewInt(env.Height), env.Time)
	gasPool := new(core.GasPool).AddGas(env.GasLimit)
	blockResult := &BlockResult{Results: make([]TxResult, len(txs)), Writes: make(map[string][]byte)}
	var blockLogIndex uint

	// Giữ nguyên thứ tự trong block: giao dịch sau quan sát state thành công của giao dịch trước.
	for i, tx := range txs {
		tr := TxResult{Index: i}
		if tx == nil {
			if e.StrictInvalidTx {
				return nil, &TxRejectedError{Index: i, Category: "other", Err: fmt.Errorf("nil transaction")}
			}
			tr.Invalid, tr.ErrMsg = true, "nil transaction"
			blockResult.Results[i] = tr
			continue
		}
		msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
		if err != nil {
			if e.StrictInvalidTx {
				return nil, &TxRejectedError{Index: i, TxHash: tx.Hash(), Category: ClassifyRejectionError(err.Error()), Err: err}
			}
			tr.Invalid, tr.ErrMsg = true, err.Error()
			blockResult.Results[i] = tr
			continue
		}
		evmEnv.SetTxContext(core.NewEVMTxContext(msg))
		logStart := len(stateDB.GetLogs())
		// Snapshot cho phép hoàn tác state nếu ApplyMessage gặp lỗi ở cấp xử lý giao dịch.
		snap := stateDB.Snapshot()
		result, err := core.ApplyMessage(evmEnv, msg, gasPool)
		if dbErr := stateDB.Err(); dbErr != nil {
			return nil, fmt.Errorf("tx %d state db error: %w", i, dbErr)
		}
		if err != nil {
			stateDB.RevertToSnapshot(snap)
			stateDB.Finalise(true)
			if e.StrictInvalidTx {
				return nil, &TxRejectedError{Index: i, TxHash: tx.Hash(), Category: ClassifyRejectionError(err.Error()), Err: err}
			}
			tr.Invalid, tr.ErrMsg = true, err.Error()
			blockResult.Results[i] = tr
			continue
		}
		// Finalise chốt các thay đổi của giao dịch thành công trước khi chạy giao dịch kế tiếp.
		stateDB.Finalise(true)
		if dbErr := stateDB.Err(); dbErr != nil {
			return nil, fmt.Errorf("tx %d state db error after finalise: %w", i, dbErr)
		}
		tr.GasUsed = result.UsedGas
		tr.Failed = result.Err != nil
		if result.Err != nil {
			tr.ErrMsg = result.Err.Error()
		}
		tr.ReturnData = append([]byte(nil), result.ReturnData...)
		if tx.To() == nil {
			tr.ContractAddress = crypto.CreateAddress(msg.From, tx.Nonce())
		}
		for _, entry := range stateDB.GetLogs()[logStart:] {
			logCopy := *entry
			logCopy.TxHash = tx.Hash()
			logCopy.TxIndex = uint(i)
			logCopy.Index = blockLogIndex
			logCopy.BlockNumber = uint64(env.Height)
			logCopy.BlockTimestamp = env.Time
			topics := make([]common.Hash, len(entry.Topics))
			copy(topics, entry.Topics)
			logCopy.Topics = topics
			logCopy.Data = append([]byte(nil), entry.Data...)
			blockLogIndex++
			tr.Logs = append(tr.Logs, &logCopy)
		}
		blockResult.GasUsed += result.UsedGas
		blockResult.Results[i] = tr
	}
	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect state writes: %w", err)
	}
	// AppHash của Pluto được tính từ state ứng dụng; đây không phải Ethereum Merkle Patricia root.
	copy(blockResult.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
	blockResult.Writes = writes
	return blockResult, nil
}
