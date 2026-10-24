// Package minivm là bản thử nghiệm cô lập của M5.1. Gói này chỉ hỗ trợ các
// opcode mà hợp đồng mẫu dùng chung cần đến và không được chương trình chính gọi.
package minivm

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

var errPaused = errors.New("mini interpreter paused")

var supported = map[vm.OpCode]struct{}{
	vm.PUSH0: {}, vm.PUSH1: {}, vm.ADD: {}, vm.SLOAD: {},
	vm.SSTORE: {}, vm.MSTORE: {}, vm.RETURN: {},
}

// SupportedOpcodeCount được cố định theo tập opcode mà hợp đồng mẫu sử dụng.
const SupportedOpcodeCount = 7

type Checkpoint struct {
	PC       uint64
	Stack    []uint256.Int
	Memory   []byte
	Gas      uint64
	Writes   map[common.Hash]common.Hash
	WarmSlot map[common.Hash]bool
}

type Machine struct {
	code     []byte
	pc       uint64
	stack    []uint256.Int
	memory   []byte
	gas      uint64
	base     map[common.Hash]common.Hash
	writes   map[common.Hash]common.Hash
	warmSlot map[common.Hash]bool
	output   []byte
	halted   bool
}

func New(code []byte, gas uint64, base map[common.Hash]common.Hash) *Machine {
	return &Machine{
		code: append([]byte(nil), code...), gas: gas, base: cloneMap(base),
		writes: make(map[common.Hash]common.Hash), warmSlot: make(map[common.Hash]bool),
	}
}

func Restore(code []byte, base map[common.Hash]common.Hash, cp Checkpoint) *Machine {
	return &Machine{
		code: append([]byte(nil), code...), pc: cp.PC,
		stack: append([]uint256.Int(nil), cp.Stack...), memory: append([]byte(nil), cp.Memory...),
		gas: cp.Gas, base: cloneMap(base), writes: cloneMap(cp.Writes), warmSlot: cloneWarm(cp.WarmSlot),
	}
}

func (m *Machine) Checkpoint() Checkpoint {
	return Checkpoint{
		PC: m.pc, Stack: append([]uint256.Int(nil), m.stack...), Memory: append([]byte(nil), m.memory...),
		Gas: m.gas, Writes: cloneMap(m.writes), WarmSlot: cloneWarm(m.warmSlot),
	}
}

func (m *Machine) Run() error { return m.RunUntil(nil) }

// RunUntil tạm dừng trước khi thực thi stopPC, nhờ đó mỗi ranh giới opcode đều
// có thể được chọn làm điểm checkpoint.
func (m *Machine) RunUntil(stopPC *uint64) error {
	for !m.halted {
		if stopPC != nil && m.pc == *stopPC {
			return errPaused
		}
		if m.pc >= uint64(len(m.code)) {
			m.halted = true
			return nil
		}
		op := vm.OpCode(m.code[m.pc])
		if _, ok := supported[op]; !ok {
			return fmt.Errorf("opcode %s is outside the spike scope", op)
		}
		if err := m.step(op); err != nil {
			return err
		}
	}
	return nil
}

