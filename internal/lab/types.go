package lab

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

type allocAccountJSON struct {
	Code    hexutil.Bytes         `json:"code,omitempty"`
	Storage map[string]string     `json:"storage,omitempty"`
	Balance *math.HexOrDecimal256 `json:"balance"`
	Nonce   math.HexOrDecimal64   `json:"nonce,omitempty"`
}

func parseHexHash32(raw string) (common.Hash, error) {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "0x"), "0X")
	if len(trimmed) > 64 {
		return common.Hash{}, fmt.Errorf("hex value exceeds 32 bytes: %q", raw)
	}
	if len(trimmed)%2 == 1 {
		trimmed = "0" + trimmed
	}
	b, err := hex.DecodeString(trimmed)
	if err != nil {
		return common.Hash{}, fmt.Errorf("invalid hex %q: %w", raw, err)
	}
	var out common.Hash
	copy(out[common.HashLength-len(b):], b)
	return out, nil
}

// MarshalAllocJSON xuất evm.LogicalState sang định dạng alloc.json chuẩn của Geth t8n.
func MarshalAllocJSON(s *evm.LogicalState) ([]byte, error) {
	if s == nil {
		return []byte("{}"), nil
	}
	addrs := make([]common.Address, 0, len(s.Accounts))
	for addr := range s.Accounts {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})

	rawMap := make(map[string]allocAccountJSON, len(addrs))
	for _, addr := range addrs {
		acc := s.Accounts[addr]
		bal := new(big.Int)
		if acc.Balance != nil {
			bal = acc.Balance.ToBig()
		}
		var storage map[string]string
		if len(acc.Storage) > 0 {
			storage = make(map[string]string, len(acc.Storage))
			for k, v := range acc.Storage {
				if v == (common.Hash{}) {
					continue
				}
				storage[k.Hex()] = v.Hex()
			}
			if len(storage) == 0 {
				storage = nil
			}
		}
		var code hexutil.Bytes
		if len(acc.Code) > 0 {
			code = bytes.Clone(acc.Code)
		}
		rawMap[addr.Hex()] = allocAccountJSON{
			Code:    code,
			Storage: storage,
			Balance: (*math.HexOrDecimal256)(bal),
			Nonce:   math.HexOrDecimal64(acc.Nonce),
		}
	}
	return json.MarshalIndent(rawMap, "", "  ")
}

// UnmarshalAllocJSON đọc alloc.json / post-alloc.json thành evm.LogicalState chuẩn hóa.
func UnmarshalAllocJSON(data []byte) (*evm.LogicalState, error) {
	var rawMap map[string]allocAccountJSON
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return nil, fmt.Errorf("decode alloc json: %w", err)
	}
	state := evm.NewLogicalState()
	for addrStr, item := range rawMap {
		if !common.IsHexAddress(addrStr) {
			return nil, fmt.Errorf("invalid address in alloc: %q", addrStr)
		}
		addr := common.HexToAddress(addrStr)
		bal := new(uint256.Int)
		if item.Balance != nil {
			u256, overflow := uint256.FromBig((*big.Int)(item.Balance))
			if overflow {
				return nil, fmt.Errorf("balance overflows uint256 for %s", addr.Hex())
			}
			bal = u256
		}
		storage := make(map[common.Hash]common.Hash, len(item.Storage))
		for kStr, vStr := range item.Storage {
			kHash, err := parseHexHash32(kStr)
			if err != nil {
				return nil, fmt.Errorf("account %s storage key: %w", addr.Hex(), err)
			}
			vHash, err := parseHexHash32(vStr)
			if err != nil {
				return nil, fmt.Errorf("account %s storage value: %w", addr.Hex(), err)
			}
			if vHash != (common.Hash{}) {
				storage[kHash] = vHash
			}
		}
		var code []byte
		if len(item.Code) > 0 {
			code = bytes.Clone(item.Code)
		}
		state.Accounts[addr] = &evm.LogicalAccount{
			Nonce:   uint64(item.Nonce),
			Balance: bal,
			Code:    code,
			Storage: storage,
		}
	}
	return state, nil
}

