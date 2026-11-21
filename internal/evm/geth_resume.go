package evm

// Tệp này là bộ kết nối trình thông dịch geth v1.16.8 được chọn trong quyết
// định M5.1. Tệp chủ ý dùng lại bảng nhảy và hàm opcode đang hoạt động của geth,
// tránh cài đặt trùng ngữ nghĩa opcode, gas động và các đợt hard fork. Các vùng
// truy cập unsafe được bảo vệ bằng kiểm thử bố cục và phiên bản geth đã ghim.

import (
	"errors"
	"fmt"
	"math"
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

// gethOperationView phải giữ bố cục giống hệt core/vm.operation.
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

	gethLayoutOnce sync.Once
	gethOffsets    gethFieldOffsets
	gethOffsetsErr error

	// pool bảng nhảy có bọc dynamicGas theo con trỏ bảng nhảy gốc của fork (vd Shanghai),
	// tránh cấp phát lại 256 operation + ~40 closure trên mỗi giao dịch.
	hookedJumpTablePools      sync.Map // map[*gethJumpTableView]*sync.Pool (mọi opcode có dynamicGas - cho fixed)
	stateHookedJumpTablePools sync.Map // map[*gethJumpTableView]*sync.Pool (chỉ opcode đọc/ghi state - cho history/adaptive)
)

// gethFieldOffsets cache offset các trường private của geth v1.16.8 một lần duy nhất
// thay vì gọi reflect.ValueOf(...).FieldByName(...) trên mỗi opcode.
type gethFieldOffsets struct {
	evmTable       uintptr
	evmDepth       uintptr
	evmReadOnly    uintptr
	evmReturnData  uintptr
	evmCallGasTemp uintptr
	memLastGasCost uintptr
	stackData      uintptr
}

func resolveStructFieldOffset(typ reflect.Type, name string, wantKind reflect.Kind, wantType reflect.Type) (uintptr, error) {
	f, ok := typ.FieldByName(name)
	if !ok {
		return 0, fmt.Errorf("geth layout %s missing field %q", typ, name)
	}
	if wantType != nil && f.Type != wantType {
		return 0, fmt.Errorf("geth layout %s.%s type=%s want=%s", typ, name, f.Type, wantType)
	}
	if wantType == nil && f.Type.Kind() != wantKind {
		return 0, fmt.Errorf("geth layout %s.%s kind=%s want=%s", typ, name, f.Type.Kind(), wantKind)
	}
	return f.Offset, nil
}

func loadGethFieldOffsets() (*gethFieldOffsets, error) {
	gethLayoutOnce.Do(func() {
		if !resumableGethVersionAvailable() {
			gethOffsetsErr = fmt.Errorf("unsupported geth version (want %s)", resumableGethVersion)
			return
		}
		evmType := reflect.TypeOf(vm.EVM{})
		memType := reflect.TypeOf(vm.Memory{})
		stackType := reflect.TypeOf(vm.Stack{})

		var off gethFieldOffsets
		var err error
		if off.evmTable, err = resolveStructFieldOffset(evmType, "table", reflect.Pointer, nil); err != nil {
			gethOffsetsErr = err
			return
		}
		// xác nhận table trỏ tới mảng 256 con trỏ khớp kích thước gethJumpTableView và bố cục đủ 7 trường vm.operation
		tableField, _ := evmType.FieldByName("table")
		if tableField.Type.Kind() != reflect.Pointer || tableField.Type.Elem().Kind() != reflect.Array ||
			tableField.Type.Elem().Len() != 256 || tableField.Type.Elem().Size() != unsafe.Sizeof(gethJumpTableView{}) {
			gethOffsetsErr = fmt.Errorf("geth JumpTable size=%d != view=%d", tableField.Type.Elem().Size(), unsafe.Sizeof(gethJumpTableView{}))
			return
		}
		opPtrType := tableField.Type.Elem().Elem()
		if opPtrType.Kind() != reflect.Pointer || opPtrType.Elem().Kind() != reflect.Struct {
			gethOffsetsErr = fmt.Errorf("geth JumpTable element type=%s is not *struct", opPtrType)
			return
		}
		if err := validateGethOperationLayout(opPtrType.Elem()); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.evmDepth, err = resolveStructFieldOffset(evmType, "depth", reflect.Int, reflect.TypeOf(int(0))); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.evmReadOnly, err = resolveStructFieldOffset(evmType, "readOnly", reflect.Bool, reflect.TypeOf(false)); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.evmReturnData, err = resolveStructFieldOffset(evmType, "returnData", reflect.Slice, reflect.TypeOf([]byte(nil))); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.evmCallGasTemp, err = resolveStructFieldOffset(evmType, "callGasTemp", reflect.Uint64, reflect.TypeOf(uint64(0))); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.memLastGasCost, err = resolveStructFieldOffset(memType, "lastGasCost", reflect.Uint64, reflect.TypeOf(uint64(0))); err != nil {
			gethOffsetsErr = err
			return
		}
		if off.stackData, err = resolveStructFieldOffset(stackType, "data", reflect.Slice, reflect.TypeOf([]uint256.Int(nil))); err != nil {
			gethOffsetsErr = err
			return
		}
		gethOffsets = off
	})
	if gethOffsetsErr != nil {
		return nil, gethOffsetsErr
	}
	return &gethOffsets, nil
}

