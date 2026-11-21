package lab

import (
	"crypto/ecdsa"
	"crypto/sha256"
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

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

const DefaultSwapPoolsSeed uint64 = 20260401

var swapExactInSelector = [4]byte{0x4d, 0xe6, 0xf7, 0xd2}

// SwapGenManifest phản ánh schema `manifest.json` của bộ `swap-pools` để tái tạo và kiểm tra 19 SHA-256.
type SwapGenManifest struct {
	BlockSchedule         []map[string]any  `json:"blockSchedule"`
	Blocks                []string          `json:"blocks"`
	BuildCommand          string            `json:"buildCommand,omitempty"`
	CaseID                string            `json:"caseId"`
	ChainID               uint64            `json:"chainId"`
	Coinbase              string            `json:"coinbase,omitempty"`
	EVMVersion            string            `json:"evmVersion,omitempty"`
	Fork                  string            `json:"fork,omitempty"`
	InitialReserve        string            `json:"initialReserve,omitempty"`
	InputSHA256           map[string]string `json:"inputSha256"`
	MetadataBytecodeHash  string            `json:"metadataBytecodeHash,omitempty"`
	OpenZeppelinVersion   string            `json:"openZeppelinVersion,omitempty"`
	OptimizerEnabled      bool              `json:"optimizerEnabled,omitempty"`
	Ordering              string            `json:"ordering,omitempty"`
	PoolAddresses         []string          `json:"poolAddresses,omitempty"`
	PoolReserve0Slot      int               `json:"poolReserve0Slot,omitempty"`
	PoolReserve1Slot      int               `json:"poolReserve1Slot,omitempty"`
	PoolRuntimeCodeHash   string            `json:"poolRuntimeCodeHash,omitempty"`
	PRNG                  string            `json:"prng,omitempty"`
	RouterAddress         string            `json:"routerAddress,omitempty"`
	RouterRuntimeCodeHash string            `json:"routerRuntimeCodeHash,omitempty"`
	Seed                  uint64            `json:"seed,omitempty"`
	SolidityVersion       string            `json:"solidityVersion,omitempty"`
	SourceCase            string            `json:"sourceCase,omitempty"`
	SwapExactInSelector   string            `json:"swapExactInSelector,omitempty"`
	TokenAddresses        []string          `json:"tokenAddresses,omitempty"`
	TokenAllowancesSlot   int               `json:"tokenAllowancesSlot,omitempty"`
	TokenBalancesSlot     int               `json:"tokenBalancesSlot,omitempty"`
	TokenRuntimeCodeHash  string            `json:"tokenRuntimeCodeHash,omitempty"`
	TraderCount           int               `json:"traderCount,omitempty"`
}

type splitMix64 struct{ state uint64 }

func (s *splitMix64) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func deterministicGenKey(seed byte) (*ecdsa.PrivateKey, error) {
	var b [32]byte
	b[31] = seed
	return crypto.ToECDSA(b[:])
}

func genMappingSlot(key common.Address, baseSlot uint64) common.Hash {
	var base [32]byte
	new(big.Int).SetUint64(baseSlot).FillBytes(base[:])
	var in [64]byte
	copy(in[12:32], key.Bytes())
	copy(in[32:64], base[:])
	return crypto.Keccak256Hash(in[:])
}

func genNestedMappingSlot(key1, key2 common.Address, baseSlot uint64) common.Hash {
	outer := genMappingSlot(key1, baseSlot)
	var in [64]byte
	copy(in[12:32], key2.Bytes())
	copy(in[32:64], outer.Bytes())
	return crypto.Keccak256Hash(in[:])
}

func padAddrHashHex(addr common.Address) string {
	var h common.Hash
	copy(h[12:32], addr.Bytes())
	return h.Hex()
}

func padUint64HashHex(v uint64) string {
	var h common.Hash
	new(big.Int).SetUint64(v).FillBytes(h[:])
	return h.Hex()
}

func sha256PrefixedHex(b []byte) string {
	sum := sha256.Sum256(b)
	return "0x" + hex.EncodeToString(sum[:])
}

func readRuntimeBytecodeHex(path string) ([]byte, common.Hash, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, common.Hash{}, err
	}
	trimmed := strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x")
	b, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, common.Hash{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return b, crypto.Keccak256Hash(b), nil
}

func genEncodeSwapExactIn(pool common.Address, zeroForOne bool, amountIn, minOut uint64, recipient common.Address) []byte {
	out := make([]byte, 164)
	copy(out[:4], swapExactInSelector[:])
	copy(out[4+12:4+32], pool.Bytes())
	if zeroForOne {
		out[36+31] = 1
	}
	new(big.Int).SetUint64(amountIn).FillBytes(out[68:100])
	new(big.Int).SetUint64(minOut).FillBytes(out[100:132])
	copy(out[132+12:132+32], recipient.Bytes())
	return out
}

// GenerateSwapPoolsFixture sinh lại toàn bộ `swap-pools` (alloc, 9 block env/txs, manifest 19 hash) từ seed.
func GenerateSwapPoolsFixture(artifactsDir, outDir string, seed uint64) (*SwapGenManifest, error) {
	if artifactsDir == "" {
		return nil, errors.New("artifactsDir must not be empty")
	}
	if outDir == "" {
		return nil, errors.New("outDir must not be empty")
	}

	tokenCode, tokenHash, err := readRuntimeBytecodeHex(filepath.Join(artifactsDir, "token-runtime-bytecode.hex"))
	if err != nil {
		return nil, err
	}
	poolCode, poolHash, err := readRuntimeBytecodeHex(filepath.Join(artifactsDir, "pool-runtime-bytecode.hex"))
	if err != nil {
		return nil, err
	}
	routerCode, routerHash, err := readRuntimeBytecodeHex(filepath.Join(artifactsDir, "router-runtime-bytecode.hex"))
	if err != nil {
		return nil, err
	}

	token0Addr := evm.DefaultSwapToken0Addr
	token1Addr := evm.DefaultSwapToken1Addr
	routerAddr := evm.DefaultSwapRouterAddr
	poolAddrs := evm.DefaultSwapPoolAddrs
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000cafe")

	const initReserve uint64 = 10_000_000_000
	const initTraderBal uint64 = 1_000_000_000
	const initTraderETH = "0x8ac7230489e80000"
	const numTraders = 64

	tok0Storage := make(map[string]string)
	tok1Storage := make(map[string]string)
	for _, p := range poolAddrs {
		slot := genMappingSlot(p, 0).Hex()
		tok0Storage[slot] = padUint64HashHex(initReserve)
		tok1Storage[slot] = padUint64HashHex(initReserve)
	}

	keys := make([]*ecdsa.PrivateKey, numTraders)
	traders := make([]common.Address, numTraders)
	alloc := make(map[string]any)
	for i := 0; i < numTraders; i++ {
		k, err := deterministicGenKey(byte(i + 1))
		if err != nil {
			return nil, err
		}
		addr := crypto.PubkeyToAddress(k.PublicKey)
		keys[i] = k
		traders[i] = addr
		alloc[strings.ToLower(addr.Hex())] = map[string]any{
			"balance": initTraderETH,
		}
		balSlot := genMappingSlot(addr, 0).Hex()
		allowSlot := genNestedMappingSlot(addr, routerAddr, 1).Hex()
		tok0Storage[balSlot] = padUint64HashHex(initTraderBal)
		tok1Storage[balSlot] = padUint64HashHex(initTraderBal)
		tok0Storage[allowSlot] = padUint64HashHex(initTraderBal)
		tok1Storage[allowSlot] = padUint64HashHex(initTraderBal)
	}

	alloc[strings.ToLower(token0Addr.Hex())] = map[string]any{
		"balance": "0x0",
		"nonce":   "0x1",
		"code":    "0x" + hex.EncodeToString(tokenCode),
		"storage": tok0Storage,
	}
	alloc[strings.ToLower(token1Addr.Hex())] = map[string]any{
		"balance": "0x0",
		"nonce":   "0x1",
		"code":    "0x" + hex.EncodeToString(tokenCode),
		"storage": tok1Storage,
	}
	alloc[strings.ToLower(routerAddr.Hex())] = map[string]any{
		"balance": "0x0",
		"nonce":   "0x1",
		"code":    "0x" + hex.EncodeToString(routerCode),
	}
	for _, p := range poolAddrs {
		alloc[strings.ToLower(p.Hex())] = map[string]any{
			"balance": "0x0",
			"nonce":   "0x1",
			"code":    "0x" + hex.EncodeToString(poolCode),
			"storage": map[string]string{
				padUint64HashHex(0): padAddrHashHex(token0Addr),
				padUint64HashHex(1): padAddrHashHex(token1Addr),
				padUint64HashHex(2): padUint64HashHex(initReserve),
				padUint64HashHex(3): padUint64HashHex(initReserve),
			},
		}
	}
	alloc[strings.ToLower(coinbase.Hex())] = map[string]any{
		"balance": "0x1",
	}

	if err := os.MkdirAll(filepath.Join(outDir, "blocks"), 0o755); err != nil {
		return nil, err
	}
	allocBytes, err := json.MarshalIndent(alloc, "", "  ")
	if err != nil {
		return nil, err
	}
	allocBytes = append(allocBytes, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "alloc.json"), allocBytes, 0o644); err != nil {
		return nil, err
	}

	rng := &splitMix64{state: seed}
	schedule := [][4]int{
		{16, 16, 16, 16},
		{16, 16, 16, 16},
		{16, 16, 16, 16},
		{48, 6, 5, 5},
		{48, 6, 5, 5},
		{48, 6, 5, 5},
		{6, 48, 5, 5},
		{6, 48, 5, 5},
		{6, 48, 5, 5},
	}

	inputHashes := map[string]string{
		"alloc.json": sha256PrefixedHex(allocBytes),
	}
	blockPaths := make([]string, 0, len(schedule))
	blockMeta := make([]map[string]any, 0, len(schedule))
	chainCfg := ShanghaiChainConfig(DefaultChainID)

	for bIdx, counts := range schedule {
		blockNum := uint64(bIdx + 1)
		blockRel := fmt.Sprintf("blocks/block-%02d", blockNum)
		blockDir := filepath.Join(outDir, filepath.FromSlash(blockRel))
		if err := os.MkdirAll(blockDir, 0o755); err != nil {
			return nil, err
		}

		env := BlockEnv{
			Coinbase:    coinbase,
			Number:      blockNum,
			Timestamp:   100 + blockNum*12,
			GasLimit:    30_000_000,
			BaseFee:     big.NewInt(1),
			Random:      common.BigToHash(new(big.Int).SetUint64(0x1000 + blockNum)),
			Withdrawals: []*types.Withdrawal{},
		}
		envBytes, err := env.MarshalJSON()
		if err != nil {
			return nil, err
		}
		envBytes = append(envBytes, '\n')
		if err := os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644); err != nil {
			return nil, err
		}
		inputHashes[blockRel+"/env.json"] = sha256PrefixedHex(envBytes)

		poolOrder := make([]int, 0, numTraders)
		for pIdx, cnt := range counts {
			for k := 0; k < cnt; k++ {
				poolOrder = append(poolOrder, pIdx)
			}
		}
		for i := len(poolOrder) - 1; i > 0; i-- {
			j := int(rng.next() % uint64(i+1))
			poolOrder[i], poolOrder[j] = poolOrder[j], poolOrder[i]
		}

		signer := types.MakeSigner(chainCfg, new(big.Int).SetUint64(env.Number), env.Timestamp)
		txList := make([]*types.Transaction, 0, numTraders)
		for i := 0; i < numTraders; i++ {
			pIdx := poolOrder[i]
			zeroForOne := i%2 == 0
			amountIn := 10_000 + uint64(i)*137 + blockNum*1_000
			data := genEncodeSwapExactIn(poolAddrs[pIdx], zeroForOne, amountIn, 1, traders[i])
			tx, err := types.SignNewTx(keys[i], signer, &types.DynamicFeeTx{
				ChainID:   new(big.Int).SetUint64(DefaultChainID),
				Nonce:     blockNum - 1,
				To:        &routerAddr,
				Gas:       300_000,
				GasFeeCap: big.NewInt(1),
				GasTipCap: big.NewInt(0),
				Value:     big.NewInt(0),
				Data:      data,
			})
			if err != nil {
				return nil, err
			}
			txList = append(txList, tx)
		}
		txsBytes, err := json.MarshalIndent(txList, "", "  ")
		if err != nil {
			return nil, err
		}
		txsBytes = append(txsBytes, '\n')
		if err := os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644); err != nil {
			return nil, err
		}
		inputHashes[blockRel+"/txs.json"] = sha256PrefixedHex(txsBytes)

		blockPaths = append(blockPaths, blockRel)
		blockMeta = append(blockMeta, map[string]any{
			"height":     blockNum,
			"dir":        blockRel,
			"txCount":    numTraders,
			"poolCounts": counts,
		})
	}

	manifest := &SwapGenManifest{
		BlockSchedule:         blockMeta,
		Blocks:                blockPaths,
		BuildCommand:          "SOLC_NODE_MODULES=/tmp/solc-oz/node_modules node testdata/phase1/swap-pools/build.mjs",
		CaseID:                "swap-pools",
		ChainID:               DefaultChainID,
		Coinbase:              strings.ToLower(coinbase.Hex()),
		EVMVersion:            "shanghai",
		Fork:                  ForkName,
		InitialReserve:        fmt.Sprintf("%d", initReserve),
		InputSHA256:           inputHashes,
		MetadataBytecodeHash:  "none",
		OpenZeppelinVersion:   "4.9.6",
		OptimizerEnabled:      false,
		PoolAddresses:         []string{strings.ToLower(poolAddrs[0].Hex()), strings.ToLower(poolAddrs[1].Hex()), strings.ToLower(poolAddrs[2].Hex()), strings.ToLower(poolAddrs[3].Hex())},
		PoolReserve0Slot:      2,
		PoolReserve1Slot:      3,
		PoolRuntimeCodeHash:   poolHash.Hex(),
		PRNG:                  "splitmix64",
		RouterAddress:         strings.ToLower(routerAddr.Hex()),
		RouterRuntimeCodeHash: routerHash.Hex(),
		Seed:                  seed,
		SolidityVersion:       "0.8.24",
		SwapExactInSelector:   "0x" + hex.EncodeToString(swapExactInSelector[:]),
		TokenAddresses:        []string{strings.ToLower(token0Addr.Hex()), strings.ToLower(token1Addr.Hex())},
		TokenAllowancesSlot:   1,
		TokenBalancesSlot:     0,
		TokenRuntimeCodeHash:  tokenHash.Hex(),
		TraderCount:           numTraders,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

// VerifyGeneratedHashesAgainstManifest đối chiếu đủ 19 khóa SHA-256 giữa bản vừa sinh từ seed và manifest gốc.
func VerifyGeneratedHashesAgainstManifest(generated map[string]string, referenceManifestPath string) error {
	raw, err := os.ReadFile(referenceManifestPath)
	if err != nil {
		return fmt.Errorf("read reference manifest %s: %w", referenceManifestPath, err)
	}
	var ref SwapGenManifest
	if err := json.Unmarshal(raw, &ref); err != nil {
		return fmt.Errorf("parse reference manifest %s: %w", referenceManifestPath, err)
	}
	if len(ref.InputSHA256) != 19 {
		return fmt.Errorf("expected 19 hashes in reference manifest, got %d", len(ref.InputSHA256))
	}
	if len(generated) != len(ref.InputSHA256) {
		return fmt.Errorf("hash map count mismatch: got=%d want=%d", len(generated), len(ref.InputSHA256))
	}
	for k, want := range ref.InputSHA256 {
		got, ok := generated[k]
		if !ok {
			return fmt.Errorf("missing generated sha256 for %q", k)
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("sha256 mismatch at %q: got=%s want=%s", k, got, want)
		}
	}
	return nil
}
