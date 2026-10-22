package evm

// This file is the focused geth-v1.16.8 interpreter adapter selected by the
// M5.1 decision. It deliberately reuses geth's active jump table and opcode
// functions, so opcode, dynamic-gas and hard-fork semantics are not duplicated.
// The unsafe views are guarded by layout tests and the pinned geth version.

import (
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"unsafe"

	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

type gethExecutionFunc func(*uint64, *vm.EVM, *vm.ScopeContext) ([]byte, error)
type gethGasFunc func(*vm.EVM, *vm.Contract, *vm.Stack, *vm.Memory, uint64) (uint64, error)
type gethMemorySizeFunc func(*vm.Stack) (uint64, bool)

// gethOperationView must stay layout-identical to core/vm.operation.
type gethOperationView struct {
	execute     gethExecutionFunc
	constantGas uint64
	dynamicGas  gethGasFunc
	minStack    int
	maxStack    int
	memorySize  gethMemorySizeFunc
	undefined   bool
}

type gethJumpTableView [256]*gethOperationView

const resumableGethVersion = "v1.16.8"

var (
	gethVersionOnce sync.Once
	gethVersionOK   bool
)

func resumableGethVersionAvailable() bool {
	gethVersionOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, dependency := range info.Deps {
			if dependency.Path != "github.com/ethereum/go-ethereum" {
				continue
			}
			version := dependency.Version
			if dependency.Replace != nil {
				version = dependency.Replace.Version
			}
			gethVersionOK = version == resumableGethVersion
			return
		}
	})
	return gethVersionOK
}

func privateField(owner any, name string) (reflect.Value, error) {
	value := reflect.ValueOf(owner)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return reflect.Value{}, fmt.Errorf("private field %s requires non-nil pointer", name)
	}
	field := value.Elem().FieldByName(name)
	if !field.IsValid() || !field.CanAddr() {
		return reflect.Value{}, fmt.Errorf("geth layout has no addressable %s field", name)
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem(), nil
}

func gethJumpTable(evmEnv *vm.EVM) (*gethJumpTableView, error) {
	field, err := privateField(evmEnv, "table")
	if err != nil {
		return nil, err
	}
	if field.IsNil() {
		return nil, errors.New("geth EVM has nil jump table")
	}
	return (*gethJumpTableView)(unsafe.Pointer(field.Pointer())), nil
}

func setGethPrivateField(owner any, name string, value any) error {
	field, err := privateField(owner, name)
	if err != nil {
		return err
	}
	source := reflect.ValueOf(value)
	if !source.IsValid() || !source.Type().AssignableTo(field.Type()) {
		return fmt.Errorf("geth field %s expects %s, got %T", name, field.Type(), value)
	}
	field.Set(source)
	return nil
}

func gethMemoryLastGasCost(memory *vm.Memory) (uint64, error) {
	field, err := privateField(memory, "lastGasCost")
	if err != nil {
		return 0, err
	}
	return field.Uint(), nil
}

func gethPrivateUint64(owner any, name string) (uint64, error) {
	field, err := privateField(owner, name)
	if err != nil {
		return 0, err
	}
	return field.Uint(), nil
}

func gethPrivateBool(owner any, name string) (bool, error) {
	field, err := privateField(owner, name)
	if err != nil {
		return false, err
	}
	return field.Bool(), nil
}

func restoreGethStack(words []uint256.Int) (*vm.Stack, error) {
	stack := new(vm.Stack)
	field, err := privateField(stack, "data")
	if err != nil {
		return nil, err
	}
	field.Set(reflect.ValueOf(append([]uint256.Int(nil), words...)))
	return stack, nil
}

func restoreGethMemory(data []byte, lastGasCost uint64) (*vm.Memory, error) {
	memory := vm.NewMemory()
	if len(data) > 0 {
		memory.Resize(uint64(len(data)))
		memory.Set(0, uint64(len(data)), data)
	}
	if err := setGethPrivateField(memory, "lastGasCost", lastGasCost); err != nil {
		memory.Free()
		return nil, err
	}
	return memory, nil
}

