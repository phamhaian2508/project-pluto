package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	treeDepth            = 14
	leafCount            = 1 << treeDepth // 16384
	wordCount            = leafCount >> 8 // 64
	walletCount          = 64
	chainID       uint64 = 1337
	forkName             = "Shanghai"
	blockGasLimit uint64 = 100_000_000
	txGasLimit    uint64 = 1_000_000
	baseFeeWei    uint64 = 1
	feeCapWei     uint64 = 1
	tipWei        uint64 = 0
	baseTimestamp uint64 = 0x70 // 112
	blockTimeStep uint64 = 12
	blockCount           = 6
	txPerBlock           = 64
	solcReqVer           = "0.8.24"
)

var (
	coinbaseAddr = common.HexToAddress("0x000000000000000000000000000000000000cafe")
	contractAddr = common.HexToAddress("0x000000000000000000000000000000000000d001")
	coinbaseBal  = "0x1000000000000"      // số dư dương sẵn có để tránh empty-account edge case
	walletBal    = "0x3635c9adc5dea00000" // 1000 ETH
	seeds        = []uint64{31, 73}
)

type splitMix64 struct {
	state uint64
}

func (s *splitMix64) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (s *splitMix64) shuffleInts(a []int) {
	for i := len(a) - 1; i > 0; i-- {
		j := int(s.next() % uint64(i+1))
		a[i], a[j] = a[j], a[i]
	}
}

type wallet struct {
	Index int
	Key   *ecdsa.PrivateKey
	Addr  common.Address
}

type compilerMeta struct {
	SolidityVersion      string `json:"solidityVersion"`
	SolcFullVersion      string `json:"solcFullVersion"`
	EVMVersion           string `json:"evmVersion"`
	OptimizerEnabled     bool   `json:"optimizerEnabled"`
	MetadataBytecodeHash string `json:"metadataBytecodeHash"`
}

type compiledContract struct {
	Meta              compilerMeta
	ABI               json.RawMessage
	StorageLayout     json.RawMessage
	CreationBytecode  []byte
	DeployedTemplate  []byte
	ImmutableOffsets  []int
	Opcodes           string
	RuntimeBytecode   []byte
	RuntimeCodeHash   common.Hash
	ClaimEarlySel     [4]byte
	ClaimLateSel      [4]byte
	ClaimedEventTopic common.Hash
}

type merkleTree struct {
	Depth      int
	LeafCount  int
	Wallets    []wallet
	LeafOwners []common.Address
	Levels     [][]common.Hash // Levels[0] = leaves, Levels[Depth] = [root]
	Root       common.Hash
}

type claimAction struct {
	BlockIndex  int // 0-based
	WordIndex   int
	BitIndex    int
	LeafIndex   int
	WalletIndex int
	Mode        string // "early" | "late"
	Proof       []common.Hash
	Calldata    []byte
	CustomError string // rỗng nếu hợp lệ
}

type proofRecord struct {
	Block       int      `json:"block"`
	TxIndex     int      `json:"txIndex"`
	Mode        string   `json:"mode"`
	LeafIndex   int      `json:"leafIndex"`
	WordIndex   int      `json:"wordIndex"`
	BitIndex    int      `json:"bitIndex"`
	Owner       string   `json:"owner"`
	Sender      string   `json:"sender"`
	LeafHash    string   `json:"leafHash,omitempty"`
	Proof       []string `json:"proof"`
	ExpectedErr string   `json:"expectedError,omitempty"`
}

type traceStats struct {
	Function          string   `json:"function"`
	TotalOpcodeSteps  int      `json:"totalOpcodeSteps"`
	MaxDepth          int      `json:"maxDepth"`
	HasExternalCall   bool     `json:"hasExternalCall"`
	FirstSLOADStep    int      `json:"firstSloadStep"`
	FirstSLOADPC      uint64   `json:"firstSloadPc"`
	FirstSSTOREStep   int      `json:"firstSstoreStep"`
	FirstSSTOREPC     uint64   `json:"firstSstorePc"`
	FirstLOGStep      int      `json:"firstLogStep"`
	KeccakBeforeSLOAD int      `json:"keccakBeforeSload"`
	KeccakAfterSLOAD  int      `json:"keccakAfterSload"`
	KeccakSteps       []int    `json:"keccakSteps"`
	ExternalCallOps   []string `json:"externalCallOps,omitempty"`
}

type t8nResult struct {
	StateRoot    string          `json:"stateRoot"`
	TxRoot       string          `json:"txRoot"`
	ReceiptsRoot string          `json:"receiptsRoot"`
	LogsHash     string          `json:"logsHash"`
	LogsBloom    string          `json:"logsBloom"`
	GasUsed      string          `json:"gasUsed"`
	Receipts     []t8nReceipt    `json:"receipts"`
	Rejected     []t8nRejectedTx `json:"rejected"`
}

type t8nReceipt struct {
	Status            string     `json:"status"`
	CumulativeGasUsed string     `json:"cumulativeGasUsed"`
	GasUsed           string     `json:"gasUsed"`
	TransactionHash   string     `json:"transactionHash"`
	Logs              []t8nLog   `json:"logs"`
}

type t8nLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type t8nRejectedTx struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

type allocAccount struct {
	Balance string            `json:"balance"`
	Code    string            `json:"code,omitempty"`
	Nonce   string            `json:"nonce,omitempty"`
	Storage map[string]string `json:"storage,omitempty"`
}

type blockEnv struct {
	CurrentCoinbase  string   `json:"currentCoinbase"`
	CurrentRandom    string   `json:"currentRandom"`
	CurrentGasLimit  string   `json:"currentGasLimit"`
	CurrentNumber    string   `json:"currentNumber"`
	CurrentTimestamp string   `json:"currentTimestamp"`
	CurrentBaseFee   string   `json:"currentBaseFee"`
	Withdrawals      []string `json:"withdrawals"`
}

