package lab

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type chainManifestJSON struct {
	CaseID         string              `json:"caseId"`
	ChainID        uint64              `json:"chainId"`
	Seed           *uint64             `json:"seed"`
	ExpectedReject *ExpectedRejectSpec `json:"expectedReject"`
	Blocks         []string            `json:"blocks"`
	InputSHA256    map[string]string   `json:"inputSha256"`
}

// AllVerifyCases trả về toàn bộ bộ ca nghiệm thu cuối Phase 1 (bao gồm revert/oog và ca cố ý sai).
func AllVerifyCases() []string {
	return []string{
		"counter-2tx",
		"counter-burst",
		"approve-independent",
		"approve-mixed",
		"swap-pools",
		"swap-pools-interleaved",
		"swap-pools-clustered",
		"revert-and-oog",
		"invalid-block",
	}
}

// AllBenchCases trả về danh sách các tải hợp lệ dùng cho phép đo `bench --case all` (Phase 1).
func AllBenchCases() []string {
	return []string{
		"counter-2tx",
		"counter-burst",
		"approve-independent",
		"approve-mixed",
		"swap-pools",
		"swap-pools-interleaved",
		"swap-pools-clustered",
	}
}

// AllPhase2VerifyCases trả về toàn bộ bộ ca nghiệm thu của suite Phase 2.
func AllPhase2VerifyCases() []string {
	return []string{
		"checkpoint-independent",
		"checkpoint-early",
		"checkpoint-late",
		"checkpoint-sstore-gas",
		"checkpoint-errors",
	}
}

// AllPhase2BenchCases trả về danh sách ca benchmark Phase 2.
func AllPhase2BenchCases() []string {
	return []string{
		"checkpoint-independent",
		"checkpoint-early",
		"checkpoint-late",
		"checkpoint-sstore-gas",
	}
}

// AllPhase3BenchCases trả về đúng 5 chuỗi Merkle 6 block × 64 tx của ma trận Phase 3.
func AllPhase3BenchCases() []string {
	return []string{
		"merkle-early",
		"merkle-late",
		"merkle-mixed",
		"merkle-independent-early",
		"merkle-independent-late",
	}
}

// AllPhase3VerifyCases trả về đủ 8 ca verify Phase 3 (5 chuỗi seeded + 3 ca nhỏ/lỗi).
func AllPhase3VerifyCases() []string {
	return []string{
		"merkle-early",
		"merkle-late",
		"merkle-mixed",
		"merkle-independent-early",
		"merkle-independent-late",
		"merkle-2tx-early",
		"merkle-2tx-late",
		"merkle-errors",
	}
}

// IsPhase3SeedIndependentCase trả true cho 3 ca nhỏ/lỗi đặt trực tiếp dưới testdata/phase3/.
func IsPhase3SeedIndependentCase(caseID string) bool {
	switch caseID {
	case "merkle-2tx-early", "merkle-2tx-late", "merkle-errors":
		return true
	default:
		return false
	}
}

// SplitPhase3CaseVariant tách (baseCase, variant) cho cột CSV Phase 3.
func SplitPhase3CaseVariant(caseID string) (string, string) {
	switch caseID {
	case "merkle-independent-early":
		return "merkle-independent", "early"
	case "merkle-independent-late":
		return "merkle-independent", "late"
	case "merkle-early":
		return "merkle-early", "early"
	case "merkle-late":
		return "merkle-late", "late"
	case "merkle-mixed":
		return "merkle-mixed", "mixed"
	case "merkle-2tx-early":
		return "merkle-2tx-early", "early"
	case "merkle-2tx-late":
		return "merkle-2tx-late", "late"
	case "merkle-errors":
		return "merkle-errors", "errors"
	default:
		return caseID, ""
	}
}

// AllPhase4Cases trả về đúng 3 bộ tải chuẩn cho ma trận chọn cấu hình Phase 4.
func AllPhase4Cases() []string {
	return []string{
		"merkle-mixed",
		"swap-pools",
		"approve-independent",
	}
}

