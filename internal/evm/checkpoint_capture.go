package evm

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

type checkpointCapture struct {
	config   CheckpointConfig
	hotKeys  map[MVKey]struct{}
	state    *TxState
	versions *MVMemory
	attempt  *optimisticAttempt
	evm      *vm.EVM
	position uint64
	err      error
}

func (c *checkpointCapture) onOpcode(pc uint64, opcode byte, gas, cost uint64, context tracing.OpContext, returnData []byte, depth int, _ error) {
	position := c.position
	c.position++
	if c == nil || c.err != nil || c.config.Policy == CheckpointPolicyP0 || depth != 1 || gas < cost {
		return
	}
	scope, ok := context.(*vm.ScopeContext)
	if !ok || scope == nil || scope.Contract == nil || scope.Stack == nil || scope.Memory == nil {
		return
	}
	op := vm.OpCode(opcode)
	readKey, hasReadKey := checkpointReadKey(op, scope)
	selected := false
	switch c.config.Policy {
	case CheckpointPolicyP1:
		selected = isCheckpointStateRead(op)
	case CheckpointPolicyP2:
		selected = position > 0 && position%c.config.Interval == 0
	case CheckpointPolicyP3:
		_, selected = c.hotKeys[readKey]
		selected = selected && hasReadKey
	}
	if !selected {
		return
	}
	memoryGas, err := gethMemoryLastGasCost(scope.Memory)
	if err != nil {
		c.err = err
		return
	}
	callGasTemp, err := gethPrivateUint64(c.evm, "callGasTemp")
	if err != nil {
		c.err = err
		return
	}
	readOnly, err := gethPrivateBool(c.evm, "readOnly")
	if err != nil {
		c.err = err
		return
	}
	value := uint256.Int{}
	if scope.Contract.Value() != nil {
		value.Set(scope.Contract.Value())
	}
	machine := InterpreterState{
		PC: pc, Stack: append([]uint256.Int(nil), scope.Stack.Data()...),
		Memory: append([]byte(nil), scope.Memory.Data()...), MemoryLastGasCost: memoryGas,
		ReturnData: append([]byte(nil), returnData...), Gas: gas - cost, CallGasTemp: callGasTemp,
		CallFrames: []CallFrameState{{
			PC: pc, Stack: append([]uint256.Int(nil), scope.Stack.Data()...),
			Memory: append([]byte(nil), scope.Memory.Data()...), ReturnData: append([]byte(nil), returnData...),
			Gas: gas - cost, Depth: depth, Caller: scope.Contract.Caller(), Address: scope.Contract.Address(),
			CodeAddress: scope.Contract.Address(), CodeHash: scope.Contract.CodeHash,
			Code: append([]byte(nil), scope.Contract.Code...), Input: append([]byte(nil), scope.Contract.Input...),
			Value: value, ReadOnly: readOnly, Deployment: scope.Contract.IsDeployment, SystemCall: scope.Contract.IsSystemCall,
		}},
	}
	execution, err := TakeExecutionCheckpoint(&machine, c.state)
	if err != nil {
		c.err = fmt.Errorf("capture production checkpoint: %w", err)
		return
	}
	reads := append([]MVKey(nil), c.state.ReadSet...)
	c.attempt.checkpoints = append(c.attempt.checkpoints, attemptCheckpoint{
		beforeRead: len(reads), opcodePosition: position, execution: execution,
		readVersions: c.versions.Snapshot(reads),
	})
}

func checkpointReadKey(op vm.OpCode, scope *vm.ScopeContext) (MVKey, bool) {
	stack := scope.Stack.Data()
	if len(stack) == 0 {
		return MVKey{}, false
	}
	top := stack[len(stack)-1]
	switch op {
	case vm.SLOAD:
		return MVKey{Address: scope.Contract.Address(), Kind: MVStorage, Slot: common.Hash(top.Bytes32())}, true
	case vm.BALANCE:
		return MVKey{Address: common.Address(top.Bytes20()), Kind: MVBalance}, true
	case vm.EXTCODESIZE, vm.EXTCODECOPY, vm.EXTCODEHASH:
		return MVKey{Address: common.Address(top.Bytes20()), Kind: MVCode}, true
	case vm.SELFBALANCE:
		return MVKey{Address: scope.Contract.Address(), Kind: MVBalance}, true
	}
	return MVKey{}, false
}

func isCheckpointStateRead(op vm.OpCode) bool {
	switch op {
	case vm.SLOAD, vm.SSTORE, vm.BALANCE, vm.SELFBALANCE,
		vm.EXTCODESIZE, vm.EXTCODECOPY, vm.EXTCODEHASH,
		vm.CALL, vm.CALLCODE, vm.DELEGATECALL, vm.STATICCALL,
		vm.CREATE, vm.CREATE2, vm.SELFDESTRUCT:
		return true
	default:
		return false
	}
}
