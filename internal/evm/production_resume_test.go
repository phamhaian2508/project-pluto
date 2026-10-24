package evm

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

func productionResumeCode(prefixPairs int) []byte {
	code := make([]byte, 0, prefixPairs*2+20)
	for i := 0; i < prefixPairs; i++ {
		code = append(code, 0x5f, 0x50) // PUSH0 rồi POP.
	}
	return append(code,
		0x5f, 0x54, 0x60, 0x01, 0x01, 0x5f, 0x55, // Tăng ô lưu trữ 0 thêm 1.
		0x5f, 0x54, 0x5f, 0x52, // Ghi slot[0] vào memory[0:32].
		0x60, 0x20, 0x5f, 0xa0, // Phát LOG0(memory[0:32]).
		0x60, 0x20, 0x5f, 0xf3, // Trả về memory[0:32].
	)
}

func sameProductionResults(left, right []TxResult) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		a, b := left[i], right[i]
		if a.Index != b.Index || a.Invalid != b.Invalid || a.Failed != b.Failed || a.GasUsed != b.GasUsed || a.ErrMsg != b.ErrMsg || !bytes.Equal(a.ReturnData, b.ReturnData) || len(a.Logs) != len(b.Logs) {
			return false
		}
		for j := range a.Logs {
			if a.Logs[j].Address != b.Logs[j].Address || !bytes.Equal(a.Logs[j].Data, b.Logs[j].Data) || len(a.Logs[j].Topics) != len(b.Logs[j].Topics) {
				return false
			}
			for k := range a.Logs[j].Topics {
				if a.Logs[j].Topics[k] != b.Logs[j].Topics[k] {
					return false
				}
			}
		}
	}
	return true
}

func runProductionResumeWorkload(t *testing.T, name string, code []byte, txCount, workers int, checkpointConfig CheckpointConfig, sequential bool) (*BlockResult, common.Hash) {
	t.Helper()
	chainID := big.NewInt(plutoconfig.EVMChainID)
	contract := common.HexToAddress("0xc0ffee")
	db, err := store.NewPebbleDB(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
	txs := make([]*types.Transaction, txCount)
	for i := range txs {
		digest := sha256.Sum256([]byte{byte(i + 1), 0x51})
		key, keyErr := crypto.ToECDSA(digest[:])
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
		tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: big.NewInt(2), Gas: 200_000, To: &contract})
		txs[i], err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	base := NewPebbleStateDB(db)
	env := BlockEnv{Height: 1, Time: 1, GasLimit: 5_000_000, BaseFee: new(big.Int)}
	var executor Executor
	if sequential {
		executor = NewSequentialExecutor(overlayTestChainConfig(chainID), chainID)
	} else {
		parallel := NewParallelExecutor(overlayTestChainConfig(chainID), chainID, workers)
		if err := parallel.ConfigureCheckpoints(checkpointConfig); err != nil {
			t.Fatal(err)
		}
		executor = parallel
	}
	result, err := executor.ExecuteBlock(base, env, txs)
	if err != nil {
		t.Fatal(err)
	}
	return result, base.GetState(contract, common.Hash{})
}

func TestProductionCheckpointPoliciesResumeAndReplayLessThanP0(t *testing.T) {
	if !resumableGethVersionAvailable() {
		t.Fatalf("resumable adapter requires geth %s", resumableGethVersion)
	}
	code := productionResumeCode(550)
	sequential, sequentialStorage := runProductionResumeWorkload(t, "resume-sequential", code, 4, 1, DefaultCheckpointConfig(), true)
	p0, p0Storage := runProductionResumeWorkload(t, "resume-p0", code, 4, 4, CheckpointConfig{Policy: CheckpointPolicyP0}, false)
	if !sameProductionResults(p0.Results, sequential.Results) || p0.GasUsed != sequential.GasUsed || p0Storage != sequentialStorage {
		t.Fatal("P0 result differs from sequential executor")
	}
	if p0.CheckpointMetrics.CheckpointsCreated != 0 || p0.CheckpointMetrics.Resumes != 0 || p0.CheckpointMetrics.FullFallbacks == 0 {
		t.Fatalf("unexpected P0 metrics: %+v", p0.CheckpointMetrics)
	}

	policies := []struct {
		name   string
		config CheckpointConfig
	}{
		{name: "P1", config: CheckpointConfig{Policy: CheckpointPolicyP1}},
		{name: "P2-N100", config: CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 100}},
		{name: "P2-N1000", config: CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 1000}},
	}
	for _, policy := range policies {
		t.Run(policy.name, func(t *testing.T) {
			result, storage := runProductionResumeWorkload(t, "resume-"+policy.name, code, 4, 4, policy.config, false)
			if !sameProductionResults(result.Results, sequential.Results) || result.GasUsed != sequential.GasUsed || storage != sequentialStorage {
				t.Fatalf("%s result differs from sequential executor", policy.name)
			}
			metrics := result.CheckpointMetrics
			if metrics.CheckpointsCreated == 0 || metrics.CheckpointsReused == 0 || metrics.Resumes == 0 {
				t.Fatalf("%s did not create and resume a production checkpoint: %+v", policy.name, metrics)
			}
			if metrics.FullFallbacks != 0 {
				t.Fatalf("%s unexpectedly used full fallback: %+v", policy.name, metrics)
			}
			if metrics.ReplayedOpcodes >= p0.CheckpointMetrics.ReplayedOpcodes {
				t.Fatalf("%s replayed %d opcodes, P0 replayed %d", policy.name, metrics.ReplayedOpcodes, p0.CheckpointMetrics.ReplayedOpcodes)
			}
		})
	}
}