// ResolvePhase4TestdataRoot chuẩn hóa thư mục gốc testdata chứa cả phase1/ và phase3/.
func ResolvePhase4TestdataRoot(testdataRoot string) string {
	if testdataRoot == "" {
		return "testdata"
	}
	// nếu người dùng truyền testdata/phase4 thì lùi lên thư mục cha chứa phase1/ và phase3/
	if st, err := os.Stat(filepath.Join(testdataRoot, "phase1")); err == nil && st.IsDir() {
		return testdataRoot
	}
	parent := filepath.Dir(filepath.Clean(testdataRoot))
	if st, err := os.Stat(filepath.Join(parent, "phase1")); err == nil && st.IsDir() {
		return parent
	}
	return testdataRoot
}

// Phase4FixtureCaseDir trả về đường dẫn thư mục thực tế của một ca trong suite Phase 4.
func Phase4FixtureCaseDir(testdataRoot string, seed uint64, caseID string) string {
	root := ResolvePhase4TestdataRoot(testdataRoot)
	if seed == 0 {
		seed = DefaultPhase3EvalSeed
	}
	if strings.HasPrefix(caseID, "merkle-") {
		if IsPhase3SeedIndependentCase(caseID) {
			return filepath.Join(root, SuitePhase3, caseID)
		}
		return filepath.Join(root, SuitePhase3, fmt.Sprintf("seed-%d", seed), caseID)
	}
	return filepath.Join(root, SuitePhase1, caseID)
}

// LoadSuiteFixtureCase nạp fixture cho phase1/phase2/phase3/phase4 (tự định tuyến seed-<seed>/ hoặc phase1/ cho Phase 4).
func LoadSuiteFixtureCase(suite, testdataRoot string, seed uint64, caseID string) (*ChainCase, error) {
	if suite == SuitePhase4 {
		root := ResolvePhase4TestdataRoot(testdataRoot)
		if seed == 0 {
			seed = DefaultPhase3EvalSeed
		}
		if strings.HasPrefix(caseID, "merkle-") {
			return LoadSuiteFixtureCase(SuitePhase3, filepath.Join(root, SuitePhase3), seed, caseID)
		}
		return LoadFixtureCase(filepath.Join(root, SuitePhase1), caseID)
	}
	if suite == SuitePhase3 && !IsPhase3SeedIndependentCase(caseID) {
		if seed > 0 {
			seedDir := filepath.Join(testdataRoot, fmt.Sprintf("seed-%d", seed))
			if st, err := os.Stat(filepath.Join(seedDir, caseID)); err == nil && st.IsDir() {
				return LoadFixtureCase(seedDir, caseID)
			}
		}
	}
	return LoadFixtureCase(testdataRoot, caseID)
}

func computeCaseInputHash(parts ...[]byte) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(p)
	}
	return "0x" + hex.EncodeToString(h.Sum(nil))
}

func verifyOptionalInputSHA256(caseID, relPath string, data []byte, expected map[string]string) error {
	if expected == nil {
		return nil
	}
	wantRaw, ok := expected[relPath]
	if !ok || wantRaw == "" {
		return nil
	}
	sum := sha256.Sum256(data)
	got := "0x" + hex.EncodeToString(sum[:])
	want := strings.ToLower(strings.TrimSpace(wantRaw))
	if !strings.HasPrefix(want, "0x") {
		want = "0x" + want
	}
	if got != want {
		return fmt.Errorf("case %q file %q sha256 mismatch: got %s want %s", caseID, relPath, got, want)
	}
	return nil
}

