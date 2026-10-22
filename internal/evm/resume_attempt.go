package evm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func resumeOptimisticAttempt(base *PebbleStateDB, versions *MVMemory, config *params.ChainConfig, chainID *big.Int, env BlockEnv, tx *types.Transaction, txIndex int, incarnation uint64, checkpoint *attemptCheckpoint, meter *attemptReplayMeter) (*optimisticAttempt, replayWork, error) {
	if checkpoint == nil || checkpoint.execution == nil || tx == nil || tx.To() == nil {
		return nil, replayWork{}, errResumeUnsupported
	}
	if config.Rules(big.NewInt(env.Height), true, env.Time).IsEIP4762 {
		return nil, replayWork{}, errResumeUnsupported
	}
	machine := InterpreterState{}
	state := newTxState(base, txIndex, incarnation, nil, nil)
	if err := checkpoint.execution.Restore(&machine, state); err != nil {
		return nil, replayWork{}, err
	}
	if len(machine.CallFrames) != 1 || machine.CallFrames[0].Depth != 1 || machine.CallFrames[0].Deployment {
		return nil, replayWork{}, errResumeUnsupported
	}
	state.TxIndex = txIndex
	state.Incarnation = incarnation
	before, err := base.PendingWrites()
	if err != nil {
		return nil, replayWork{}, err
	}
	signer := types.LatestSignerForChainID(chainID)
	msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
	if err != nil {
		return nil, replayWork{}, err
	}
	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer, Transfer: core.Transfer, GetHash: env.GetHash,
		Coinbase: env.Coinbase, BlockNumber: big.NewInt(env.Height), Time: env.Time,
		Difficulty: new(big.Int), Random: &env.Random, BaseFee: new(big.Int).Set(env.BaseFee),
		BlobBaseFee: new(big.Int), GasLimit: env.GasLimit,
	}
	evmEnv := vm.NewEVM(blockContext, state, config, vm.Config{})
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))
	frame := machine.CallFrames[0]
	ret, gasRemaining, vmErr := resumeGethFrame(evmEnv, frame, machine, meter)
	if vmErr != nil {
		if revision, ok := state.oldestSnapshotID(); ok {
			state.RevertToSnapshot(revision)
		}
		if vmErr != vm.ErrExecutionReverted {
			gasRemaining = 0
		}
	}
	usedGas, err := finalizeResumedMessage(state, config, env, msg, tx.Gas(), gasRemaining)
	if err != nil {
		return nil, replayWork{}, err
	}
	attempt := &optimisticAttempt{state: state, status: attemptEstimate, result: TxResult{Index: txIndex}}
	attempt.result.GasUsed = usedGas
	attempt.result.Failed = vmErr != nil
	if vmErr != nil {
		attempt.result.ErrMsg = vmErr.Error()
	}
	attempt.result.ReturnData = append([]byte(nil), ret...)
	for _, entry := range state.GetLogs() {
		copyLog := *entry
		copyLog.TxHash = tx.Hash()
		copyLog.Topics = append([]common.Hash(nil), entry.Topics...)
		copyLog.Data = append([]byte(nil), entry.Data...)
		attempt.result.Logs = append(attempt.result.Logs, &copyLog)
	}
	state.Finalise(true)
	reads, _ := state.AccessSets()
	attempt.readVersion = versions.Snapshot(reads)
	after, err := state.PendingWrites()
	if err != nil {
		return nil, replayWork{}, err
	}
	attempt.delta = diffPendingWrites(before, after)
	work := replayWork{opcodes: meter.opcodes}
	if machine.Gas >= gasRemaining {
		work.gas = machine.Gas - gasRemaining
	}
	return attempt, work, nil
}

func finalizeResumedMessage(state *TxState, config *params.ChainConfig, env BlockEnv, msg *core.Message, initialGas, gasRemaining uint64) (uint64, error) {
	rules := config.Rules(big.NewInt(env.Height), true, env.Time)
	usedBeforeRefund := initialGas - gasRemaining
	quotient := uint64(params.RefundQuotient)
	if rules.IsLondon {
		quotient = params.RefundQuotientEIP3529
	}
	refund := usedBeforeRefund / quotient
	if available := state.GetRefund(); refund > available {
		refund = available
	}
	gasRemaining += refund
	if rules.IsPrague {
		floor, err := core.FloorDataGas(msg.Data)
		if err != nil {
			return 0, err
		}
		if initialGas-gasRemaining < floor {
			gasRemaining = initialGas - floor
		}
	}
	remainingValue := new(uint256.Int).SetUint64(gasRemaining)
	remainingValue.Mul(remainingValue, uint256.MustFromBig(msg.GasPrice))
	state.AddBalance(msg.From, remainingValue, tracing.BalanceIncreaseGasReturn)
	usedGas := initialGas - gasRemaining
	if !(msg.GasFeeCap.Sign() == 0 && msg.GasTipCap.Sign() == 0) {
		effectiveTip := new(big.Int).Set(msg.GasPrice)
		if rules.IsLondon {
			effectiveTip.Sub(effectiveTip, env.BaseFee)
		}
		fee := new(uint256.Int).SetUint64(usedGas)
		fee.Mul(fee, uint256.MustFromBig(effectiveTip))
		state.AddBalance(env.Coinbase, fee, tracing.BalanceIncreaseRewardTransactionFee)
	}
	return usedGas, nil
}

func (s *PebbleStateDB) oldestSnapshotID() (int, bool) {
	if s == nil || len(s.snapshots) == 0 {
		return 0, false
	}
	return s.snapshots[0].id, true
}