func validateGethOperationLayout(opType reflect.Type) error {
	viewType := reflect.TypeOf(gethOperationView{})
	if opType.Size() != viewType.Size() || opType.NumField() != viewType.NumField() {
		return fmt.Errorf("geth operation layout mismatch: size=%d/%d fields=%d/%d", opType.Size(), viewType.Size(), opType.NumField(), viewType.NumField())
	}
	for i := 0; i < viewType.NumField(); i++ {
		vf := viewType.Field(i)
		of, ok := opType.FieldByName(vf.Name)
		if !ok {
			return fmt.Errorf("geth operation missing field %q", vf.Name)
		}
		if of.Offset != vf.Offset || of.Type.Size() != vf.Type.Size() || of.Type.Kind() != vf.Type.Kind() {
			return fmt.Errorf("geth operation.%s mismatch: offset=%d/%d size=%d/%d kind=%s/%s",
				vf.Name, of.Offset, vf.Offset, of.Type.Size(), vf.Type.Size(), of.Type.Kind(), vf.Type.Kind())
		}
	}
	return nil
}

func resumableGethVersionAvailable() bool {
	gethVersionOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		// Go 1.24.1 test binaries can omit the dependency list from build info.
		// go.mod pins geth in that case; still check the runtime layout before
		// using the private interpreter adapter.
		if len(info.Deps) == 0 {
			gethVersionOK = true
			return
		}
		for _, dependency := range info.Deps {
			if dependency.Path != "github.com/ethereum/go-ethereum" {
				continue
			}
			version := dependency.Version
			if dependency.Replace != nil && dependency.Replace.Version != "" {
				version = dependency.Replace.Version
			}
			gethVersionOK = version == resumableGethVersion
			return
		}
	})
	return gethVersionOK
}

func gethJumpTable(evmEnv *vm.EVM) (*gethJumpTableView, error) {
	if evmEnv == nil {
		return nil, errors.New("geth EVM is nil")
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		return nil, err
	}
	table := *(**gethJumpTableView)(unsafe.Add(unsafe.Pointer(evmEnv), off.evmTable))
	if table == nil {
		return nil, errors.New("geth EVM has nil jump table")
	}
	return table, nil
}

func setGethJumpTable(evmEnv *vm.EVM, table *gethJumpTableView) error {
	if evmEnv == nil {
		return errors.New("geth EVM is nil")
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		return err
	}
	*(**gethJumpTableView)(unsafe.Add(unsafe.Pointer(evmEnv), off.evmTable)) = table
	return nil
}

func fastGethDepth(evmEnv *vm.EVM) (int, error) {
	if evmEnv == nil {
		return 0, errors.New("geth EVM is nil")
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		return 0, err
	}
	return *(*int)(unsafe.Add(unsafe.Pointer(evmEnv), off.evmDepth)), nil
}

