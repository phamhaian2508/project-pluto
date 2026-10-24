package evm

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestExecutionCheckpointRestoresInterpreterAndTransactionState(t *testing.T) {
	base := testStateDB(t)
	state := newTxState(base, 4, 3, nil, nil)
	addr := common.HexToAddress("0x1234")
	other := common.HexToAddress("0x5678")
	slot := common.HexToHash("0x01")
	transientSlot := common.HexToHash("0x02")

	state.AddBalanceBig(addr, big.NewInt(25))
	state.SetNonce(addr, 2, 0)
	state.SetCode(addr, []byte{0x60, 0x01}, 0)
	state.SetState(addr, slot, common.HexToHash("0xaa"))
	state.SetTransientState(addr, transientSlot, common.HexToHash("0xbb"))
	state.AddRefund(17)
	state.AddLog(&types.Log{Address: addr, Topics: []common.Hash{slot}, Data: []byte{0x10}})
	state.AddAddressToAccessList(addr)
	state.AddSlotToAccessList(addr, slot)
	state.Snapshot()

	stackWord := *uint256.NewInt(7)
	frameValue := *uint256.NewInt(9)
	interpreter := InterpreterState{
		PC: 11, Stack: []uint256.Int{stackWord}, Memory: []byte{1, 2},
		ReturnData: []byte{3, 4}, Gas: 50_000,
		CallFrames: []CallFrameState{{
			PC: 5, Stack: []uint256.Int{stackWord}, Memory: []byte{5}, ReturnData: []byte{6}, Gas: 7_000,
			Depth: 1, CallType: 0xf1, Caller: other, Address: addr, CodeAddress: addr,
			Code: []byte{0x60, 0x00}, Input: []byte{0xca, 0xfe}, Value: frameValue, ReadOnly: true,
		}},
	}
	wantInterpreter := cloneInterpreterState(interpreter)
	wantReads, wantWrites := state.AccessSets()

	checkpoint, err := TakeExecutionCheckpoint(&interpreter, state)
	if err != nil {
		t.Fatal(err)
	}

	interpreter.PC = 99
	interpreter.Stack[0].SetUint64(99)
	interpreter.Memory[0] = 99
	interpreter.ReturnData[0] = 99
	interpreter.CallFrames[0].Code[0] = 99
	state.Incarnation = 8
	state.SetNonce(addr, 8, 0)
	state.SetCode(addr, []byte{0x60, 0x02}, 0)
	state.SetState(addr, slot, common.HexToHash("0xcc"))
	state.SetTransientState(addr, transientSlot, common.HexToHash("0xdd"))
	state.AddRefund(5)
	state.AddLog(&types.Log{Address: other, Data: []byte{0x20}})
	state.AddAddressToAccessList(other)
	state.GetBalance(other)
	state.SetNonce(other, 1, 0)
	state.SelfDestruct(other)

	if err := checkpoint.Restore(&interpreter, state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(interpreter, wantInterpreter) {
		t.Fatalf("interpreter restore mismatch:\n got  %+v\n want %+v", interpreter, wantInterpreter)
	}
	if state.TxIndex != 4 || state.Incarnation != 3 {
		t.Fatalf("identity after restore = tx %d incarnation %d", state.TxIndex, state.Incarnation)
	}
	reads, writes := state.AccessSets()
	if !reflect.DeepEqual(reads, wantReads) || !reflect.DeepEqual(writes, wantWrites) {
		t.Fatalf("access sets after restore differ: reads=%v writes=%v", reads, writes)
	}
	if got := state.GetBalance(addr).ToBig(); got.Cmp(big.NewInt(25)) != 0 {
		t.Fatalf("balance after restore = %s", got)
	}
	if got := state.GetNonce(addr); got != 2 {
		t.Fatalf("nonce after restore = %d", got)
	}
	if got := state.GetCode(addr); !reflect.DeepEqual(got, []byte{0x60, 0x01}) {
		t.Fatalf("code after restore = %x", got)
	}
	if got := state.GetState(addr, slot); got != common.HexToHash("0xaa") {
		t.Fatalf("storage after restore = %s", got)
	}
	if got := state.GetCommittedState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("origin storage after restore = %s, want zero", got)
	}
	if got := state.GetTransientState(addr, transientSlot); got != common.HexToHash("0xbb") {
		t.Fatalf("transient storage after restore = %s", got)
	}
	if got := state.GetRefund(); got != 17 {
		t.Fatalf("refund after restore = %d", got)
	}
	if logs := state.GetLogs(); len(logs) != 1 || logs[0].Data[0] != 0x10 || logs[0].Topics[0] != slot {
		t.Fatalf("logs after restore = %+v", logs)
	}
	if !state.AddressInAccessList(addr) {
		t.Fatal("warm address was not restored")
	}
	if _, warm := state.SlotInAccessList(addr, slot); !warm {
		t.Fatal("warm slot was not restored")
	}
	if state.AddressInAccessList(other) {
		t.Fatal("post-checkpoint warm address survived restore")
	}
	if state.HasSelfDestructed(other) {
		t.Fatal("post-checkpoint self-destruct survived restore")
	}
	if got := base.GetNonce(addr); got != 0 {
		t.Fatalf("RAM checkpoint changed base/DB-backed state: nonce=%d", got)
	}

	// Nhật ký và ảnh chụp sau khi khôi phục vẫn phải dùng được.
	state.SetState(addr, slot, common.HexToHash("0xee"))
	state.RevertToSnapshot(0)
	if got := state.GetState(addr, slot); got != common.HexToHash("0xaa") {
		t.Fatalf("restored journal revert = %s, want 0xaa", got)
	}
}

func TestParallelExecutorP0DoesNotCaptureCheckpoint(t *testing.T) {
	executor := NewParallelExecutor(params.AllDevChainProtocolChanges, big.NewInt(1), 1)
	if executor.Checkpoints != DefaultCheckpointConfig() {
		t.Fatalf("default checkpoint config = %+v", executor.Checkpoints)
	}
	selector, err := executor.NewCheckpointSelector()
	if err != nil {
		t.Fatal(err)
	}
	if _, selected := selector.BeforeOpcode(100); selected {
		t.Fatal("P0 selected an opcode checkpoint")
	}
	if _, selected := selector.BeforeStateRead(100, MVKey{}); selected {
		t.Fatal("P0 selected a state-read checkpoint")
	}
}
