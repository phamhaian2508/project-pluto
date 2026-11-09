package harness_test

import (
	"fmt"
	"math/big"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const (
	checkpointReportTransactions = 64
	checkpointReportSamples      = 15
	checkpointReportWorkers      = 4
)

type checkpointWorkloadSample struct {
	duration time.Duration
	metrics  evm.CheckpointMetrics
}

type checkpointWorkloadSummary struct {
	duration time.Duration
	metrics  evm.CheckpointMetrics
}

// TestCheckpointPoliciesAcrossRepresentativeWorkloadsReport compares P0-P3
// on contention and no-contention contract workloads. Timing covers only
// ExecuteBlock; state setup and correctness comparison are outside the timer.
func TestCheckpointPoliciesAcrossRepresentativeWorkloadsReport(t *testing.T) {
	policies := []checkpointMetricPolicyCase{
		{name: "P0", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP0}},
		{name: "P1", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}},
		{name: "P2-N100", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 100}},
		{name: "P3", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3}},
	}
	counter := newCheckpointResumeBenchmarkWorkload(t, checkpointReportTransactions)
	counter.name = "counter-after-long-computation"
	workloads := []differentialWorkload{
		counter,
		newLimitedMintCheckpointWorkload(t, checkpointReportTransactions),
		newAMMCheckpointWorkload(t, checkpointReportTransactions),
		newShardedStorageWorkload(t, checkpointReportTransactions),
	}

	fmt.Fprintf(os.Stdout, "\nCHECKPOINT POLICY REPORT: %d transactions, %d workers, median of %d samples\n", checkpointReportTransactions, checkpointReportWorkers, checkpointReportSamples)
	fmt.Fprintln(os.Stdout, "Timing covers ExecuteBlock only. Every sample must match geth for results, gas, logs, balances, nonces, code and observed storage.")
	fmt.Fprintln(os.Stdout, "WORKLOAD                               POLICY   ms/block  vs P0    retries  replay-op  checkpoints  reused  resumes  fallbacks  checkpoint-KiB  RESULT")

	for _, workload := range workloads {
		reference := runGethDifferentialWorkload(t, workload)
		summaries := make(map[string]checkpointWorkloadSummary, len(policies))
		for _, policy := range policies {
			// One unreported warm-up reduces one-time runtime noise.
			_ = runCheckpointWorkloadSample(t, workload, policy, reference)
			samples := make([]checkpointWorkloadSample, 0, checkpointReportSamples)
			for sample := 0; sample < checkpointReportSamples; sample++ {
				samples = append(samples, runCheckpointWorkloadSample(t, workload, policy, reference))
			}
			summaries[policy.name] = summarizeCheckpointWorkloadSamples(samples)
		}

		baseline := summaries["P0"].duration
		for _, policy := range policies {
			summary := summaries[policy.name]
			ratio := float64(summary.duration) / float64(baseline)
			fmt.Fprintf(os.Stdout, "%-38s %-8s %8.3f  %6.2fx  %7d  %9d  %11d  %6d  %7d  %9d  %14.1f  PASS\n",
				workload.name, policy.name, float64(summary.duration)/float64(time.Millisecond), ratio,
				summary.metrics.Retries, summary.metrics.ReplayedOpcodes, summary.metrics.CheckpointsCreated,
				summary.metrics.CheckpointsReused, summary.metrics.Resumes, summary.metrics.FullFallbacks,
				float64(summary.metrics.CheckpointMemoryBytes)/1024,
			)
		}
	}
}

func runCheckpointWorkloadSample(t *testing.T, workload differentialWorkload, policy checkpointMetricPolicyCase, reference differentialObservation) checkpointWorkloadSample {
	t.Helper()
	db, err := store.NewPebbleDB("checkpoint-workload-report", t.TempDir())
	if err != nil {
		t.Fatalf("create %s/%s state: %v", workload.name, policy.name, err)
	}
	seed := evm.NewPebbleStateDB(db)
	for address, balance := range workloadBalances(workload) {
		seed.AddBalanceBig(address, balance)
	}
	for address, code := range workload.initialCode {
		seed.SetCode(address, code, tracing.CodeChangeUnspecified)
	}
	for address, slots := range workload.initialStorage {
		for slot, value := range slots {
			seed.SetState(address, slot, value)
		}
	}
	if err := seed.Commit(); err != nil {
		_ = db.Close()
		t.Fatalf("seed %s/%s state: %v", workload.name, policy.name, err)
	}

	executor := evm.NewParallelExecutor(newChainConfig(), chainID, checkpointReportWorkers)
	if err := executor.ConfigureCheckpoints(policy.config); err != nil {
		_ = db.Close()
		t.Fatalf("configure %s: %v", policy.name, err)
	}
	stateDB := evm.NewPebbleStateDB(db)
	started := time.Now()
	result, err := executor.ExecuteBlock(stateDB, workload.env, workload.transactions)
	duration := time.Since(started)
	if err != nil {
		_ = db.Close()
		t.Fatalf("execute %s/%s: %v", workload.name, policy.name, err)
	}
	observation := capturePlutoObservation(stateDB, result, workload)
	if mismatches := differentialMismatches(workload, policy.name, observation, "geth", reference); len(mismatches) > 0 {
		_ = db.Close()
		t.Fatalf("%s/%s differs from geth: %s", workload.name, policy.name, mismatches[0])
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s/%s state: %v", workload.name, policy.name, err)
	}
	return checkpointWorkloadSample{duration: duration, metrics: result.CheckpointMetrics}
}

