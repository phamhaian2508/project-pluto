package evm

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// --- Helpers dựng bytecode EVM Shanghai tự chứa cho bộ test Phase 2 ---

func shanghaiTestChainConfig(chainID uint64) *params.ChainConfig {
	zeroTime := uint64(0)
	return &params.ChainConfig{
		ChainID:                 new(big.Int).SetUint64(chainID),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		MuirGlacierBlock:        big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		ArrowGlacierBlock:       big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		TerminalTotalDifficulty: big.NewInt(0),
		ShanghaiTime:            &zeroTime,
	}
}

func testPhase2Key(t *testing.T, seed byte) (*ecdsa.PrivateKey, common.Address) {
	t.Helper()
	sum := sha256.Sum256([]byte("pluto-phase2-key-" + string([]byte{seed})))
	key, err := crypto.ToECDSA(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return key, crypto.PubkeyToAddress(key.PublicKey)
}

func signType2Tx(t *testing.T, chainID uint64, key *ecdsa.PrivateKey, nonce uint64, to common.Address, gas uint64, data []byte, accessList types.AccessList) *types.Transaction {
	t.Helper()
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:    new(big.Int).SetUint64(chainID),
		Nonce:      nonce,
		GasTipCap:  big.NewInt(0),
		GasFeeCap:  big.NewInt(1),
		Gas:        gas,
		To:         &to,
		Value:      big.NewInt(0),
		Data:       data,
		AccessList: accessList,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(new(big.Int).SetUint64(chainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// appendHashPrefix tạo đoạn tính toán thuần: ghi calldata[0:32] vào memory[0:32] rồi băm KECCAK256 rounds lần.
// Sau đoạn này memory[0:32] chứa digest cuối cùng (thuần tính toán, không chạm state).
func appendHashPrefix(code []byte, rounds int) []byte {
	// CALLDATALOAD(0) -> MSTORE(0)
	code = append(code, 0x5f, 0x35, 0x5f, 0x52)
	for i := 0; i < rounds; i++ {
		// KECCAK256(0, 32) -> MSTORE(0)
		code = append(code, 0x60, 0x20, 0x5f, 0x20, 0x5f, 0x52)
	}
	return code
}

// buildLateProbeCode: băm pure prefix `rounds` lần -> SLOAD(slot) -> +1 -> SSTORE(slot) -> MSTORE(32, newVal) -> LOG0(0, 64) -> RETURN(0, 64).
func buildLateProbeCode(rounds int, slot byte) []byte {
	code := appendHashPrefix(nil, rounds)
	code = append(code,
		0x60, slot, 0x54, // SLOAD(slot)
		0x60, 0x01, 0x01, // +1
		0x80,             // DUP1
		0x60, slot, 0x55, // SSTORE(slot, val+1)
		0x60, 0x20, 0x52, // MSTORE(32, val+1)
		0x60, 0x40, 0x5f, 0xa0, // LOG0(0, 64) (digest + newVal)
		0x60, 0x40, 0x5f, 0xf3, // RETURN(0, 64)
	)
	return code
}

// buildEarlyProbeCode: SLOAD(slot) TRƯỚC -> băm `rounds` lần -> +1 -> SSTORE(slot) -> LOG0 -> RETURN.
func buildEarlyProbeCode(rounds int, slot byte) []byte {
	code := []byte{0x60, slot, 0x54} // SLOAD(slot) nằm trên stack
	code = appendHashPrefix(code, rounds)
	code = append(code,
		0x60, 0x01, 0x01, // val + 1
		0x80,
		0x60, slot, 0x55, // SSTORE(slot, val+1)
		0x60, 0x20, 0x52, // MSTORE(32, val+1)
		0x60, 0x40, 0x5f, 0xa0, // LOG0(0, 64)
		0x60, 0x40, 0x5f, 0xf3, // RETURN(0, 64)
	)
	return code
}

// buildDirectSStoreCode: băm `rounds` lần rồi SSTORE(slot, calldata[0]) TRỰC TIẾP không có SLOAD trước đó!
// calldata[0:32] chứa giá trị cần ghi (vd A truyền 5, B truyền 7).
func buildDirectSStoreCode(rounds int, slot byte) []byte {
	code := appendHashPrefix(nil, rounds)
	code = append(code,
		0x5f, 0x35, // CALLDATALOAD(0) -> value
		0x60, slot, // slot
		0x55, // SSTORE(slot, value) — KHÔNG có SLOAD trước đó!
		0x60, 0x20, 0x5f, 0xa0, // LOG0(0, 32) phát digest
		0x60, 0x20, 0x5f, 0xf3, // RETURN(0, 32)
	)
	return code
}

// buildMemExpandWarmRefundCode:
// Prefix thuần: mở rộng memory tới 128 byte (MSTORE tại 0x60) + băm.
// Suffix (sau checkpoint): mở rộng memory tiếp tới 256 byte (MSTORE tại 0xe0),
// SLOAD(0) 2 lần (cold -> warm), ghi SSTORE(0, val+1), rồi SSTORE(0, calldata[0]) (kích hoạt EIP-2200/3529 refund nếu đặt về original hoặc 0).
func buildMemExpandWarmRefundCode(rounds int) []byte {
	code := []byte{
		0x60, 0xaa, 0x60, 0x60, 0x52, // MSTORE(0x60, 0xaa) -> mở rộng memory lên 128 byte trước checkpoint
	}
	code = appendHashPrefix(code, rounds)
	code = append(code,
		0x5f, 0x54, // SLOAD(0) lần 1 (cold nếu không có access list, warm nếu có)
		0x5f, 0x54, // SLOAD(0) lần 2 (luôn warm)
		0x01,             // tổng 2 lần đọc
		0x60, 0xe0, 0x52, // MSTORE(0xe0, sum) -> mở rộng memory lên 256 byte SAU checkpoint (kiểm lastGasCost)
		0x60, 0x01, 0x01, // lấy thêm giá trị tạm (actually stack đang trống sau MSTORE -> push 99)
	)
	// xây lại phần suffix gọn và chuẩn stack:
	code = []byte{
		0x60, 0xaa, 0x60, 0x60, 0x52, // MSTORE(0x60, 0xaa) -> mở rộng memory lên 128 byte trước checkpoint
	}
	code = appendHashPrefix(code, rounds)
	code = append(code,
		0x5f, 0x54, // SLOAD(0) lần 1 -> [v]
		0x5f, 0x54, // SLOAD(0) lần 2 -> [v, v]
		0x01,             // ADD -> [2v]
		0x60, 0xe0, 0x52, // MSTORE(0xe0, 2v) -> mở rộng memory lên 256 byte SAU checkpoint!
		0x60, 0x42, 0x5f, 0x55, // SSTORE(0, 0x42)
		0x5f, 0x35, 0x5f, 0x55, // SSTORE(0, calldata[0]) -> đặt về original hoặc 0 để kích hoạt refund EIP-3529
		0x60, 0x20, 0x60, 0xe0, 0xa0, // LOG0(0xe0, 32)
		0x60, 0x20, 0x60, 0xe0, 0xf3, // RETURN(0xe0, 32)
	)
	return code
}

// buildWriteLogRevertAfterCheckpointCode:
// Prefix thuần: băm `rounds` lần.
// Suffix: đọc slot 0 -> nếu calldata[0]==1 thì ghi slot 0 = val+1 và thành công (cho A);
// nếu calldata[0]==2 thì ghi slot 1 = 0x99, phát LOG0, rồi nếu slot 0 == 11 thì REVERT(0, 32), ngược lại RETURN(0, 32)!
func buildWriteLogRevertAfterCheckpointCode(rounds int) []byte {
	code := appendHashPrefix(nil, rounds)
	// đọc slot 0 -> [val]
	code = append(code, 0x5f, 0x54)
	// kiểm calldata[0] == 1 (A mode)
	code = append(code, 0x5f, 0x35, 0x60, 0x01, 0x14) // [val, isA]
	// JUMPI tới nhãn A (sẽ patch offset)
	jumpATargetPos := len(code) + 1
	code = append(code, 0x61, 0x00, 0x00, 0x57) // PUSH2 targetA, JUMPI

	// Nhánh B: [val]
	// ghi slot 1 = 0x99 và phát LOG0(0, 32)
	code = append(code,
		0x60, 0x99, 0x60, 0x01, 0x55, // SSTORE(1, 0x99)
		0x60, 0x20, 0x5f, 0xa0, // LOG0(0, 32)
		0x60, 0x0b, 0x14, // val == 11 ?
	)
	jumpRevTargetPos := len(code) + 1
	code = append(code, 0x61, 0x00, 0x00, 0x57) // PUSH2 targetRev, JUMPI
	// không revert: RETURN(0, 32)
	code = append(code, 0x60, 0x20, 0x5f, 0xf3)

	// targetRev: JUMPDEST; REVERT(0, 32)
	revDest := len(code)
	code[jumpRevTargetPos] = byte(revDest >> 8)
	code[jumpRevTargetPos+1] = byte(revDest)
	code = append(code, 0x5b, 0x60, 0x20, 0x5f, 0xfd)

	// targetA: JUMPDEST; [val] -> val+1 -> SSTORE(0) -> RETURN(0, 32)
	aDest := len(code)
	code[jumpATargetPos] = byte(aDest >> 8)
	code[jumpATargetPos+1] = byte(aDest)
	code = append(code, 0x5b, 0x60, 0x01, 0x01, 0x5f, 0x55, 0x60, 0x20, 0x5f, 0xf3)
	return code
}

// buildSuffixCallBranchCode:
// Prefix thuần: băm `rounds` lần.
// Suffix: đọc slot 0 -> nếu calldata[0]==1 thì ghi slot 0 = 11 (tx A).
// Nếu calldata[0]==2 (tx B): nếu slot 0 == 11 thì gọi CALL tới helperAddr rồi ghi slot 1 = 77; ngược lại chỉ ghi slot 1 = 10.
func buildSuffixCallBranchCode(rounds int, helperAddr common.Address) []byte {
	code := appendHashPrefix(nil, rounds)
	code = append(code, 0x5f, 0x54)                   // [val]
	code = append(code, 0x5f, 0x35, 0x60, 0x01, 0x14) // [val, isA]
	jumpAPos := len(code) + 1
	code = append(code, 0x61, 0x00, 0x00, 0x57) // JUMPI targetA

	// Nhánh B: [val]
	code = append(code, 0x60, 0x0b, 0x14) // val == 11 ?
	jumpCallPos := len(code) + 1
	code = append(code, 0x61, 0x00, 0x00, 0x57) // JUMPI targetCall
	// không gọi CALL: SSTORE(1, 10); RETURN(0, 32)
	code = append(code, 0x60, 0x0a, 0x60, 0x01, 0x55, 0x60, 0x20, 0x5f, 0xf3)

	// targetCall: JUMPDEST; CALL(gas, helperAddr, 0, 0, 0, 0, 0); POP; SSTORE(1, 77); RETURN(0, 32)
	callDest := len(code)
	code[jumpCallPos] = byte(callDest >> 8)
	code[jumpCallPos+1] = byte(callDest)
	code = append(code, 0x5b, 0x5f, 0x5f, 0x5f, 0x5f, 0x5f, 0x73)
	code = append(code, helperAddr.Bytes()...)
	code = append(code, 0x5a, 0xf1, 0x50, 0x60, 0x4d, 0x60, 0x01, 0x55, 0x60, 0x20, 0x5f, 0xf3)

	// targetA: JUMPDEST; POP; SSTORE(0, 11); RETURN(0, 32)
	aDest := len(code)
	code[jumpAPos] = byte(aDest >> 8)
	code[jumpAPos+1] = byte(aDest)
	code = append(code, 0x5b, 0x50, 0x60, 0x0b, 0x5f, 0x55, 0x60, 0x20, 0x5f, 0xf3)
	return code
}

// buildPrefixCallThenHashCode: gọi STATICCALL tới helperAddr NGAY ĐẦU -> sau khi trả về depth=1 thì băm `rounds` lần -> SLOAD(0)+1 -> SSTORE(0).
func buildPrefixCallThenHashCode(rounds int, helperAddr common.Address) []byte {
	code := []byte{0x5f, 0x5f, 0x5f, 0x5f, 0x73}
	code = append(code, helperAddr.Bytes()...)
	code = append(code, 0x5a, 0xfa, 0x50) // STATICCALL(gas, helper, 0, 0, 0, 0); POP
	code = appendHashPrefix(code, rounds)
	code = append(code,
		0x5f, 0x54, 0x60, 0x01, 0x01, 0x5f, 0x55,
		0x60, 0x20, 0x5f, 0xf3,
	)
	return code
}

// --- Helper gọi trực tiếp binary `evm t8n` độc lập để đối chiếu Cổng Quyết Định Lựa Chọn 1 ---

type t8nTestResult struct {
	GasUsed  uint64
	Receipts []struct {
		Status            hexutil.Uint64 `json:"status"`
		GasUsed           hexutil.Uint64 `json:"gasUsed"`
		CumulativeGasUsed hexutil.Uint64 `json:"cumulativeGasUsed"`
		Logs              []*types.Log   `json:"logs"`
	}
	Rejected []struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	}
	PostAlloc map[common.Address]struct {
		Balance *gethmath.HexOrDecimal256 `json:"balance"`
		Nonce   gethmath.HexOrDecimal64   `json:"nonce"`
		Storage map[string]string         `json:"storage"`
	}
}

func resolveTestEVMBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("EVM_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	const fallback = "/tmp/plutof-evm-v1168/evm"
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Skip("EVM_BIN not found; skipping evm t8n oracle cross-check")
	return ""
}

func runEVMT8NOracle(t *testing.T, evmBin string, pre *LogicalState, env BlockEnv, txs []*types.Transaction, chainID uint64) *t8nTestResult {
	t.Helper()
	tmp := t.TempDir()

	allocMap := make(map[string]any, len(pre.Accounts))
	for addr, acc := range pre.Accounts {
		item := map[string]any{
			"balance": "0x" + acc.Balance.ToBig().Text(16),
			"nonce":   fmt.Sprintf("0x%x", acc.Nonce),
		}
		if len(acc.Code) > 0 {
			item["code"] = hexutil.Encode(acc.Code)
		}
		if len(acc.Storage) > 0 {
			st := make(map[string]string, len(acc.Storage))
			for k, v := range acc.Storage {
				if v != (common.Hash{}) {
					st[k.Hex()] = v.Hex()
				}
			}
			if len(st) > 0 {
				item["storage"] = st
			}
		}
		allocMap[addr.Hex()] = item
	}
	allocBytes, err := json.MarshalIndent(allocMap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	envMap := map[string]any{
		"currentCoinbase":  env.Coinbase.Hex(),
		"currentGasLimit":  fmt.Sprintf("0x%x", env.GasLimit),
		"currentNumber":    fmt.Sprintf("0x%x", env.NumberOrHeight()),
		"currentTimestamp": fmt.Sprintf("0x%x", env.Time),
		"currentBaseFee":   "0x" + env.BaseFee.Text(16),
		"currentRandom":    "0x" + new(big.Int).SetBytes(env.Random[:]).Text(16),
		"withdrawals":      []any{},
	}
	envBytes, err := json.MarshalIndent(envMap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	txsBytes, err := json.MarshalIndent(txs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	allocPath := filepath.Join(tmp, "alloc.json")
	envPath := filepath.Join(tmp, "env.json")
	txsPath := filepath.Join(tmp, "txs.json")
	outAlloc := filepath.Join(tmp, "out-alloc.json")
	outResult := filepath.Join(tmp, "out-result.json")
	for p, b := range map[string][]byte{allocPath: allocBytes, envPath: envBytes, txsPath: txsBytes} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, evmBin, "t8n",
		"--state.fork", "Shanghai",
		"--state.chainid", fmt.Sprintf("%d", chainID),
		"--input.alloc", allocPath,
		"--input.env", envPath,
		"--input.txs", txsPath,
		"--output.basedir", tmp,
		"--output.alloc", "out-alloc.json",
		"--output.result", "out-result.json",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("evm t8n failed: %v\noutput: %s", err, string(out))
	}

	var parsed struct {
		GasUsed  hexutil.Uint64 `json:"gasUsed"`
		Receipts []struct {
			Status            hexutil.Uint64 `json:"status"`
			GasUsed           hexutil.Uint64 `json:"gasUsed"`
			CumulativeGasUsed hexutil.Uint64 `json:"cumulativeGasUsed"`
			Logs              []*types.Log   `json:"logs"`
		} `json:"receipts"`
		Rejected []struct {
			Index int    `json:"index"`
			Error string `json:"error"`
		} `json:"rejected"`
	}
	resBytes, err := os.ReadFile(outResult)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resBytes, &parsed); err != nil {
		t.Fatalf("parse t8n result: %v\nraw: %s", err, string(resBytes))
	}
	postAllocBytes, err := os.ReadFile(outAlloc)
	if err != nil {
		t.Fatal(err)
	}
	out := &t8nTestResult{
		GasUsed:  uint64(parsed.GasUsed),
		Receipts: parsed.Receipts,
		Rejected: parsed.Rejected,
	}
	if err := json.Unmarshal(postAllocBytes, &out.PostAlloc); err != nil {
		t.Fatalf("parse t8n post-alloc: %v", err)
	}
	return out
}

func (e BlockEnv) NumberOrHeight() uint64 {
	if e.Height < 0 {
		return 0
	}
	return uint64(e.Height)
}

func assertBlockMatchesT8N(t *testing.T, res *BlockResult, post *LogicalState, oracle *t8nTestResult) {
	t.Helper()
	if len(oracle.Rejected) != 0 {
		t.Fatalf("unexpected t8n rejected txs: %+v", oracle.Rejected)
	}
	if res.GasUsed != oracle.GasUsed {
		t.Fatalf("block gasUsed=%d != geth t8n=%d", res.GasUsed, oracle.GasUsed)
	}
	if len(res.Results) != len(oracle.Receipts) {
		t.Fatalf("receipt count=%d != geth t8n=%d", len(res.Results), len(oracle.Receipts))
	}
	var cumGas uint64
	for i, tr := range res.Results {
		gr := oracle.Receipts[i]
		if tr.Invalid {
			t.Fatalf("tx %d invalid in pluto: %s", i, tr.ErrMsg)
		}
		wantStatus := uint64(types.ReceiptStatusSuccessful)
		if tr.Failed {
			wantStatus = types.ReceiptStatusFailed
		}
		if wantStatus != uint64(gr.Status) {
			t.Fatalf("tx %d status=%d != geth t8n=%d (err=%q)", i, wantStatus, gr.Status, tr.ErrMsg)
		}
		if tr.GasUsed != uint64(gr.GasUsed) {
			t.Fatalf("tx %d gasUsed=%d != geth t8n=%d", i, tr.GasUsed, gr.GasUsed)
		}
		cumGas += tr.GasUsed
		if cumGas != uint64(gr.CumulativeGasUsed) {
			t.Fatalf("tx %d cumulativeGas=%d != geth t8n=%d", i, cumGas, gr.CumulativeGasUsed)
		}
		if len(tr.Logs) != len(gr.Logs) {
			t.Fatalf("tx %d log count=%d != geth t8n=%d", i, len(tr.Logs), len(gr.Logs))
		}
		for j := range tr.Logs {
			if tr.Logs[j].Address != gr.Logs[j].Address || !bytes.Equal(tr.Logs[j].Data, gr.Logs[j].Data) {
				t.Fatalf("tx %d log %d mismatch: pluto=(%s,%x) t8n=(%s,%x)", i, j, tr.Logs[j].Address.Hex(), tr.Logs[j].Data, gr.Logs[j].Address.Hex(), gr.Logs[j].Data)
			}
		}
	}
	for addr, gAcc := range oracle.PostAlloc {
		pAcc, ok := post.Accounts[addr]
		if !ok {
			t.Fatalf("post-state missing account %s present in t8n", addr.Hex())
		}
		if pAcc.Nonce != uint64(gAcc.Nonce) {
			t.Fatalf("account %s nonce=%d != t8n=%d", addr.Hex(), pAcc.Nonce, gAcc.Nonce)
		}
		gBal := new(big.Int)
		if gAcc.Balance != nil {
			gBal = (*big.Int)(gAcc.Balance)
		}
		if pAcc.Balance.ToBig().Cmp(gBal) != 0 {
			t.Fatalf("account %s balance=%s != t8n=%s", addr.Hex(), pAcc.Balance.ToBig(), gBal)
		}
		for kStr, vStr := range gAcc.Storage {
			k := common.HexToHash(kStr)
			v := common.HexToHash(vStr)
			if pAcc.Storage[k] != v {
				t.Fatalf("account %s slot %s = %s != t8n %s", addr.Hex(), k.Hex(), pAcc.Storage[k].Hex(), v.Hex())
			}
		}
		for k, v := range pAcc.Storage {
			if v == (common.Hash{}) {
				continue
			}
			found := false
			for gkStr, gvStr := range gAcc.Storage {
				if common.HexToHash(gkStr) == k && common.HexToHash(gvStr) == v {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("account %s extra storage slot %s=%s not in t8n", addr.Hex(), k.Hex(), v.Hex())
			}
		}
	}
}

func runPhase2Scenario(
	t *testing.T,
	pre *LogicalState,
	env BlockEnv,
	txs []*types.Transaction,
	chainID uint64,
	workers int,
	cfg CheckpointConfig,
	hooks *OCCHooks,
	recordTrace bool,
) (*BlockResult, *LogicalState, error) {
	t.Helper()
	db, err := store.NewPebbleDB("p2-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	seed := NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(pre); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	chainCfg := shanghaiTestChainConfig(chainID)
	exec := NewParallelExecutor(chainCfg, new(big.Int).SetUint64(chainID), workers)
	exec.SlidingWindow = true
	exec.StrictInvalidTx = true
	exec.Phase2Mode = true
	exec.RecordTrace = recordTrace
	exec.OCCHooks = hooks
	exec.Checkpoints = cfg

	stateDB := NewPebbleStateDB(db)
	res, execErr := exec.ExecuteBlock(stateDB, env, txs)
	if execErr != nil {
		return nil, nil, execErr
	}
	if err := stateDB.Commit(); err != nil {
		t.Fatal(err)
	}
	post, err := NewPebbleStateDB(db).ExportLogicalState()
	if err != nil {
		t.Fatal(err)
	}
	return res, post, nil
}

func runSequentialScenario(t *testing.T, pre *LogicalState, env BlockEnv, txs []*types.Transaction, chainID uint64) (*BlockResult, *LogicalState) {
	t.Helper()
	db, err := store.NewPebbleDB("p2-seq", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(pre); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	chainCfg := shanghaiTestChainConfig(chainID)
	seq := NewSequentialExecutor(chainCfg, new(big.Int).SetUint64(chainID))
	seq.StrictInvalidTx = true
	stateDB := NewPebbleStateDB(db)
	res, err := seq.ExecuteBlock(stateDB, env, txs)
	if err != nil {
		t.Fatalf("sequential failed: %v", err)
	}
	if err := stateDB.Commit(); err != nil {
		t.Fatal(err)
	}
	post, err := NewPebbleStateDB(db).ExportLogicalState()
	if err != nil {
		t.Fatal(err)
	}
	return res, post
}

func makePreStateWithContract(contract common.Address, code []byte, initStorage map[common.Hash]common.Hash, coinbase common.Address, senders ...common.Address) *LogicalState {
	st := NewLogicalState()
	st.Accounts[coinbase] = &LogicalAccount{
		Balance: uint256.NewInt(1_000_000),
		Storage: map[common.Hash]common.Hash{},
	}
	storage := map[common.Hash]common.Hash{}
	for k, v := range initStorage {
		storage[k] = v
	}
	st.Accounts[contract] = &LogicalAccount{
		Nonce:   1,
		Balance: uint256.NewInt(0),
		Code:    append([]byte(nil), code...),
		Storage: storage,
	}
	for _, s := range senders {
		st.Accounts[s] = &LogicalAccount{
			Nonce:   0,
			Balance: uint256.NewInt(100_000_000_000),
			Storage: map[common.Hash]common.Hash{},
		}
	}
	return st
}

// --- TEST 1: Dừng/chạy tiếp trên cùng nền & cố ý sửa hỏng stack/memory bản cũ ---

func TestPhase2SameBasePauseResumeAndDataIsolation(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c001")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee1")
	keyA, addrA := testPhase2Key(t, 1)

	// Hợp đồng: mở rộng memory -> băm 16 vòng -> SLOAD(0) -> +1 -> SSTORE(0) -> mở rộng memory thêm -> RETURN
	code := buildMemExpandWarmRefundCode(16)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA)
	tx := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(10)).Bytes(), nil)
	env := BlockEnv{Height: 1, Time: 1700000000, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	oracle := runEVMT8NOracle(t, evmBin, pre, env, []*types.Transaction{tx}, chainID)

	db, err := store.NewPebbleDB("same-base", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(pre); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	chainCfg := shanghaiTestChainConfig(chainID)
	signer := types.MakeSigner(chainCfg, big.NewInt(env.Height), env.Time)
	base := NewPebbleStateDB(db)
	versions := NewMVMemory()
	prefix := newPrefixSnapshot(base, versions, 0, env.GasLimit)

	// Chạy lần 1 với fixed N=1 để chụp ở mọi bước pure prefix (gồm trước MSTORE mở rộng memory và trước SLOAD đầu tiên)
	cpStore := NewCheckpointStore(DefaultCheckpointBudgetBytes)
	spec := &windowCaptureSpec{
		policyName:      "fixed",
		effectivePolicy: "fixed",
		interval:        1,
		store:           cpStore,
		countOpcodes:    true,
		enableCapture:   true,
	}
	fullOut := runWindowAttemptWithCapture(context.Background(), 0, 1, tx, prefix, env, chainCfg, signer, nil, spec)
	if fullOut.internalErr != nil || fullOut.applyErr != nil || fullOut.result.Failed {
		t.Fatalf("full run failed: internal=%v apply=%v res=%+v", fullOut.internalErr, fullOut.applyErr, fullOut.result)
	}
	cps := cpStore.CheckpointsForTx(0)
	if len(cps) < 20 {
		t.Fatalf("expected >= 20 pure-prefix checkpoints, got %d", len(cps))
	}

	// Tìm checkpoint ngay trước SLOAD đầu tiên (bước cuối cùng được chụp vì sau SLOAD bị disarm)
	lastCP := cps[len(cps)-1]
	if vm.OpCode(lastCP.Opcode) != vm.SLOAD {
		t.Fatalf("expected last pure-prefix checkpoint right before SLOAD, got %s at step %d", vm.OpCode(lastCP.Opcode), lastCP.NextStep)
	}

	// Cố ý phá hủy bản trạng thái lần chạy cũ để chứng minh checkpoint sở hữu bản sao độc lập
	fullOut.state.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(999999)))
	fullOut.state = nil

	// Khôi phục trên cùng nền từ checkpoint ngay trước SLOAD
	resumed, fbReason, resumedSteps, resErr := resumePurePrefixAttempt(0, 2, tx, prefix, env, chainCfg, signer, lastCP)
	if resErr != nil {
		t.Fatalf("same-base resume failed (reason=%s): %v", fbReason, resErr)
	}
	if resumedSteps >= fullOut.opcodeSteps {
		t.Fatalf("expected resumed steps (%d) < full steps (%d) to prove prefix skipped", resumedSteps, fullOut.opcodeSteps)
	}
	if resumed.result.GasUsed != fullOut.result.GasUsed || !bytes.Equal(resumed.result.ReturnData, fullOut.result.ReturnData) {
		t.Fatalf("resumed output differs: gas=%d/%d ret=%x/%x", resumed.result.GasUsed, fullOut.result.GasUsed, resumed.result.ReturnData, fullOut.result.ReturnData)
	}
	if err := base.ApplyPendingWrites(resumed.delta); err != nil {
		t.Fatal(err)
	}
	if err := base.Commit(); err != nil {
		t.Fatal(err)
	}
	post, err := NewPebbleStateDB(db).ExportLogicalState()
	if err != nil {
		t.Fatal(err)
	}
	assertBlockMatchesT8N(t, &BlockResult{GasUsed: resumed.result.GasUsed, Results: []TxResult{resumed.result}}, post, oracle)

	// Giải phóng store và xác minh remaining == 0
	cpStore.ReleaseAll()
	if used, _, _, _ := cpStore.Stats(); used != 0 {
		t.Fatalf("expected 0 remaining bytes after ReleaseAll, got %d", used)
	}
}

// --- TEST 2: checkpoint-late (buộc B đọc 10 trước khi A chốt 11 -> B resume bỏ qua vòng băm, đọc 11, ghi 12) ---

func TestPhase2CheckpointLateResumesAndSkipsHashing(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c002")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee2")
	keyA, addrA := testPhase2Key(t, 2)
	keyB, addrB := testPhase2Key(t, 3)

	code := buildLateProbeCode(32, 0)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(111)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(222)).Bytes(), nil)
	txs := []*types.Transaction{txA, txB}
	env := BlockEnv{Height: 1, Time: 1700000001, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)
	seqRes, _ := runSequentialScenario(t, pre, env, txs, chainID)

	// Điểm đồng bộ tất định: buộc B đọc xong slot 0 (=10) ở attempt 1 TRƯỚC khi A giao kết quả để chốt 11!
	bReadDone := make(chan struct{})
	var once sync.Once
	hooks := &OCCHooks{
		OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
			if txIndex == 1 && attempt == 1 && addr == contract && slot == (common.Hash{}) {
				if val != common.BigToHash(big.NewInt(10)) {
					t.Errorf("expected tx B attempt 1 to read stale value 10, got %s", val.Hex())
				}
				once.Do(func() { close(bReadDone) })
			}
		},
		OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
			if txIndex == 0 && attempt == 1 {
				select {
				case <-bReadDone:
				case <-ctx.Done():
				}
			}
		},
	}

	res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy:      CheckpointPolicyFixed,
		Interval:    64,
		BudgetBytes: DefaultCheckpointBudgetBytes,
		UseBudget:   true,
	}, hooks, true)
	if err != nil {
		t.Fatalf("checkpoint-late failed: %v", err)
	}
	assertBlockMatchesT8N(t, res, post, oracle)
	if res.StateRoot != seqRes.StateRoot {
		t.Fatalf("stateRoot mismatch vs sequential")
	}
	if post.Accounts[contract].Storage[common.Hash{}] != common.BigToHash(big.NewInt(12)) {
		t.Fatalf("expected final count=12, got %s", post.Accounts[contract].Storage[common.Hash{}].Hex())
	}
	m := res.Phase2Metrics
	if m == nil || m.ResumeCommitted != 1 || m.PrefixStepsSaved == 0 || m.AccountedRemainingBytes != 0 {
		t.Fatalf("expected 1 committed resume with saved prefix steps and 0 remaining bytes, got %+v", m)
	}
	// Xác nhận trace nối conflict -> checkpoint_selected -> resume_start -> commit(resumed=true) cho tx 1
	var sawConflict, sawSelected, sawStart, sawResumedCommit bool
	for _, ev := range res.Phase2Trace {
		if ev.TxIndex != 1 {
			continue
		}
		switch ev.Event {
		case "conflict":
			sawConflict = true
		case "checkpoint_selected":
			sawSelected = true
		case "resume_start":
			sawStart = true
		case "commit":
			if ev.Resumed && ev.Attempt == 2 && ev.ResumedFromStep > 0 {
				sawResumedCommit = true
			}
		}
	}
	if !sawConflict || !sawSelected || !sawStart || !sawResumedCommit {
		t.Fatalf("incomplete resume trace for tx 1: conflict=%v selected=%v start=%v commit=%v", sawConflict, sawSelected, sawStart, sawResumedCommit)
	}
}

