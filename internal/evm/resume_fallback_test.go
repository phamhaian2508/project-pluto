package evm

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/core/vm"
)

func TestResumeUnsupportedFallsBackToFullReplayAndMatchesSequential(t *testing.T) {
	code := productionResumeCode(550)
	sequential, sequentialStorage := runProductionResumeWorkload(t, "resume-fallback-seq", code, 4, 1, DefaultCheckpointConfig(), true)

	origResume := runResumedGethFrame
	calls := 0
	runResumedGethFrame = func(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter) ([]byte, uint64, error) {
		calls++
		return nil, 0, errResumeUnsupported
	}
	t.Cleanup(func() {
		runResumedGethFrame = origResume
	})

	result, storage := runProductionResumeWorkload(t, "resume-fallback-p2", code, 4, 4, CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 100}, false)
	if calls == 0 {
		t.Fatalf("expected resumeGethFrame to be invoked before falling back to FullReplay")
	}
	if !sameProductionResults(result.Results, sequential.Results) {
		t.Fatalf("fallback results differ from sequential:\ngot=%+v\nwant=%+v", result.Results, sequential.Results)
	}
	if result.GasUsed != sequential.GasUsed {
		t.Fatalf("fallback gasUsed=%d != sequential=%d", result.GasUsed, sequential.GasUsed)
	}
	if result.StateRoot != sequential.StateRoot {
		t.Fatalf("fallback stateRoot=%x != sequential=%x", result.StateRoot, sequential.StateRoot)
	}
	if storage != sequentialStorage {
		t.Fatalf("fallback storage=%s != sequential=%s", storage.Hex(), sequentialStorage.Hex())
	}
	for i, res := range result.Results {
		if res.Failed || res.Invalid {
			t.Fatalf("tx %d unexpectedly failed/invalid after errResumeUnsupported fallback: %+v", i, res)
		}
	}
	if result.CheckpointMetrics.Resumes != 0 {
		t.Fatalf("expected 0 successful resumes when errResumeUnsupported is returned, got %+v", result.CheckpointMetrics)
	}
	if result.CheckpointMetrics.FullFallbacks == 0 {
		t.Fatalf("expected FullFallbacks > 0 when errResumeUnsupported is returned, got %+v", result.CheckpointMetrics)
	}
}

func TestResumeOptimisticAttemptPropagatesUnsupportedError(t *testing.T) {
	origResume := runResumedGethFrame
	runResumedGethFrame = func(evmEnv *vm.EVM, frame CallFrameState, machine InterpreterState, meter *attemptReplayMeter) ([]byte, uint64, error) {
		return nil, 0, errResumeUnsupported
	}
	t.Cleanup(func() {
		runResumedGethFrame = origResume
	})

	if _, _, err := resumeOptimisticAttempt(nil, nil, nil, nil, BlockEnv{}, nil, 0, 2, nil, nil); !errors.Is(err, errResumeUnsupported) {
		t.Fatalf("expected errResumeUnsupported for nil checkpoint, got %v", err)
	}
}
