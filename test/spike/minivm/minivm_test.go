package minivm

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

const initialGas = uint64(100_000)

var sampleAddress = common.BytesToAddress([]byte("contract"))

func TestCheckpointResumeMatchesUninterruptedAndGeth(t *testing.T) {
	base := map[common.Hash]common.Hash{}
	uninterrupted := New(gethfork.SampleContract, initialGas, base)
	if err := uninterrupted.Run(); err != nil {
		t.Fatal(err)
	}

	paused := New(gethfork.SampleContract, initialGas, base)
	stopPC := uint64(9) // Ngay sau SSTORE.
	if err := paused.RunUntil(&stopPC); !errors.Is(err, errPaused) {
		t.Fatalf("pause error = %v", err)
	}
	resumed := Restore(gethfork.SampleContract, base, paused.Checkpoint())
	if err := resumed.Run(); err != nil {
		t.Fatal(err)
	}

	gethOutput, gethGas, gethStorage := runGeth(t)
	for name, machine := range map[string]*Machine{"uninterrupted": uninterrupted, "resumed": resumed} {
		if !bytes.Equal(machine.Output(), gethOutput) {
			t.Fatalf("%s output = %x, geth = %x", name, machine.Output(), gethOutput)
		}
		if machine.Gas() != gethGas {
			t.Fatalf("%s gas left = %d, geth = %d", name, machine.Gas(), gethGas)
		}
		if got := machine.State(common.BigToHash(big.NewInt(1))); got != gethStorage {
			t.Fatalf("%s storage = %s, geth = %s", name, got, gethStorage)
		}
	}
}

func TestScopeIsExactlySampleContractOpcodes(t *testing.T) {
	if len(supported) != SupportedOpcodeCount || SupportedOpcodeCount != 7 {
		t.Fatalf("supported opcode count = %d/%d, want 7", len(supported), SupportedOpcodeCount)
	}
	machine := New([]byte{byte(vm.STOP)}, initialGas, nil)
	if err := machine.Run(); err == nil {
		t.Fatal("opcode outside the sample set was accepted")
	}
}

func runGeth(t *testing.T) ([]byte, uint64, common.Hash) {
	t.Helper()
	st, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	st.CreateAccount(sampleAddress)
	st.SetCode(sampleAddress, gethfork.SampleContract, tracing.CodeChangeUnspecified)
	output, gas, err := runtime.Call(sampleAddress, nil, &runtime.Config{State: st, GasLimit: initialGas})
	if err != nil {
		t.Fatal(err)
	}
	return output, gas, st.GetState(sampleAddress, common.BigToHash(big.NewInt(1)))
}