// BlockEnv chứa thông tin môi trường block theo chuẩn env.json của Geth t8n (Shanghai).
type BlockEnv struct {
	Coinbase    common.Address
	Number      uint64
	Timestamp   uint64
	GasLimit    uint64
	BaseFee     *big.Int
	Random      common.Hash
	Difficulty  *big.Int
	BlockHashes map[uint64]common.Hash
	Withdrawals []*types.Withdrawal
}

func (e BlockEnv) Clone() BlockEnv {
	out := BlockEnv{
		Coinbase:  e.Coinbase,
		Number:    e.Number,
		Timestamp: e.Timestamp,
		GasLimit:  e.GasLimit,
		Random:    e.Random,
	}
	if e.BaseFee != nil {
		out.BaseFee = new(big.Int).Set(e.BaseFee)
	}
	if e.Difficulty != nil {
		out.Difficulty = new(big.Int).Set(e.Difficulty)
	}
	if e.BlockHashes != nil {
		out.BlockHashes = make(map[uint64]common.Hash, len(e.BlockHashes))
		for k, v := range e.BlockHashes {
			out.BlockHashes[k] = v
		}
	}
	if e.Withdrawals != nil {
		out.Withdrawals = make([]*types.Withdrawal, len(e.Withdrawals))
		for i, w := range e.Withdrawals {
			if w != nil {
				cp := *w
				out.Withdrawals[i] = &cp
			}
		}
	}
	return out
}

func (e BlockEnv) ValidateShanghai() error {
	if e.BaseFee == nil {
		return errors.New("missing currentBaseFee in Shanghai env")
	}
	if e.BaseFee.Sign() < 0 {
		return errors.New("negative currentBaseFee in env")
	}
	if e.Withdrawals == nil {
		return errors.New("missing withdrawals field in Shanghai env (expected [])")
	}
	if len(e.Withdrawals) > 0 {
		return fmt.Errorf("phase 1 requires empty withdrawals, got %d", len(e.Withdrawals))
	}
	if e.Difficulty != nil && e.Difficulty.Sign() != 0 {
		return fmt.Errorf("post-merge Shanghai requires zero/omitted difficulty, got %s", e.Difficulty)
	}
	return nil
}

type blockEnvJSON struct {
	Coinbase    *common.Address                     `json:"currentCoinbase"`
	Difficulty  *math.HexOrDecimal256               `json:"currentDifficulty,omitempty"`
	Random      *math.HexOrDecimal256               `json:"currentRandom,omitempty"`
	GasLimit    *math.HexOrDecimal64                `json:"currentGasLimit"`
	Number      *math.HexOrDecimal64                `json:"currentNumber"`
	Timestamp   *math.HexOrDecimal64                `json:"currentTimestamp"`
	BaseFee     *math.HexOrDecimal256               `json:"currentBaseFee,omitempty"`
	BlockHashes map[math.HexOrDecimal64]common.Hash `json:"blockHashes,omitempty"`
	Withdrawals []*types.Withdrawal                 `json:"withdrawals"`
}

func (e BlockEnv) MarshalJSON() ([]byte, error) {
	withdrawals := e.Withdrawals
	if withdrawals == nil {
		withdrawals = []*types.Withdrawal{}
	}
	var blockHashes map[math.HexOrDecimal64]common.Hash
	if len(e.BlockHashes) > 0 {
		blockHashes = make(map[math.HexOrDecimal64]common.Hash, len(e.BlockHashes))
		for k, v := range e.BlockHashes {
			blockHashes[math.HexOrDecimal64(k)] = v
		}
	}
	rndBig := new(big.Int).SetBytes(e.Random[:])
	coinbase := e.Coinbase
	gasLimit := math.HexOrDecimal64(e.GasLimit)
	number := math.HexOrDecimal64(e.Number)
	ts := math.HexOrDecimal64(e.Timestamp)

	enc := blockEnvJSON{
		Coinbase:    &coinbase,
		Difficulty:  (*math.HexOrDecimal256)(e.Difficulty),
		Random:      (*math.HexOrDecimal256)(rndBig),
		GasLimit:    &gasLimit,
		Number:      &number,
		Timestamp:   &ts,
		BaseFee:     (*math.HexOrDecimal256)(e.BaseFee),
		BlockHashes: blockHashes,
		Withdrawals: withdrawals,
	}
	return json.MarshalIndent(enc, "", "  ")
}

