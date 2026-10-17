package app

import "testing"

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