// --- TEST 3: checkpoint-early (đọc trước băm -> ngừng chụp sau lần đọc) ---

func TestPhase2CheckpointEarlyDisarmsAfterFirstRead(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c003")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee3")
	keyA, addrA := testPhase2Key(t, 4)
	keyB, addrB := testPhase2Key(t, 5)

	// SLOAD(0) nằm ở step 1 (ngay sau PUSH1 0), rồi mới băm 32 vòng
	code := buildEarlyProbeCode(32, 0)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	txs := []*types.Transaction{txA, txB}
	env := BlockEnv{Height: 1, Time: 1700000002, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)

	bReadDone := make(chan struct{})
	var once sync.Once
	hooks := &OCCHooks{
		OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
			if txIndex == 1 && attempt == 1 && addr == contract {
				once.Do(func() { close(bReadDone) })
			}
		},
		OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
			if txIndex == 0 && attempt == 1 {
				select {
				case <-bReadDone:
				case <-ctx.Done():
				}
			}
		},
	}

	// Dùng N=1 để nếu disarm hỏng sẽ lập tức tạo hàng trăm checkpoint trong vòng băm sau SLOAD!
	res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy:      CheckpointPolicyFixed,
		Interval:    1,
		BudgetBytes: DefaultCheckpointBudgetBytes,
		UseBudget:   true,
	}, hooks, true)
	if err != nil {
		t.Fatalf("checkpoint-early failed: %v", err)
	}
	assertBlockMatchesT8N(t, res, post, oracle)

	// SLOAD ở step 1 -> mỗi tx chỉ được chụp tối đa 2 điểm (step 0 trước PUSH1 và step 1 trước SLOAD); TUYỆT ĐỐI KHÔNG chụp sau step 1!
	for _, ev := range res.Phase2Trace {
		if ev.Event == "capture_created" && ev.Step > 1 {
			t.Fatalf("checkpoint created AFTER first SLOAD (step %d, op=%s) in checkpoint-early!", ev.Step, ev.Opcode)
		}
	}
	if res.Phase2Metrics.PrefixStepsSaved > 1 {
		t.Fatalf("checkpoint-early must not skip hashing after SLOAD, got PrefixStepsSaved=%d", res.Phase2Metrics.PrefixStepsSaved)
	}
}

