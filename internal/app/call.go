package app

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/huyCuong73/pluto/internal/evm"
)

type CallArgs = evm.CallArgs
type CallResult = evm.CallResult

// Call executes against the latest committed state and discards all changes.
func (app *App) Call(args CallArgs) (*CallResult, error) {
	if args.Gas == 0 {
		args.Gas = blockGasLimit
	}
	if args.Gas > blockGasLimit {
		return nil, fmt.Errorf("call gas limit %d exceeds block gas limit %d", args.Gas, blockGasLimit)
	}
	return app.executeReadOnlyCall(args)
}

// EstimateGas finds the smallest gas limit for which the call succeeds.
func (app *App) EstimateGas(args CallArgs) (uint64, error) {
	intrinsic, err := core.IntrinsicGas(args.Data, args.AccessList, nil, args.To == nil, true, true, true)
	if err != nil {
		return 0, fmt.Errorf("calculate intrinsic gas: %w", err)
	}
	cap := args.Gas
	if cap == 0 || cap > blockGasLimit {
		cap = blockGasLimit
	}
	if cap < intrinsic {
		return 0, fmt.Errorf("gas required exceeds allowance (%d)", cap)
	}

	args.Gas = cap
	result, err := app.executeReadOnlyCall(args)
	if err != nil {
		return 0, fmt.Errorf("estimate gas at allowance %d: %w", cap, err)
	}
	if result.Err != nil {
		return 0, callFailure(cap, result)
	}

	low, high := intrinsic-1, cap
	for low+1 < high {
		mid := low + (high-low)/2
		args.Gas = mid
		result, err = app.executeReadOnlyCall(args)
		if err != nil {
			if errors.Is(err, core.ErrIntrinsicGas) {
				low = mid
				continue
			}
			return 0, fmt.Errorf("estimate gas at %d: %w", mid, err)
		}
		if result.Err == nil {
			high = mid
			continue
		}
		if isOutOfGas(result.Err) {
			low = mid
			continue
		}
		return 0, callFailure(mid, result)
	}
	return high, nil
}

func (app *App) executeReadOnlyCall(args CallArgs) (*CallResult, error) {
	return evm.ExecuteCall(evm.NewPebbleStateDB(app.db), newChainConfig(), app.callBlockEnv(), args)
}

func (app *App) callBlockEnv() evm.BlockEnv {
	height := app.currentHeight
	env := evm.BlockEnv{
		Height:       height,
		GasLimit:     blockGasLimit,
		BaseFee:      new(big.Int),
		PreviousRoot: append([]byte(nil), app.appHash...),
	}
	if height > 0 {
		if block, found, err := app.EthereumBlockByNumber(uint64(height)); err == nil && found {
			env.Time = block.Timestamp
			env.Coinbase = block.Coinbase
		}
	}
	env.GetHash = func(number uint64) common.Hash {
		if number >= uint64(height) || uint64(height)-number > 256 {
			return common.Hash{}
		}
		encoded, err := app.db.Get(blockHashKey(number))
		if err != nil {
			return common.Hash{}
		}
		return common.BytesToHash(encoded)
	}
	return env
}

func isOutOfGas(err error) bool {
	return errors.Is(err, vm.ErrOutOfGas) || errors.Is(err, vm.ErrCodeStoreOutOfGas)
}

func callFailure(gas uint64, result *CallResult) error {
	if isOutOfGas(result.Err) {
		return fmt.Errorf("gas required exceeds allowance (%d)", gas)
	}
	if len(result.RevertData) > 0 {
		return fmt.Errorf("execution reverted (data %x): %w", result.RevertData, result.Err)
	}
	return fmt.Errorf("execution failed: %w", result.Err)
}