func main() {
	var (
		repoRoot = flag.String("repo", ".", "project-pluto repository root")
		solcBin  = flag.String("solc", envOrDefault("SOLC_BIN", "/tmp/pluto-phase1-oracle.hnKQJzTu/solc"), "path to solc 0.8.24 binary")
		evmBin   = flag.String("evm", envOrDefault("EVM_BIN", "/tmp/plutof-evm-v1168/evm"), "path to Geth evm v1.16.8 binary")
	)
	flag.Parse()

	absRepo, err := filepath.Abs(*repoRoot)
	must(err)
	contractPath := filepath.Join(absRepo, "contracts", "MerkleClaim.sol")
	outRoot := filepath.Join(absRepo, "testdata", "phase3")

	fmt.Printf("1. Building deterministic 64 wallets & depth-%d Merkle tree (%d leaves)...\n", treeDepth, leafCount)
	tree := buildMerkleTree()
	fmt.Printf("   MERKLE_ROOT = %s\n", tree.Root.Hex())

	fmt.Printf("2. Compiling %s with %s...\n", contractPath, *solcBin)
	cc, err := compileMerkleClaim(*solcBin, contractPath, tree.Root)
	must(err)
	fmt.Printf("   solc = %s\n", cc.Meta.SolcFullVersion)
	fmt.Printf("   runtimeBytecode = %d bytes, codeHash = %s\n", len(cc.RuntimeBytecode), cc.RuntimeCodeHash.Hex())
	fmt.Printf("   selectors: claimEarly=0x%x claimLate=0x%x\n", cc.ClaimEarlySel, cc.ClaimLateSel)

	fmt.Printf("3. Cross-checking constructor deployment bytecode via %s t8n...\n", *evmBin)
	must(verifyDeploymentBytecode(*evmBin, cc, tree.Root))
	fmt.Printf("   constructor deploy output matches spliced runtime bytecode byte-for-byte!\n")

	// build shared genesis alloc
	allocBytes, err := marshalGenesisAlloc(tree.Wallets, cc.RuntimeBytecode)
	must(err)

	// write top-level shared contract artifacts
	must(os.MkdirAll(outRoot, 0o755))
	must(writeFormattedJSON(filepath.Join(outRoot, "abi.json"), cc.ABI))
	must(writeFormattedJSON(filepath.Join(outRoot, "storage-layout.json"), cc.StorageLayout))
	must(os.WriteFile(filepath.Join(outRoot, "runtime-bytecode.hex"), []byte("0x"+hex.EncodeToString(cc.RuntimeBytecode)+"\n"), 0o644))

	// 4. Generate small & error fixtures (seed-independent)
	fmt.Printf("4. Generating & verifying small/error cases...\n")
	early2txDir := filepath.Join(outRoot, "merkle-2tx-early")
	late2txDir := filepath.Join(outRoot, "merkle-2tx-late")
	errCaseDir := filepath.Join(outRoot, "merkle-errors")

	early2txSummary := generate2TxCase(*evmBin, early2txDir, "merkle-2tx-early", "early", tree, cc, allocBytes)
	late2txSummary := generate2TxCase(*evmBin, late2txDir, "merkle-2tx-late", "late", tree, cc, allocBytes)
	errSummary := generateErrorsCase(*evmBin, errCaseDir, tree, cc, allocBytes)

	fmt.Printf("   merkle-2tx-early: word0=%d, status=%v, logs=%v, gas=%d\n", early2txSummary.word0, early2txSummary.statuses, early2txSummary.logCounts, early2txSummary.totalGas)
	fmt.Printf("   merkle-2tx-late : word0=%d, status=%v, logs=%v, gas=%d\n", late2txSummary.word0, late2txSummary.statuses, late2txSummary.logCounts, late2txSummary.totalGas)
	fmt.Printf("   merkle-errors   : word0=%d, status=%v, logs=%v, gas=%d\n", errSummary.word0, errSummary.statuses, errSummary.logCounts, errSummary.totalGas)

	// 5. Trace claimEarly vs claimLate via evm t8n --trace
	fmt.Printf("5. Verifying EVM opcode trace (1 call frame, SLOAD vs KECCAK256 order)...\n")
	earlyTrace, err := traceSingleTx(*evmBin, early2txDir, "claimEarly")
	must(err)
	lateTrace, err := traceSingleTx(*evmBin, late2txDir, "claimLate")
	must(err)

	must(assertTraceInvariants(earlyTrace, lateTrace))
	fmt.Printf("   claimEarly: steps=%d maxDepth=%d extCall=%v firstSLOAD=(step=%d,pc=%d) keccakBeforeSLOAD=%d keccakAfterSLOAD=%d firstSSTORE=(step=%d,pc=%d)\n",
		earlyTrace.TotalOpcodeSteps, earlyTrace.MaxDepth, earlyTrace.HasExternalCall,
		earlyTrace.FirstSLOADStep, earlyTrace.FirstSLOADPC,
		earlyTrace.KeccakBeforeSLOAD, earlyTrace.KeccakAfterSLOAD,
		earlyTrace.FirstSSTOREStep, earlyTrace.FirstSSTOREPC)
	fmt.Printf("   claimLate : steps=%d maxDepth=%d extCall=%v firstSLOAD=(step=%d,pc=%d) keccakBeforeSLOAD=%d keccakAfterSLOAD=%d firstSSTORE=(step=%d,pc=%d)\n",
		lateTrace.TotalOpcodeSteps, lateTrace.MaxDepth, lateTrace.HasExternalCall,
		lateTrace.FirstSLOADStep, lateTrace.FirstSLOADPC,
		lateTrace.KeccakBeforeSLOAD, lateTrace.KeccakAfterSLOAD,
		lateTrace.FirstSSTOREStep, lateTrace.FirstSSTOREPC)

	// 6. Generate 6-block seeded suites for seed 31 & seed 73
	seedTxOrders := make(map[uint64]map[string][]string)
	seedManifestHashes := make(map[uint64]string)
	for _, seed := range seeds {
		fmt.Printf("6. Generating & verifying 6-block suite for seed %d...\n", seed)
		seedDir := filepath.Join(outRoot, fmt.Sprintf("seed-%d", seed))
		must(os.MkdirAll(seedDir, 0o755))

		caseNames := []string{
			"merkle-early",
			"merkle-late",
			"merkle-mixed",
			"merkle-independent-early",
			"merkle-independent-late",
		}
		seedTxOrders[seed] = make(map[string][]string, len(caseNames))
		caseManifests := make(map[string]map[string]any, len(caseNames))

		earlyBlocks := planContendedBlocks(seed, "early", tree)
		lateBlocks := planContendedBlocks(seed, "late", tree)
		assertSameIndexOrder("merkle-early vs merkle-late", earlyBlocks, lateBlocks)

		mixedBlocks := planMixedBlocks(seed, tree)

		indEarlyBlocks := planIndependentBlocks(seed, "early", tree)
		indLateBlocks := planIndependentBlocks(seed, "late", tree)
		assertSameIndexOrder("merkle-independent-early vs merkle-independent-late", indEarlyBlocks, indLateBlocks)

		allPlans := map[string]struct {
			ordering string
			blocks   [][]claimAction
		}{
			"merkle-early":             {ordering: "grouped-by-word-then-shuffled-groups-and-lanes", blocks: earlyBlocks},
			"merkle-late":              {ordering: "grouped-by-word-then-shuffled-groups-and-lanes", blocks: lateBlocks},
			"merkle-mixed":             {ordering: "grouped-by-word-then-shuffled-groups-and-lanes", blocks: mixedBlocks},
			"merkle-independent-early": {ordering: "shuffled-64-distinct-words", blocks: indEarlyBlocks},
			"merkle-independent-late":  {ordering: "shuffled-64-distinct-words", blocks: indLateBlocks},
		}

		for _, caseID := range caseNames {
			plan := allPlans[caseID]
			caseDir := filepath.Join(seedDir, caseID)
			txHashes, info := generateMultiBlockCase(*evmBin, caseDir, caseID, seed, plan.ordering, plan.blocks, tree, cc, allocBytes, earlyTrace, lateTrace)
			seedTxOrders[seed][caseID] = txHashes
			caseManifests[caseID] = info
			fmt.Printf("   seed-%d/%-24s: %d blocks, %d txs, all status=1, totalGas=%d\n",
				seed, caseID, blockCount, blockCount*txPerBlock, info["totalGasUsed"])
		}

		// write seed-level summary manifest
		seedSummary := map[string]any{
			"seed":                 seed,
			"chainId":              chainID,
			"fork":                 forkName,
			"contractAddress":      contractAddr.Hex(),
			"merkleRoot":           tree.Root.Hex(),
			"runtimeCodeHash":      cc.RuntimeCodeHash.Hex(),
			"solidityVersion":      cc.Meta.SolidityVersion,
			"solcFullVersion":      cc.Meta.SolcFullVersion,
			"evmVersion":           cc.Meta.EVMVersion,
			"optimizerEnabled":     cc.Meta.OptimizerEnabled,
			"metadataBytecodeHash": cc.Meta.MetadataBytecodeHash,
			"cases":                caseManifests,
		}
		seedManifestBytes, err := json.MarshalIndent(seedSummary, "", "  ")
		must(err)
		seedManifestBytes = append(seedManifestBytes, '\n')
		seedManifestPath := filepath.Join(seedDir, "manifest.json")
		must(os.WriteFile(seedManifestPath, seedManifestBytes, 0o644))
		seedManifestHashes[seed] = sha256Hex(seedManifestBytes)
	}

	// 7. Cross-seed assertions
	fmt.Printf("7. Running cross-seed invariants (seed 31 vs seed 73)...\n")
	for _, caseID := range []string{
		"merkle-early",
		"merkle-late",
		"merkle-mixed",
		"merkle-independent-early",
		"merkle-independent-late",
	} {
		h31 := seedTxOrders[31][caseID]
		h73 := seedTxOrders[73][caseID]
		if len(h31) != len(h73) || len(h31) == 0 {
			must(fmt.Errorf("unexpected tx hash count for %s: 31=%d 73=%d", caseID, len(h31), len(h73)))
		}
		same := true
		for i := range h31 {
			if h31[i] != h73[i] {
				same = false
				break
			}
		}
		if same {
			must(fmt.Errorf("seed 31 and seed 73 produced identical tx order for %s", caseID))
		}
	}
	fmt.Printf("   seed 31 & seed 73 share identical runtimeCodeHash (%s) and merkleRoot (%s), with distinct signed tx orders across all 5 cases!\n",
		cc.RuntimeCodeHash.Hex(), tree.Root.Hex())

	// 8. Write top-level merkle-tree-meta.json
	walletAddrs := make([]string, len(tree.Wallets))
	for i, w := range tree.Wallets {
		walletAddrs[i] = w.Addr.Hex()
	}
	metaDoc := map[string]any{
		"contract":             "MerkleClaim",
		"contractSource":       "contracts/MerkleClaim.sol",
		"contractAddress":      contractAddr.Hex(),
		"coinbase":             coinbaseAddr.Hex(),
		"chainId":              chainID,
		"fork":                 forkName,
		"baseFeeWei":           baseFeeWei,
		"feeCapWei":            feeCapWei,
		"tipWei":               tipWei,
		"blockGasLimit":        blockGasLimit,
		"txGasLimit":           txGasLimit,
		"treeDepth":            treeDepth,
		"leafCount":            leafCount,
		"wordCount":            wordCount,
		"walletCount":          walletCount,
		"walletRule":           "walletIdx = (word % 16) * 4 + ((bit + (word / 16)) % 4)",
		"leafHashRule":         "keccak256(abi.encode(uint256(index), address(owner)))",
		"nodeHashRule":         "keccak256(abi.encode(left, right))",
		"claimedBitMapSlot":    0,
		"merkleRoot":           tree.Root.Hex(),
		"runtimeCodeHash":      cc.RuntimeCodeHash.Hex(),
		"runtimeBytecodeBytes": len(cc.RuntimeBytecode),
		"compiler":             cc.Meta,
		"selectors": map[string]string{
			"claimEarly": "0x" + hex.EncodeToString(cc.ClaimEarlySel[:]),
			"claimLate":  "0x" + hex.EncodeToString(cc.ClaimLateSel[:]),
		},
		"claimedEventTopic": cc.ClaimedEventTopic.Hex(),
		"traceVerification": map[string]any{
			"claimEarly": earlyTrace,
			"claimLate":  lateTrace,
		},
		"wallets":            walletAddrs,
		"seedManifestSha256": seedManifestHashes,
	}
	metaBytes, err := json.MarshalIndent(metaDoc, "", "  ")
	must(err)
	metaBytes = append(metaBytes, '\n')
	must(os.WriteFile(filepath.Join(outRoot, "merkle-tree-meta.json"), metaBytes, 0o644))

	fmt.Printf("ALL DONE! Phase 3 Merkle fixtures generated and 100%% verified via evm t8n.\n")
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "0x" + hex.EncodeToString(sum[:])
}