// --- TEST 4 & 5: CỔNG QUYẾT ĐỊNH LỰA CHỌN 1 (checkpoint-sstore-gas + OOG lần đầu 0->7, PASS 5->7 đối chiếu evm t8n) ---

func TestPhase2Option1GateSStoreGasAndInitialOOGMatchesT8N(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c004")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee4")
	keyA, addrA := testPhase2Key(t, 6)
	keyB, addrB := testPhase2Key(t, 7)

	// Bytecode KHÔNG có bất kỳ SLOAD nào trước SSTORE(0, calldata[0])!
	code := buildDirectSStoreCode(24, 0)
	pre := makePreStateWithContract(contract, code, nil, coinbase, addrA, addrB) // slot 0 ban đầu = 0
	env := BlockEnv{Height: 1, Time: 1700000003, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	// Ca 4: checkpoint-sstore-gas (đủ gas cho cả 0->7 và 5->7, kiểm phát hiện phụ thuộc gas ẩn + tính lại đúng gas)
	t.Run("checkpoint-sstore-gas", func(t *testing.T) {
		txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(5)).Bytes(), nil)
		txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(7)).Bytes(), nil)
		txs := []*types.Transaction{txA, txB}

		oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)
		seqRes, _ := runSequentialScenario(t, pre, env, txs, chainID)

		// Buộc B chạy xong trên nền trước A (slot 0 == 0) trước khi A giao kết quả (slot 0 == 5)
		bDone := make(chan struct{})
		var once sync.Once
		hooks := &OCCHooks{
			OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
				if txIndex == 1 && attempt == 1 {
					once.Do(func() { close(bDone) })
				}
				if txIndex == 0 && attempt == 1 {
					select {
					case <-bDone:
					case <-ctx.Done():
					}
				}
			},
		}

		res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
			Policy:      CheckpointPolicyFixed,
			Interval:    1,
			BudgetBytes: DefaultCheckpointBudgetBytes,
			UseBudget:   true,
		}, hooks, true)
		if err != nil {
			t.Fatalf("checkpoint-sstore-gas failed: %v", err)
		}
		assertBlockMatchesT8N(t, res, post, oracle)
		if res.StateRoot != seqRes.StateRoot {
			t.Fatalf("stateRoot mismatch vs sequential")
		}
		// B phải bị xung đột dù KHÔNG có SLOAD tường minh (do SSTORE dynamic gas đọc slot 0) và resume ngay trước SSTORE!
		if res.OCCStats.RetriedTxs != 1 {
			t.Fatalf("expected tx B to detect hidden SSTORE gas dependency (RetriedTxs=1), got %+v", res.OCCStats)
		}
		if res.Phase2Metrics.ResumeCommitted != 1 || res.Phase2Metrics.PrefixStepsSaved == 0 {
			t.Fatalf("expected tx B to resume right before SSTORE and skip hash prefix, got %+v", res.Phase2Metrics)
		}
		// Xác nhận B trả gas nhỏ hơn A đúng 17_100 (SSTORE 5->7 = 5000 vs 0->5 = 22100)!
		if res.Results[0].GasUsed-res.Results[1].GasUsed != 17_100 {
			t.Fatalf("expected gasA - gasB == 17100, got gasA=%d gasB=%d diff=%d",
				res.Results[0].GasUsed, res.Results[1].GasUsed, res.Results[0].GasUsed-res.Results[1].GasUsed)
		}
		t.Logf("OPTION 1 GATE [checkpoint-sstore-gas]: PASS! gasA=%d gasB=%d (diff=%d) resumeCommitted=%d prefixStepsSaved=%d",
			res.Results[0].GasUsed, res.Results[1].GasUsed, res.Results[0].GasUsed-res.Results[1].GasUsed,
			res.Phase2Metrics.ResumeCommitted, res.Phase2Metrics.PrefixStepsSaved)
	})

	// Ca 5: Hết gas lần đầu (0->7 OOG, 5->7 PASS) — xác định ngưỡng gas bằng chính evm t8n!
	t.Run("initial-attempt-oog-then-resume-pass", func(t *testing.T) {
		txAProbe := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(5)).Bytes(), nil)
		txBProbe := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(7)).Bytes(), nil)
		probeOracle := runEVMT8NOracle(t, evmBin, pre, env, []*types.Transaction{txAProbe, txBProbe}, chainID)
		gas0to5 := uint64(probeOracle.Receipts[0].GasUsed)
		gas5to7 := uint64(probeOracle.Receipts[1].GasUsed)

		// Chọn gasLimit của B = gas5to7 + 3000 (< gas0to5):
		// Trên nền stale (slot 0 == 0), B cần gas0to5 nên CHẮC CHẮN OOG tại SSTORE!
		// Trên nền sau A (slot 0 == 5), B chỉ cần gas5to7 nên THÀNH CÔNG!
		tightGasB := gas5to7 + 3_000
		if tightGasB >= gas0to5 {
			t.Fatalf("unexpected gas margin: gas5to7=%d tightGasB=%d gas0to5=%d", gas5to7, tightGasB, gas0to5)
		}

		// Kiểm chứng độc lập với t8n: nếu B chạy một mình trên slot 0 == 0 thì B thực sự OOG (status=0)
		txBSolo := signType2Tx(t, chainID, keyB, 0, contract, tightGasB, common.BigToHash(big.NewInt(7)).Bytes(), nil)
		soloOracle := runEVMT8NOracle(t, evmBin, pre, env, []*types.Transaction{txBSolo}, chainID)
		if uint64(soloOracle.Receipts[0].Status) != types.ReceiptStatusFailed {
			t.Fatalf("expected solo tx B on 0->7 with gas=%d to OOG, got status=%d", tightGasB, soloOracle.Receipts[0].Status)
		}

		// Giờ chạy block [A, B] với tightGasB: trên t8n cả A và B đều thành công!
		txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(5)).Bytes(), nil)
		txB := signType2Tx(t, chainID, keyB, 0, contract, tightGasB, common.BigToHash(big.NewInt(7)).Bytes(), nil)
		txs := []*types.Transaction{txA, txB}
		oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)
		if uint64(oracle.Receipts[1].Status) != types.ReceiptStatusSuccessful {
			t.Fatalf("expected tx B after A (5->7) to succeed on t8n, got status=%d", oracle.Receipts[1].Status)
		}

		// Buộc B chạy thử attempt 1 trên nền trước A (0->7 -> OOG!) trước khi A giao kết quả
		bDone := make(chan struct{})
		var once sync.Once
		hooks := &OCCHooks{
			OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
				if txIndex == 1 && attempt == 1 {
					once.Do(func() { close(bDone) })
				}
				if txIndex == 0 && attempt == 1 {
					select {
					case <-bDone:
					case <-ctx.Done():
					}
				}
			},
		}

		res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
			Policy:      CheckpointPolicyFixed,
			Interval:    1,
			BudgetBytes: DefaultCheckpointBudgetBytes,
			UseBudget:   true,
		}, hooks, true)
		if err != nil {
			t.Fatalf("initial-OOG resume failed: %v", err)
		}
		assertBlockMatchesT8N(t, res, post, oracle)
		if res.Results[1].Failed {
			t.Fatalf("expected resumed tx B to succeed, got failed: %s", res.Results[1].ErrMsg)
		}
		if res.Phase2Metrics.ResumeCommitted != 1 || res.Phase2Metrics.PrefixStepsSaved == 0 {
			t.Fatalf("expected tx B to resume from pre-SSTORE checkpoint even after attempt 1 OOG, got %+v", res.Phase2Metrics)
		}
		t.Logf("OPTION 1 GATE [initial-OOG 0->7 -> resume 5->7 PASS]: PASS! tightGasB=%d gasUsedB=%d resumeCommitted=%d prefixStepsSaved=%d",
			tightGasB, res.Results[1].GasUsed, res.Phase2Metrics.ResumeCommitted, res.Phase2Metrics.PrefixStepsSaved)
	})
}