func fastGethReturnDataView(evmEnv *vm.EVM) ([]byte, error) {
	if evmEnv == nil {
		return nil, errors.New("geth EVM is nil")
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		return nil, err
	}
	return *(*[]byte)(unsafe.Add(unsafe.Pointer(evmEnv), off.evmReturnData)), nil
}

func setGethPrivateField(owner any, name string, value any) error {
	off, err := loadGethFieldOffsets()
	if err != nil {
		return err
	}
	switch obj := owner.(type) {
	case *vm.EVM:
		if obj == nil {
			return fmt.Errorf("private field %s requires non-nil pointer", name)
		}
		switch name {
		case "depth":
			if v, ok := value.(int); ok {
				*(*int)(unsafe.Add(unsafe.Pointer(obj), off.evmDepth)) = v
				return nil
			}
		case "readOnly":
			if v, ok := value.(bool); ok {
				*(*bool)(unsafe.Add(unsafe.Pointer(obj), off.evmReadOnly)) = v
				return nil
			}
		case "returnData":
			if v, ok := value.([]byte); ok {
				*(*[]byte)(unsafe.Add(unsafe.Pointer(obj), off.evmReturnData)) = v
				return nil
			}
		case "callGasTemp":
			if v, ok := value.(uint64); ok {
				*(*uint64)(unsafe.Add(unsafe.Pointer(obj), off.evmCallGasTemp)) = v
				return nil
			}
		}
	case *vm.Memory:
		if obj == nil {
			return fmt.Errorf("private field %s requires non-nil pointer", name)
		}
		if name == "lastGasCost" {
			if v, ok := value.(uint64); ok {
				*(*uint64)(unsafe.Add(unsafe.Pointer(obj), off.memLastGasCost)) = v
				return nil
			}
		}
	case *vm.Stack:
		if obj == nil {
			return fmt.Errorf("private field %s requires non-nil pointer", name)
		}
		if name == "data" {
			if v, ok := value.([]uint256.Int); ok {
				*(*[]uint256.Int)(unsafe.Add(unsafe.Pointer(obj), off.stackData)) = v
				return nil
			}
		}
	}
	return fmt.Errorf("unsupported geth private field %T.%s with value %T", owner, name, value)
}

func gethMemoryLastGasCost(memory *vm.Memory) (uint64, error) {
	if memory == nil {
		return 0, errors.New("geth memory is nil")
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		return 0, err
	}
	return *(*uint64)(unsafe.Add(unsafe.Pointer(memory), off.memLastGasCost)), nil
}

func gethPrivateUint64(owner any, name string) (uint64, error) {
	off, err := loadGethFieldOffsets()
	if err != nil {
		return 0, err
	}
	switch obj := owner.(type) {
	case *vm.EVM:
		if obj != nil && name == "callGasTemp" {
			return *(*uint64)(unsafe.Add(unsafe.Pointer(obj), off.evmCallGasTemp)), nil
		}
	case *vm.Memory:
		if obj != nil && name == "lastGasCost" {
			return *(*uint64)(unsafe.Add(unsafe.Pointer(obj), off.memLastGasCost)), nil
		}
	}
	return 0, fmt.Errorf("unsupported geth uint64 field %T.%s", owner, name)
}

func gethPrivateBool(owner any, name string) (bool, error) {
	off, err := loadGethFieldOffsets()
	if err != nil {
		return false, err
	}
	if obj, ok := owner.(*vm.EVM); ok && obj != nil && name == "readOnly" {
		return *(*bool)(unsafe.Add(unsafe.Pointer(obj), off.evmReadOnly)), nil
	}
	return false, fmt.Errorf("unsupported geth bool field %T.%s", owner, name)
}

func gethPrivateInt(owner any, name string) (int, error) {
	off, err := loadGethFieldOffsets()
	if err != nil {
		return 0, err
	}
	if obj, ok := owner.(*vm.EVM); ok && obj != nil && name == "depth" {
		return *(*int)(unsafe.Add(unsafe.Pointer(obj), off.evmDepth)), nil
	}
	return 0, fmt.Errorf("unsupported geth int field %T.%s", owner, name)
}

