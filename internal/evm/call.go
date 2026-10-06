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

// CallArgs describes a transaction-like EVM simulation. The supplied state is
// mutated only in memory and must be discarded by the caller after execution.
type CallArgs struct {
	From       common.Address
	To         *common.Address
	Gas        uint64
	GasPrice   *big.Int
	Value      *big.Int
	Data       []byte
	AccessList types.AccessList
}

type CallResult struct {
	ReturnData []byte
	RevertData []byte
	GasUsed    uint64
	Err        error
}

// ExecuteCall runs a transaction-like message without committing the state.
func ExecuteCall(state *PebbleStateDB, chainConfig *params.ChainConfig, env BlockEnv, args CallArgs) (*CallResult, error) {
	if state == nil || chainConfig == nil {
		return nil, fmt.Errorf("call requires state and chain config")
	}
	if args.Gas == 0 {
		return nil, fmt.Errorf("call gas limit must be greater than zero")
	}
	if env.BaseFee == nil {
		env.BaseFee = new(big.Int)
	}
	if env.GetHash == nil {
		env.GetHash = func(uint64) common.Hash { return common.Hash{} }
	}

	gasPrice := new(big.Int)
	if args.GasPrice != nil {
		gasPrice.Set(args.GasPrice)
	}
	value := new(big.Int)
	if args.Value != nil {
		value.Set(args.Value)
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
	message := &core.Message{
		To:                    args.To,
		From:                  args.From,
		Nonce:                 state.GetNonce(args.From),
		Value:                 value,
		GasLimit:              args.Gas,
		GasPrice:              gasPrice,
		GasFeeCap:             new(big.Int).Set(gasPrice),
		GasTipCap:             new(big.Int).Set(gasPrice),
		Data:                  common.CopyBytes(args.Data),
		AccessList:            args.AccessList,
		BlobGasFeeCap:         new(big.Int),
		SkipNonceChecks:       true,
		SkipTransactionChecks: true,
	}

	evmEnv := vm.NewEVM(blockContext, state, chainConfig, vm.Config{NoBaseFee: true})
	result, err := core.ApplyMessage(evmEnv, message, new(core.GasPool).AddGas(args.Gas))
	if err != nil {
		return nil, err
	}
	return &CallResult{
		ReturnData: common.CopyBytes(result.Return()),
		RevertData: common.CopyBytes(result.Revert()),
		GasUsed:    result.UsedGas,
		Err:        result.Err,
	}, nil
}