func summarizeCheckpointWorkloadSamples(samples []checkpointWorkloadSample) checkpointWorkloadSummary {
	durations := make([]time.Duration, len(samples))
	for i := range samples {
		durations[i] = samples[i].duration
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	// Checkpoint counters are deterministic for a fixed workload and policy.
	return checkpointWorkloadSummary{duration: durations[len(durations)/2], metrics: samples[0].metrics}
}

func newLimitedMintCheckpointWorkload(t testing.TB, txCount int) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 0x1_1A17, txCount)
	contract := common.HexToAddress("0x0000000000000000000000000000000000001A17")
	workload := differentialWorkload{
		name: "limited-mint-shared-total-supply", transactions: make([]*types.Transaction, txCount),
		contract: contract, accounts: []common.Address{contract},
		initialBalances: make(map[common.Address]*big.Int, txCount),
		initialCode:     map[common.Address][]byte{contract: limitedMintRuntime(256)},
		storageSlots:    map[common.Address][]common.Hash{contract: {common.Hash{}}},
		env:             evm.BlockEnv{Height: 41, Time: 41, GasLimit: blockGasLimit, BaseFee: new(big.Int)},
	}
	for i, account := range pool {
		workload.accounts = append(workload.accounts, account.address)
		workload.initialBalances[account.address] = big.NewInt(1_000_000_000)
		workload.transactions[i] = signedTx(t, account.keyHex, 0, 200_000, &contract, new(big.Int), nil)
	}
	return workload
}

// limitedMintRuntime performs deterministic eligibility work, then increments
// shared totalMinted in slot 0 and rejects values above the fixed supply of 64.
func limitedMintRuntime(iterations uint16) []byte {
	return []byte{
		0x5f, 0x5f, 0x52,
		0x61, byte(iterations >> 8), byte(iterations),
		0x5b,
		0x60, 0x20, 0x5f, 0x20, 0x50,
		0x60, 0x01, 0x90, 0x03,
		0x80, 0x60, 0x06, 0x57,
		0x5f, 0x54, 0x60, 0x01, 0x01,
		0x80, 0x60, 0x40, 0x90, 0x11, 0x60, 0x24, 0x57,
		0x5f, 0x55, 0x00,
		0x5b, 0x5f, 0x5f, 0xfd,
	}
}

func newAMMCheckpointWorkload(t testing.TB, swaps int) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 0xA660, swaps)
	contract := common.HexToAddress("0x000000000000000000000000000000000000A660")
	creation := simpleAMMCreationCode()
	const runtimeOffset = 28
	if len(creation) <= runtimeOffset {
		t.Fatal("AMM creation code has no runtime")
	}
	selector := crypto.Keccak256([]byte("swap(uint256)"))[:4]
	workload := differentialWorkload{
		name: "constant-product-amm-shared-reserves", transactions: make([]*types.Transaction, swaps),
		contract: contract, accounts: []common.Address{contract},
		initialBalances: make(map[common.Address]*big.Int, swaps),
		initialCode:     map[common.Address][]byte{contract: append([]byte(nil), creation[runtimeOffset:]...)},
		initialStorage: map[common.Address]map[common.Hash]common.Hash{contract: {
			common.Hash{}:                   common.BigToHash(big.NewInt(1_000_000)),
			common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(1_000_000)),
		}},
		storageSlots: map[common.Address][]common.Hash{contract: {common.Hash{}, common.BigToHash(big.NewInt(1))}},
		env:          evm.BlockEnv{Height: 42, Time: 42, GasLimit: blockGasLimit, BaseFee: new(big.Int)},
	}
	for i, account := range pool {
		amountIn := big.NewInt(int64(1_000 + i))
		data := append([]byte(nil), selector...)
		data = append(data, common.LeftPadBytes(amountIn.Bytes(), 32)...)
		workload.accounts = append(workload.accounts, account.address)
		workload.initialBalances[account.address] = big.NewInt(1_000_000_000)
		workload.transactions[i] = signedTx(t, account.keyHex, 0, 200_000, &contract, new(big.Int), data)
	}
	return workload
}