// --- TEST 6: Warm/cold + original storage refund + mở rộng memory trước & sau điểm lưu (có và không có access list) ---

func TestPhase2WarmColdOriginalRefundAndMemoryExpansionMatchesT8N(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c006")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee6")
	keyA, addrA := testPhase2Key(t, 8)
	keyB, addrB := testPhase2Key(t, 9)

	code := buildMemExpandWarmRefundCode(16)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	env := BlockEnv{Height: 1, Time: 1700000006, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	for _, withAL := range []bool{false, true} {
		name := "no-access-list"
		var al types.AccessList
		if withAL {
			name = "with-access-list"
			al = types.AccessList{{Address: contract, StorageKeys: []common.Hash{common.BigToHash(big.NewInt(0))}}}
		}
		t.Run(name, func(t *testing.T) {
			// A ghi slot 0 -> 15; B ghi 0x42 rồi đặt về 15 (khớp original sau A -> kích hoạt EIP-2200/3529 refund!)
			txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(15)).Bytes(), nil)
			txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(15)).Bytes(), al)
			txs := []*types.Transaction{txA, txB}
			oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)

			bDone := make(chan struct{})
			var once sync.Once
			hooks := &OCCHooks{
				OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
					if txIndex == 1 && attempt == 1 {
						once.Do(func() { close(bDone) })
					}
					if txIndex == 0 && attempt == 1 {
						select {
						case <-bDone:
						case <-ctx.Done():
						}
					}
				},
			}

			res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
				Policy:      CheckpointPolicyFixed,
				Interval:    1,
				BudgetBytes: DefaultCheckpointBudgetBytes,
				UseBudget:   true,
			}, hooks, true)
			if err != nil {
				t.Fatalf("warm/cold/refund/mem failed: %v", err)
			}
			assertBlockMatchesT8N(t, res, post, oracle)
			if res.Phase2Metrics.ResumeCommitted != 1 || res.Phase2Metrics.PrefixStepsSaved == 0 {
				t.Fatalf("expected resumed commit, got %+v", res.Phase2Metrics)
			}
		})
	}
}

