package evm

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// BlockEnv contains the deterministic inputs shared by all execution engines.
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

// StateReader is the read-only state surface shared with alternate executors.
type StateReader interface {
	Account(addr common.Address) (*Account, bool)
	Storage(addr common.Address, key common.Hash) common.Hash
	Code(addr common.Address) []byte
}

type TxResult struct {
	Index      int
	Invalid    bool
	Failed     bool
	GasUsed    uint64
	Logs       []*types.Log
	ReturnData []byte
	ErrMsg     string
}

type BlockResult struct {
	Results   []TxResult
	GasUsed   uint64
	StateRoot [32]byte
	Writes    map[string][]byte
	Metrics   map[string]float64
}

type Executor interface {
	ExecuteBlock(base StateReader, env BlockEnv, txs []*types.Transaction) (*BlockResult, error)
}

// SequentialExecutor is the reference engine. It applies transactions in
// order using geth's validation, gas accounting, and EVM transition logic.
type SequentialExecutor struct {
	ChainConfig *params.ChainConfig
	ChainID     *big.Int
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
	signer := types.LatestSignerForChainID(e.ChainID)
	gasPool := new(core.GasPool).AddGas(env.GasLimit)
	blockResult := &BlockResult{Results: make([]TxResult, len(txs)), Writes: make(map[string][]byte)}

	for i, tx := range txs {
		tr := TxResult{Index: i}
		if tx == nil {
			tr.Invalid, tr.ErrMsg = true, "nil transaction"
			blockResult.Results[i] = tr
			continue
		}
		msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
		if err != nil {
			tr.Invalid, tr.ErrMsg = true, err.Error()
			blockResult.Results[i] = tr
			continue
		}
		evmEnv.SetTxContext(core.NewEVMTxContext(msg))
		logStart := len(stateDB.GetLogs())
		snap := stateDB.Snapshot()
		result, err := core.ApplyMessage(evmEnv, msg, gasPool)
		if err != nil {
			stateDB.RevertToSnapshot(snap)
			stateDB.Finalise(true)
			tr.Invalid, tr.ErrMsg = true, err.Error()
			blockResult.Results[i] = tr
			continue
		}
		stateDB.Finalise(true)
		tr.GasUsed = result.UsedGas
		tr.Failed = result.Err != nil
		if result.Err != nil {
			tr.ErrMsg = result.Err.Error()
		}
		tr.ReturnData = append([]byte(nil), result.ReturnData...)
		for _, entry := range stateDB.GetLogs()[logStart:] {
			logCopy := *entry
			logCopy.TxHash = tx.Hash()
			tr.Logs = append(tr.Logs, &logCopy)
		}
		blockResult.GasUsed += result.UsedGas
		blockResult.Results[i] = tr
	}
	copy(blockResult.StateRoot[:], stateDB.ComputeAppHash(env.PreviousRoot))
	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect state writes: %w", err)
	}
	blockResult.Writes = writes
	return blockResult, nil
}
