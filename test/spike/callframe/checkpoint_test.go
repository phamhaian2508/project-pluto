package callframe

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/phamhaian2508/project-pluto/test/spike/gethfork"
)

var (
	parentAddress = common.HexToAddress("0x1000")
	childAddress  = common.HexToAddress("0x2000")
	slotOne       = common.BigToHash(big.NewInt(1))
)

func newState(t *testing.T, childCode []byte, childGas byte) *state.StateDB {
	t.Helper()
	st, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	st.CreateAccount(parentAddress)
	st.CreateAccount(childAddress)
	st.SetCode(parentAddress, forwardingContract(childAddress, childGas), tracing.CodeChangeUnspecified)
	st.SetCode(childAddress, childCode, tracing.CodeChangeUnspecified)
	return st
}

// forwardingContract gọi CALL, bỏ qua cờ thành công rồi trả về vùng đầu ra CALL
// dài 32 byte. childGas=0 nghĩa là chuyển tiếp toàn bộ gas; nếu không, dùng gas PUSH1.
func forwardingContract(child common.Address, childGas byte) []byte {
	code := []byte{
		byte(vm.PUSH1), 0x20, byte(vm.PUSH0), byte(vm.PUSH0), byte(vm.PUSH0), byte(vm.PUSH0),
		byte(vm.PUSH20),
	}
	code = append(code, child.Bytes()...)
	if childGas == 0 {
		code = append(code, byte(vm.GAS))
	} else {
		code = append(code, byte(vm.PUSH1), childGas)
	}
	return append(code,
		byte(vm.CALL), byte(vm.POP),
		byte(vm.PUSH1), 0x20, byte(vm.PUSH0), byte(vm.RETURN),
	)
}

func run(t *testing.T, st *state.StateDB, to common.Address, input []byte, gas uint64) ([]byte, uint64, *recorder, error) {
	t.Helper()
	rec := new(recorder)
	out, left, err := runtime.Call(to, input, &runtime.Config{
		State: st, GasLimit: gas, EVMConfig: vm.Config{Tracer: rec.hooks()},
	})
	return out, left, rec, err
}

func nestedExit(t *testing.T, rec *recorder) callExit {
	t.Helper()
	for _, exit := range rec.exited {
		if exit.depth == 1 {
			return exit
		}
	}
	t.Fatal("nested CALL exit was not observed")
	return callExit{}
}

func TestCallBoundaryCheckpointRestoresAndReplaysWholeNestedFrame(t *testing.T) {
	st := newState(t, gethfork.SampleContract, 0)
	checkpoint := takeBoundaryCheckpoint(st, parentAddress, nil, 100_000)

	firstOutput, firstGas, firstTrace, err := run(t, st, checkpoint.to, checkpoint.input, checkpoint.gas)
	if err != nil {
		t.Fatal(err)
	}
	firstStorage := st.GetState(childAddress, slotOne)
	firstNested := nestedExit(t, firstTrace)

	checkpoint.restore(st)
	secondOutput, secondGas, secondTrace, err := run(t, st, checkpoint.to, checkpoint.input, checkpoint.gas)
	if err != nil {
		t.Fatal(err)
	}
	secondNested := nestedExit(t, secondTrace)

	if !bytes.Equal(firstOutput, secondOutput) || firstGas != secondGas {
		t.Fatalf("replayed call differs: output %x/%x gas %d/%d", firstOutput, secondOutput, firstGas, secondGas)
	}
	if got := st.GetState(childAddress, slotOne); got != firstStorage || got != common.BigToHash(big.NewInt(3)) {
		t.Fatalf("replayed storage = %s, first = %s, want 3", got, firstStorage)
	}
	if !bytes.Equal(firstNested.output, secondNested.output) || firstNested.gasUsed != secondNested.gasUsed {
		t.Fatal("nested CALL trace differs after full-frame replay")
	}
}

func TestNestedCallOutcomesAreObservableButNotResumable(t *testing.T) {
	logContract := append([]byte(nil), gethfork.SampleContract[:14]...)
	logContract = append(logContract,
		byte(vm.PUSH1), 0xaa, byte(vm.PUSH1), 0x20, byte(vm.PUSH0), byte(vm.LOG1),
		byte(vm.PUSH1), 0x20, byte(vm.PUSH0), byte(vm.RETURN),
	)

	tests := []struct {
		name       string
		code       []byte
		childGas   byte
		wantOutput byte
		wantErr    bool
		wantRevert bool
		wantLogs   int
	}{
		{name: "returnData", code: gethfork.SampleContract, wantOutput: 3},
		{name: "revert", code: []byte{byte(vm.PUSH0), byte(vm.PUSH0), byte(vm.REVERT)}, wantErr: true, wantRevert: true},
		{name: "out-of-gas", code: gethfork.SampleContract, childGas: 5, wantErr: true, wantRevert: true},
		{name: "logs", code: logContract, wantOutput: 3, wantLogs: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newState(t, tt.code, tt.childGas)
			checkpoint := takeBoundaryCheckpoint(st, parentAddress, nil, 100_000)
			firstOutput, firstGas, rec, topErr := run(t, st, checkpoint.to, checkpoint.input, checkpoint.gas)
			if topErr != nil {
				t.Fatalf("parent frame must handle child failure: %v", topErr)
			}
			exit := nestedExit(t, rec)
			if (exit.err != nil) != tt.wantErr || exit.reverted != tt.wantRevert {
				t.Fatalf("nested outcome: err=%v reverted=%v", exit.err, exit.reverted)
			}
			if tt.wantOutput != 0 && (len(exit.output) != 32 || exit.output[31] != tt.wantOutput) {
				t.Fatalf("nested returnData = %x, want final byte %d", exit.output, tt.wantOutput)
			}
			if got := len(st.Logs()); got != tt.wantLogs {
				t.Fatalf("logs = %d, want %d", got, tt.wantLogs)
			}

			checkpoint.restore(st)
			if got := len(st.Logs()); got != 0 {
				t.Fatalf("logs after restore = %d, want 0", got)
			}
			secondOutput, secondGas, secondTrace, topErr := run(t, st, checkpoint.to, checkpoint.input, checkpoint.gas)
			if topErr != nil {
				t.Fatalf("replayed parent frame: %v", topErr)
			}
			secondExit := nestedExit(t, secondTrace)
			if !bytes.Equal(firstOutput, secondOutput) || firstGas != secondGas ||
				!bytes.Equal(exit.output, secondExit.output) || exit.gasUsed != secondExit.gasUsed ||
				(exit.err != nil) != (secondExit.err != nil) || exit.reverted != secondExit.reverted {
				t.Fatal("whole-frame replay did not reproduce the nested outcome")
			}
			if got := len(st.Logs()); got != tt.wantLogs {
				t.Fatalf("logs after replay = %d, want %d", got, tt.wantLogs)
			}
			if err := nestedCheckpoint(exit); !errors.Is(err, errNestedResumeUnsupported) {
				t.Fatalf("nested checkpoint error = %v", err)
			}
		})
	}
}
