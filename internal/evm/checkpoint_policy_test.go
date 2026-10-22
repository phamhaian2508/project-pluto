package evm

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

func TestCheckpointPoliciesSelectExpectedPositions(t *testing.T) {
	keyA := MVKey{Address: common.HexToAddress("0x01"), Kind: MVStorage, Slot: common.HexToHash("0xa")}
	keyB := MVKey{Address: common.HexToAddress("0x02"), Kind: MVBalance}

	tests := []struct {
		name   string
		config CheckpointConfig
		run    func(*CheckpointSelector) []uint64
		want   []uint64
	}{
		{
			name:   "P0",
			config: CheckpointConfig{Policy: CheckpointPolicyP0},
			run: func(selector *CheckpointSelector) []uint64 {
				return collectOpcodePositions(selector, 1_100)
			},
		},
		{
			name:   "P1 before every state read",
			config: CheckpointConfig{Policy: CheckpointPolicyP1},
			run: func(selector *CheckpointSelector) []uint64 {
				var positions []uint64
				for _, event := range []struct {
					position uint64
					key      MVKey
				}{{3, keyA}, {8, keyB}, {8, keyA}, {13, keyB}} {
					if decision, selected := selector.BeforeStateRead(event.position, event.key); selected {
						if decision.Reason != CheckpointBeforeStateRead || decision.ReadKey != event.key {
							t.Fatalf("P1 decision = %+v", decision)
						}
						positions = append(positions, decision.OpcodePosition)
					}
				}
				return positions
			},
			want: []uint64{3, 8, 8, 13},
		},
		{
			name:   "P2 interval 100",
			config: CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 100},
			run: func(selector *CheckpointSelector) []uint64 {
				return collectOpcodePositions(selector, 250)
			},
			want: []uint64{100, 200},
		},
		{
			name:   "P2 interval 1000",
			config: CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 1_000},
			run: func(selector *CheckpointSelector) []uint64 {
				return collectOpcodePositions(selector, 2_100)
			},
			want: []uint64{1_000, 2_000},
		},
		{
			name:   "P3 before conflict-hot reads",
			config: CheckpointConfig{Policy: CheckpointPolicyP3},
			run: func(selector *CheckpointSelector) []uint64 {
				var positions []uint64
				if _, selected := selector.BeforeStateRead(4, keyA); selected {
					t.Fatal("P3 selected a cold key")
				}
				selector.MarkReadConflict(keyA)
				if _, selected := selector.BeforeStateRead(5, keyB); selected {
					t.Fatal("P3 selected an unrelated cold key")
				}
				if decision, selected := selector.BeforeStateRead(6, keyA); selected {
					if decision.Reason != CheckpointBeforeHotRead || decision.ReadKey != keyA {
						t.Fatalf("P3 decision = %+v", decision)
					}
					positions = append(positions, decision.OpcodePosition)
				}
				selector.MarkReadConflict(keyB)
				if decision, selected := selector.BeforeStateRead(7, keyB); selected {
					positions = append(positions, decision.OpcodePosition)
				}
				return positions
			},
			want: []uint64{6, 7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := NewParallelExecutor(params.AllDevChainProtocolChanges, big.NewInt(1), 1)
			if err := executor.ConfigureCheckpoints(tt.config); err != nil {
				t.Fatal(err)
			}
			first, err := executor.NewCheckpointSelector()
			if err != nil {
				t.Fatal(err)
			}
			second, err := executor.NewCheckpointSelector()
			if err != nil {
				t.Fatal(err)
			}
			got := tt.run(first)
			again := tt.run(second)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("positions = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(again, got) {
				t.Fatalf("same event stream was nondeterministic: %v then %v", got, again)
			}
		})
	}
}

func TestCheckpointConfigRejectsInvalidP2IntervalAndPolicy(t *testing.T) {
	executor := NewParallelExecutor(params.AllDevChainProtocolChanges, big.NewInt(1), 1)
	for _, config := range []CheckpointConfig{
		{Policy: CheckpointPolicyP2},
		{Policy: CheckpointPolicy(99), Interval: 100},
	} {
		if err := executor.ConfigureCheckpoints(config); err == nil {
			t.Fatalf("accepted invalid checkpoint config %+v", config)
		}
	}
	if executor.Checkpoints != DefaultCheckpointConfig() {
		t.Fatalf("invalid config changed defaults: %+v", executor.Checkpoints)
	}
}

func collectOpcodePositions(selector *CheckpointSelector, through uint64) []uint64 {
	var positions []uint64
	for position := uint64(0); position <= through; position++ {
		if decision, selected := selector.BeforeOpcode(position); selected {
			positions = append(positions, decision.OpcodePosition)
		}
	}
	return positions
}
