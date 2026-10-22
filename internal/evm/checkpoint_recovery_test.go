package evm

import (
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type recoveryRunnerStub struct {
	resumeSupported bool
	resumeAttempt   *optimisticAttempt
	fullAttempt     *optimisticAttempt
	resumeOpcodes   uint64
	fullOpcodes     uint64
	resumeCalls     int
	fullCalls       int
	selected        *attemptCheckpoint
	onResume        func(*attemptCheckpoint) error
}

func (r *recoveryRunnerStub) Resume(checkpoint *attemptCheckpoint, _ uint64) (*optimisticAttempt, replayWork, error) {
	r.resumeCalls++
	r.selected = checkpoint
	if !r.resumeSupported {
		return nil, replayWork{}, errResumeUnsupported
	}
	if r.onResume != nil {
		if err := r.onResume(checkpoint); err != nil {
			return nil, replayWork{}, err
		}
	}
	return r.resumeAttempt, replayWork{opcodes: r.resumeOpcodes, gas: r.resumeAttempt.result.GasUsed}, nil
}

func (r *recoveryRunnerStub) FullReplay(_ uint64) (*optimisticAttempt, replayWork, error) {
	r.fullCalls++
	work := replayWork{opcodes: r.fullOpcodes}
	if r.fullAttempt != nil {
		work.gas = r.fullAttempt.result.GasUsed
	}
	return r.fullAttempt, work, nil
}

func TestRecoveryUsesNearestValidCheckpointAndReplaysFewerOpcodesThanP0(t *testing.T) {
	versions := NewMVMemory()
	checkpointState := newTxState(testStateDB(t), 7, 1, nil, nil)
	checkpointMachine := InterpreterState{PC: 60, Gas: 40_000, Memory: []byte{0xaa}}
	execution, err := TakeExecutionCheckpoint(&checkpointMachine, checkpointState)
	if err != nil {
		t.Fatal(err)
	}
	checkpointMachine.PC = 100
	checkpointMachine.Memory[0] = 0xff
	keys := []MVKey{
		{Address: common.HexToAddress("0x01"), Kind: MVStorage, Slot: common.HexToHash("0x01")},
		{Address: common.HexToAddress("0x01"), Kind: MVStorage, Slot: common.HexToHash("0x02")},
		{Address: common.HexToAddress("0x01"), Kind: MVStorage, Slot: common.HexToHash("0x03")},
	}
	failed := &optimisticAttempt{
		state:       &TxState{ReadSet: append([]MVKey(nil), keys...)},
		readVersion: map[MVKey]uint64{keys[0]: 0, keys[1]: 0, keys[2]: 0},
		checkpoints: []attemptCheckpoint{
			{beforeRead: 0, opcodePosition: 0, execution: &ExecutionCheckpoint{}, readVersions: map[MVKey]uint64{}},
			{beforeRead: 2, opcodePosition: 60, execution: execution, readVersions: map[MVKey]uint64{keys[0]: 0, keys[1]: 0}},
		},
	}
	versions.Commit([]MVKey{keys[2]})
	resumed := &optimisticAttempt{
		state:       &TxState{ReadSet: append([]MVKey(nil), keys...)},
		readVersion: map[MVKey]uint64{keys[0]: 0, keys[1]: 0, keys[2]: 1},
	}
	runner := &recoveryRunnerStub{
		resumeSupported: true, resumeAttempt: resumed, resumeOpcodes: 40,
		fullAttempt: resumed, fullOpcodes: 100,
		onResume: func(checkpoint *attemptCheckpoint) error {
			if err := checkpoint.execution.Restore(&checkpointMachine, checkpointState); err != nil {
				return err
			}
			if checkpointMachine.PC != 60 || len(checkpointMachine.Memory) != 1 || checkpointMachine.Memory[0] != 0xaa {
				t.Fatalf("resume did not restore machine: %+v", checkpointMachine)
			}
			return nil
		},
	}

	failedRead := firstInvalidRead(failed, versions)
	if failedRead != 2 {
		t.Fatalf("failed read = %d, want 2", failedRead)
	}
	outcome, err := recoverOptimisticAttempt(versions, failed, failedRead, 2, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.usedCheckpoint || runner.selected == nil || runner.selected.beforeRead != 2 {
		t.Fatalf("selected checkpoint = %+v, used=%v", runner.selected, outcome.usedCheckpoint)
	}
	if outcome.replayedOpcodes != 40 || outcome.replayedOpcodes >= runner.fullOpcodes {
		t.Fatalf("replayed opcodes = %d, P0 = %d", outcome.replayedOpcodes, runner.fullOpcodes)
	}
	if outcome.resumes != 1 || outcome.fullFallbacks != 0 || !reflect.DeepEqual(outcome.reusedPositions, []uint64{60}) {
		t.Fatalf("resume metrics = resumes:%d fallback:%d positions:%v", outcome.resumes, outcome.fullFallbacks, outcome.reusedPositions)
	}
	if runner.fullCalls != 0 || runner.resumeCalls != 1 {
		t.Fatalf("resume/full calls = %d/%d", runner.resumeCalls, runner.fullCalls)
	}
}

func TestNoValidCheckpointFallsBackToP0(t *testing.T) {
	versions := NewMVMemory()
	key := MVKey{Address: common.HexToAddress("0x01"), Kind: MVNonce}
	versions.Commit([]MVKey{key})
	failed := &optimisticAttempt{
		state: &TxState{ReadSet: []MVKey{key}}, readVersion: map[MVKey]uint64{key: 0},
		checkpoints: []attemptCheckpoint{{
			beforeRead: 0, execution: &ExecutionCheckpoint{}, readVersions: map[MVKey]uint64{key: 0},
		}},
	}
	valid := &optimisticAttempt{state: &TxState{ReadSet: []MVKey{key}}, readVersion: map[MVKey]uint64{key: 1}}
	runner := &recoveryRunnerStub{resumeSupported: true, resumeAttempt: valid, fullAttempt: valid, fullOpcodes: 100}
	outcome, err := recoverOptimisticAttempt(versions, failed, 0, 2, runner)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.usedCheckpoint || runner.resumeCalls != 0 || runner.fullCalls != 1 {
		t.Fatalf("invalid checkpoint did not fall back: resume/full=%d/%d", runner.resumeCalls, runner.fullCalls)
	}
	if outcome.fullFallbacks != 1 || outcome.resumes != 0 {
		t.Fatalf("fallback metrics = fallback:%d resume:%d", outcome.fullFallbacks, outcome.resumes)
	}
}

func TestUnsupportedResumeFallsBackWithoutChangingEVMOutcome(t *testing.T) {
	key := MVKey{Address: common.HexToAddress("0x01"), Kind: MVStorage}
	checkpoint := attemptCheckpoint{beforeRead: 0, execution: &ExecutionCheckpoint{}, readVersions: map[MVKey]uint64{}}
	tests := []struct {
		name   string
		result TxResult
	}{
		{name: "nested CALL", result: TxResult{GasUsed: 31_000, ReturnData: []byte{0x01}}},
		{name: "revert", result: TxResult{GasUsed: 25_000, Failed: true, ErrMsg: "execution reverted", ReturnData: []byte{0xde, 0xad}}},
		{name: "out-of-gas", result: TxResult{GasUsed: 50_000, Failed: true, ErrMsg: "out of gas"}},
		{name: "logs and gas", result: TxResult{GasUsed: 27_345, Logs: []*types.Log{{Address: common.HexToAddress("0x02"), Topics: []common.Hash{common.HexToHash("0xaa")}, Data: []byte{0xbb}}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versions := NewMVMemory()
			failed := &optimisticAttempt{
				state: &TxState{ReadSet: []MVKey{key}}, readVersion: map[MVKey]uint64{key: 0},
				checkpoints: []attemptCheckpoint{checkpoint},
			}
			versions.Commit([]MVKey{key})
			full := &optimisticAttempt{result: tt.result, readVersion: map[MVKey]uint64{key: 1}}
			runner := &recoveryRunnerStub{fullAttempt: full, fullOpcodes: 100}
			outcome, err := recoverOptimisticAttempt(versions, failed, 0, 2, runner)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.usedCheckpoint || runner.resumeCalls != 1 || runner.fullCalls != 1 {
				t.Fatalf("fallback calls resume/full=%d/%d used=%v", runner.resumeCalls, runner.fullCalls, outcome.usedCheckpoint)
			}
			if !reflect.DeepEqual(outcome.attempt.result, tt.result) {
				t.Fatalf("fallback result = %+v, want %+v", outcome.attempt.result, tt.result)
			}
		})
	}
}

func TestRecoveryNeverCommitsAbortedDelta(t *testing.T) {
	base := testStateDB(t)
	addr := common.HexToAddress("0x1234")
	makeAttempt := func(nonce uint64) *optimisticAttempt {
		state := newTxState(base, 0, nonce, nil, nil)
		state.SetNonce(addr, nonce, 0)
		delta, err := state.PendingWrites()
		if err != nil {
			t.Fatal(err)
		}
		return &optimisticAttempt{state: state, delta: delta, readVersion: map[MVKey]uint64{}}
	}
	failed, accepted := makeAttempt(99), makeAttempt(2)
	runner := &recoveryRunnerStub{fullAttempt: accepted}
	outcome, err := recoverOptimisticAttempt(NewMVMemory(), failed, -1, 2, runner)
	if err != nil {
		t.Fatal(err)
	}
	if got := base.GetNonce(addr); got != 0 {
		t.Fatalf("recovery committed state before ordered resolve: nonce=%d", got)
	}
	if err := base.ApplyPendingWrites(outcome.attempt.delta); err != nil {
		t.Fatal(err)
	}
	if got := base.GetNonce(addr); got != 2 {
		t.Fatalf("resolved nonce = %d, want accepted value 2", got)
	}
}

func TestRecoveryStopsAtRetryLimit(t *testing.T) {
	versions := NewMVMemory()
	key := MVKey{Address: common.HexToAddress("0x01"), Kind: MVNonce}
	versions.Commit([]MVKey{key})
	stale := &optimisticAttempt{state: &TxState{ReadSet: []MVKey{key}}, readVersion: map[MVKey]uint64{key: 0}}
	runner := &recoveryRunnerStub{fullAttempt: stale}
	_, err := recoverOptimisticAttempt(versions, stale, 0, 2, runner)
	if err == nil {
		t.Fatal("retry limit accepted a permanently stale attempt")
	}
	if runner.fullCalls != maxCheckpointRetries {
		t.Fatalf("full replay calls = %d, want %d", runner.fullCalls, maxCheckpointRetries)
	}
}