// --- TEST 7: Ghi + log + REVERT sau điểm lưu (giữ thay đổi A, mất ghi/log B, đúng phí/nonce, ngừng chụp sau ghi/log) ---

func TestPhase2WriteLogAndRevertAfterCheckpointMatchesT8N(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c007")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee7")
	keyA, addrA := testPhase2Key(t, 10)
	keyB, addrB := testPhase2Key(t, 11)

	code := buildWriteLogRevertAfterCheckpointCode(16)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	env := BlockEnv{Height: 1, Time: 1700000007, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	txs := []*types.Transaction{txA, txB}
	oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)

	bDone := make(chan struct{})
	var once sync.Once
	hooks := &OCCHooks{
		OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
			if txIndex == 1 && attempt == 1 {
				once.Do(func() { close(bDone) })
			}
			if txIndex == 0 && attempt == 1 {
				select {
				case <-bDone:
				case <-ctx.Done():
				}
			}
		},
	}

	res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy:      CheckpointPolicyFixed,
		Interval:    1,
		BudgetBytes: DefaultCheckpointBudgetBytes,
		UseBudget:   true,
	}, hooks, true)
	if err != nil {
		t.Fatalf("write/log/revert failed: %v", err)
	}
	assertBlockMatchesT8N(t, res, post, oracle)
	if !res.Results[1].Failed || len(res.Results[1].Logs) != 0 {
		t.Fatalf("expected tx B to revert with 0 logs, got %+v", res.Results[1])
	}
	// A giữ slot 0 == 11, ghi slot 1 của B bị hoàn tác (== 0), nonce B tăng lên 1
	if post.Accounts[contract].Storage[common.BigToHash(big.NewInt(0))] != common.BigToHash(big.NewInt(11)) {
		t.Fatalf("A's slot 0 lost after B revert")
	}
	if post.Accounts[contract].Storage[common.BigToHash(big.NewInt(1))] != (common.Hash{}) {
		t.Fatalf("B's reverted write on slot 1 leaked")
	}
	if post.Accounts[addrB].Nonce != 1 {
		t.Fatalf("expected B nonce=1 after revert, got %d", post.Accounts[addrB].Nonce)
	}
	if res.Phase2Metrics.ResumeCommitted != 1 || res.Phase2Metrics.AccountedRemainingBytes != 0 {
		t.Fatalf("expected B to resume cleanly and revert, got %+v", res.Phase2Metrics)
	}
	// Xác nhận không có checkpoint nào được tạo sau SSTORE/LOG0
	for _, ev := range res.Phase2Trace {
		if ev.Event == "capture_created" && (ev.Opcode == "LOG0" || ev.Opcode == "REVERT") {
			t.Fatalf("unexpected checkpoint created after state write/log: %+v", ev)
		}
	}
}