func gethPrivateBytes(owner any, name string) ([]byte, error) {
	off, err := loadGethFieldOffsets()
	if err != nil {
		return nil, err
	}
	if obj, ok := owner.(*vm.EVM); ok && obj != nil && name == "returnData" {
		raw := *(*[]byte)(unsafe.Add(unsafe.Pointer(obj), off.evmReturnData))
		if len(raw) == 0 {
			return nil, nil
		}
		return append([]byte(nil), raw...), nil
	}
	return nil, fmt.Errorf("unsupported geth bytes field %T.%s", owner, name)
}

type preGasHookTarget interface {
	beforeDynamicGas(opcode byte, evmEnv *vm.EVM, contract *vm.Contract, stack *vm.Stack, memory *vm.Memory, constantGas uint64)
}

// hookedJumpTable giữ một bản sao bảng nhảy riêng cho từng goroutine mượn từ sync.Pool,
// cho phép tráo target theo từng giao dịch mà không cấp phát lại 256 operation/closure.
type hookedJumpTable struct {
	base   *gethJumpTableView
	table  gethJumpTableView
	ops    [256]gethOperationView
	target preGasHookTarget
}

func isStateDynamicGasOpcode(op vm.OpCode) bool {
	switch op {
	case vm.SLOAD, vm.SSTORE, vm.BALANCE,
		vm.EXTCODESIZE, vm.EXTCODECOPY, vm.EXTCODEHASH,
		vm.CALL, vm.CALLCODE, vm.DELEGATECALL, vm.STATICCALL,
		vm.CREATE, vm.CREATE2, vm.SELFDESTRUCT:
		return true
	default:
		return false
	}
}

func newHookedJumpTable(base *gethJumpTableView, stateOpsOnly bool) *hookedJumpTable {
	h := &hookedJumpTable{base: base}
	for opcode, operation := range base {
		if operation == nil {
			continue
		}
		h.ops[opcode] = *operation
		if operation.dynamicGas != nil && (!stateOpsOnly || isStateDynamicGasOpcode(vm.OpCode(opcode))) {
			original := operation.dynamicGas
			opcodeByte := byte(opcode)
			constantGas := operation.constantGas
			h.ops[opcode].dynamicGas = func(evmEnv *vm.EVM, contract *vm.Contract, stack *vm.Stack, memory *vm.Memory, memorySize uint64) (uint64, error) {
				if tgt := h.target; tgt != nil {
					tgt.beforeDynamicGas(opcodeByte, evmEnv, contract, stack, memory, constantGas)
				}
				return original(evmEnv, contract, stack, memory, memorySize)
			}
		}
		h.table[opcode] = &h.ops[opcode]
	}
	return h
}

func poolForBaseJumpTable(base *gethJumpTableView, stateOpsOnly bool) *sync.Pool {
	pools := &hookedJumpTablePools
	if stateOpsOnly {
		pools = &stateHookedJumpTablePools
	}
	if existing, ok := pools.Load(base); ok {
		return existing.(*sync.Pool)
	}
	p := &sync.Pool{
		New: func() any {
			return newHookedJumpTable(base, stateOpsOnly)
		},
	}
	actual, _ := pools.LoadOrStore(base, p)
	return actual.(*sync.Pool)
}

// checkoutPreGasCheckpointHooks mượn bảng nhảy đã bọc từ sync.Pool và gắn vào evmEnv.
// Bên gọi BẮT BUỘC gọi hàm release() sau khi giao dịch kết thúc (defer).
func checkoutPreGasCheckpointHooks(evmEnv *vm.EVM, target preGasHookTarget) (func(), error) {
	return checkoutPreGasCheckpointHooksWithMode(evmEnv, target, false)
}

