package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

const defaultT8NTimeout = 30 * time.Second

// T8NRejectedTx phản ánh một mục trong mảng `rejected` do `evm t8n` trả ra.
type T8NRejectedTx struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// T8NBlockResult giữ đầy đủ đầu ra của một lần chạy `evm t8n` độc lập.
type T8NBlockResult struct {
	StateRoot     common.Hash
	TxRoot        common.Hash
	ReceiptsRoot  common.Hash
	LogsHash      common.Hash
	LogsBloom     types.Bloom
	GasUsed       uint64
	Receipts      []*TxReceipt
	Rejected      []T8NRejectedTx
	PostState     *evm.LogicalState
	RawResultJSON []byte
	Stderr        string
}

type t8nResultJSON struct {
	StateRoot    *common.Hash         `json:"stateRoot"`
	TxRoot       *common.Hash         `json:"txRoot"`
	ReceiptsRoot *common.Hash         `json:"receiptsRoot"`
	LogsHash     *common.Hash         `json:"logsHash"`
	LogsBloom    *types.Bloom         `json:"logsBloom"`
	Receipts     []json.RawMessage    `json:"receipts"`
	Rejected     []T8NRejectedTx      `json:"rejected,omitempty"`
	GasUsed      *math.HexOrDecimal64 `json:"gasUsed"`
}

// ResolveEVMBin tìm binary `evm` theo ưu tiên: tham số tường minh -> biến môi trường EVM_BIN -> PATH.
func ResolveEVMBin(explicit string) (string, error) {
	candidate := strings.TrimSpace(explicit)
	if candidate == "" {
		candidate = strings.TrimSpace(os.Getenv("EVM_BIN"))
	}
	if candidate == "" {
		p, err := exec.LookPath("evm")
		if err != nil {
			return "", fmt.Errorf("evm binary not found (set EVM_BIN or add evm to PATH): %w", err)
		}
		candidate = p
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("stat evm binary %q: %w", candidate, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("evm binary path is a directory: %s", candidate)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("evm binary is not executable: %s", candidate)
	}
	return candidate, nil
}

// RunT8NBlock gọi tiến trình `evm t8n` nguyên bản trong thư mục tạm riêng với timeout.
func RunT8NBlock(
	ctx context.Context,
	evmBin string,
	alloc *evm.LogicalState,
	env BlockEnv,
	rawTxsJSON []byte,
	timeout time.Duration,
) (*T8NBlockResult, error) {
	if err := env.ValidateShanghai(); err != nil {
		return nil, fmt.Errorf("t8n invalid env: %w", err)
	}
	bin, err := ResolveEVMBin(evmBin)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultT8NTimeout
	}

	tmpDir, err := os.MkdirTemp("", "pluto-lab-t8n-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir for t8n: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	allocBytes, err := MarshalAllocJSON(alloc)
	if err != nil {
		return nil, fmt.Errorf("marshal t8n alloc: %w", err)
	}
	envBytes, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal t8n env: %w", err)
	}
	if len(rawTxsJSON) == 0 {
		rawTxsJSON = []byte("[]")
	}

	allocPath := filepath.Join(tmpDir, "alloc.json")
	envPath := filepath.Join(tmpDir, "env.json")
	txsPath := filepath.Join(tmpDir, "txs.json")
	if err := os.WriteFile(allocPath, allocBytes, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(envPath, envBytes, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(txsPath, rawTxsJSON, 0o600); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"t8n",
		"--state.fork", ForkName,
		"--state.chainid", strconv.FormatUint(DefaultChainID, 10),
		"--state.reward", "-1",
		"--input.alloc", allocPath,
		"--input.env", envPath,
		"--input.txs", txsPath,
		"--output.basedir", tmpDir,
		"--output.result", "result.json",
		"--output.alloc", "post-alloc.json",
		"--output.body", "body.rlp",
	}
	cmd := exec.CommandContext(runCtx, bin, args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("evm t8n failed (%v): stderr=%s", err, strings.TrimSpace(stderrBuf.String()))
	}

	resBytes, err := os.ReadFile(filepath.Join(tmpDir, "result.json"))
	if err != nil {
		return nil, fmt.Errorf("read t8n result.json: %w (stderr=%s)", err, strings.TrimSpace(stderrBuf.String()))
	}
	postAllocBytes, err := os.ReadFile(filepath.Join(tmpDir, "post-alloc.json"))
	if err != nil {
		return nil, fmt.Errorf("read t8n post-alloc.json: %w (stderr=%s)", err, strings.TrimSpace(stderrBuf.String()))
	}

	parsed, err := parseT8NResultJSON(resBytes)
	if err != nil {
		return nil, fmt.Errorf("validate t8n result.json schema: %w (stderr=%s)", err, strings.TrimSpace(stderrBuf.String()))
	}
	postState, err := UnmarshalAllocJSON(postAllocBytes)
	if err != nil {
		return nil, fmt.Errorf("parse t8n post-alloc.json: %w", err)
	}
	parsed.PostState = postState
	parsed.RawResultJSON = resBytes
	parsed.Stderr = stderrBuf.String()
	return parsed, nil
}

func parseT8NResultJSON(data []byte) (*T8NBlockResult, error) {
	var dec t8nResultJSON
	if err := json.Unmarshal(data, &dec); err != nil {
		return nil, err
	}
	if dec.StateRoot == nil || dec.TxRoot == nil || dec.ReceiptsRoot == nil || dec.LogsBloom == nil || dec.GasUsed == nil {
		return nil, errors.New("missing required root/bloom/gasUsed fields")
	}
	if dec.Receipts == nil {
		return nil, errors.New("missing required receipts array")
	}
	receipts := make([]*TxReceipt, len(dec.Receipts))
	for i, rawRec := range dec.Receipts {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawRec, &fields); err != nil {
			return nil, fmt.Errorf("receipt %d invalid json: %w", i, err)
		}
		rawLogs, hasLogs := fields["logs"]
		if !hasLogs {
			return nil, fmt.Errorf("receipt %d missing required field 'logs'", i)
		}
		if bytes.Equal(bytes.TrimSpace(rawLogs), []byte("null")) {
			fields["logs"] = json.RawMessage("[]")
			var err error
			rawRec, err = json.Marshal(fields)
			if err != nil {
				return nil, fmt.Errorf("receipt %d normalize logs: %w", i, err)
			}
		}
		var rec types.Receipt
		if err := json.Unmarshal(rawRec, &rec); err != nil {
			return nil, fmt.Errorf("receipt %d decode: %w", i, err)
		}
		receipts[i] = TxReceiptFromGeth(&rec)
	}
	var logsHash common.Hash
	if dec.LogsHash != nil {
		logsHash = *dec.LogsHash
	}
	return &T8NBlockResult{
		StateRoot:    *dec.StateRoot,
		TxRoot:       *dec.TxRoot,
		ReceiptsRoot: *dec.ReceiptsRoot,
		LogsHash:     logsHash,
		LogsBloom:    *dec.LogsBloom,
		GasUsed:      uint64(*dec.GasUsed),
		Receipts:     receipts,
		Rejected:     dec.Rejected,
	}, nil
}