func (e *BlockEnv) UnmarshalJSON(data []byte) error {
	var dec blockEnvJSON
	if err := json.Unmarshal(data, &dec); err != nil {
		return err
	}
	if dec.Coinbase == nil {
		return errors.New("missing required field currentCoinbase")
	}
	if dec.GasLimit == nil {
		return errors.New("missing required field currentGasLimit")
	}
	if dec.Number == nil {
		return errors.New("missing required field currentNumber")
	}
	if dec.Timestamp == nil {
		return errors.New("missing required field currentTimestamp")
	}

	out := BlockEnv{
		Coinbase:    *dec.Coinbase,
		Number:      uint64(*dec.Number),
		Timestamp:   uint64(*dec.Timestamp),
		GasLimit:    uint64(*dec.GasLimit),
		BaseFee:     (*big.Int)(dec.BaseFee),
		Difficulty:  (*big.Int)(dec.Difficulty),
		Withdrawals: dec.Withdrawals,
	}
	if dec.Random != nil {
		out.Random = common.BigToHash((*big.Int)(dec.Random))
	}
	if len(dec.BlockHashes) > 0 {
		out.BlockHashes = make(map[uint64]common.Hash, len(dec.BlockHashes))
		for k, v := range dec.BlockHashes {
			out.BlockHashes[uint64(k)] = v
		}
	}
	*e = out
	return nil
}

// SignedTx giữ giao dịch đã ký theo đúng thứ tự block cùng raw bytes chuẩn hóa.
type SignedTx struct {
	Index  int
	Tx     *types.Transaction
	Raw    []byte
	Hash   common.Hash
	Sender common.Address
}

// TxReceipt chuẩn hóa biên nhận để đối chiếu với evm t8n.
type TxReceipt struct {
	Type              uint8
	Status            uint64
	CumulativeGasUsed uint64
	Bloom             types.Bloom
	Logs              []*types.Log
	TxHash            common.Hash
	ContractAddress   common.Address
	GasUsed           uint64
	BlockHash         common.Hash
	BlockNumber       uint64
	TransactionIndex  uint
}

func TxReceiptFromGeth(r *types.Receipt) *TxReceipt {
	if r == nil {
		return nil
	}
	logs := make([]*types.Log, len(r.Logs))
	for i, l := range r.Logs {
		if l == nil {
			continue
		}
		cp := *l
		cp.Topics = append([]common.Hash(nil), l.Topics...)
		cp.Data = bytes.Clone(l.Data)
		logs[i] = &cp
	}
	var blockNum uint64
	if r.BlockNumber != nil {
		blockNum = r.BlockNumber.Uint64()
	}
	return &TxReceipt{
		Type:              r.Type,
		Status:            r.Status,
		CumulativeGasUsed: r.CumulativeGasUsed,
		Bloom:             r.Bloom,
		Logs:              logs,
		TxHash:            r.TxHash,
		ContractAddress:   r.ContractAddress,
		GasUsed:           r.GasUsed,
		BlockHash:         r.BlockHash,
		BlockNumber:       blockNum,
		TransactionIndex:  r.TransactionIndex,
	}
}

