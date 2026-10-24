// Package gethfork là bản thử nghiệm khả thi cô lập của M5.1. Gói này không
// tham gia đường thực thi chính thức của Pluto.
package gethfork

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

// SampleContract được dùng chung bởi các bản thử nghiệm M5.1. Hợp đồng tăng ô
// lưu trữ 1 thêm ba đơn vị rồi trả về kết quả dạng từ 32 byte.
var SampleContract = []byte{
	byte(vm.PUSH1), 0x01, byte(vm.SLOAD),
	byte(vm.PUSH1), 0x03, byte(vm.ADD),
	byte(vm.PUSH1), 0x01, byte(vm.SSTORE),
	byte(vm.PUSH1), 0x01, byte(vm.SLOAD),
	byte(vm.PUSH0), byte(vm.MSTORE),
	byte(vm.PUSH1), 0x20, byte(vm.PUSH0), byte(vm.RETURN),
}

var errPaused = errors.New("interpreter paused at checkpoint")

// Checkpoint chứa các trường trình thông dịch và lớp phủ trạng thái có thể thay
// đổi mà bản rẽ nhánh có thể tiếp tục cần sở hữu. Slice và map được sao chép sâu.
type Checkpoint struct {
	PC         uint64
	Stack      []uint256.Int
	Memory     []byte
	ReturnData []byte
	Gas        uint64
	CallDepth  int
	Writes     map[common.Hash]common.Hash
}

type storageOverlay struct {
	base   map[common.Hash]common.Hash
	writes map[common.Hash]common.Hash
}

func newStorageOverlay(base map[common.Hash]common.Hash) *storageOverlay {
	return &storageOverlay{base: cloneStorage(base), writes: make(map[common.Hash]common.Hash)}
}

func (s *storageOverlay) get(key common.Hash) common.Hash {
	if value, ok := s.writes[key]; ok {
		return value
	}
	return s.base[key]
}

func (s *storageOverlay) set(key, value common.Hash) { s.writes[key] = value }