// RunT8NChain chạy chuỗi block qua `evm t8n`, tự nối post-alloc của Geth sang block tiếp theo.
func RunT8NChain(ctx context.Context, evmBin string, c *ChainCase, timeout time.Duration) ([]*T8NBlockResult, error) {
	if c == nil {
		return nil, errors.New("chain case must not be nil")
	}
	curAlloc := c.GenesisAlloc.Clone()
	out := make([]*T8NBlockResult, 0, len(c.Blocks))
	for i, blk := range c.Blocks {
		rawTxs := blk.RawTxsJSON
		if len(rawTxs) == 0 {
			var err error
			rawTxs, err = MarshalTxsJSON(blk.Txs)
			if err != nil {
				return nil, fmt.Errorf("block %d marshal txs: %w", i, err)
			}
		}
		res, err := RunT8NBlock(ctx, evmBin, curAlloc, blk.Env, rawTxs, timeout)
		if err != nil {
			return nil, fmt.Errorf("t8n block %d (height=%d): %w", i, blk.Env.Number, err)
		}
		out = append(out, res)
		curAlloc = res.PostState
	}
	return out, nil
}

// CompareT8NReferenceResult đối chiếu replay `result.json` với `reference-result.json`, bỏ duy nhất logsHash
// vì Geth v1.16.8 tính logsHash qua duyệt Go map có thứ tự ngẫu nhiên.
func CompareT8NReferenceResult(refJSON, replayJSON []byte) error {
	var refMap, replayMap map[string]any
	if err := json.Unmarshal(refJSON, &refMap); err != nil {
		return fmt.Errorf("decode reference result.json: %w", err)
	}
	if err := json.Unmarshal(replayJSON, &replayMap); err != nil {
		return fmt.Errorf("decode replay result.json: %w", err)
	}
	delete(refMap, "logsHash")
	delete(replayMap, "logsHash")

	if reflect.DeepEqual(refMap, replayMap) {
		return nil
	}

	keys := make([]string, 0, len(refMap)+len(replayMap))
	seen := make(map[string]struct{})
	for k := range refMap {
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for k := range replayMap {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		rv, rok := refMap[k]
		pv, pok := replayMap[k]
		if !rok || !pok || !reflect.DeepEqual(rv, pv) {
			return fmt.Errorf("reference-result mismatch at field %q: want=%v got=%v", k, rv, pv)
		}
	}
	return errors.New("reference-result mismatch")
}