// --- TEST 8: Gặp CALL ở suffix -> hủy resume, trả đủ gas pool cho tx C đúng ngưỡng Geth, FullReplay thành công ---

func TestPhase2SuffixCallCancelsResumeAndPreservesBlockGasPoolForTxC(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	helper := common.HexToAddress("0x000000000000000000000000000000000000c080")
	contract := common.HexToAddress("0x000000000000000000000000000000000000c008")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee8")
	keyA, addrA := testPhase2Key(t, 12)
	keyB, addrB := testPhase2Key(t, 13)
	keyC, addrC := testPhase2Key(t, 14)

	helperCode := []byte{0x60, 0x01, 0x5f, 0x52, 0x60, 0x20, 0x5f, 0xf3} // RETURN(1)
	mainCode := buildSuffixCallBranchCode(16, helper)
	pre := makePreStateWithContract(contract, mainCode, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB, addrC)
	pre.Accounts[helper] = &LogicalAccount{Nonce: 1, Balance: uint256.NewInt(0), Code: helperCode, Storage: map[common.Hash]common.Hash{}}

	txAProbe := signType2Tx(t, chainID, keyA, 0, contract, 300_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txBProbe := signType2Tx(t, chainID, keyB, 0, contract, 300_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	probeEnv := BlockEnv{Height: 1, Time: 1700000008, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	probeOracle := runEVMT8NOracle(t, evmBin, pre, probeEnv, []*types.Transaction{txAProbe, txBProbe}, chainID)

	// Chọn gasLimit chặt cho A và B để cả hai lọt blockGasLimit, đồng thời sau A+B còn đúng 21_000 gas cho C!
	gasUsedA := uint64(probeOracle.Receipts[0].GasUsed)
	gasUsedB := uint64(probeOracle.Receipts[1].GasUsed)
	txA := signType2Tx(t, chainID, keyA, 0, contract, gasUsedA+5_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, gasUsedB+10_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	exactGasC := uint64(21_000)
	exactBlockGasLimit := gasUsedA + gasUsedB + exactGasC
	txCExact := signType2Tx(t, chainID, keyC, 0, coinbase, exactGasC, nil, nil)

	t.Run("tx-C-exact-remaining-gas-accepted", func(t *testing.T) {
		env := BlockEnv{Height: 1, Time: 1700000008, Coinbase: coinbase, GasLimit: exactBlockGasLimit, BaseFee: big.NewInt(1)}
		txs := []*types.Transaction{txA, txB, txCExact}
		oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)

		bDone := make(chan struct{})
		var once sync.Once
		hooks := &OCCHooks{
			OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
				if txIndex == 1 && attempt == 1 {
					once.Do(func() { close(bDone) })
				}
				if txIndex == 0 && attempt == 1 {
					select {
					case <-bDone:
					case <-ctx.Done():
					}
				}
			},
		}

		res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
			Policy:      CheckpointPolicyFixed,
			Interval:    1,
			BudgetBytes: DefaultCheckpointBudgetBytes,
			UseBudget:   true,
		}, hooks, true)
		if err != nil {
			t.Fatalf("suffix CALL cancel + tx C exact gas failed: %v", err)
		}
		assertBlockMatchesT8N(t, res, post, oracle)
		m := res.Phase2Metrics
		if m.ResumeCancelled != 1 || m.FallbackReasons["unsupported_suffix_opcode"] != 1 {
			t.Fatalf("expected 1 cancelled resume with unsupported_suffix_opcode, got %+v", m)
		}
		if m.AccountedRemainingBytes != 0 {
			t.Fatalf("expected 0 remaining bytes, got %d", m.AccountedRemainingBytes)
		}
	})

	t.Run("tx-C-exceeds-remaining-gas-by-1-rejected", func(t *testing.T) {
		env := BlockEnv{Height: 1, Time: 1700000008, Coinbase: coinbase, GasLimit: exactBlockGasLimit, BaseFee: big.NewInt(1)}
		txCOver := signType2Tx(t, chainID, keyC, 0, coinbase, exactGasC+1, nil, nil)
		txs := []*types.Transaction{txA, txB, txCOver}

		oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)
		if len(oracle.Rejected) != 1 || oracle.Rejected[0].Index != 2 {
			t.Fatalf("expected t8n to reject tx 2 (gas limit reached), got %+v", oracle.Rejected)
		}

		_, _, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
			Policy:      CheckpointPolicyFixed,
			Interval:    1,
			BudgetBytes: DefaultCheckpointBudgetBytes,
			UseBudget:   true,
		}, nil, false)
		var rej *TxRejectedError
		if !errors.As(err, &rej) || rej.Index != 2 || rej.Category != "gas_limit_reached" {
			t.Fatalf("expected TxRejectedError(index=2, gas_limit_reached), got %v", err)
		}
	})
}