func cloneStorage(src map[common.Hash]common.Hash) map[common.Hash]common.Hash {
	dst := make(map[common.Hash]common.Hash, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

// interpreter là bản sao có chủ ý giới hạn của cấu trúc vòng lặp theo ngữ cảnh
// trong geth. Chỉ các opcode cần cho dữ liệu thử nghiệm mới được cài đặt.
type interpreter struct {
	code       []byte
	pc         uint64
	stack      []uint256.Int
	memory     []byte
	returnData []byte
	gas        uint64
	callDepth  int
	state      *storageOverlay
	output     []byte
	halted     bool
}

func newInterpreter(code []byte, gas uint64, base map[common.Hash]common.Hash) *interpreter {
	return &interpreter{
		code:       append([]byte(nil), code...),
		returnData: []byte{0xca, 0xfe},
		gas:        gas,
		callDepth:  1,
		state:      newStorageOverlay(base),
	}
}

func restoreInterpreter(code []byte, base map[common.Hash]common.Hash, checkpoint Checkpoint) *interpreter {
	return &interpreter{
		code:       append([]byte(nil), code...),
		pc:         checkpoint.PC,
		stack:      append([]uint256.Int(nil), checkpoint.Stack...),
		memory:     append([]byte(nil), checkpoint.Memory...),
		returnData: append([]byte(nil), checkpoint.ReturnData...),
		gas:        checkpoint.Gas,
		callDepth:  checkpoint.CallDepth,
		state:      &storageOverlay{base: cloneStorage(base), writes: cloneStorage(checkpoint.Writes)},
	}
}

func (in *interpreter) checkpoint() Checkpoint {
	return Checkpoint{
		PC:         in.pc,
		Stack:      append([]uint256.Int(nil), in.stack...),
		Memory:     append([]byte(nil), in.memory...),
		ReturnData: append([]byte(nil), in.returnData...),
		Gas:        in.gas,
		CallDepth:  in.callDepth,
		Writes:     cloneStorage(in.state.writes),
	}
}

func (in *interpreter) run() error { return in.runUntil(nil) }

// runUntil theo cấu trúc vòng lặp lấy lệnh/tính phí/thực thi của geth. stopPC
// tạm dừng trước opcode đó để khi khôi phục checkpoint không chạy lặp lại lệnh.
func (in *interpreter) runUntil(stopPC *uint64) error {
	for !in.halted {
		if stopPC != nil && in.pc == *stopPC {
			return errPaused
		}
		if in.pc >= uint64(len(in.code)) {
			in.halted = true
			return nil
		}
		op := vm.OpCode(in.code[in.pc])
		if err := in.charge(op); err != nil {
			return err
		}
		in.pc++
		if err := in.execute(op); err != nil {
			return err
		}
	}
	return nil
}

func (in *interpreter) charge(op vm.OpCode) error {
	var cost uint64
	switch op {
	case vm.PUSH0:
		cost = 2
	case vm.PUSH1, vm.ADD, vm.MSTORE:
		cost = 3
	case vm.SLOAD, vm.SSTORE:
		cost = 100
	case vm.RETURN:
		cost = 0
	default:
		return fmt.Errorf("spike does not support opcode %s", op)
	}
	if in.gas < cost {
		return errors.New("out of gas")
	}
	in.gas -= cost
	return nil
}

func (in *interpreter) execute(op vm.OpCode) error {
	switch op {
	case vm.PUSH0:
		in.pushUint64(0)
	case vm.PUSH1:
		if in.pc >= uint64(len(in.code)) {
			return errors.New("truncated PUSH1")
		}
		in.pushUint64(uint64(in.code[in.pc]))
		in.pc++
	case vm.ADD:
		x, err := in.pop()
		if err != nil {
			return err
		}
		y, err := in.pop()
		if err != nil {
			return err
		}
		var sum uint256.Int
		sum.Add(&x, &y)
		in.stack = append(in.stack, sum)
	case vm.SLOAD:
		key, err := in.pop()
		if err != nil {
			return err
		}
		value := in.state.get(common.Hash(key.Bytes32()))
		var word uint256.Int
		word.SetBytes(value[:])
		in.stack = append(in.stack, word)
	case vm.SSTORE:
		key, err := in.pop()
		if err != nil {
			return err
		}
		value, err := in.pop()
		if err != nil {
			return err
		}
		in.state.set(common.Hash(key.Bytes32()), common.Hash(value.Bytes32()))
	case vm.MSTORE:
		offset, err := in.popUint64()
		if err != nil {
			return err
		}
		value, err := in.pop()
		if err != nil {
			return err
		}
		in.ensureMemory(offset + 32)
		word := value.Bytes32()
		copy(in.memory[offset:offset+32], word[:])
	case vm.RETURN:
		offset, err := in.popUint64()
		if err != nil {
			return err
		}
		size, err := in.popUint64()
		if err != nil {
			return err
		}
		if offset+size < offset || offset+size > uint64(len(in.memory)) {
			return errors.New("return outside memory")
		}
		in.output = append([]byte(nil), in.memory[offset:offset+size]...)
		in.halted = true
	}
	return nil
}

func (in *interpreter) pushUint64(value uint64) {
	var word uint256.Int
	word.SetUint64(value)
	in.stack = append(in.stack, word)
}

func (in *interpreter) pop() (uint256.Int, error) {
	if len(in.stack) == 0 {
		return uint256.Int{}, errors.New("stack underflow")
	}
	last := len(in.stack) - 1
	value := in.stack[last]
	in.stack = in.stack[:last]
	return value, nil
}

func (in *interpreter) popUint64() (uint64, error) {
	value, err := in.pop()
	if err != nil {
		return 0, err
	}
	result, overflow := value.Uint64WithOverflow()
	if overflow {
		return 0, errors.New("uint64 overflow")
	}
	return result, nil
}

func (in *interpreter) ensureMemory(size uint64) {
	if uint64(len(in.memory)) >= size {
		return
	}
	in.memory = append(in.memory, make([]byte, size-uint64(len(in.memory)))...)
}
