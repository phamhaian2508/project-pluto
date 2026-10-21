package harness_test

import (
	"bytes"
	"encoding/csv"
	"os"
	"strconv"
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

type checkpointMetricPolicyCase struct {
	name   string
	config evm.CheckpointConfig
}

func checkpointMetricPolicyCases() []checkpointMetricPolicyCase {
	return []checkpointMetricPolicyCase{
		{name: "P0", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP0}},
		{name: "P1", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}},
		{name: "P2-N100", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 100}},
		{name: "P2-N1000", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 1000}},
		{name: "P3", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3}},
	}
}

func runCheckpointMetricPolicy(t testing.TB, item checkpointMetricPolicyCase, workload differentialWorkload) evm.CheckpointMetrics {
	t.Helper()
	executor := evm.NewParallelExecutor(newChainConfig(), chainID, 4)
	if err := executor.ConfigureCheckpoints(item.config); err != nil {
		t.Fatalf("configure %s: %v", item.name, err)
	}
	return runPlutoDifferentialWorkload(t, executor, workload).checkpoint
}

func checkpointMetricRows(t testing.TB) []evm.CheckpointCSVRow {
	t.Helper()
	workload := newCounterContentionWorkload(t, 32)
	cases := checkpointMetricPolicyCases()
	rows := make([]evm.CheckpointCSVRow, 0, len(cases))
	for _, item := range cases {
		rows = append(rows, evm.CheckpointCSVRow{
			Run: item.name, Policy: item.config.Policy, Interval: item.config.Interval,
			Metrics: runCheckpointMetricPolicy(t, item, workload),
		})
	}
	return rows
}

// TestCheckpointPolicyCSVExport runs every policy against the same deterministic
// workload. Set PLUTO_CHECKPOINT_CSV to persist the otherwise in-memory CSV.
func TestCheckpointPolicyCSVExport(t *testing.T) {
	var output bytes.Buffer
	if err := evm.WriteCheckpointCSV(&output, checkpointMetricRows(t)); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(output.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("CSV records = %d, want one header and five policy runs", len(records))
	}
	wantRuns := []string{"P0", "P1", "P2-N100", "P2-N1000", "P3"}
	for index, record := range records[1:] {
		if record[0] != wantRuns[index] {
			t.Fatalf("CSV run %d = %q, want %q", index, record[0], wantRuns[index])
		}
		for _, column := range []int{3, 4, 9, 10, 12} {
			value, parseErr := strconv.ParseUint(record[column], 10, 64)
			if parseErr != nil || value == 0 {
				t.Fatalf("run %s column %s = %q, want positive integer", record[0], records[0][column], record[column])
			}
		}
	}
	if path := os.Getenv("PLUTO_CHECKPOINT_CSV"); path != "" {
		if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
			t.Fatalf("write checkpoint CSV: %v", err)
		}
	}
	t.Logf("checkpoint benchmark CSV:\n%s", output.String())
}

func BenchmarkCheckpointPoliciesCSV(b *testing.B) {
	workload := newCounterContentionWorkload(b, 32)
	for _, item := range checkpointMetricPolicyCases() {
		b.Run(item.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				metrics := runCheckpointMetricPolicy(b, item, workload)
				b.ReportMetric(float64(metrics.ReplayedOpcodes), "replay-op")
				b.ReportMetric(float64(metrics.ReplayedGas), "replay-gas")
				b.ReportMetric(float64(metrics.ReplayNanoseconds), "replay-ns")
				b.ReportMetric(float64(metrics.CheckpointsCreated), "checkpoints")
				b.ReportMetric(float64(metrics.CheckpointsReused), "reused")
				b.ReportMetric(float64(metrics.CheckpointMemoryBytes), "checkpoint-B")
				b.ReportMetric(float64(metrics.Aborts), "aborts")
				b.ReportMetric(float64(metrics.Retries), "retries")
				b.ReportMetric(float64(metrics.Resumes), "resumes")
				b.ReportMetric(float64(metrics.FullFallbacks), "fallbacks")
			}
		})
	}
}