func TestProductionP3LearnsHotStorageKeyAndResumes(t *testing.T) {
	code := productionResumeCode(8)
	sequential, sequentialStorage := runProductionResumeWorkload(t, "resume-p3-sequential", code, 10, 1, DefaultCheckpointConfig(), true)
	result, storage := runProductionResumeWorkload(t, "resume-p3", code, 10, 1, CheckpointConfig{Policy: CheckpointPolicyP3}, false)
	if !sameProductionResults(result.Results, sequential.Results) || result.GasUsed != sequential.GasUsed || storage != sequentialStorage {
		t.Fatal("P3 result differs from sequential executor")
	}
	metrics := result.CheckpointMetrics
	if metrics.CheckpointsCreated == 0 || metrics.CheckpointsReused == 0 || metrics.Resumes == 0 {
		t.Fatalf("P3 did not learn and reuse the hot storage key: %+v", metrics)
	}
}

func nestedResumeWrapper(target common.Address, prefixPairs int) []byte {
	code := make([]byte, 0, prefixPairs*2+40)
	for i := 0; i < prefixPairs; i++ {
		code = append(code, 0x5f, 0x50)
	}
	code = append(code, 0x36, 0x5f, 0x5f, 0x37, 0x5f, 0x5f, 0x36, 0x5f, 0x5f, 0x73)
	code = append(code, target.Bytes()...)
	return append(code, 0x5a, 0xf1, 0x50, 0x5f, 0x5f, 0xf3)
}

func runNestedResumeWorkload(t *testing.T, name string, sequential bool, checkpointConfig CheckpointConfig) (*BlockResult, common.Hash) {
	t.Helper()
	chainID := big.NewInt(plutoconfig.EVMChainID)
	callee := common.HexToAddress("0xca11ee")
	wrapper := common.HexToAddress("0xf04a4d")
	db, err := store.NewPebbleDB(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	seed.SetCode(callee, productionResumeCode(0), tracing.CodeChangeUnspecified)
	seed.SetCode(wrapper, nestedResumeWrapper(callee, 550), tracing.CodeChangeUnspecified)
	txs := make([]*types.Transaction, 4)
	for i := range txs {
		digest := sha256.Sum256([]byte{byte(i + 41), 0x72})
		key, keyErr := crypto.ToECDSA(digest[:])
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
		tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 300_000, To: &wrapper})
		txs[i], err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	base := NewPebbleStateDB(db)
	env := BlockEnv{Height: 1, Time: 1, GasLimit: 5_000_000, BaseFee: new(big.Int)}
	var executor Executor = NewSequentialExecutor(overlayTestChainConfig(chainID), chainID)
	if !sequential {
		parallel := NewParallelExecutor(overlayTestChainConfig(chainID), chainID, 4)
		if err := parallel.ConfigureCheckpoints(checkpointConfig); err != nil {
			t.Fatal(err)
		}
		executor = parallel
	}
	result, err := executor.ExecuteBlock(base, env, txs)
	if err != nil {
		t.Fatal(err)
	}
	return result, base.GetState(callee, common.Hash{})
}

