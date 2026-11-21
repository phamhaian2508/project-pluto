package main

import (
	"path/filepath"
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

func TestParseCheckpointSetting(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		interval uint64
		want     evm.CheckpointConfig
		wantErr  bool
	}{
		{name: "default", policy: "p0", interval: 100, want: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP0}},
		{name: "case insensitive", policy: "P1", interval: 100, want: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}},
		{name: "periodic", policy: "p2", interval: 50, want: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 50}},
		{name: "history", policy: "p3", interval: 100, want: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3, Interval: 100}},
		{name: "adaptive requires model", policy: "adaptive", interval: 100, wantErr: true},
		{name: "missing p2 interval", policy: "p2", interval: 0, wantErr: true},
		{name: "unknown", policy: "p5", interval: 100, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCheckpointSetting(test.policy, test.interval)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseCheckpointSetting(%q, %d) succeeded, want error", test.policy, test.interval)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCheckpointSetting(%q, %d): %v", test.policy, test.interval, err)
			}
			if got != test.want {
				t.Fatalf("config = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseCheckpointConfigWithPhase4NamesAndBudget(t *testing.T) {
	cfg, err := parseCheckpointConfig("off", 100, -1)
	if err != nil || cfg.Policy != evm.CheckpointPolicyOff || cfg.UseBudget {
		t.Fatalf("off config = %+v, err=%v", cfg, err)
	}
	cfg, err = parseCheckpointConfig("fixed", 25, 65536)
	if err != nil || cfg.Policy != evm.CheckpointPolicyFixed || cfg.Interval != 25 || !cfg.UseBudget || cfg.BudgetBytes != 65536 {
		t.Fatalf("fixed config = %+v, err=%v", cfg, err)
	}
	cfg, err = parseCheckpointConfig("history", 100, 0)
	if err != nil || cfg.Policy != evm.CheckpointPolicyHistory || cfg.Interval != 100 || !cfg.UseBudget || cfg.BudgetBytes != 0 {
		t.Fatalf("history budget=0 config = %+v, err=%v", cfg, err)
	}
	if _, err := parseCheckpointConfigWithModel("adaptive", 256, 3356, ""); err == nil {
		t.Fatalf("expected adaptive without model path to fail")
	}
	if got := resolveCheckpointFlag("fixed", "p0"); got != "fixed" {
		t.Fatalf("resolveCheckpointFlag = %q, want fixed", got)
	}
	if got := resolveCheckpointFlag("", "p2"); got != "p2" {
		t.Fatalf("resolveCheckpointFlag fallback = %q, want p2", got)
	}
	// khi --checkpoint-interval không được đặt (0), adaptive phải lấy CheckpointEvery từ model.json tự khép kín (không phụ thuộc results/)
	modelPath := filepath.Join(t.TempDir(), "model.json")
	if _, err := evm.SaveAdaptiveModel(modelPath, &evm.AdaptiveModel{
		FormatVersion:    evm.AdaptiveModelFormatVersion,
		CheckpointEvery:  256,
		TightBudgetBytes: 3356,
		WideBudgetBytes:  1 << 20,
		ScoreThreshold:   5.5,
		Entries: []evm.AdaptiveCalibrationEntry{{
			CodeHash:   "0x0000000000000000000000000000000000000000000000000000000000000001",
			Selector:   "0xde2afb9c",
			PC:         1173,
			SizeBucket: 2048,
			Samples:    10,
			GainNs:     29000,
			CaptureNs:  1500,
		}},
	}); err != nil {
		t.Fatalf("SaveAdaptiveModel: %v", err)
	}
	cfg, err = parseCheckpointConfigWithModel("adaptive", 0, -1, modelPath)
	if err != nil {
		t.Fatalf("parseCheckpointConfigWithModel(adaptive, interval=0): %v", err)
	}
	if cfg.AdaptiveModel == nil || cfg.Interval != cfg.AdaptiveModel.CheckpointEvery || cfg.Interval != 256 {
		t.Fatalf("adaptive interval = %d, want model.CheckpointEvery (256)", cfg.Interval)
	}
	if !cfg.UseBudget || cfg.BudgetBytes != cfg.AdaptiveModel.TightBudgetBytes || cfg.BudgetBytes != 3356 {
		t.Fatalf("adaptive budget = (%v, %d), want (true, 3356)", cfg.UseBudget, cfg.BudgetBytes)
	}
}
