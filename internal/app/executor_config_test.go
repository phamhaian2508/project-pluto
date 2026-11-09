package app

import (
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

func TestConfigureExecutorAcceptsKnownModesAndPositiveWorkers(t *testing.T) {
	application := &App{}
	if err := application.ConfigureExecutor("sequential", 1); err != nil {
		t.Fatalf("configure sequential executor: %v", err)
	}
	if err := application.ConfigureExecutor("parallel", 4); err != nil {
		t.Fatalf("configure parallel executor: %v", err)
	}
	if application.executorMode != "parallel" || application.workers != 4 {
		t.Fatalf("stored executor config = %q/%d, want parallel/4", application.executorMode, application.workers)
	}
}

func TestConfigureExecutorRejectsUnknownModeOrZeroWorkers(t *testing.T) {
	application := &App{}
	if err := application.ConfigureExecutor("concurrent", 4); err == nil {
		t.Fatal("unknown executor mode was accepted")
	}
	if err := application.ConfigureExecutor("parallel", 0); err == nil {
		t.Fatal("zero workers was accepted")
	}
}

func TestConfigureCheckpointsRequiresParallelExecutor(t *testing.T) {
	application := &App{}
	if err := application.ConfigureExecutor("sequential", 1); err != nil {
		t.Fatalf("configure sequential executor: %v", err)
	}
	if err := application.ConfigureCheckpoints(evm.DefaultCheckpointConfig()); err != nil {
		t.Fatalf("default P0 should preserve sequential behavior: %v", err)
	}
	if err := application.ConfigureCheckpoints(evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}); err == nil {
		t.Fatal("sequential executor accepted an active checkpoint policy")
	}

	if err := application.ConfigureExecutor("parallel", 2); err != nil {
		t.Fatalf("configure parallel executor: %v", err)
	}
	if err := application.ConfigureCheckpoints(evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 50}); err != nil {
		t.Fatalf("parallel executor rejected P2: %v", err)
	}
}