func TestProductionResumeAcrossNestedCallMatchesSequential(t *testing.T) {
	sequential, sequentialStorage := runNestedResumeWorkload(t, "nested-resume-sequential", true, DefaultCheckpointConfig())
	policies := []CheckpointConfig{
		{Policy: CheckpointPolicyP1},
		{Policy: CheckpointPolicyP2, Interval: 1000},
	}
	for _, policy := range policies {
		parallel, parallelStorage := runNestedResumeWorkload(t, "nested-resume-"+policy.Policy.String(), false, policy)
		if !sameProductionResults(parallel.Results, sequential.Results) || parallel.GasUsed != sequential.GasUsed || parallelStorage != sequentialStorage {
			t.Fatalf("nested CALL resume differs from sequential executor for %s", policy.Policy)
		}
		if parallel.CheckpointMetrics.Resumes == 0 || parallel.CheckpointMetrics.CheckpointsReused == 0 {
			t.Fatalf("nested CALL workload did not resume for %s: %+v", policy.Policy, parallel.CheckpointMetrics)
		}
	}
}

func failureResumeWrapper(target common.Address, revertAfterCall, limitedGas bool) []byte {
	code := make([]byte, 0, 1150)
	for i := 0; i < 550; i++ {
		code = append(code, 0x5f, 0x50)
	}
	code = append(code, 0x36, 0x5f, 0x5f, 0x37, 0x5f, 0x5f, 0x36, 0x5f, 0x5f, 0x73)
	code = append(code, target.Bytes()...)
	if limitedGas {
		code = append(code, 0x61, 0x08, 0x98) // 2.200 gas: đủ đến SLOAD rồi hết gas tại SSTORE.
	} else {
		code = append(code, 0x5a) // Opcode GAS.
	}
	code = append(code, 0xf1, 0x50) // Gọi CALL rồi bỏ cờ thành công.
	if revertAfterCall {
		return append(code, 0x5f, 0x5f, 0xfd)
	}
	return append(code, 0x5f, 0x5f, 0xf3)
}

func runFailureResumeWorkload(t *testing.T, name string, sequential bool) (*BlockResult, common.Hash) {
	t.Helper()
	chainID := big.NewInt(plutoconfig.EVMChainID)
	callee := common.HexToAddress("0xca11ee")
	targets := []common.Address{common.HexToAddress("0x1001"), common.HexToAddress("0x1002"), common.HexToAddress("0x1003")}
	db, err := store.NewPebbleDB(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	seed.SetCode(callee, productionResumeCode(0), tracing.CodeChangeUnspecified)
	seed.SetCode(targets[0], failureResumeWrapper(callee, false, false), tracing.CodeChangeUnspecified)
	seed.SetCode(targets[1], failureResumeWrapper(callee, true, false), tracing.CodeChangeUnspecified)
	seed.SetCode(targets[2], failureResumeWrapper(callee, false, true), tracing.CodeChangeUnspecified)
	txs := make([]*types.Transaction, len(targets))
	for i, target := range targets {
		digest := sha256.Sum256([]byte{byte(i + 81), 0x39})
		key, keyErr := crypto.ToECDSA(digest[:])
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
		tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 300_000, To: &target})
		txs[i], err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	base := NewPebbleStateDB(db)
	env := BlockEnv{Height: 1, Time: 1, GasLimit: 5_000_000, BaseFee: new(big.Int)}
	var executor Executor = NewSequentialExecutor(overlayTestChainConfig(chainID), chainID)
	if !sequential {
		parallel := NewParallelExecutor(overlayTestChainConfig(chainID), chainID, 3)
		if err := parallel.ConfigureCheckpoints(CheckpointConfig{Policy: CheckpointPolicyP2, Interval: 1000}); err != nil {
			t.Fatal(err)
		}
		executor = parallel
	}
	result, err := executor.ExecuteBlock(base, env, txs)
	if err != nil {
		t.Fatal(err)
	}
	return result, base.GetState(callee, common.Hash{})
}

func TestProductionResumePreservesRevertOutOfGasAndLogs(t *testing.T) {
	sequential, sequentialStorage := runFailureResumeWorkload(t, "failure-resume-sequential", true)
	parallel, parallelStorage := runFailureResumeWorkload(t, "failure-resume-parallel", false)
	if !sameProductionResults(parallel.Results, sequential.Results) || parallel.GasUsed != sequential.GasUsed || parallelStorage != sequentialStorage {
		t.Fatal("resume with revert/out-of-gas differs from sequential executor")
	}
	if !parallel.Results[1].Failed || len(parallel.Results[1].Logs) != 0 || len(parallel.Results[2].Logs) != 0 {
		t.Fatalf("unexpected revert/OOG/log results: %+v", parallel.Results)
	}
	if parallelStorage != common.BigToHash(big.NewInt(1)) {
		t.Fatalf("reverted/OOG nested writes leaked: storage=%s", parallelStorage)
	}
	if parallel.CheckpointMetrics.Resumes < 2 {
		t.Fatalf("revert/OOG transactions did not resume: %+v", parallel.CheckpointMetrics)
	}
}
