package harness_test

import (
	"bytes"
	"encoding/csv"
	"math/big"
	"os"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
	workload := newCheckpointResumeBenchmarkWorkload(t, 64)
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

func checkpointBenchmarkCode() []byte {
	code := make([]byte, 0, 1120)
	for i := 0; i < 550; i++ {
		code = append(code, 0x5f, 0x50)
	}
	return append(code, 0x5f, 0x54, 0x60, 0x01, 0x01, 0x5f, 0x55, 0x00)
}

func newCheckpointResumeBenchmarkWorkload(t testing.TB, txCount int) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 0x5_500_005, txCount)
	contract := common.HexToAddress("0x0000000000000000000000000000000000c0ffee")
	workload := differentialWorkload{
		name:         "checkpoint-resume-independent-senders-hot-storage",
		transactions: make([]*types.Transaction, txCount), contract: contract,
		accounts:        []common.Address{contract},
		initialBalances: make(map[common.Address]*big.Int, txCount),
		initialCode:     map[common.Address][]byte{contract: checkpointBenchmarkCode()},
		storageSlots:    map[common.Address][]common.Hash{contract: {common.Hash{}}},
		env:             evm.BlockEnv{Height: 15, Time: 15, GasLimit: blockGasLimit, BaseFee: new(big.Int)},
	}
	for i, account := range pool {
		workload.accounts = append(workload.accounts, account.address)
		workload.initialBalances[account.address] = big.NewInt(1_000_000_000)
		workload.transactions[i] = signedTx(t, account.keyHex, 0, 200_000, &contract, new(big.Int), nil)
	}
	return workload
}

// TestCheckpointPolicyCSVExport chạy mọi chính sách trên cùng workload tất định.
// Đặt PLUTO_CHECKPOINT_CSV để lưu tệp CSV thay vì chỉ giữ trong bộ nhớ.
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
		values := make(map[int]uint64)
		for _, column := range []int{3, 4, 6, 7, 9, 10, 11, 12} {
			value, parseErr := strconv.ParseUint(record[column], 10, 64)
			if parseErr != nil {
				t.Fatalf("run %s column %s = %q, want integer", record[0], records[0][column], record[column])
			}
			values[column] = value
		}
		if values[3] == 0 || values[4] == 0 || values[9] == 0 || values[10] == 0 {
			t.Fatalf("run %s did not exercise replay: %v", record[0], record)
		}
		if record[0] == "P0" {
			if values[6] != 0 || values[7] != 0 || values[11] != 0 || values[12] == 0 {
				t.Fatalf("unexpected P0 checkpoint metrics: %v", record)
			}
		} else if values[6] == 0 || values[7] == 0 || values[11] == 0 {
			t.Fatalf("run %s did not create/reuse/resume checkpoints: %v", record[0], record)
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
	workload := newCheckpointResumeBenchmarkWorkload(b, 64)
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