func writeFormattedJSON(path string, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func walletForLeaf(wordIndex, bitIndex int) int {
	return (wordIndex%16)*4 + ((bitIndex + (wordIndex / 16)) % 4)
}

func leafHash(index int, owner common.Address) common.Hash {
	var buf [64]byte
	new(big.Int).SetUint64(uint64(index)).FillBytes(buf[0:32])
	copy(buf[44:64], owner.Bytes())
	return crypto.Keccak256Hash(buf[:])
}

func parentHash(left, right common.Hash) common.Hash {
	var buf [64]byte
	copy(buf[0:32], left.Bytes())
	copy(buf[32:64], right.Bytes())
	return crypto.Keccak256Hash(buf[:])
}

func buildMerkleTree() *merkleTree {
	wallets := make([]wallet, walletCount)
	for i := 0; i < walletCount; i++ {
		var keyBytes [32]byte
		keyBytes[31] = byte(i + 1)
		pk, err := crypto.ToECDSA(keyBytes[:])
		must(err)
		wallets[i] = wallet{
			Index: i,
			Key:   pk,
			Addr:  crypto.PubkeyToAddress(pk.PublicKey),
		}
	}

	owners := make([]common.Address, leafCount)
	leaves := make([]common.Hash, leafCount)
	for idx := 0; idx < leafCount; idx++ {
		wIdx := walletForLeaf(idx>>8, idx&255)
		owners[idx] = wallets[wIdx].Addr
		leaves[idx] = leafHash(idx, owners[idx])
	}

	levels := make([][]common.Hash, treeDepth+1)
	levels[0] = leaves
	for d := 0; d < treeDepth; d++ {
		prev := levels[d]
		next := make([]common.Hash, len(prev)/2)
		for j := 0; j < len(next); j++ {
			next[j] = parentHash(prev[2*j], prev[2*j+1])
		}
		levels[d+1] = next
	}

	return &merkleTree{
		Depth:      treeDepth,
		LeafCount:  leafCount,
		Wallets:    wallets,
		LeafOwners: owners,
		Levels:     levels,
		Root:       levels[treeDepth][0],
	}
}

func (t *merkleTree) Proof(index int) []common.Hash {
	proof := make([]common.Hash, t.Depth)
	idx := index
	for d := 0; d < t.Depth; d++ {
		proof[d] = t.Levels[d][idx^1]
		idx >>= 1
	}
	// self-verify proof in Go
	node := t.Levels[0][index]
	cur := index
	for d := 0; d < t.Depth; d++ {
		if (cur & 1) == 0 {
			node = parentHash(node, proof[d])
		} else {
			node = parentHash(proof[d], node)
		}
		cur >>= 1
	}
	if node != t.Root {
		panic(fmt.Sprintf("internal proof verification failed for leaf %d", index))
	}
	return proof
}

func compileMerkleClaim(solcBin, contractPath string, root common.Hash) (*compiledContract, error) {
	verOut, err := exec.Command(solcBin, "--version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run solc --version: %w (%s)", err, string(verOut))
	}
	fullVer := ""
	for _, line := range strings.Split(string(verOut), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Version:") {
			fullVer = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
			break
		}
	}
	if !strings.HasPrefix(fullVer, solcReqVer) {
		return nil, fmt.Errorf("expected solc %s, got %q", solcReqVer, fullVer)
	}

	srcBytes, err := os.ReadFile(contractPath)
	if err != nil {
		return nil, err
	}

	stdIn := map[string]any{
		"language": "Solidity",
		"sources": map[string]any{
			"MerkleClaim.sol": map[string]any{"content": string(srcBytes)},
		},
		"settings": map[string]any{
			"evmVersion": "shanghai",
			"optimizer":  map[string]any{"enabled": false},
			"metadata":   map[string]any{"bytecodeHash": "none"},
			"outputSelection": map[string]any{
				"*": map[string]any{
					"*": []string{
						"abi",
						"evm.bytecode.object",
						"evm.deployedBytecode.object",
						"evm.deployedBytecode.immutableReferences",
						"evm.deployedBytecode.opcodes",
						"storageLayout",
					},
				},
			},
		},
	}
	inBytes, err := json.Marshal(stdIn)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(solcBin, "--standard-json")
	cmd.Stdin = bytes.NewReader(inBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("solc --standard-json failed: %w (stderr=%s)", err, stderr.String())
	}

	var out struct {
		Errors []struct {
			Severity         string `json:"severity"`
			FormattedMessage string `json:"formattedMessage"`
		} `json:"errors"`
		Contracts map[string]map[string]struct {
			ABI           json.RawMessage `json:"abi"`
			StorageLayout json.RawMessage `json:"storageLayout"`
			EVM           struct {
				Bytecode struct {
					Object string `json:"object"`
				} `json:"bytecode"`
				DeployedBytecode struct {
					Object              string `json:"object"`
					Opcodes             string `json:"opcodes"`
					ImmutableReferences map[string][]struct {
						Start  int `json:"start"`
						Length int `json:"length"`
					} `json:"immutableReferences"`
				} `json:"deployedBytecode"`
			} `json:"evm"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse solc output: %w", err)
	}
	for _, e := range out.Errors {
		return nil, fmt.Errorf("solc %s:\n%s", e.Severity, e.FormattedMessage)
	}

	c, ok := out.Contracts["MerkleClaim.sol"]["MerkleClaim"]
	if !ok {
		return nil, errors.New("MerkleClaim contract missing from solc output")
	}

	// check no external call opcodes in static disassembly
	forbidden := map[string]bool{
		"CALL": true, "STATICCALL": true, "DELEGATECALL": true,
		"CALLCODE": true, "CREATE": true, "CREATE2": true, "SELFDESTRUCT": true,
	}
	for _, tok := range strings.Fields(c.EVM.DeployedBytecode.Opcodes) {
		if forbidden[tok] {
			return nil, fmt.Errorf("forbidden external-call opcode %s found in runtime bytecode", tok)
		}
	}

	creationBytes, err := hex.DecodeString(c.EVM.Bytecode.Object)
	if err != nil {
		return nil, fmt.Errorf("decode creation bytecode: %w", err)
	}
	deployedTemplate, err := hex.DecodeString(c.EVM.DeployedBytecode.Object)
	if err != nil {
		return nil, fmt.Errorf("decode deployed bytecode: %w", err)
	}

	runtimeBytes := bytes.Clone(deployedTemplate)
	var offsets []int
	for _, refs := range c.EVM.DeployedBytecode.ImmutableReferences {
		for _, ref := range refs {
			if ref.Length != 32 {
				return nil, fmt.Errorf("unexpected immutable ref length %d at offset %d", ref.Length, ref.Start)
			}
			copy(runtimeBytes[ref.Start:ref.Start+32], root.Bytes())
			offsets = append(offsets, ref.Start)
		}
	}
	if len(offsets) == 0 {
		return nil, errors.New("MERKLE_ROOT immutable reference missing from deployedBytecode")
	}
	sort.Ints(offsets)

	var earlySel, lateSel [4]byte
	copy(earlySel[:], crypto.Keccak256([]byte("claimEarly(uint256,bytes32[])"))[:4])
	copy(lateSel[:], crypto.Keccak256([]byte("claimLate(uint256,bytes32[])"))[:4])

	return &compiledContract{
		Meta: compilerMeta{
			SolidityVersion:      solcReqVer,
			SolcFullVersion:      fullVer,
			EVMVersion:           "shanghai",
			OptimizerEnabled:     false,
			MetadataBytecodeHash: "none",
		},
		ABI:               c.ABI,
		StorageLayout:     c.StorageLayout,
		CreationBytecode:  creationBytes,
		DeployedTemplate:  deployedTemplate,
		ImmutableOffsets:  offsets,
		Opcodes:           c.EVM.DeployedBytecode.Opcodes,
		RuntimeBytecode:   runtimeBytes,
		RuntimeCodeHash:   crypto.Keccak256Hash(runtimeBytes),
		ClaimEarlySel:     earlySel,
		ClaimLateSel:      lateSel,
		ClaimedEventTopic: crypto.Keccak256Hash([]byte("Claimed(uint256,address)")),
	}, nil
}

func verifyDeploymentBytecode(evmBin string, cc *compiledContract, root common.Hash) error {
	var pkBytes [32]byte
	pkBytes[31] = 1
	pk, err := crypto.ToECDSA(pkBytes[:])
	if err != nil {
		return err
	}
	deployer := crypto.PubkeyToAddress(pk.PublicKey)

	initCode := append(bytes.Clone(cc.CreationBytecode), root.Bytes()...)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(int64(chainID)),
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(1),
		Gas:       5_000_000,
		To:        nil,
		Value:     big.NewInt(0),
		Data:      initCode,
	})
	signed, err := types.SignTx(tx, types.NewLondonSigner(big.NewInt(int64(chainID))), pk)
	if err != nil {
		return err
	}
	txsJSON, err := json.MarshalIndent([]*types.Transaction{signed}, "", "  ")
	if err != nil {
		return err
	}

	alloc := map[string]allocAccount{
		coinbaseAddr.Hex(): {Balance: coinbaseBal},
		deployer.Hex():     {Balance: walletBal},
	}
	allocJSON, err := json.MarshalIndent(alloc, "", "  ")
	if err != nil {
		return err
	}
	envJSON := mustMarshalBlockEnv(1)

	res, postAllocBytes, err := runT8N(evmBin, allocJSON, envJSON, txsJSON, false)
	if err != nil {
		return err
	}
	if len(res.Rejected) != 0 || len(res.Receipts) != 1 || res.Receipts[0].Status != "0x1" {
		return fmt.Errorf("deploy tx failed: rejected=%v receipts=%+v", res.Rejected, res.Receipts)
	}
	var postAlloc map[string]allocAccount
	if err := json.Unmarshal(postAllocBytes, &postAlloc); err != nil {
		return err
	}
	deployedAddr := crypto.CreateAddress(deployer, 0)
	var deployedCodeHex string
	for k, acc := range postAlloc {
		if strings.EqualFold(k, deployedAddr.Hex()) {
			deployedCodeHex = acc.Code
			break
		}
	}
	deployedBytes, err := hex.DecodeString(strings.TrimPrefix(deployedCodeHex, "0x"))
	if err != nil {
		return err
	}
	if !bytes.Equal(deployedBytes, cc.RuntimeBytecode) {
		return fmt.Errorf("spliced runtime bytecode (%d bytes) != constructor deployed bytecode (%d bytes)", len(cc.RuntimeBytecode), len(deployedBytes))
	}
	return nil
}

func marshalGenesisAlloc(wallets []wallet, runtimeCode []byte) ([]byte, error) {
	alloc := make(map[string]allocAccount, len(wallets)+2)
	alloc[coinbaseAddr.Hex()] = allocAccount{
		Balance: coinbaseBal,
	}
	alloc[contractAddr.Hex()] = allocAccount{
		Balance: "0x0",
		Nonce:   "0x1",
		Code:    "0x" + hex.EncodeToString(runtimeCode),
	}
	for _, w := range wallets {
		alloc[w.Addr.Hex()] = allocAccount{
			Balance: walletBal,
		}
	}
	b, err := json.MarshalIndent(alloc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func mustMarshalBlockEnv(height uint64) []byte {
	env := blockEnv{
		CurrentCoinbase:  coinbaseAddr.Hex(),
		CurrentRandom:    fmt.Sprintf("0x%x", 0x3000+height),
		CurrentGasLimit:  fmt.Sprintf("0x%x", blockGasLimit),
		CurrentNumber:    fmt.Sprintf("0x%x", height),
		CurrentTimestamp: fmt.Sprintf("0x%x", baseTimestamp+(height-1)*blockTimeStep),
		CurrentBaseFee:   fmt.Sprintf("0x%x", baseFeeWei),
		Withdrawals:      []string{},
	}
	b, err := json.MarshalIndent(env, "", "  ")
	must(err)
	return append(b, '\n')
}

func encodeClaimCalldata(selector [4]byte, index uint64, proof []common.Hash) []byte {
	out := make([]byte, 4+32+32+32+32*len(proof))
	copy(out[0:4], selector[:])
	new(big.Int).SetUint64(index).FillBytes(out[4:36])
	new(big.Int).SetUint64(64).FillBytes(out[36:68])
	new(big.Int).SetUint64(uint64(len(proof))).FillBytes(out[68:100])
	for i, h := range proof {
		copy(out[100+32*i:132+32*i], h.Bytes())
	}
	return out
}

func signClaimTx(w wallet, nonce uint64, calldata []byte) *types.Transaction {
	signer := types.NewLondonSigner(big.NewInt(int64(chainID)))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(int64(chainID)),
		Nonce:     nonce,
		GasTipCap: new(big.Int).SetUint64(tipWei),
		GasFeeCap: new(big.Int).SetUint64(feeCapWei),
		Gas:       txGasLimit,
		To:        &contractAddr,
		Value:     big.NewInt(0),
		Data:      calldata,
	})
	signed, err := types.SignTx(tx, signer, w.Key)
	must(err)
	return signed
}

func runT8N(evmBin string, allocJSON, envJSON, txsJSON []byte, withTrace bool) (*t8nResult, []byte, error) {
	tmpDir, err := os.MkdirTemp("", "pluto-p3-t8n-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmpDir)

	allocPath := filepath.Join(tmpDir, "alloc.json")
	envPath := filepath.Join(tmpDir, "env.json")
	txsPath := filepath.Join(tmpDir, "txs.json")
	if err := os.WriteFile(allocPath, allocJSON, 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(envPath, envJSON, 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(txsPath, txsJSON, 0o600); err != nil {
		return nil, nil, err
	}

	args := []string{
		"t8n",
		"--state.fork", forkName,
		"--state.chainid", strconv.FormatUint(chainID, 10),
		"--state.reward", "-1",
		"--input.alloc", allocPath,
		"--input.env", envPath,
		"--input.txs", txsPath,
		"--output.basedir", tmpDir,
		"--output.result", "result.json",
		"--output.alloc", "post-alloc.json",
		"--output.body", "body.rlp",
	}
	if withTrace {
		args = append(args, "--trace")
	}

	cmd := exec.Command(evmBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("evm t8n failed: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}

	resBytes, err := os.ReadFile(filepath.Join(tmpDir, "result.json"))
	if err != nil {
		return nil, nil, err
	}
	postAllocBytes, err := os.ReadFile(filepath.Join(tmpDir, "post-alloc.json"))
	if err != nil {
		return nil, nil, err
	}
	var parsed t8nResult
	if err := json.Unmarshal(resBytes, &parsed); err != nil {
		return nil, nil, err
	}
	return &parsed, postAllocBytes, nil
}

func runT8NKeepResult(evmBin string, allocJSON, envJSON, txsJSON []byte) (*t8nResult, []byte, []byte, error) {
	tmpDir, err := os.MkdirTemp("", "pluto-p3-t8n-*")
	if err != nil {
		return nil, nil, nil, err
	}
	defer os.RemoveAll(tmpDir)

	allocPath := filepath.Join(tmpDir, "alloc.json")
	envPath := filepath.Join(tmpDir, "env.json")
	txsPath := filepath.Join(tmpDir, "txs.json")
	if err := os.WriteFile(allocPath, allocJSON, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(envPath, envJSON, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(txsPath, txsJSON, 0o600); err != nil {
		return nil, nil, nil, err
	}

	args := []string{
		"t8n",
		"--state.fork", forkName,
		"--state.chainid", strconv.FormatUint(chainID, 10),
		"--state.reward", "-1",
		"--input.alloc", allocPath,
		"--input.env", envPath,
		"--input.txs", txsPath,
		"--output.basedir", tmpDir,
		"--output.result", "result.json",
		"--output.alloc", "post-alloc.json",
		"--output.body", "body.rlp",
	}
	cmd := exec.Command(evmBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, nil, nil, fmt.Errorf("evm t8n failed: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}
	resBytes, err := os.ReadFile(filepath.Join(tmpDir, "result.json"))
	if err != nil {
		return nil, nil, nil, err
	}
	postAllocBytes, err := os.ReadFile(filepath.Join(tmpDir, "post-alloc.json"))
	if err != nil {
		return nil, nil, nil, err
	}
	var parsed t8nResult
	if err := json.Unmarshal(resBytes, &parsed); err != nil {
		return nil, nil, nil, err
	}
	return &parsed, resBytes, postAllocBytes, nil
}

func parseHexUint64(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"), 16, 64)
	must(err)
	return v
}

func readContractStorageMap(postAllocBytes []byte) map[common.Hash]common.Hash {
	var postAlloc map[string]allocAccount
	must(json.Unmarshal(postAllocBytes, &postAlloc))
	out := make(map[common.Hash]common.Hash)
	for addrStr, acc := range postAlloc {
		if strings.EqualFold(addrStr, contractAddr.Hex()) {
			for k, v := range acc.Storage {
				out[common.HexToHash(k)] = common.HexToHash(v)
			}
			break
		}
	}
	return out
}

func assertBitMapMatches(caseID string, postAllocBytes []byte, expectedWords map[int]*big.Int) {
	storage := readContractStorageMap(postAllocBytes)
	expectedNonZero := 0
	for w, val := range expectedWords {
		if val.Sign() == 0 {
			continue
		}
		expectedNonZero++
		var slotKey common.Hash
		new(big.Int).SetUint64(uint64(w)).FillBytes(slotKey[:])
		var wantVal common.Hash
		val.FillBytes(wantVal[:])
		gotVal, ok := storage[slotKey]
		if !ok || gotVal != wantVal {
			must(fmt.Errorf("%s: bitmap word %d mismatch: got %s want %s", caseID, w, gotVal.Hex(), wantVal.Hex()))
		}
	}
	if len(storage) != expectedNonZero {
		must(fmt.Errorf("%s: unexpected non-zero storage slot count: got %d want %d", caseID, len(storage), expectedNonZero))
	}
}

type smallCaseSummary struct {
	word0     uint64
	statuses  []string
	logCounts []int
	totalGas  uint64
}

func generate2TxCase(evmBin, caseDir, caseID, mode string, tree *merkleTree, cc *compiledContract, allocBytes []byte) smallCaseSummary {
	must(os.RemoveAll(caseDir))
	blockDir := filepath.Join(caseDir, "blocks", "block-01")
	must(os.MkdirAll(blockDir, 0o755))

	sel := cc.ClaimEarlySel
	if mode == "late" {
		sel = cc.ClaimLateSel
	}

	indices := []int{7, 8}
	txs := make([]*types.Transaction, 0, len(indices))
	proofs := make([]proofRecord, 0, len(indices))
	for i, leafIdx := range indices {
		wIdx := walletForLeaf(leafIdx>>8, leafIdx&255)
		w := tree.Wallets[wIdx]
		proof := tree.Proof(leafIdx)
		calldata := encodeClaimCalldata(sel, uint64(leafIdx), proof)
		txs = append(txs, signClaimTx(w, 0, calldata))
		proofHex := make([]string, len(proof))
		for j, h := range proof {
			proofHex[j] = h.Hex()
		}
		proofs = append(proofs, proofRecord{
			Block:     1,
			TxIndex:   i,
			Mode:      mode,
			LeafIndex: leafIdx,
			WordIndex: leafIdx >> 8,
			BitIndex:  leafIdx & 255,
			Owner:     w.Addr.Hex(),
			Sender:    w.Addr.Hex(),
			LeafHash:  tree.Levels[0][leafIdx].Hex(),
			Proof:     proofHex,
		})
	}

	envBytes := mustMarshalBlockEnv(1)
	txsBytes, err := json.MarshalIndent(txs, "", "  ")
	must(err)
	txsBytes = append(txsBytes, '\n')

	res, resBytes, postAllocBytes, err := runT8NKeepResult(evmBin, allocBytes, envBytes, txsBytes)
	must(err)
	if len(res.Rejected) != 0 {
		must(fmt.Errorf("%s: unexpected rejected txs: %+v", caseID, res.Rejected))
	}
	if len(res.Receipts) != 2 {
		must(fmt.Errorf("%s: expected 2 receipts, got %d", caseID, len(res.Receipts)))
	}

	var statuses []string
	var logCounts []int
	for i, r := range res.Receipts {
		statuses = append(statuses, r.Status)
		logCounts = append(logCounts, len(r.Logs))
		if r.Status != "0x1" || len(r.Logs) != 1 {
			must(fmt.Errorf("%s tx %d: expected status=0x1 and 1 log, got status=%s logs=%d", caseID, i, r.Status, len(r.Logs)))
		}
		if !strings.EqualFold(r.Logs[0].Topics[0], cc.ClaimedEventTopic.Hex()) {
			must(fmt.Errorf("%s tx %d: unexpected log topic0 %s", caseID, i, r.Logs[0].Topics[0]))
		}
	}

	// check word 0 == (1<<7) | (1<<8) = 384
	expectedWords := map[int]*big.Int{0: big.NewInt(384)}
	assertBitMapMatches(caseID, postAllocBytes, expectedWords)

	// write files
	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cc.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cc.StorageLayout))
	proofsBytes, err := json.MarshalIndent(proofs, "", "  ")
	must(err)
	proofsBytes = append(proofsBytes, '\n')
	must(os.WriteFile(filepath.Join(caseDir, "proofs.json"), proofsBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))

	writeCaseManifest(caseDir, caseID, nil, "fixed-index-7-and-8", []string{"blocks/block-01"}, []map[string]any{
		{
			"dir":             "blocks/block-01",
			"height":          1,
			"txCount":         2,
			"mode":            mode,
			"expectedWord0":   384,
			"expectedStatus":  []int{1, 1},
			"gasUsed":         parseHexUint64(res.GasUsed),
		},
	}, tree, cc, nil, nil)

	return smallCaseSummary{
		word0:     384,
		statuses:  statuses,
		logCounts: logCounts,
		totalGas:  parseHexUint64(res.GasUsed),
	}
}

func generateErrorsCase(evmBin, caseDir string, tree *merkleTree, cc *compiledContract, allocBytes []byte) smallCaseSummary {
	must(os.RemoveAll(caseDir))
	blockDir := filepath.Join(caseDir, "blocks", "block-01")
	must(os.MkdirAll(blockDir, 0o755))

	wIdx7 := walletForLeaf(0, 7) // wallet 3
	wIdx8 := walletForLeaf(0, 8) // wallet 0
	wOther := 1                  // wallet 1 (wrong sender for index 8)
	wOOB := 2                    // wallet 2 (out-of-bounds index)

	proof7 := tree.Proof(7)
	proof8 := tree.Proof(8)
	badProof8 := append([]common.Hash(nil), proof8...)
	badProof8[0][0] ^= 0x01 // flip 1 bit
	shortProof8 := proof8[:treeDepth-1]

	type errSpec struct {
		mode        string
		sel         [4]byte
		leafIdx     int
		senderIdx   int
		nonce       uint64
		proof       []common.Hash
		wantStatus  string
		wantLogs    int
		expectedErr string
	}
	specs := []errSpec{
		{mode: "early", sel: cc.ClaimEarlySel, leafIdx: 7, senderIdx: wIdx7, nonce: 0, proof: proof7, wantStatus: "0x1", wantLogs: 1},
		{mode: "early", sel: cc.ClaimEarlySel, leafIdx: 7, senderIdx: wIdx7, nonce: 1, proof: proof7, wantStatus: "0x0", wantLogs: 0, expectedErr: "AlreadyClaimed(7)"},
		{mode: "late", sel: cc.ClaimLateSel, leafIdx: 7, senderIdx: wIdx7, nonce: 2, proof: proof7, wantStatus: "0x0", wantLogs: 0, expectedErr: "AlreadyClaimed(7)"},
		{mode: "early", sel: cc.ClaimEarlySel, leafIdx: 8, senderIdx: wIdx8, nonce: 0, proof: badProof8, wantStatus: "0x0", wantLogs: 0, expectedErr: "InvalidProof()"},
		{mode: "late", sel: cc.ClaimLateSel, leafIdx: 8, senderIdx: wIdx8, nonce: 1, proof: badProof8, wantStatus: "0x0", wantLogs: 0, expectedErr: "InvalidProof()"},
		{mode: "late", sel: cc.ClaimLateSel, leafIdx: 8, senderIdx: wOther, nonce: 0, proof: proof8, wantStatus: "0x0", wantLogs: 0, expectedErr: "InvalidProof() [wrong msg.sender]"},
		{mode: "late", sel: cc.ClaimLateSel, leafIdx: 8, senderIdx: wIdx8, nonce: 2, proof: shortProof8, wantStatus: "0x0", wantLogs: 0, expectedErr: "InvalidProofLength(13)"},
		{mode: "early", sel: cc.ClaimEarlySel, leafIdx: leafCount, senderIdx: wOOB, nonce: 0, proof: proof8, wantStatus: "0x0", wantLogs: 0, expectedErr: "InvalidIndex(16384)"},
		{mode: "late", sel: cc.ClaimLateSel, leafIdx: 8, senderIdx: wIdx8, nonce: 3, proof: proof8, wantStatus: "0x1", wantLogs: 1},
	}

	txs := make([]*types.Transaction, 0, len(specs))
	proofs := make([]proofRecord, 0, len(specs))
	for i, sp := range specs {
		calldata := encodeClaimCalldata(sp.sel, uint64(sp.leafIdx), sp.proof)
		txs = append(txs, signClaimTx(tree.Wallets[sp.senderIdx], sp.nonce, calldata))
		pHex := make([]string, len(sp.proof))
		for j, h := range sp.proof {
			pHex[j] = h.Hex()
		}
		ownerStr := ""
		leafHashStr := ""
		if sp.leafIdx < leafCount {
			ownerStr = tree.LeafOwners[sp.leafIdx].Hex()
			leafHashStr = tree.Levels[0][sp.leafIdx].Hex()
		}
		proofs = append(proofs, proofRecord{
			Block:       1,
			TxIndex:     i,
			Mode:        sp.mode,
			LeafIndex:   sp.leafIdx,
			WordIndex:   sp.leafIdx >> 8,
			BitIndex:    sp.leafIdx & 255,
			Owner:       ownerStr,
			Sender:      tree.Wallets[sp.senderIdx].Addr.Hex(),
			LeafHash:    leafHashStr,
			Proof:       pHex,
			ExpectedErr: sp.expectedErr,
		})
	}

	envBytes := mustMarshalBlockEnv(1)
	txsBytes, err := json.MarshalIndent(txs, "", "  ")
	must(err)
	txsBytes = append(txsBytes, '\n')

	res, resBytes, postAllocBytes, err := runT8NKeepResult(evmBin, allocBytes, envBytes, txsBytes)
	must(err)
	if len(res.Rejected) != 0 {
		must(fmt.Errorf("merkle-errors: unexpected rejected txs: %+v", res.Rejected))
	}
	if len(res.Receipts) != len(specs) {
		must(fmt.Errorf("merkle-errors: expected %d receipts, got %d", len(specs), len(res.Receipts)))
	}

	var statuses []string
	var logCounts []int
	for i, sp := range specs {
		r := res.Receipts[i]
		statuses = append(statuses, r.Status)
		logCounts = append(logCounts, len(r.Logs))
		if r.Status != sp.wantStatus || len(r.Logs) != sp.wantLogs {
			must(fmt.Errorf("merkle-errors tx %d (%s): got status=%s logs=%d, want status=%s logs=%d",
				i, sp.expectedErr, r.Status, len(r.Logs), sp.wantStatus, sp.wantLogs))
		}
	}
	assertBitMapMatches("merkle-errors", postAllocBytes, map[int]*big.Int{0: big.NewInt(384)})

	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cc.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cc.StorageLayout))
	proofsBytes, err := json.MarshalIndent(proofs, "", "  ")
	must(err)
	proofsBytes = append(proofsBytes, '\n')
	must(os.WriteFile(filepath.Join(caseDir, "proofs.json"), proofsBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
	must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))

	writeCaseManifest(caseDir, "merkle-errors", nil, "error-and-revert-suite", []string{"blocks/block-01"}, []map[string]any{
		{
			"dir":            "blocks/block-01",
			"height":         1,
			"txCount":        len(specs),
			"expectedWord0":  384,
			"expectedStatus": []int{1, 0, 0, 0, 0, 0, 0, 0, 1},
			"gasUsed":        parseHexUint64(res.GasUsed),
		},
	}, tree, cc, nil, nil)

	return smallCaseSummary{
		word0:     384,
		statuses:  statuses,
		logCounts: logCounts,
		totalGas:  parseHexUint64(res.GasUsed),
	}
}

func planContendedBlocks(seed uint64, mode string, tree *merkleTree) [][]claimAction {
	rng := &splitMix64{state: seed ^ 0x4d45524b4c450001}
	blocks := make([][]claimAction, blockCount)
	usedLeaves := make(map[int]bool)

	for h := 0; h < blockCount; h++ {
		words := make([]int, 16)
		for i := range words {
			words[i] = i
		}
		rng.shuffleInts(words)

		var blk []claimAction
		usedWallets := make(map[int]bool)
		for _, w := range words {
			lanes := []int{0, 1, 2, 3}
			rng.shuffleInts(lanes)
			for _, lane := range lanes {
				bit := 4*h + lane
				leafIdx := 256*w + bit
				if usedLeaves[leafIdx] {
					panic(fmt.Sprintf("duplicate leaf %d", leafIdx))
				}
				usedLeaves[leafIdx] = true
				wIdx := walletForLeaf(w, bit)
				if usedWallets[wIdx] {
					panic(fmt.Sprintf("duplicate wallet %d in block %d", wIdx, h))
				}
				usedWallets[wIdx] = true
				blk = append(blk, claimAction{
					BlockIndex:  h,
					WordIndex:   w,
					BitIndex:    bit,
					LeafIndex:   leafIdx,
					WalletIndex: wIdx,
					Mode:        mode,
					Proof:       tree.Proof(leafIdx),
				})
			}
		}
		blocks[h] = blk
	}
	return blocks
}

func planMixedBlocks(seed uint64, tree *merkleTree) [][]claimAction {
	rng := &splitMix64{state: seed ^ 0x4d45524b4c450002}
	blocks := make([][]claimAction, blockCount)
	usedLeaves := make(map[int]bool)

	for h := 0; h < blockCount; h++ {
		wordBase := 0
		if h >= 3 {
			wordBase = 16
		}
		words := make([]int, 16)
		for i := range words {
			words[i] = wordBase + i
		}
		rng.shuffleInts(words)

		var blk []claimAction
		usedWallets := make(map[int]bool)
		for _, w := range words {
			// h < 3: 1 early (lane 0) + 3 late (lanes 1,2,3)
			// h >= 3: 3 early (lanes 0,1,2) + 1 late (lane 3)
			lanes := []int{0, 1, 2, 3}
			rng.shuffleInts(lanes)
			for _, lane := range lanes {
				bit := 4*h + lane
				leafIdx := 256*w + bit
				if usedLeaves[leafIdx] {
					panic(fmt.Sprintf("duplicate leaf %d", leafIdx))
				}
				usedLeaves[leafIdx] = true
				wIdx := walletForLeaf(w, bit)
				if usedWallets[wIdx] {
					panic(fmt.Sprintf("duplicate wallet %d in block %d", wIdx, h))
				}
				usedWallets[wIdx] = true
				mode := "late"
				if h < 3 {
					if lane == 0 {
						mode = "early"
					}
				} else {
					if lane < 3 {
						mode = "early"
					}
				}
				blk = append(blk, claimAction{
					BlockIndex:  h,
					WordIndex:   w,
					BitIndex:    bit,
					LeafIndex:   leafIdx,
					WalletIndex: wIdx,
					Mode:        mode,
					Proof:       tree.Proof(leafIdx),
				})
			}
		}
		blocks[h] = blk
	}
	return blocks
}

func planIndependentBlocks(seed uint64, mode string, tree *merkleTree) [][]claimAction {
	rng := &splitMix64{state: seed ^ 0x4d45524b4c450003}
	blocks := make([][]claimAction, blockCount)
	usedLeaves := make(map[int]bool)

	for h := 0; h < blockCount; h++ {
		words := make([]int, wordCount)
		for i := range words {
			words[i] = i
		}
		rng.shuffleInts(words)

		var blk []claimAction
		usedWallets := make(map[int]bool)
		for _, w := range words {
			bit := h
			leafIdx := 256*w + bit
			if usedLeaves[leafIdx] {
				panic(fmt.Sprintf("duplicate leaf %d", leafIdx))
			}
			usedLeaves[leafIdx] = true
			wIdx := walletForLeaf(w, bit)
			if usedWallets[wIdx] {
				panic(fmt.Sprintf("duplicate wallet %d in block %d", wIdx, h))
			}
			usedWallets[wIdx] = true
			blk = append(blk, claimAction{
				BlockIndex:  h,
				WordIndex:   w,
				BitIndex:    bit,
				LeafIndex:   leafIdx,
				WalletIndex: wIdx,
				Mode:        mode,
				Proof:       tree.Proof(leafIdx),
			})
		}
		blocks[h] = blk
	}
	return blocks
}

func assertSameIndexOrder(label string, a, b [][]claimAction) {
	if len(a) != len(b) {
		must(fmt.Errorf("%s: block count mismatch", label))
	}
	for h := range a {
		if len(a[h]) != len(b[h]) {
			must(fmt.Errorf("%s block %d: tx count mismatch", label, h))
		}
		for i := range a[h] {
			if a[h][i].LeafIndex != b[h][i].LeafIndex || a[h][i].WalletIndex != b[h][i].WalletIndex {
				must(fmt.Errorf("%s block %d tx %d: mismatch (%d,%d) vs (%d,%d)",
					label, h, i, a[h][i].LeafIndex, a[h][i].WalletIndex, b[h][i].LeafIndex, b[h][i].WalletIndex))
			}
		}
	}
}

func generateMultiBlockCase(
	evmBin, caseDir, caseID string,
	seed uint64,
	ordering string,
	blocks [][]claimAction,
	tree *merkleTree,
	cc *compiledContract,
	allocBytes []byte,
	earlyTrace, lateTrace *traceStats,
) ([]string, map[string]any) {
	must(os.RemoveAll(caseDir))
	must(os.MkdirAll(caseDir, 0o755))

	nonces := make([]uint64, walletCount)
	expectedWords := make(map[int]*big.Int)
	curAlloc := allocBytes

	var allTxHashes []string
	var blockRels []string
	var schedule []map[string]any
	var allProofs []proofRecord
	var totalGas uint64

	for h, actions := range blocks {
		height := uint64(h + 1)
		rel := fmt.Sprintf("blocks/block-%02d", height)
		blockDir := filepath.Join(caseDir, filepath.FromSlash(rel))
		must(os.MkdirAll(blockDir, 0o755))
		blockRels = append(blockRels, rel)

		txs := make([]*types.Transaction, 0, len(actions))
		earlyCnt, lateCnt := 0, 0
		wordSet := make(map[int]bool)

		for i, act := range actions {
			sel := cc.ClaimEarlySel
			if act.Mode == "late" {
				sel = cc.ClaimLateSel
				lateCnt++
			} else {
				earlyCnt++
			}
			wordSet[act.WordIndex] = true
			calldata := encodeClaimCalldata(sel, uint64(act.LeafIndex), act.Proof)
			w := tree.Wallets[act.WalletIndex]
			signed := signClaimTx(w, nonces[act.WalletIndex], calldata)
			nonces[act.WalletIndex]++
			txs = append(txs, signed)
			allTxHashes = append(allTxHashes, signed.Hash().Hex())

			// update expected bitmap word
			if expectedWords[act.WordIndex] == nil {
				expectedWords[act.WordIndex] = new(big.Int)
			}
			bitMask := new(big.Int).Lsh(big.NewInt(1), uint(act.BitIndex))
			expectedWords[act.WordIndex].Or(expectedWords[act.WordIndex], bitMask)

			pHex := make([]string, len(act.Proof))
			for j, ph := range act.Proof {
				pHex[j] = ph.Hex()
			}
			allProofs = append(allProofs, proofRecord{
				Block:     int(height),
				TxIndex:   i,
				Mode:      act.Mode,
				LeafIndex: act.LeafIndex,
				WordIndex: act.WordIndex,
				BitIndex:  act.BitIndex,
				Owner:     w.Addr.Hex(),
				Sender:    w.Addr.Hex(),
				LeafHash:  tree.Levels[0][act.LeafIndex].Hex(),
				Proof:     pHex,
			})
		}

		envBytes := mustMarshalBlockEnv(height)
		txsBytes, err := json.MarshalIndent(txs, "", "  ")
		must(err)
		txsBytes = append(txsBytes, '\n')

		res, resBytes, postAllocBytes, err := runT8NKeepResult(evmBin, curAlloc, envBytes, txsBytes)
		must(err)
		if len(res.Rejected) != 0 {
			must(fmt.Errorf("%s block %d: rejected txs: %+v", caseID, height, res.Rejected))
		}
		if len(res.Receipts) != txPerBlock {
			must(fmt.Errorf("%s block %d: expected %d receipts, got %d", caseID, height, txPerBlock, len(res.Receipts)))
		}
		for i, r := range res.Receipts {
			if r.Status != "0x1" {
				must(fmt.Errorf("%s block %d tx %d: status=%s (want 0x1)", caseID, height, i, r.Status))
			}
			if len(r.Logs) != 1 {
				must(fmt.Errorf("%s block %d tx %d: logs=%d (want 1)", caseID, height, i, len(r.Logs)))
			}
			if parseHexUint64(r.GasUsed) >= txGasLimit {
				must(fmt.Errorf("%s block %d tx %d: gasUsed %d >= txGasLimit", caseID, height, i, parseHexUint64(r.GasUsed)))
			}
		}
		blkGas := parseHexUint64(res.GasUsed)
		if blkGas > blockGasLimit {
			must(fmt.Errorf("%s block %d: block gasUsed %d > limit %d", caseID, height, blkGas, blockGasLimit))
		}
		totalGas += blkGas

		// verify bitmap post-state word-by-word
		assertBitMapMatches(fmt.Sprintf("%s/block-%02d", caseID, height), postAllocBytes, expectedWords)

		must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))

		// chain post-alloc to next block
		curAlloc = postAllocBytes

		wordList := make([]int, 0, len(wordSet))
		for w := range wordSet {
			wordList = append(wordList, w)
		}
		sort.Ints(wordList)
		schedule = append(schedule, map[string]any{
			"dir":           rel,
			"height":        height,
			"txCount":       len(actions),
			"earlyCount":    earlyCnt,
			"lateCount":     lateCnt,
			"distinctWords": len(wordList),
			"wordRange":     []int{wordList[0], wordList[len(wordList)-1]},
			"gasUsed":       blkGas,
		})
	}

	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cc.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cc.StorageLayout))
	proofsBytes, err := json.MarshalIndent(allProofs, "", "  ")
	must(err)
	proofsBytes = append(proofsBytes, '\n')
	must(os.WriteFile(filepath.Join(caseDir, "proofs.json"), proofsBytes, 0o644))

	sCopy := seed
	manifestHash := writeCaseManifest(caseDir, caseID, &sCopy, ordering, blockRels, schedule, tree, cc, earlyTrace, lateTrace)
	return allTxHashes, map[string]any{
		"dir":             caseID,
		"manifestSha256":  manifestHash,
		"blocks":          len(blocks),
		"totalTxs":        len(allTxHashes),
		"totalGasUsed":    totalGas,
		"runtimeCodeHash": cc.RuntimeCodeHash.Hex(),
		"firstTxHash":     allTxHashes[0],
		"lastTxHash":      allTxHashes[len(allTxHashes)-1],
	}
}

func writeCaseManifest(
	caseDir, caseID string,
	seed *uint64,
	ordering string,
	blockRels []string,
	schedule []map[string]any,
	tree *merkleTree,
	cc *compiledContract,
	earlyTrace, lateTrace *traceStats,
) string {
	relFiles := []string{
		"abi.json",
		"alloc.json",
		"proofs.json",
		"storage-layout.json",
	}
	for _, rel := range blockRels {
		relFiles = append(relFiles,
			rel+"/env.json",
			rel+"/txs.json",
			rel+"/reference-result.json",
		)
	}
	sort.Strings(relFiles)
	hashes := make(map[string]string, len(relFiles))
	for _, rel := range relFiles {
		data, err := os.ReadFile(filepath.Join(caseDir, filepath.FromSlash(rel)))
		must(err)
		hashes[rel] = sha256Hex(data)
	}

	manifest := map[string]any{
		"caseId":               caseID,
		"chainId":              chainID,
		"fork":                 forkName,
		"coinbase":             coinbaseAddr.Hex(),
		"contractAddress":      contractAddr.Hex(),
		"claimedBitMapSlot":    0,
		"treeDepth":            treeDepth,
		"leafCount":            leafCount,
		"walletCount":          walletCount,
		"merkleRoot":           tree.Root.Hex(),
		"runtimeCodeHash":      cc.RuntimeCodeHash.Hex(),
		"solidityVersion":      cc.Meta.SolidityVersion,
		"solcFullVersion":      cc.Meta.SolcFullVersion,
		"evmVersion":           cc.Meta.EVMVersion,
		"optimizerEnabled":     cc.Meta.OptimizerEnabled,
		"metadataBytecodeHash": cc.Meta.MetadataBytecodeHash,
		"prng":                 "splitmix64",
		"txOrdering":           ordering,
		"selectors": map[string]string{
			"claimEarly": "0x" + hex.EncodeToString(cc.ClaimEarlySel[:]),
			"claimLate":  "0x" + hex.EncodeToString(cc.ClaimLateSel[:]),
		},
		"claimedEventTopic": cc.ClaimedEventTopic.Hex(),
		"feeConfig": map[string]any{
			"baseFeeWei":    baseFeeWei,
			"feeCapWei":     feeCapWei,
			"tipWei":        tipWei,
			"blockGasLimit": blockGasLimit,
			"txGasLimit":    txGasLimit,
		},
		"blocks":        blockRels,
		"blockSchedule": schedule,
		"inputSha256":   hashes,
	}
	if seed != nil {
		manifest["seed"] = *seed
	}
	if earlyTrace != nil && lateTrace != nil {
		manifest["traceSummary"] = map[string]any{
			"claimEarlyFirstSloadStep": earlyTrace.FirstSLOADStep,
			"claimEarlyKeccakBefore":   earlyTrace.KeccakBeforeSLOAD,
			"claimEarlyKeccakAfter":    earlyTrace.KeccakAfterSLOAD,
			"claimLateFirstSloadStep":  lateTrace.FirstSLOADStep,
			"claimLateKeccakBefore":    lateTrace.KeccakBeforeSLOAD,
			"claimLateKeccakAfter":     lateTrace.KeccakAfterSLOAD,
		}
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	must(err)
	b = append(b, '\n')
	must(os.WriteFile(filepath.Join(caseDir, "manifest.json"), b, 0o644))
	return sha256Hex(b)
}

func traceSingleTx(evmBin, caseDir, fnName string) (*traceStats, error) {
	tmpDir, err := os.MkdirTemp("", "pluto-p3-trace-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	allocPath := filepath.Join(caseDir, "alloc.json")
	envPath := filepath.Join(caseDir, "blocks", "block-01", "env.json")
	txsPath := filepath.Join(caseDir, "blocks", "block-01", "txs.json")

	args := []string{
		"t8n",
		"--state.fork", forkName,
		"--state.chainid", strconv.FormatUint(chainID, 10),
		"--state.reward", "-1",
		"--input.alloc", allocPath,
		"--input.env", envPath,
		"--input.txs", txsPath,
		"--output.basedir", tmpDir,
		"--output.result", "result.json",
		"--output.alloc", "post-alloc.json",
		"--output.body", "body.rlp",
		"--trace",
	}
	cmd := exec.Command(evmBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("evm t8n --trace failed: %w (stderr=%s)", err, stderr.String())
	}

	// Geth t8n --trace writes trace-0-<txhash>.jsonl for tx 0
	matches, err := filepath.Glob(filepath.Join(tmpDir, "trace-0-*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("no trace-0-*.jsonl generated in %s", tmpDir)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stats := &traceStats{
		Function:        fnName,
		FirstSLOADStep:  -1,
		FirstSSTOREStep: -1,
		FirstLOGStep:    -1,
	}
	extCalls := map[string]bool{
		"CALL": true, "STATICCALL": true, "DELEGATECALL": true,
		"CALLCODE": true, "CREATE": true, "CREATE2": true, "SELFDESTRUCT": true,
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	step := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			PC     uint64 `json:"pc"`
			OpName string `json:"opName"`
			Depth  int    `json:"depth"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.OpName == "" {
			// final summary line in t8n trace has no opName
			continue
		}
		if rec.Depth > stats.MaxDepth {
			stats.MaxDepth = rec.Depth
		}
		if extCalls[rec.OpName] {
			stats.HasExternalCall = true
			stats.ExternalCallOps = append(stats.ExternalCallOps, rec.OpName)
		}
		if rec.OpName == "SLOAD" && stats.FirstSLOADStep == -1 {
			stats.FirstSLOADStep = step
			stats.FirstSLOADPC = rec.PC
		}
		if rec.OpName == "SSTORE" && stats.FirstSSTOREStep == -1 {
			stats.FirstSSTOREStep = step
			stats.FirstSSTOREPC = rec.PC
		}
		if strings.HasPrefix(rec.OpName, "LOG") && stats.FirstLOGStep == -1 {
			stats.FirstLOGStep = step
		}
		if rec.OpName == "KECCAK256" || rec.OpName == "SHA3" {
			stats.KeccakSteps = append(stats.KeccakSteps, step)
			if stats.FirstSLOADStep == -1 {
				stats.KeccakBeforeSLOAD++
			} else {
				stats.KeccakAfterSLOAD++
			}
		}
		step++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	stats.TotalOpcodeSteps = step
	return stats, nil
}

func assertTraceInvariants(early, late *traceStats) error {
	for _, st := range []*traceStats{early, late} {
		if st.MaxDepth != 1 {
			return fmt.Errorf("%s: expected maxDepth=1 (single call frame), got %d", st.Function, st.MaxDepth)
		}
		if st.HasExternalCall {
			return fmt.Errorf("%s: unexpected external call opcodes: %v", st.Function, st.ExternalCallOps)
		}
		if st.FirstSLOADStep < 0 || st.FirstSSTOREStep < 0 || st.FirstLOGStep < 0 {
			return fmt.Errorf("%s: missing SLOAD/SSTORE/LOG in trace: %+v", st.Function, st)
		}
		if st.KeccakBeforeSLOAD+st.KeccakAfterSLOAD != treeDepth+1 {
			return fmt.Errorf("%s: expected %d KECCAK256 (1 leaf + %d levels), got %d",
				st.Function, treeDepth+1, treeDepth, st.KeccakBeforeSLOAD+st.KeccakAfterSLOAD)
		}
	}
	if early.KeccakBeforeSLOAD != 0 || early.KeccakAfterSLOAD != treeDepth+1 {
		return fmt.Errorf("claimEarly: expected 0 KECCAK256 before SLOAD and %d after, got before=%d after=%d",
			treeDepth+1, early.KeccakBeforeSLOAD, early.KeccakAfterSLOAD)
	}
	if late.KeccakBeforeSLOAD != treeDepth+1 || late.KeccakAfterSLOAD != 0 {
		return fmt.Errorf("claimLate: expected %d KECCAK256 before SLOAD and 0 after, got before=%d after=%d",
			treeDepth+1, late.KeccakBeforeSLOAD, late.KeccakAfterSLOAD)
	}
	if late.FirstSLOADStep <= early.FirstSLOADStep {
		return fmt.Errorf("expected claimLate SLOAD step (%d) > claimEarly SLOAD step (%d)",
			late.FirstSLOADStep, early.FirstSLOADStep)
	}
	return nil
}