// --- TEST 9: Prefix từng qua CALL -> disarm không chụp tiếp ---

func TestPhase2PrefixWithCallDisarmsCaptureAfterSubcallReturns(t *testing.T) {
	const chainID = 1337
	evmBin := resolveTestEVMBin(t)
	helper := common.HexToAddress("0x000000000000000000000000000000000000c090")
	contract := common.HexToAddress("0x000000000000000000000000000000000000c009")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee9")
	keyA, addrA := testPhase2Key(t, 15)
	keyB, addrB := testPhase2Key(t, 16)

	helperCode := []byte{0x60, 0x01, 0x5f, 0x52, 0x60, 0x20, 0x5f, 0xf3}
	// STATICCALL ở step 6 -> sau đó mới băm 20 vòng -> SLOAD/SSTORE
	mainCode := buildPrefixCallThenHashCode(20, helper)
	pre := makePreStateWithContract(contract, mainCode, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	pre.Accounts[helper] = &LogicalAccount{Nonce: 1, Balance: uint256.NewInt(0), Code: helperCode, Storage: map[common.Hash]common.Hash{}}

	txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	txs := []*types.Transaction{txA, txB}
	env := BlockEnv{Height: 1, Time: 1700000009, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	oracle := runEVMT8NOracle(t, evmBin, pre, env, txs, chainID)

	res, post, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy:      CheckpointPolicyFixed,
		Interval:    1,
		BudgetBytes: DefaultCheckpointBudgetBytes,
		UseBudget:   true,
	}, nil, true)
	if err != nil {
		t.Fatalf("prefix CALL disarm failed: %v", err)
	}
	assertBlockMatchesT8N(t, res, post, oracle)

	// STATICCALL nằm ở step 6 (0..5 là PUSH0*4, PUSH20, GAS; step 6 là STATICCALL).
	// TUYỆT ĐỐI KHÔNG được tạo bất kỳ checkpoint nào ở step > 6 sau khi lời gọi con trả về frame gốc!
	for _, ev := range res.Phase2Trace {
		if ev.Event == "capture_created" && ev.Step > 6 {
			t.Fatalf("checkpoint captured AFTER STATICCALL returned to root frame: %+v", ev)
		}
	}
}

// --- TEST 10: Cạnh tranh CheckpointStore nhiều worker, accounting breakdown, overflow, M=0 / M<1 / M đủ ---

func TestCheckpointStoreAccountingOverflowAndConcurrency(t *testing.T) {
	// 1. Kiểm tra từng thành phần hạch toán & làm tròn 8 byte
	total, bd, err := EstimatePureCheckpointBytes(10, 33, 5, 4)
	if err != nil {
		t.Fatal(err)
	}
	if bd.StackBytes != 10*32 {
		t.Fatalf("StackBytes=%d want %d", bd.StackBytes, 320)
	}
	if bd.MemoryBytes != 40 { // align8(33) == 40
		t.Fatalf("MemoryBytes=%d want 40", bd.MemoryBytes)
	}
	if bd.ReturnDataBytes != 8 { // align8(5) == 8
		t.Fatalf("ReturnDataBytes=%d want 8", bd.ReturnDataBytes)
	}
	if bd.StructOverheadBytes == 0 || bd.StoreOverheadBytes != checkpointStoreEntryOverheadBytes {
		t.Fatalf("unexpected overhead breakdown: %+v", bd)
	}
	sum := bd.StructOverheadBytes + bd.StoreOverheadBytes + bd.StackBytes + bd.MemoryBytes + bd.ReturnDataBytes + bd.PreReadsSliceBytes + bd.PreReadsMapBytes
	if total != sum {
		t.Fatalf("total=%d != sum=%d", total, sum)
	}

	// 2. Kiểm tra tràn uint64
	if _, _, err := EstimatePureCheckpointBytesUint64(math.MaxUint64/16, 0, 0, 0); !errors.Is(err, ErrCheckpointSizeOverflow) {
		t.Fatalf("expected overflow for huge stackLen, got %v", err)
	}
	if _, _, err := EstimatePureCheckpointBytesUint64(0, math.MaxUint64, 0, 0); !errors.Is(err, ErrCheckpointSizeOverflow) {
		t.Fatalf("expected overflow for MaxUint64 memLen, got %v", err)
	}

	// 3. Kiểm tra M = 0 không bao giờ cấp chỗ
	zeroStore := NewCheckpointStore(0)
	if _, ok := zeroStore.Reserve(100); ok {
		t.Fatal("M=0 store unexpectedly granted reservation")
	}
	if used, peak, _, _ := zeroStore.Stats(); used != 0 || peak != 0 {
		t.Fatalf("M=0 store stats non-zero: used=%d peak=%d", used, peak)
	}

	// 4. Stress test cạnh tranh nhiều goroutine (kiểm tra với -race)
	const budget uint64 = 4096
	const itemSize uint64 = 512
	st := NewCheckpointStore(budget)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		txIdx := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iter := 0; iter < 100; iter++ {
				id, ok := st.Reserve(itemSize)
				if !ok {
					continue
				}
				if iter%5 == 0 {
					st.CancelReservation(id)
					continue
				}
				cp := &PurePrefixCheckpoint{TxIndex: txIdx, NextStep: uint64(iter), AccountedBytes: itemSize}
				if err := st.CommitReservation(id, txIdx, cp); err != nil {
					t.Errorf("CommitReservation failed: %v", err)
				}
				if iter%3 == 0 {
					st.ReleaseTx(txIdx)
				}
			}
			st.ReleaseTx(txIdx)
		}()
	}
	wg.Wait()
	used, peak, _, skipped := st.Stats()
	if peak > budget {
		t.Fatalf("budget exceeded: peak=%d > budget=%d", peak, budget)
	}
	if used != 0 {
		t.Fatalf("leaked bytes after all workers finished: used=%d", used)
	}
	if skipped == 0 {
		t.Fatalf("expected contention to trigger budget skips")
	}
}

