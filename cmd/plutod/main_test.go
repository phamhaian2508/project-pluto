package main

import (
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
		{name: "adaptive", policy: "p3", interval: 100, want: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3}},
		{name: "missing p2 interval", policy: "p2", interval: 0, wantErr: true},
		{name: "unknown", policy: "p4", interval: 100, wantErr: true},
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
