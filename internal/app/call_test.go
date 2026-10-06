package app

import (
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/evm"
)

func TestCallAndEstimateGasDoNotChangeState(t *testing.T) {
	app, from, _ := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000ca11")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	// Store 1 in slot 0, then return the 32-byte value 1.
	state.SetCode(contract, common.FromHex("0x6001600055600160005260206000f3"), 0)
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}

	beforeDB := snapshotDB(t, app)
	beforeHash := append([]byte(nil), app.appHash...)
	beforeHeight := app.currentHeight
	beforeState := evm.NewPebbleStateDB(app.db)
	beforeBalance := beforeState.GetBalance(from).ToBig()
	beforeNonce := beforeState.GetNonce(from)

	args := CallArgs{From: from, To: &contract, Gas: 100000}
	result, err := app.Call(args)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil {
		t.Fatalf("call failed: %v", result.Err)
	}
	if len(result.ReturnData) != common.HashLength || common.BytesToHash(result.ReturnData) != common.BigToHash(common.Big1) {
		t.Fatalf("return data = %x, want 1", result.ReturnData)
	}
	if result.GasUsed == 0 {
		t.Fatal("call used no gas")
	}

	estimated, err := app.EstimateGas(CallArgs{From: from, To: &contract})
	if err != nil {
		t.Fatal(err)
	}
	if estimated < 21000 || estimated > args.Gas {
		t.Fatalf("estimated gas = %d, want [21000, %d]", estimated, args.Gas)
	}
	result, err = app.Call(CallArgs{From: from, To: &contract, Gas: estimated})
	if err != nil || result.Err != nil {
		t.Fatalf("call with estimated gas failed: result=%+v err=%v", result, err)
	}
	result, err = app.Call(CallArgs{From: from, To: &contract, Gas: estimated - 1})
	if err == nil && result.Err == nil {
		t.Fatalf("call below estimated gas (%d) unexpectedly succeeded", estimated-1)
	}

	afterState := evm.NewPebbleStateDB(app.db)
	if got := afterState.GetState(contract, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("read-only call persisted storage: %s", got)
	}
	if got := afterState.GetNonce(from); got != beforeNonce {
		t.Fatalf("read-only call changed nonce: got %d, want %d", got, beforeNonce)
	}
	if got := afterState.GetBalance(from).ToBig(); got.Cmp(beforeBalance) != 0 {
		t.Fatalf("read-only call changed balance: got %s, want %s", got, beforeBalance)
	}
	if app.currentHeight != beforeHeight || !reflect.DeepEqual(app.appHash, beforeHash) {
		t.Fatalf("read-only call changed app metadata: height=%d appHash=%x", app.currentHeight, app.appHash)
	}
	if afterDB := snapshotDB(t, app); !reflect.DeepEqual(afterDB, beforeDB) {
		t.Fatal("Call or EstimateGas wrote to PebbleDB")
	}
}

func TestEstimateGasRejectsRevertingCall(t *testing.T) {
	app, from, _ := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000bad0")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	state.SetCode(contract, common.FromHex("0x60006000fd"), 0)
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}

	beforeDB := snapshotDB(t, app)
	result, err := app.Call(CallArgs{From: from, To: &contract, Gas: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if result.Err == nil {
		t.Fatal("reverting call unexpectedly succeeded")
	}
	if _, err := app.EstimateGas(CallArgs{From: from, To: &contract}); err == nil {
		t.Fatal("EstimateGas unexpectedly accepted reverting call")
	}
	if afterDB := snapshotDB(t, app); !reflect.DeepEqual(afterDB, beforeDB) {
		t.Fatal("reverting Call or EstimateGas wrote to PebbleDB")
	}
}

func snapshotDB(t *testing.T, app *App) map[string]string {
	t.Helper()
	iterator, err := app.db.Iterator(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer iterator.Close()

	snapshot := make(map[string]string)
	for ; iterator.Valid(); iterator.Next() {
		snapshot[string(iterator.Key())] = string(iterator.Value())
	}
	if err := iterator.Error(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