// NormalizeReceipts chuyển kết quả thực thi của internal/evm sang biên nhận chuẩn Geth/lab,
// đồng thời kiểm tra tính nhất quán của Log.TxHash, Log.TxIndex và Log.Index do executor tạo ra.
func NormalizeReceipts(txs []SignedTx, evmRes *evm.BlockResult, blockNum uint64, blockHash common.Hash, blockTime uint64) ([]*TxReceipt, error) {
	receipts := make([]*TxReceipt, 0, len(txs))
	var cumulativeGas uint64
	var logIdx uint
	for i, stx := range txs {
		tr := evmRes.Results[i]
		cumulativeGas += tr.GasUsed
		status := types.ReceiptStatusSuccessful
		if tr.Failed {
			status = types.ReceiptStatusFailed
		}
		logs := make([]*types.Log, len(tr.Logs))
		for j, l := range tr.Logs {
			if l.TxHash != stx.Hash {
				return nil, fmt.Errorf("block %d tx[%d] log[%d] TxHash=%s, want %s", blockNum, i, j, l.TxHash.Hex(), stx.Hash.Hex())
			}
			if l.TxIndex != uint(i) {
				return nil, fmt.Errorf("block %d tx[%d] log[%d] TxIndex=%d, want %d", blockNum, i, j, l.TxIndex, i)
			}
			if l.Index != logIdx {
				return nil, fmt.Errorf("block %d tx[%d] log[%d] Index=%d, want %d", blockNum, i, j, l.Index, logIdx)
			}
			cp := *l
			cp.Topics = append([]common.Hash(nil), l.Topics...)
			cp.Data = bytes.Clone(l.Data)
			cp.BlockNumber = blockNum
			cp.BlockHash = blockHash
			cp.BlockTimestamp = blockTime
			logIdx++
			logs[j] = &cp
		}
		gethRec := &types.Receipt{
			Type:              stx.Tx.Type(),
			Status:            status,
			CumulativeGasUsed: cumulativeGas,
			Logs:              logs,
			TxHash:            stx.Hash,
			ContractAddress:   tr.ContractAddress,
			GasUsed:           tr.GasUsed,
			BlockHash:         blockHash,
			BlockNumber:       new(big.Int).SetUint64(blockNum),
			TransactionIndex:  uint(i),
		}
		gethRec.Bloom = types.CreateBloom(gethRec)
		receipts = append(receipts, TxReceiptFromGeth(gethRec))
	}
	return receipts, nil
}

// BlockResult là đầu ra chuẩn hóa của một block từ bất kỳ executor nào trong internal/evm.
type BlockResult struct {
	BlockNumber    uint64
	PrevAppHash    common.Hash
	AppHash        common.Hash
	GasUsed        uint64
	Receipts       []*TxReceipt
	TxReturnData   [][]byte
	TxErrMsgs      []string
	OCCStats       *evm.OCCBlockStats
	OCCTrace       *evm.OCCBlockTrace
	Phase2Metrics  *evm.Phase2CheckpointMetrics
	Phase2Trace    []evm.Phase2TraceEvent
	DecisionTrace  []evm.AdaptiveDecisionEvent
	GroupedStats   *evm.GroupedBlockStats
	GroupedTrace   *evm.GroupedBlockTrace
	PredictedStats *evm.PredictedOCCBlockStats
	PredictedTrace *evm.PredictedOCCBlockTrace
}

// BlockSpec mô tả đầu vào một block trong ca thử.
type BlockSpec struct {
	Env             BlockEnv
	Txs             []SignedTx
	RawTxsJSON      []byte
	ReferenceResult []byte
}

// ExpectedRejectSpec mô tả kỳ vọng từ chối cho fixture cố ý sai để nghiệm thu verify --case all.
type ExpectedRejectSpec struct {
	BlockIndex              int    `json:"blockIndex"`
	BlockNumber             uint64 `json:"blockNumber"`
	TxIndex                 int    `json:"txIndex"`
	Category                string `json:"category"`
	ExpectedCommittedHeight uint64 `json:"expectedCommittedHeight"`
}

// ChainCase mô tả một ca thử đơn block hoặc chuỗi nhiều block.
type ChainCase struct {
	ID              string
	InputHash       string
	Seed            *uint64
	ExpectedReject  *ExpectedRejectSpec
	GenesisAlloc    *evm.LogicalState
	Blocks          []BlockSpec
	ReferenceResult []byte
}
