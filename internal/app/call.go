package app

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

type CallArgs = evm.CallArgs
type CallResult = evm.CallResult

// Call thực thi trên một snapshot PebbleDB cố định đã chốt rồi loại bỏ mọi thay đổi.
func (app *App) Call(args CallArgs) (*CallResult, error) {
	if args.Gas == 0 {
		args.Gas = blockGasLimit
	}
	if args.Gas > blockGasLimit {
		return nil, fmt.Errorf("call gas limit %d exceeds block gas limit %d", args.Gas, blockGasLimit)
	}
	committedHeight, appHash, snap := app.committedSnapshot()
	defer snap.Close()
	env := app.callBlockEnvFromKV(committedHeight, appHash, snap)
	return app.executeReadOnlyCallOnKV(snap, env, args)
}

// EstimateGas tìm giới hạn gas nhỏ nhất để lời gọi thực thi thành công trên cùng 1 snapshot nhất quán.
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

	committedHeight, appHash, snap := app.committedSnapshot()
	defer snap.Close()
	env := app.callBlockEnvFromKV(committedHeight, appHash, snap)

	args.Gas = cap
	result, err := app.executeReadOnlyCallOnKV(snap, env, args)
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
		result, err = app.executeReadOnlyCallOnKV(snap, env, args)
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

func (app *App) executeReadOnlyCallOnKV(kv evm.ReadOnlyKV, env evm.BlockEnv, args CallArgs) (*CallResult, error) {
	stateDB := evm.NewPebbleStateDBFromKV(kv)
	res, err := evm.ExecuteCall(stateDB, app.chainConfig(), env, args)
	if err != nil {
		return nil, err
	}
	if dbErr := stateDB.Err(); dbErr != nil {
		return nil, fmt.Errorf("read-only call state db error: %w", dbErr)
	}
	return res, nil
}

func (app *App) callBlockEnvFromKV(committedHeight uint64, appHash common.Hash, kv evm.ReadOnlyKV) evm.BlockEnv {
	height := int64(committedHeight)
	env := evm.BlockEnv{
		Height:       height,
		GasLimit:     blockGasLimit,
		BaseFee:      new(big.Int),
		PreviousRoot: append([]byte(nil), appHash.Bytes()...),
	}
	if committedHeight > 0 {
		if block, found, err := ethereumBlockByNumberFromKV(kv, committedHeight); err == nil && found {
			env.Time = block.Timestamp
			env.Coinbase = block.Coinbase
			env.BaseFee = new(big.Int)
			if block.BaseFeePerGas != nil {
				env.BaseFee.Set(block.BaseFeePerGas)
			}
		}
	}
	env.GetHash = func(number uint64) common.Hash {
		if number >= committedHeight || committedHeight-number > 256 || kv == nil {
			return common.Hash{}
		}
		encoded, err := kv.Get(blockHashKey(number))
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