// checkoutPreGasCheckpointHooksWithMode chọn bảng đầy đủ (fixed) hoặc chỉ bọc opcode state (history/adaptive/calibrate).
func checkoutPreGasCheckpointHooksWithMode(evmEnv *vm.EVM, target preGasHookTarget, stateOpsOnly bool) (func(), error) {
	if evmEnv == nil || target == nil {
		return nil, errors.New("pre-gas checkpoint hooks require EVM and capture state")
	}
	base, err := gethJumpTable(evmEnv)
	if err != nil {
		return nil, err
	}
	pool := poolForBaseJumpTable(base, stateOpsOnly)
	h := pool.Get().(*hookedJumpTable)
	h.target = target
	if err := setGethJumpTable(evmEnv, &h.table); err != nil {
		h.target = nil
		pool.Put(h)
		return nil, err
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = setGethJumpTable(evmEnv, base)
		h.target = nil
		pool.Put(h)
	}, nil
}

// installPreGasCheckpointHooks gives this EVM a private jump table whose
// dynamic-gas functions capture the selected checkpoint before dynamic gas is
// evaluated. The shared geth fork tables remain untouched.
func installPreGasCheckpointHooks(evmEnv *vm.EVM, capture *checkpointCapture) error {
	if capture == nil {
		return errors.New("pre-gas checkpoint hooks require EVM and capture state")
	}
	table, err := installPreGasCheckpointHooksWithTarget(evmEnv, capture)
	if err != nil {
		return err
	}
	capture.jumpTable = table
	return nil
}

func installPreGasCheckpointHooksWithTarget(evmEnv *vm.EVM, target preGasHookTarget) (*gethJumpTableView, error) {
	if evmEnv == nil || target == nil {
		return nil, errors.New("pre-gas checkpoint hooks require EVM and capture state")
	}
	table, err := gethJumpTable(evmEnv)
	if err != nil {
		return nil, err
	}
	h := newHookedJumpTable(table, false)
	h.target = target
	if err := setGethJumpTable(evmEnv, &h.table); err != nil {
		return nil, err
	}
	return &h.table, nil
}

func restoreGethStack(words []uint256.Int) (*vm.Stack, error) {
	stack := new(vm.Stack)
	off, err := loadGethFieldOffsets()
	if err != nil {
		return nil, err
	}
	cloned := make([]uint256.Int, len(words))
	copy(cloned, words)
	*(*[]uint256.Int)(unsafe.Add(unsafe.Pointer(stack), off.stackData)) = cloned
	return stack, nil
}

func restoreGethMemory(data []byte, lastGasCost uint64) (*vm.Memory, error) {
	memory := vm.NewMemory()
	if len(data) > 0 {
		memory.Resize(uint64(len(data)))
		memory.Set(0, uint64(len(data)), data)
	}
	off, err := loadGethFieldOffsets()
	if err != nil {
		memory.Free()
		return nil, err
	}
	*(*uint64)(unsafe.Add(unsafe.Pointer(memory), off.memLastGasCost)) = lastGasCost
	return memory, nil
}

// resumeGethFrame tiếp tục từ checkpoint được chụp trước gas (giữ tương thích legacy P0-P3).
func resumeGethFrame(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter) (ret []byte, gasLeft uint64, runErr error) {
	return resumeGethFrameWithOptions(evmEnv, frame, machine, meter, false)
}

// resumeGethFrameStrict tiếp tục từ checkpoint Phase 2 ở frame gốc:
// - Tính lại constantGas + dynamicGas (và memory expansion từ lastGasCost) đúng thứ tự Geth v1.16.8.
// - Nếu suffix gặp opcode vượt phạm vi (CALL/STATICCALL/DELEGATECALL/CALLCODE/CREATE/CREATE2/SELFDESTRUCT)
//   thì trả ngay errResumeUnsupported để hủy bản tạm và chạy FullReplay.
func resumeGethFrameStrict(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter) (ret []byte, gasLeft uint64, runErr error) {
	return resumeGethFrameWithOptions(evmEnv, frame, machine, meter, true)
}

func isOutOfScopeSuffixOpcode(op vm.OpCode) bool {
	switch op {
	case vm.CALL, vm.CALLCODE, vm.DELEGATECALL, vm.STATICCALL,
		vm.CREATE, vm.CREATE2, vm.SELFDESTRUCT:
		return true
	default:
		return false
	}
}

