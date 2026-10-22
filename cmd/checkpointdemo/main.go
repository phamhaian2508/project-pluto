package main

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const (
	demoTransactions = 64
	demoWorkers      = 4
)

type policyCase struct {
	name        string
	description string
	config      evm.CheckpointConfig
}

type demoResult struct {
	policyCase
	metrics    evm.CheckpointMetrics
	trace      []evm.CheckpointTraceEvent
	finalValue *big.Int
	correct    bool
}

func main() {
	fmt.Println("=== DEMO M5: KHONG CHECKPOINT vs CO CHECKPOINT ===")
	fmt.Printf("Tinh huong: %d giao dich chay song song, cung tang Counter them 1.\n", demoTransactions)
	fmt.Println("Do cung doc/ghi Counter, mot so giao dich se xung dot va phai chay lai.")
	fmt.Println()

	tempDir, err := os.MkdirTemp("", "pluto-checkpoint-demo-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tempDir)

	policies := []policyCase{
		{name: "P0", description: "Khong checkpoint", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP0}},
		{name: "P1", description: "Checkpoint truoc khi doc state", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}},
		{name: "P2-100", description: "Checkpoint moi 100 opcode", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 100}},
		{name: "P2-1000", description: "Checkpoint moi 1000 opcode", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 1000}},
		{name: "P3", description: "Checkpoint tai diem hay xung dot", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3}},
	}

	results := make([]demoResult, 0, len(policies))
	for index, policy := range policies {
		result, runErr := runPolicy(filepath.Join(tempDir, fmt.Sprintf("run-%d", index)), policy)
		if runErr != nil {
			fatal(fmt.Errorf("%s: %w", policy.name, runErr))
		}
		results = append(results, result)
	}

	printResults(results)
}

func runPolicy(dir string, policy policyCase) (demoResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return demoResult{}, err
	}
	db, err := store.NewPebbleDB("checkpoint-demo", dir)
	if err != nil {
		return demoResult{}, err
	}
	defer db.Close()

	chainID := big.NewInt(plutoconfig.EVMChainID)
	contract := common.HexToAddress("0x0000000000000000000000000000000000c0ffee")
	seed := evm.NewPebbleStateDB(db)
	seed.SetCode(contract, demoContractCode(), tracing.CodeChangeUnspecified)

	txs := make([]*types.Transaction, demoTransactions)
	for i := range txs {
		digest := sha256.Sum256([]byte{byte(i + 1), 0x5a})
		key, keyErr := crypto.ToECDSA(digest[:])
		if keyErr != nil {
			return demoResult{}, keyErr
		}
		seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
		tx := types.NewTx(&types.LegacyTx{
			Nonce: 0, GasPrice: big.NewInt(1), Gas: 200_000, To: &contract,
		})
		txs[i], err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			return demoResult{}, err
		}
	}
	if err := seed.Commit(); err != nil {
		return demoResult{}, err
	}

	state := evm.NewPebbleStateDB(db)
	executor := evm.NewParallelExecutor(demoChainConfig(chainID), chainID, demoWorkers)
	if err := executor.ConfigureCheckpoints(policy.config); err != nil {
		return demoResult{}, err
	}
	block, err := executor.ExecuteBlock(state, evm.BlockEnv{
		Height: 1, Time: 1, GasLimit: 30_000_000, BaseFee: new(big.Int),
	}, txs)
	if err != nil {
		return demoResult{}, err
	}

	allTransactionsValid := true
	for _, txResult := range block.Results {
		if txResult.Invalid || txResult.Failed {
			allTransactionsValid = false
			break
		}
	}
	value := new(big.Int).SetBytes(state.GetState(contract, common.Hash{}).Bytes())
	return demoResult{
		policyCase: policy,
		metrics:    block.CheckpointMetrics,
		trace:      block.CheckpointTrace,
		finalValue: value,
		correct:    allTransactionsValid && value.Uint64() == demoTransactions,
	}, nil
}

// The demo contract performs a long deterministic prefix, then executes:
// Counter = SLOAD(0) + 1; SSTORE(0, Counter).
func demoContractCode() []byte {
	code := make([]byte, 0, 1120)
	for i := 0; i < 550; i++ {
		code = append(code, 0x5f, 0x50) // PUSH0, POP
	}
	return append(code, 0x5f, 0x54, 0x60, 0x01, 0x01, 0x5f, 0x55, 0x00)
}

func demoChainConfig(chainID *big.Int) *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	return &params.ChainConfig{
		ChainID: chainID, HomesteadBlock: zero, EIP150Block: zero, EIP155Block: zero,
		EIP158Block: zero, ByzantiumBlock: zero, ConstantinopleBlock: zero,
		PetersburgBlock: zero, IstanbulBlock: zero, MuirGlacierBlock: zero,
		BerlinBlock: zero, LondonBlock: zero, ArrowGlacierBlock: zero,
		GrayGlacierBlock: zero, MergeNetsplitBlock: zero,
		TerminalTotalDifficulty: zero, ShanghaiTime: &shanghai,
	}
}

func printResults(results []demoResult) {
	printExecutionTrace(results)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POLICY\tCHECKPOINT TAO\tRESUME\tCHAY LAI OPCODE\tCHAY LAI TU DAU\tKET QUA")
	for _, result := range results {
		status := "DUNG"
		if !result.correct {
			status = "SAI"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s (Counter=%s)\n",
			result.name,
			result.metrics.CheckpointsCreated,
			result.metrics.Resumes,
			result.metrics.ReplayedOpcodes,
			result.metrics.FullFallbacks,
			status,
			result.finalValue.String(),
		)
	}
	w.Flush()

	fmt.Println()
	fmt.Println("Cach doc ket qua:")
	fmt.Println("- P0 khong co checkpoint: khi xung dot, giao dich chay lai tu dau.")
	fmt.Println("- P1/P2/P3 co checkpoint: RESUME > 0 nghia la da quay lai checkpoint va chay tiep.")
	fmt.Println("- CHAY LAI OPCODE cang nho thi phan phai lam lai cang it.")
	fmt.Printf("- Tat ca Counter=%d va DUNG: checkpoint toi uu cach chay, khong doi ket qua.\n", demoTransactions)
}

func printExecutionTrace(results []demoResult) {
	fmt.Println("--- TRACE THUC TE BEN TRONG PARALLEL EXECUTOR ---")
	for _, policyName := range []string{"P0", "P1"} {
		for _, result := range results {
			if result.name != policyName {
				continue
			}
			fmt.Printf("\n%s - %s:\n", result.name, result.description)
			limit := 3
			if len(result.trace) < limit {
				limit = len(result.trace)
			}
			for _, event := range result.trace[:limit] {
				fmt.Printf("  TX %d: validation THAT BAI tai read #%d\n", event.TxIndex, event.FailedRead)
				if event.Action == "resume" {
					fmt.Printf("        checkpoints=%v\n", event.CheckpointPositions)
					fmt.Printf("        -> khoi phuc checkpoint opcode %v, chay tiep %d opcode\n",
						event.ReusedPositions, event.ReplayedOpcodes)
				} else {
					fmt.Printf("        khong co checkpoint -> CHAY LAI TU DAU %d opcode\n", event.ReplayedOpcodes)
				}
			}
			if len(result.trace) > limit {
				fmt.Printf("  ... va %d giao dich xung dot khac\n", len(result.trace)-limit)
			}
		}
	}
	fmt.Println("\n--- TONG KET TOAN BO WORKLOAD ---")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Demo that bai:", err)
	os.Exit(1)
}
