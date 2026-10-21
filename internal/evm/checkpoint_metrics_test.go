package evm

import (
	"bytes"
	"encoding/csv"
	"reflect"
	"testing"
)

func TestCheckpointCSVHasStableHeaderAndOneRowPerRun(t *testing.T) {
	rows := []CheckpointCSVRow{
		{Run: "p0-run", Policy: CheckpointPolicyP0},
		{
			Run: "p2-100-run", Policy: CheckpointPolicyP2, Interval: 100,
			Metrics: CheckpointMetrics{
				ReplayedOpcodes: 40, ReplayedGas: 21_000, ReplayNanoseconds: 900,
				CheckpointsCreated: 3, CheckpointsReused: 2, CheckpointMemoryBytes: 4096,
				Aborts: 1, Retries: 1, Resumes: 1, ReusedPositions: []uint64{100, 200},
			},
		},
	}
	var output bytes.Buffer
	if err := WriteCheckpointCSV(&output, rows); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&output).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(rows)+1 {
		t.Fatalf("CSV records = %d, want header + %d runs", len(records), len(rows))
	}
	if !reflect.DeepEqual(records[0], CheckpointCSVHeader) {
		t.Fatalf("CSV header = %v, want %v", records[0], CheckpointCSVHeader)
	}
	if got := records[2]; got[0] != "p2-100-run" || got[1] != "P2" || got[2] != "100" || got[13] != "100;200" {
		t.Fatalf("unexpected P2 CSV row: %v", got)
	}
	for rowIndex, record := range records {
		if len(record) != len(CheckpointCSVHeader) {
			t.Fatalf("CSV row %d has %d columns, want %d", rowIndex, len(record), len(CheckpointCSVHeader))
		}
	}
}

func TestCheckpointMemoryEstimateTracksOwnedPayload(t *testing.T) {
	state := newTxState(testStateDB(t), 1, 2, nil, nil)
	machine := &InterpreterState{Memory: make([]byte, 128), ReturnData: make([]byte, 16)}
	checkpoint, err := TakeExecutionCheckpoint(machine, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkpoint.EstimatedMemoryBytes(); got < 144 {
		t.Fatalf("estimated checkpoint memory = %d, want at least interpreter payload", got)
	}
}