// LoadFixtureCase nạp đầy đủ alloc, env, txs và reference-result (đơn block hoặc chuỗi nhiều block qua manifest.json).
func LoadFixtureCase(testdataRoot, caseID string) (*ChainCase, error) {
	if caseID == "" {
		return nil, errors.New("case ID must not be empty")
	}
	caseDir := filepath.Join(testdataRoot, caseID)
	info, err := os.Stat(caseDir)
	if err != nil {
		return nil, fmt.Errorf("open fixture case %q: %w", caseID, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fixture case %q is not a directory: %s", caseID, caseDir)
	}

	manifestPath := filepath.Join(caseDir, "manifest.json")
	if manifestBytes, err := os.ReadFile(manifestPath); err == nil {
		var manifest chainManifestJSON
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return nil, fmt.Errorf("parse manifest.json for %q: %w", caseID, err)
		}
		if len(manifest.Blocks) == 0 {
			return nil, fmt.Errorf("manifest.json for %q has no blocks", caseID)
		}
		chainID := manifest.ChainID
		if chainID == 0 {
			chainID = DefaultChainID
		}

		allocBytes, err := os.ReadFile(filepath.Join(caseDir, "alloc.json"))
		if err != nil {
			return nil, fmt.Errorf("read alloc.json for %q: %w", caseID, err)
		}
		if err := verifyOptionalInputSHA256(caseID, "alloc.json", allocBytes, manifest.InputSHA256); err != nil {
			return nil, err
		}
		alloc, err := UnmarshalAllocJSON(allocBytes)
		if err != nil {
			return nil, fmt.Errorf("parse alloc.json for %q: %w", caseID, err)
		}

		blocks := make([]BlockSpec, 0, len(manifest.Blocks))
		hashParts := make([][]byte, 0, 1+2*len(manifest.Blocks))
		hashParts = append(hashParts, allocBytes)
		for i, rel := range manifest.Blocks {
			cleanRel := filepath.FromSlash(rel)
			bDir := filepath.Join(caseDir, cleanRel)

			envBytes, err := os.ReadFile(filepath.Join(bDir, "env.json"))
			if err != nil {
				return nil, fmt.Errorf("read %s/env.json for %q: %w", rel, caseID, err)
			}
			if err := verifyOptionalInputSHA256(caseID, rel+"/env.json", envBytes, manifest.InputSHA256); err != nil {
				return nil, err
			}
			var env BlockEnv
			if err := json.Unmarshal(envBytes, &env); err != nil {
				return nil, fmt.Errorf("parse %s/env.json for %q: %w", rel, caseID, err)
			}
			if err := env.ValidateShanghai(); err != nil {
				return nil, fmt.Errorf("invalid %s/env.json for %q: %w", rel, caseID, err)
			}

			txsBytes, err := os.ReadFile(filepath.Join(bDir, "txs.json"))
			if err != nil {
				return nil, fmt.Errorf("read %s/txs.json for %q: %w", rel, caseID, err)
			}
			if err := verifyOptionalInputSHA256(caseID, rel+"/txs.json", txsBytes, manifest.InputSHA256); err != nil {
				return nil, err
			}
			txs, err := LoadSignedTxsJSON(txsBytes, env, chainID)
			if err != nil {
				return nil, fmt.Errorf("parse %s/txs.json for %q (block index %d): %w", rel, caseID, i, err)
			}

			var blockRef []byte
			if data, err := os.ReadFile(filepath.Join(bDir, "reference-result.json")); err == nil {
				blockRef = data
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read %s/reference-result.json for %q: %w", rel, caseID, err)
			}

			hashParts = append(hashParts, envBytes, txsBytes)
			blocks = append(blocks, BlockSpec{
				Env:             env,
				Txs:             txs,
				RawTxsJSON:      txsBytes,
				ReferenceResult: blockRef,
			})
		}

		return &ChainCase{
			ID:             caseID,
			InputHash:      computeCaseInputHash(hashParts...),
			Seed:           manifest.Seed,
			ExpectedReject: manifest.ExpectedReject,
			GenesisAlloc:   alloc,
			Blocks:         blocks,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read manifest.json for %q: %w", caseID, err)
	}

	allocBytes, err := os.ReadFile(filepath.Join(caseDir, "alloc.json"))
	if err != nil {
		return nil, fmt.Errorf("read alloc.json for %q: %w", caseID, err)
	}
	alloc, err := UnmarshalAllocJSON(allocBytes)
	if err != nil {
		return nil, fmt.Errorf("parse alloc.json for %q: %w", caseID, err)
	}

	envBytes, err := os.ReadFile(filepath.Join(caseDir, "env.json"))
	if err != nil {
		return nil, fmt.Errorf("read env.json for %q: %w", caseID, err)
	}
	var env BlockEnv
	if err := json.Unmarshal(envBytes, &env); err != nil {
		return nil, fmt.Errorf("parse env.json for %q: %w", caseID, err)
	}
	if err := env.ValidateShanghai(); err != nil {
		return nil, fmt.Errorf("invalid env.json for %q: %w", caseID, err)
	}

	txsBytes, err := os.ReadFile(filepath.Join(caseDir, "txs.json"))
	if err != nil {
		return nil, fmt.Errorf("read txs.json for %q: %w", caseID, err)
	}
	txs, err := LoadSignedTxsJSON(txsBytes, env, DefaultChainID)
	if err != nil {
		return nil, fmt.Errorf("parse txs.json for %q: %w", caseID, err)
	}

	var refBytes []byte
	refPath := filepath.Join(caseDir, "reference-result.json")
	if data, err := os.ReadFile(refPath); err == nil {
		refBytes = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read reference-result.json for %q: %w", caseID, err)
	}

	return &ChainCase{
		ID:           caseID,
		InputHash:    computeCaseInputHash(allocBytes, envBytes, txsBytes),
		GenesisAlloc: alloc,
		Blocks: []BlockSpec{
			{
				Env:             env,
				Txs:             txs,
				RawTxsJSON:      txsBytes,
				ReferenceResult: refBytes,
			},
		},
		ReferenceResult: refBytes,
	}, nil
}

// LoadSignedTxsJSON giải mã txs.json theo kiểu Geth, canonicalize bytes, xác minh hash và người gửi.
func LoadSignedTxsJSON(data []byte, env BlockEnv, chainID uint64) ([]SignedTx, error) {
	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err != nil {
		return nil, fmt.Errorf("decode txs array: %w", err)
	}

	chainCfg := ShanghaiChainConfig(chainID)
	signer := types.MakeSigner(chainCfg, new(big.Int).SetUint64(env.Number), env.Timestamp)

	out := make([]SignedTx, 0, len(rawItems))
	for i, item := range rawItems {
		var meta struct {
			Hash *common.Hash `json:"hash"`
		}
		if err := json.Unmarshal(item, &meta); err != nil {
			return nil, fmt.Errorf("tx %d decode hash metadata: %w", i, err)
		}

		var tx types.Transaction
		if err := json.Unmarshal(item, &tx); err != nil {
			return nil, fmt.Errorf("tx %d unmarshal geth transaction: %w", i, err)
		}
		v, r, s := tx.RawSignatureValues()
		if v == nil || r == nil || s == nil || (v.Sign() == 0 && r.Sign() == 0 && s.Sign() == 0) {
			return nil, fmt.Errorf("tx %d is unsigned", i)
		}

		rawBytes, err := tx.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("tx %d marshal binary: %w", i, err)
		}
		var canonical types.Transaction
		if err := canonical.UnmarshalBinary(rawBytes); err != nil {
			return nil, fmt.Errorf("tx %d unmarshal canonical binary: %w", i, err)
		}

		computedHash := crypto.Keccak256Hash(rawBytes)
		if canonical.Hash() != computedHash {
			return nil, fmt.Errorf("tx %d canonical hash mismatch: tx.Hash=%s keccak=%s", i, canonical.Hash().Hex(), computedHash.Hex())
		}
		if meta.Hash != nil && *meta.Hash != computedHash {
			return nil, fmt.Errorf("tx %d declared hash %s != computed %s", i, meta.Hash.Hex(), computedHash.Hex())
		}

		sender, err := types.Sender(signer, &canonical)
		if err != nil {
			return nil, fmt.Errorf("tx %d recover sender (chainId=%d): %w", i, chainID, err)
		}

		out = append(out, SignedTx{
			Index:  i,
			Tx:     &canonical,
			Raw:    rawBytes,
			Hash:   computedHash,
			Sender: sender,
		})
	}
	return out, nil
}

// MarshalTxsJSON tuần tự hóa danh sách SignedTx sang JSON chuẩn của `evm t8n`.
func MarshalTxsJSON(txs []SignedTx) ([]byte, error) {
	gethTxs := make([]*types.Transaction, len(txs))
	for i, stx := range txs {
		if stx.Tx == nil {
			return nil, fmt.Errorf("tx %d is nil", i)
		}
		gethTxs[i] = stx.Tx
	}
	return json.MarshalIndent(gethTxs, "", "  ")
}