func TestPhase2BudgetLimitsInOCCZeroTooSmallAndExact(t *testing.T) {
	const chainID = 1337
	contract := common.HexToAddress("0x000000000000000000000000000000000000c00a")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000feea")
	keyA, addrA := testPhase2Key(t, 17)
	keyB, addrB := testPhase2Key(t, 18)

	code := buildLateProbeCode(20, 0)
	pre := makePreStateWithContract(contract, code, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	txA := signType2Tx(t, chainID, keyA, 0, contract, 500_000, common.BigToHash(big.NewInt(1)).Bytes(), nil)
	txB := signType2Tx(t, chainID, keyB, 0, contract, 500_000, common.BigToHash(big.NewInt(2)).Bytes(), nil)
	txs := []*types.Transaction{txA, txB}
	env := BlockEnv{Height: 1, Time: 1700000010, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	seqRes, _ := runSequentialScenario(t, pre, env, txs, chainID)

	// M = 0 -> hành xử như off
	res0, _, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy: CheckpointPolicyFixed, Interval: 16, BudgetBytes: 0, UseBudget: true,
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if res0.StateRoot != seqRes.StateRoot {
		t.Fatal("M=0 stateRoot mismatch")
	}
	if res0.Phase2Metrics.CaptureCreated != 0 || res0.Phase2Metrics.AccountedPeakBytes != 0 || res0.Phase2Metrics.AccountedRemainingBytes != 0 {
		t.Fatalf("M=0 expected 0 created/peak/rem, got %+v", res0.Phase2Metrics)
	}

	// M = 64 byte (< kích thước 1 checkpoint) -> bỏ hết do hết ngân sách, vẫn khớp sequential
	resSmall, _, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy: CheckpointPolicyFixed, Interval: 16, BudgetBytes: 64, UseBudget: true,
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if resSmall.StateRoot != seqRes.StateRoot {
		t.Fatal("M<1cp stateRoot mismatch")
	}
	if resSmall.Phase2Metrics.CaptureCreated != 0 || resSmall.Phase2Metrics.CaptureSkippedBudget == 0 ||
		resSmall.Phase2Metrics.AccountedPeakBytes != 0 || resSmall.Phase2Metrics.AccountedRemainingBytes != 0 {
		t.Fatalf("M=64 expected skipped>0 and created=0, got %+v", resSmall.Phase2Metrics)
	}

	// M = 2048 byte (vừa đủ cho 1–2 điểm) -> peak <= 2048 và remaining == 0
	const tightBudget uint64 = 2048
	resTight, _, err := runPhase2Scenario(t, pre, env, txs, chainID, 2, CheckpointConfig{
		Policy: CheckpointPolicyFixed, Interval: 16, BudgetBytes: tightBudget, UseBudget: true,
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if resTight.StateRoot != seqRes.StateRoot {
		t.Fatal("M=2048 stateRoot mismatch")
	}
	if resTight.Phase2Metrics.CaptureCreated == 0 || resTight.Phase2Metrics.AccountedPeakBytes > tightBudget || resTight.Phase2Metrics.AccountedRemainingBytes != 0 {
		t.Fatalf("M=2048 violated budget invariant: %+v", resTight.Phase2Metrics)
	}
}

// --- TEST 11: history học từ block 1 sang block 2 và đổi slot nóng ở block 3 (chứng minh không nhìn trước tương lai) ---

func TestPhase2HistoryCrossBlockLearningAndNoLookahead(t *testing.T) {
	const chainID = 1337
	contract0 := common.HexToAddress("0x000000000000000000000000000000000000c010")
	contract1 := common.HexToAddress("0x000000000000000000000000000000000000c011")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000feeb")
	keyA, addrA := testPhase2Key(t, 19)
	keyB, addrB := testPhase2Key(t, 20)

	codeSlot0 := buildLateProbeCode(24, 0)
	codeSlot1 := buildLateProbeCode(24, 1)
	pre := makePreStateWithContract(contract0, codeSlot0, map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)): common.BigToHash(big.NewInt(10)),
	}, coinbase, addrA, addrB)
	pre.Accounts[contract1] = &LogicalAccount{
		Nonce:   1,
		Balance: uint256.NewInt(0),
		Code:    codeSlot1,
		Storage: map[common.Hash]common.Hash{common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(20))},
	}

	db, err := store.NewPebbleDB("history-3blk", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(pre); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	chainCfg := shanghaiTestChainConfig(chainID)
	exec := NewParallelExecutor(chainCfg, new(big.Int).SetUint64(chainID), 2)
	exec.SlidingWindow = true
	exec.StrictInvalidTx = true
	exec.Phase2Mode = true
	exec.RecordTrace = true
	exec.Checkpoints = CheckpointConfig{
		Policy:      CheckpointPolicyHistory,
		Interval:    64,
		BudgetBytes: DefaultCheckpointBudgetBytes,
		UseBudget:   true,
	}

	makeSyncHooks := func() *OCCHooks {
		bDone := make(chan struct{})
		var once sync.Once
		return &OCCHooks{
			OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
				if txIndex == 1 && attempt == 1 {
					once.Do(func() { close(bDone) })
				}
				if txIndex == 0 && attempt == 1 {
					select {
					case <-bDone:
					case <-ctx.Done():
					}
				}
			},
		}
	}

	runBlk := func(height int64, target common.Address, nonceA, nonceB uint64) *BlockResult {
		exec.OCCHooks = makeSyncHooks()
		txA := signType2Tx(t, chainID, keyA, nonceA, target, 500_000, common.BigToHash(big.NewInt(height*10+1)).Bytes(), nil)
		txB := signType2Tx(t, chainID, keyB, nonceB, target, 500_000, common.BigToHash(big.NewInt(height*10+2)).Bytes(), nil)
		env := BlockEnv{Height: height, Time: uint64(1700000100 + height), Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
		sdb := NewPebbleStateDB(db)
		res, err := exec.ExecuteBlock(sdb, env, []*types.Transaction{txA, txB})
		if err != nil {
			t.Fatalf("block %d failed: %v", height, err)
		}
		if err := sdb.Commit(); err != nil {
			t.Fatal(err)
		}
		return res
	}

	slot0Key := MVKey{Address: contract0, Kind: MVStorage, Slot: common.BigToHash(big.NewInt(0))}
	slot1Key := MVKey{Address: contract1, Kind: MVStorage, Slot: common.BigToHash(big.NewInt(1))}

	// Block 1: xung đột ở (contract0, slot 0) -> chưa có lịch sử nên dùng fixed, sau block 1 học được slot0Key
	b1 := runBlk(1, contract0, 0, 0)
	if b1.Phase2Metrics.EffectivePolicy != "fixed" {
		t.Fatalf("block 1 expected effectivePolicy=fixed, got %s", b1.Phase2Metrics.EffectivePolicy)
	}
	hotAfter1 := exec.HotSlots()
	if len(hotAfter1) != 1 || hotAfter1[0] != slot0Key {
		t.Fatalf("expected hotSlots=[%s] after block 1, got %v", slot0Key, hotAfter1)
	}

	// Block 2: tiếp tục xung đột ở (contract0, slot 0) -> history chụp ĐÚNG trước SLOAD(slot 0) và resume thành công!
	b2 := runBlk(2, contract0, 1, 1)
	if b2.Phase2Metrics.EffectivePolicy != "history" {
		t.Fatalf("block 2 expected effectivePolicy=history, got %s", b2.Phase2Metrics.EffectivePolicy)
	}
	if b2.Phase2Metrics.CaptureCreated != 2 || b2.Phase2Metrics.ResumeCommitted != 1 {
		t.Fatalf("block 2 expected 2 history captures (1 per tx right before SLOAD) and 1 resume, got %+v", b2.Phase2Metrics)
	}
	for _, ev := range b2.Phase2Trace {
		if ev.Event == "capture_created" && (ev.Reason != "history" || ev.Opcode != "SLOAD" || ev.SlotKey != slot0Key.String()) {
			t.Fatalf("unexpected block 2 history capture event: %+v", ev)
		}
	}

	// Block 3: đổi sang xung đột ở (contract1, slot 1) -> lịch sử trong Block 3 VẪN là [slot0Key] (không nhìn trước tương lai!)
	b3 := runBlk(3, contract1, 2, 2)
	if b3.Phase2Metrics.EffectivePolicy != "history" {
		t.Fatalf("block 3 expected effectivePolicy=history, got %s", b3.Phase2Metrics.EffectivePolicy)
	}
	if b3.Phase2Metrics.CaptureCreated != 0 || b3.Phase2Metrics.ResumeCommitted != 0 {
		t.Fatalf("block 3 must NOT look ahead into slot1Key (expected 0 captures), got %+v", b3.Phase2Metrics)
	}
	hotAfter3 := exec.HotSlots()
	if len(hotAfter3) != 1 || hotAfter3[0] != slot1Key {
		t.Fatalf("expected hotSlots=[%s] after block 3, got %v", slot1Key, hotAfter3)
	}

	// Block 4: giờ đã học được slot1Key -> chụp và resume thành công trên (contract1, slot 1)!
	b4 := runBlk(4, contract1, 3, 3)
	if b4.Phase2Metrics.CaptureCreated != 2 || b4.Phase2Metrics.ResumeCommitted != 1 {
		t.Fatalf("block 4 expected history to capture & resume on learned slot1Key, got %+v", b4.Phase2Metrics)
	}

	// Block 5: chỉ 1 tx (0 xung đột) -> sau block 5 danh sách nóng phải rỗng; block 6 không chụp điểm nào
	exec.OCCHooks = nil
	txSolo := signType2Tx(t, chainID, keyA, 4, contract1, 500_000, common.BigToHash(big.NewInt(51)).Bytes(), nil)
	env5 := BlockEnv{Height: 5, Time: 1700000105, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	sdb5 := NewPebbleStateDB(db)
	if _, err := exec.ExecuteBlock(sdb5, env5, []*types.Transaction{txSolo}); err != nil {
		t.Fatalf("block 5 failed: %v", err)
	}
	if err := sdb5.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(exec.HotSlots()) != 0 {
		t.Fatalf("expected empty hotSlots after zero-conflict block 5, got %v", exec.HotSlots())
	}
	b6 := runBlk(6, contract1, 5, 4)
	if b6.Phase2Metrics.EffectivePolicy != "history" || b6.Phase2Metrics.CaptureCreated != 0 {
		t.Fatalf("block 6 after zero-conflict block expected 0 captures, got %+v", b6.Phase2Metrics)
	}
}
