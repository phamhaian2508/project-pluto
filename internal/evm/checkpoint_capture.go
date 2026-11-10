package evm

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

type checkpointCapture struct {
	config    CheckpointConfig
	hotKeys   map[MVKey]struct{}
	state     *TxState
	versions  *MVMemory
	attempt   *optimisticAttempt
	evm       *vm.EVM
	position  uint64
	pending   *pendingPreGasCheckpoint
	jumpTable *gethJumpTableView
	err       error
}

type pendingPreGasCheckpoint struct {
	opcode     byte
	position   uint64
	checkpoint attemptCheckpoint
}

func (c *checkpointCapture) onOpcode(pc uint64, opcode byte, gas, cost uint64, context tracing.OpContext, returnData []byte, depth int, opErr error) {
	position := c.position
	c.position++
	if c == nil || c.err != nil || c.config.Policy == CheckpointPolicyP0 || depth != 1 || gas < cost || opErr != nil {
		c.pending = nil
		return
	}
	scope, ok := context.(*vm.ScopeContext)
	if !ok || scope == nil || scope.Contract == nil || scope.Stack == nil || scope.Memory == nil {
		c.pending = nil
		return
	}
	if pending := c.pending; pending != nil {
		c.pending = nil
		if pending.opcode != opcode || pending.position != position {
			c.err = fmt.Errorf("pre-gas checkpoint boundary changed: got opcode %d position %d, want opcode %d position %d", opcode, position, pending.opcode, pending.position)
			return
		}
		pending.checkpoint.execution.interpreter.PC = pc
		pending.checkpoint.execution.interpreter.ReturnData = append([]byte(nil), returnData...)
		for i := range pending.checkpoint.execution.interpreter.CallFrames {
			pending.checkpoint.execution.interpreter.CallFrames[i].PC = pc
			pending.checkpoint.execution.interpreter.CallFrames[i].ReturnData = append([]byte(nil), returnData...)
		}
		c.attempt.checkpoints = append(c.attempt.checkpoints, pending.checkpoint)
		return
	}
	if !c.shouldCapture(position, vm.OpCode(opcode), scope) {
		return
	}
	checkpoint, err := c.captureCheckpoint(position, pc, gas, scope, returnData, depth)
	if err != nil {
		c.err = err
		return
	}
	c.attempt.checkpoints = append(c.attempt.checkpoints, checkpoint)
}

// beforeDynamicGas runs from a private per-EVM jump-table wrapper. At this
// point geth has charged only constantGas; dynamic gas has not read or mutated
// access lists, refunds, memory-gas bookkeeping, or callGasTemp yet.
func (c *checkpointCapture) beforeDynamicGas(opcode byte, evmEnv *vm.EVM, contract *vm.Contract, stack *vm.Stack, memory *vm.Memory, constantGas uint64) {
	if c == nil || c.err != nil || c.config.Policy == CheckpointPolicyP0 {
		return
	}
	depth, err := gethPrivateInt(evmEnv, "depth")
	if err != nil {
		c.err = err
		return
	}
	if depth != 1 {
		return
	}
	scope := &vm.ScopeContext{Contract: contract, Stack: stack, Memory: memory}
	if !c.shouldCapture(c.position, vm.OpCode(opcode), scope) {
		return
	}
	if contract.Gas > ^uint64(0)-constantGas {
		c.err = fmt.Errorf("pre-gas checkpoint gas overflow")
		return
	}
	returnData, err := gethPrivateBytes(evmEnv, "returnData")
	if err != nil {
		c.err = err
		return
	}
	checkpoint, err := c.captureCheckpoint(c.position, 0, contract.Gas+constantGas, scope, returnData, depth)
	if err != nil {
		c.err = err
		return
	}
	c.pending = &pendingPreGasCheckpoint{opcode: opcode, position: c.position, checkpoint: checkpoint}
}

func (c *checkpointCapture) shouldCapture(position uint64, op vm.OpCode, scope *vm.ScopeContext) bool {
	readKey, hasReadKey := checkpointReadKey(op, scope)
	switch c.config.Policy {
	case CheckpointPolicyP1:
		return isCheckpointStateRead(op)
	case CheckpointPolicyP2:
		return position > 0 && position%c.config.Interval == 0
	case CheckpointPolicyP3:
		_, selected := c.hotKeys[readKey]
		return selected && hasReadKey
	default:
		return false
	}
}

func (c *checkpointCapture) captureCheckpoint(position, pc, gas uint64, scope *vm.ScopeContext, returnData []byte, depth int) (attemptCheckpoint, error) {
	memoryGas, err := gethMemoryLastGasCost(scope.Memory)
	if err != nil {
		return attemptCheckpoint{}, err
	}
	callGasTemp, err := gethPrivateUint64(c.evm, "callGasTemp")
	if err != nil {
		return attemptCheckpoint{}, err
	}
	readOnly, err := gethPrivateBool(c.evm, "readOnly")
	if err != nil {
		return attemptCheckpoint{}, err
	}
	value := uint256.Int{}
	if scope.Contract.Value() != nil {
		value.Set(scope.Contract.Value())
	}
	machine := InterpreterState{
		PC: pc, Stack: append([]uint256.Int(nil), scope.Stack.Data()...),
		Memory: append([]byte(nil), scope.Memory.Data()...), MemoryLastGasCost: memoryGas,
		ReturnData: append([]byte(nil), returnData...), Gas: gas, CallGasTemp: callGasTemp,
		CallFrames: []CallFrameState{{
			PC: pc, Stack: append([]uint256.Int(nil), scope.Stack.Data()...),
			Memory: append([]byte(nil), scope.Memory.Data()...), ReturnData: append([]byte(nil), returnData...),
			Gas: gas, Depth: depth, Caller: scope.Contract.Caller(), Address: scope.Contract.Address(),
			CodeAddress: scope.Contract.Address(), CodeHash: scope.Contract.CodeHash,
			Code: append([]byte(nil), scope.Contract.Code...), Input: append([]byte(nil), scope.Contract.Input...),
			Value: value, ReadOnly: readOnly, Deployment: scope.Contract.IsDeployment, SystemCall: scope.Contract.IsSystemCall,
		}},
	}
	execution, err := TakeExecutionCheckpoint(&machine, c.state)
	if err != nil {
		return attemptCheckpoint{}, fmt.Errorf("capture production checkpoint: %w", err)
	}
	reads := append([]MVKey(nil), c.state.ReadSet...)
	return attemptCheckpoint{
		beforeRead: len(reads), opcodePosition: position, execution: execution,
		readVersions: c.versions.Snapshot(reads),
	}, nil
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
