package gethfork

import (
	"bytes"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
)

func TestCopiedGethLoopCheckpointResumeMatchesUninterruptedAndGeth(t *testing.T) {
	const initialGas = uint64(1_000)
	base := map[common.Hash]common.Hash{}

	uninterrupted := newInterpreter(SampleContract, initialGas, base)
	if err := uninterrupted.run(); err != nil {
		t.Fatal(err)
	}

	paused := newInterpreter(SampleContract, initialGas, base)
	stopPC := uint64(16) // after SSTORE and MSTORE; stack contains RETURN size
	if err := paused.runUntil(&stopPC); !errors.Is(err, errPaused) {
		t.Fatalf("pause error = %v, want %v", err, errPaused)
	}
	checkpoint := paused.checkpoint()
	if checkpoint.PC != stopPC || checkpoint.CallDepth != 1 || len(checkpoint.Stack) != 1 || len(checkpoint.Memory) != 32 {
		t.Fatalf("incomplete checkpoint: pc=%d depth=%d stack=%d memory=%d", checkpoint.PC, checkpoint.CallDepth, len(checkpoint.Stack), len(checkpoint.Memory))
	}
	if got := checkpoint.Writes[common.BigToHash(big.NewInt(1))]; got != common.BigToHash(big.NewInt(3)) {
		t.Fatalf("checkpoint overlay slot 1 = %s, want 3", got)
	}

	resumed := restoreInterpreter(SampleContract, base, checkpoint)
	if err := resumed.run(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resumed.output, uninterrupted.output) {
		t.Fatalf("resumed output %x != uninterrupted output %x", resumed.output, uninterrupted.output)
	}
	if resumed.gas != uninterrupted.gas || resumed.pc != uninterrupted.pc || resumed.callDepth != uninterrupted.callDepth {
		t.Fatalf("resumed machine differs: gas %d/%d pc %d/%d depth %d/%d", resumed.gas, uninterrupted.gas, resumed.pc, uninterrupted.pc, resumed.callDepth, uninterrupted.callDepth)
	}
	if !bytes.Equal(resumed.returnData, uninterrupted.returnData) || !reflect.DeepEqual(resumed.state.writes, uninterrupted.state.writes) {
		t.Fatal("resumed returnData or state overlay differs from uninterrupted execution")
	}

	gethOutput, gethState, err := runtime.Execute(SampleContract, nil, &runtime.Config{GasLimit: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resumed.output, gethOutput) {
		t.Fatalf("resumed output %x != geth output %x", resumed.output, gethOutput)
	}
	contract := common.BytesToAddress([]byte("contract"))
	if got := gethState.GetState(contract, common.BigToHash(big.NewInt(1))); got != resumed.state.get(common.BigToHash(big.NewInt(1))) {
		t.Fatalf("resumed storage %s != geth storage %s", resumed.state.get(common.BigToHash(big.NewInt(1))), got)
	}
}

func TestCheckpointOwnsMutableData(t *testing.T) {
	machine := newInterpreter(SampleContract, 1_000, nil)
	stopPC := uint64(16)
	if err := machine.runUntil(&stopPC); !errors.Is(err, errPaused) {
		t.Fatalf("pause error = %v", err)
	}
	checkpoint := machine.checkpoint()
	machine.stack[0].SetUint64(99)
	machine.memory[31] = 99
	machine.returnData[0] = 99
	machine.state.writes[common.BigToHash(big.NewInt(1))] = common.BigToHash(big.NewInt(99))

	if checkpoint.Stack[0].Uint64() != 32 || checkpoint.Memory[31] != 3 || checkpoint.ReturnData[0] != 0xca {
		t.Fatal("checkpoint retained mutable interpreter references")
	}
	if checkpoint.Writes[common.BigToHash(big.NewInt(1))] != common.BigToHash(big.NewInt(3)) {
		t.Fatal("checkpoint retained mutable overlay references")
	}
}