func resumeGethFrameWithOptions(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter, disallowSubcalls bool) (ret []byte, gasLeft uint64, runErr error) {
	if !resumableGethVersionAvailable() {
		return nil, 0, errResumeUnsupported
	}
	if len(machine.CallFrames) != 1 || frame.Depth != 1 {
		return nil, 0, errResumeUnsupported
	}
	table, err := gethJumpTable(evmEnv)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: geth jump table: %v", errResumeUnsupported, err)
	}
	stack, err := restoreGethStack(machine.Stack)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: restore stack: %v", errResumeUnsupported, err)
	}
	memory, err := restoreGethMemory(machine.Memory, machine.MemoryLastGasCost)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: restore memory: %v", errResumeUnsupported, err)
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
		return nil, 0, fmt.Errorf("%w: set depth: %v", errResumeUnsupported, err)
	}
	defer func() {
		_ = setGethPrivateField(evmEnv, "depth", 0)
	}()
	if err := setGethPrivateField(evmEnv, "readOnly", frame.ReadOnly); err != nil {
		return nil, 0, fmt.Errorf("%w: set readOnly: %v", errResumeUnsupported, err)
	}
	if err := setGethPrivateField(evmEnv, "returnData", append([]byte(nil), machine.ReturnData...)); err != nil {
		return nil, 0, fmt.Errorf("%w: set returnData: %v", errResumeUnsupported, err)
	}
	if err := setGethPrivateField(evmEnv, "callGasTemp", machine.CallGasTemp); err != nil {
		return nil, 0, fmt.Errorf("%w: set callGasTemp: %v", errResumeUnsupported, err)
	}

	pc := machine.PC
	for {
		op := contract.GetOp(pc)
		if disallowSubcalls && isOutOfScopeSuffixOpcode(op) {
			return nil, 0, errResumeUnsupported
		}
		operation := table[byte(op)]
		if operation == nil || operation.execute == nil {
			return nil, 0, fmt.Errorf("%w: geth operation %s unavailable", errResumeUnsupported, op)
		}
		if meter != nil {
			// đếm mọi bước đi tới ranh giới trước gas, gồm cả opcode cuối gây lỗi
			meter.opcodes++
		}
		stackLen := len(stack.Data())
		if stackLen < operation.minStack {
			return nil, 0, fmt.Errorf("stack underflow (%d <=> %d)", stackLen, operation.minStack)
		}
		if stackLen > operation.maxStack {
			return nil, 0, fmt.Errorf("stack limit reached %d (%d)", stackLen, operation.maxStack)
		}

		gasBefore := contract.Gas
		cost := operation.constantGas
		if contract.Gas < cost {
			return nil, 0, vm.ErrOutOfGas
		}
		contract.Gas -= cost

		var memorySize uint64
		if operation.dynamicGas != nil {
			if operation.memorySize != nil {
				requested, overflow := operation.memorySize(stack)
				// khớp Geth toWordSize+SafeMul: tránh wrap uint64 tại [2^64-31, 2^64-1]
				if overflow || requested > math.MaxUint64-31 {
					return nil, 0, vm.ErrGasUintOverflow
				}
				memorySize, overflow = gethmath.SafeMul((requested+31)/32, 32)
				if overflow {
					return nil, 0, vm.ErrGasUintOverflow
				}
			}
			dynamicCost, gasErr := operation.dynamicGas(evmEnv, contract, stack, memory, memorySize)
			cost += dynamicCost
			if gasErr != nil {
				return nil, 0, fmt.Errorf("%w: %v", vm.ErrOutOfGas, gasErr)
			}
			if contract.Gas < dynamicCost {
				return nil, 0, vm.ErrOutOfGas
			}
			contract.Gas -= dynamicCost
		}
		if tracer := evmEnv.Config.Tracer; tracer != nil && tracer.OnOpcode != nil {
			retData, _ := gethPrivateBytes(evmEnv, "returnData")
			tracer.OnOpcode(pc, byte(op), gasBefore, cost, scope, retData, frame.Depth, nil)
		}
		if memorySize > 0 {
			memory.Resize(memorySize)
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