// resumeGethFrame resumes at a tracer checkpoint. Geth invokes OnOpcode after
// charging constant/dynamic gas but before memory expansion and execution, so
// the first resumed opcode must execute without charging a second time.
func resumeGethFrame(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter) (ret []byte, gasLeft uint64, runErr error) {
	if !resumableGethVersionAvailable() {
		return nil, 0, errResumeUnsupported
	}
	if len(machine.CallFrames) != 1 || frame.Depth != 1 {
		return nil, 0, errResumeUnsupported
	}
	table, err := gethJumpTable(evmEnv)
	if err != nil {
		return nil, 0, fmt.Errorf("geth jump table: %w", err)
	}
	stack, err := restoreGethStack(machine.Stack)
	if err != nil {
		return nil, 0, err
	}
	memory, err := restoreGethMemory(machine.Memory, machine.MemoryLastGasCost)
	if err != nil {
		return nil, 0, err
	}
	defer memory.Free()

	value := frame.Value
	contract := vm.NewContract(frame.Caller, frame.Address, &value, machine.Gas, nil)
	contract.SetCallCode(frame.CodeHash, append([]byte(nil), frame.Code...))
	contract.Input = append([]byte(nil), frame.Input...)
	contract.IsDeployment = frame.Deployment
	contract.IsSystemCall = frame.SystemCall
	scope := &vm.ScopeContext{Memory: memory, Stack: stack, Contract: contract}
	if err := setGethPrivateField(evmEnv, "depth", frame.Depth); err != nil {
		return nil, 0, err
	}
	if err := setGethPrivateField(evmEnv, "readOnly", frame.ReadOnly); err != nil {
		return nil, 0, err
	}
	if err := setGethPrivateField(evmEnv, "returnData", append([]byte(nil), machine.ReturnData...)); err != nil {
		return nil, 0, err
	}
	if err := setGethPrivateField(evmEnv, "callGasTemp", machine.CallGasTemp); err != nil {
		return nil, 0, err
	}

	pc := machine.PC
	firstCharged := true
	for {
		op := contract.GetOp(pc)
		operation := table[byte(op)]
		if operation == nil || operation.execute == nil {
			return nil, 0, fmt.Errorf("geth operation %s unavailable", op)
		}
		stackLen := len(stack.Data())
		if stackLen < operation.minStack || stackLen > operation.maxStack {
			return nil, 0, fmt.Errorf("invalid resumed stack length %d for %s", stackLen, op)
		}

		var memorySize uint64
		if operation.memorySize != nil {
			requested, overflow := operation.memorySize(stack)
			if overflow {
				return nil, 0, vm.ErrGasUintOverflow
			}
			memorySize, overflow = gethmath.SafeMul((requested+31)/32, 32)
			if overflow {
				return nil, 0, vm.ErrGasUintOverflow
			}
		}
		gasBefore := contract.Gas
		cost := uint64(0)
		if firstCharged {
			firstCharged = false
		} else {
			cost = operation.constantGas
			if contract.Gas < cost {
				return nil, 0, vm.ErrOutOfGas
			}
			contract.Gas -= cost
			if operation.dynamicGas != nil {
				dynamicCost, gasErr := operation.dynamicGas(evmEnv, contract, stack, memory, memorySize)
				cost += dynamicCost
				if gasErr != nil || contract.Gas < dynamicCost {
					return nil, 0, vm.ErrOutOfGas
				}
				contract.Gas -= dynamicCost
			}
		}
		if memorySize > 0 {
			memory.Resize(memorySize)
		}
		if meter != nil {
			meter.opcodes++
		}
		if tracer := evmEnv.Config.Tracer; tracer != nil && tracer.OnOpcode != nil {
			tracer.OnOpcode(pc, byte(op), gasBefore, cost, scope, machine.ReturnData, frame.Depth, nil)
		}
		res, execErr := operation.execute(&pc, evmEnv, scope)
		if execErr != nil {
			if execErr.Error() == "stop token" {
				return res, contract.Gas, nil
			}
			return res, contract.Gas, execErr
		}
		pc++
	}
}