func (m *Machine) step(op vm.OpCode) error {
	switch op {
	case vm.PUSH0:
		if err := m.charge(vm.GasQuickStep); err != nil {
			return err
		}
		m.pc++
		m.pushUint64(0)
	case vm.PUSH1:
		if m.pc+1 >= uint64(len(m.code)) {
			return errors.New("truncated PUSH1")
		}
		if err := m.charge(vm.GasFastestStep); err != nil {
			return err
		}
		m.pushUint64(uint64(m.code[m.pc+1]))
		m.pc += 2
	case vm.ADD:
		if err := m.charge(vm.GasFastestStep); err != nil {
			return err
		}
		x, y, err := m.pop2()
		if err != nil {
			return err
		}
		var sum uint256.Int
		sum.Add(&x, &y)
		m.stack = append(m.stack, sum)
		m.pc++
	case vm.SLOAD:
		keyWord, err := m.peek()
		if err != nil {
			return err
		}
		key := common.Hash(keyWord.Bytes32())
		cost := params.WarmStorageReadCostEIP2929
		if !m.warmSlot[key] {
			cost = params.ColdSloadCostEIP2929
		}
		if err := m.charge(cost); err != nil {
			return err
		}
		m.warmSlot[key] = true
		_, _ = m.pop()
		var value uint256.Int
		value.SetBytes(m.get(key).Bytes())
		m.stack = append(m.stack, value)
		m.pc++
	case vm.SSTORE:
		keyWord, valueWord, err := m.peek2()
		if err != nil {
			return err
		}
		key, value := common.Hash(keyWord.Bytes32()), common.Hash(valueWord.Bytes32())
		cost := m.sstoreCost(key, value)
		if !m.warmSlot[key] {
			cost += params.ColdSloadCostEIP2929
		}
		if err := m.charge(cost); err != nil {
			return err
		}
		m.warmSlot[key] = true
		_, _, _ = m.pop2()
		m.writes[key] = value
		m.pc++
	case vm.MSTORE:
		offsetWord, value, err := m.peek2()
		if err != nil {
			return err
		}
		offset, overflow := offsetWord.Uint64WithOverflow()
		if overflow || offset > ^uint64(0)-32 {
			return errors.New("memory offset overflow")
		}
		if err := m.charge(vm.GasFastestStep + memoryExpansionCost(uint64(len(m.memory)), offset+32)); err != nil {
			return err
		}
		_, _, _ = m.pop2()
		m.growMemory(offset + 32)
		word := value.Bytes32()
		copy(m.memory[offset:offset+32], word[:])
		m.pc++
	case vm.RETURN:
		offsetWord, sizeWord, err := m.peek2()
		if err != nil {
			return err
		}
		offset, overflowOffset := offsetWord.Uint64WithOverflow()
		size, overflowSize := sizeWord.Uint64WithOverflow()
		if overflowOffset || overflowSize || offset+size < offset {
			return errors.New("return range overflow")
		}
		if err := m.charge(memoryExpansionCost(uint64(len(m.memory)), offset+size)); err != nil {
			return err
		}
		_, _, _ = m.pop2()
		m.growMemory(offset + size)
		m.output = append([]byte(nil), m.memory[offset:offset+size]...)
		m.pc++
		m.halted = true
	}
	return nil
}

func (m *Machine) sstoreCost(key, value common.Hash) uint64 {
	current, original := m.get(key), m.base[key]
	if current == value {
		return params.WarmStorageReadCostEIP2929
	}
	if current == original {
		if original == (common.Hash{}) {
			return params.SstoreSetGasEIP2200
		}
		return params.SstoreResetGasEIP2200 - params.ColdSloadCostEIP2929
	}
	return params.WarmStorageReadCostEIP2929
}

func (m *Machine) charge(cost uint64) error {
	if m.gas < cost {
		return errors.New("out of gas")
	}
	m.gas -= cost
	return nil
}

func memoryExpansionCost(oldBytes, newBytes uint64) uint64 {
	if newBytes <= oldBytes {
		return 0
	}
	oldWords, newWords := (oldBytes+31)/32, (newBytes+31)/32
	fee := func(words uint64) uint64 { return words*params.MemoryGas + words*words/512 }
	return fee(newWords) - fee(oldWords)
}

func (m *Machine) get(key common.Hash) common.Hash {
	if value, ok := m.writes[key]; ok {
		return value
	}
	return m.base[key]
}

func (m *Machine) peek() (uint256.Int, error) {
	if len(m.stack) == 0 {
		return uint256.Int{}, errors.New("stack underflow")
	}
	return m.stack[len(m.stack)-1], nil
}

func (m *Machine) peek2() (uint256.Int, uint256.Int, error) {
	if len(m.stack) < 2 {
		return uint256.Int{}, uint256.Int{}, errors.New("stack underflow")
	}
	return m.stack[len(m.stack)-1], m.stack[len(m.stack)-2], nil
}

func (m *Machine) pop() (uint256.Int, error) {
	value, err := m.peek()
	if err != nil {
		return uint256.Int{}, err
	}
	m.stack = m.stack[:len(m.stack)-1]
	return value, nil
}

func (m *Machine) pop2() (uint256.Int, uint256.Int, error) {
	x, err := m.pop()
	if err != nil {
		return uint256.Int{}, uint256.Int{}, err
	}
	y, err := m.pop()
	return x, y, err
}

func (m *Machine) pushUint64(value uint64) {
	var word uint256.Int
	word.SetUint64(value)
	m.stack = append(m.stack, word)
}

func (m *Machine) growMemory(size uint64) {
	if size > uint64(len(m.memory)) {
		m.memory = append(m.memory, make([]byte, size-uint64(len(m.memory)))...)
	}
}

func cloneMap(src map[common.Hash]common.Hash) map[common.Hash]common.Hash {
	dst := make(map[common.Hash]common.Hash, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneWarm(src map[common.Hash]bool) map[common.Hash]bool {
	dst := make(map[common.Hash]bool, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (m *Machine) Output() []byte { return append([]byte(nil), m.output...) }
func (m *Machine) Gas() uint64    { return m.gas }
func (m *Machine) State(key common.Hash) common.Hash {
	return m.get(key)
}
