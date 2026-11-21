package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	hashRounds    uint64 = 256
	initialCount  uint64 = 10
	suiteSeed     uint64 = 20260201
	solcReqVer           = "0.8.24"

	slotSstoreGasX uint64 = 100
	slotSstoreGasY uint64 = 200
	slotErrOOG     uint64 = 300
	slotErrOOGOnly uint64 = 301
	slotErrWarm0A  uint64 = 310
	slotErrWarm0B  uint64 = 311
	slotErrWarmNZ  uint64 = 312
	slotErrMem     uint64 = 320
	slotErrRevertY uint64 = 330
)

var (
	coinbaseAddr = common.HexToAddress("0x000000000000000000000000000000000000cafe")
	probeAddr    = common.HexToAddress("0x000000000000000000000000000000000000c001")
	helperAddr   = common.HexToAddress("0x000000000000000000000000000000000000c002")
	coinbaseBal  = "0x1000000000000"      // số dư dương sẵn có để tránh empty-account edge case
	walletBal    = "0x3635c9adc5dea00000" // 1000 ETH
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

type compiledUnit struct {
	Name             string
	ABI              json.RawMessage
	StorageLayout    json.RawMessage
	CreationBytecode []byte
	RuntimeBytecode  []byte
	RuntimeCodeHash  common.Hash
	Opcodes          string
}

type compiledSuite struct {
	Meta   compilerMeta
	Probe  compiledUnit
	Helper compiledUnit

	SelReadThenHashIncrement      [4]byte
	SelHashThenIncrement          [4]byte
	SelHashThenIncrementOwnSlot   [4]byte
	SelHashThenDirectSstore       [4]byte
	SelHashThenWriteLogRevert     [4]byte
	SelHashThenExpandMemoryStore  [4]byte
	SelHashThenWarmColdRefund     [4]byte
	SelHashThenExternalCall       [4]byte
	SelExternalCallThenRead       [4]byte
	TopicCountUpdated             common.Hash
	TopicSlotUpdated              common.Hash
	TopicWarmColdChecked          common.Hash
	TopicExternalCallDone         common.Hash
	TopicHelperPinged             common.Hash
}

type traceStats struct {
	Function           string   `json:"function"`
	TotalOpcodeSteps   int      `json:"totalOpcodeSteps"`
	MaxDepth           int      `json:"maxDepth"`
	HasExternalCall    bool     `json:"hasExternalCall"`
	SLOADCount         int      `json:"sloadCount"`
	SSTORECount        int      `json:"sstoreCount"`
	FirstSLOADStep     int      `json:"firstSloadStep"`
	FirstSLOADPC       uint64   `json:"firstSloadPc"`
	FirstSSTOREStep    int      `json:"firstSstoreStep"`
	FirstSSTOREPC      uint64   `json:"firstSstorePc"`
	FirstLOGStep       int      `json:"firstLogStep"`
	FirstCALLStep      int      `json:"firstCallStep"`
	KeccakTotal        int      `json:"keccakTotal"`
	KeccakBeforeSLOAD  int      `json:"keccakBeforeSload"`
	KeccakAfterSLOAD   int      `json:"keccakAfterSload"`
	KeccakBeforeSSTORE int      `json:"keccakBeforeSstore"`
	KeccakAfterSSTORE  int      `json:"keccakAfterSstore"`
	ExternalCallOps    []string `json:"externalCallOps,omitempty"`
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
	Status            string   `json:"status"`
	CumulativeGasUsed string   `json:"cumulativeGasUsed"`
	GasUsed           string   `json:"gasUsed"`
	TransactionHash   string   `json:"transactionHash"`
	Logs              []t8nLog `json:"logs"`
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

type oogThresholdInfo struct {
	Gas0To7          uint64 `json:"gasUsed0To7"`
	Gas5To7          uint64 `json:"gasUsed5To7"`
	GasDiff          uint64 `json:"gasDiff"`
	ChosenTxGasLimit uint64 `json:"chosenTxGasLimit"`
	StandaloneStatus string `json:"standalone0To7Status"`
	OrderedStatus    string `json:"ordered5To7Status"`
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
	contractPath := filepath.Join(absRepo, "contracts", "CheckpointProbe.sol")
	outRoot := filepath.Join(absRepo, "testdata", "phase2")

	fmt.Printf("1. Building %d deterministic wallets...\n", walletCount)
	wallets := buildWallets()

	fmt.Printf("2. Compiling %s with %s...\n", contractPath, *solcBin)
	cs, err := compileCheckpointProbe(*solcBin, contractPath)
	must(err)
	fmt.Printf("   solc = %s\n", cs.Meta.SolcFullVersion)
	fmt.Printf("   CheckpointProbe: runtime=%d bytes, codeHash=%s\n", len(cs.Probe.RuntimeBytecode), cs.Probe.RuntimeCodeHash.Hex())
	fmt.Printf("   HelperTarget   : runtime=%d bytes, codeHash=%s\n", len(cs.Helper.RuntimeBytecode), cs.Helper.RuntimeCodeHash.Hex())

	fmt.Printf("3. Cross-checking constructor deployment bytecode via %s t8n...\n", *evmBin)
	must(verifyDeploymentBytecode(*evmBin, cs.Probe))
	must(verifyDeploymentBytecode(*evmBin, cs.Helper))
	fmt.Printf("   both CheckpointProbe and HelperTarget match deployed bytecode byte-for-byte!\n")

	// build genesis allocs
	// - standardAlloc: CheckpointProbe (slot 0 = 10), coinbase, 64 wallets (shared across independent/early/late)
	// - sstoreGasAlloc: CheckpointProbe (all slots 0), coinbase, 64 wallets
	// - errorsAlloc: CheckpointProbe (slot 0 = 10) + HelperTarget (all slots 0), coinbase, 64 wallets
	standardAlloc, err := marshalGenesisAlloc(wallets, cs, map[uint64]uint64{0: initialCount}, false)
	must(err)
	sstoreGasAlloc, err := marshalGenesisAlloc(wallets, cs, nil, false)
	must(err)
	errorsAlloc, err := marshalGenesisAlloc(wallets, cs, map[uint64]uint64{0: initialCount}, true)
	must(err)

	// write top-level shared contract artifacts
	must(os.MkdirAll(outRoot, 0o755))
	must(writeFormattedJSON(filepath.Join(outRoot, "abi.json"), cs.Probe.ABI))
	must(writeFormattedJSON(filepath.Join(outRoot, "storage-layout.json"), cs.Probe.StorageLayout))
	must(os.WriteFile(filepath.Join(outRoot, "runtime-bytecode.hex"), []byte("0x"+hex.EncodeToString(cs.Probe.RuntimeBytecode)+"\n"), 0o644))
	must(writeFormattedJSON(filepath.Join(outRoot, "helper-abi.json"), cs.Helper.ABI))
	must(os.WriteFile(filepath.Join(outRoot, "helper-runtime-bytecode.hex"), []byte("0x"+hex.EncodeToString(cs.Helper.RuntimeBytecode)+"\n"), 0o644))

	// 4. Trace verification via evm t8n --trace on every key function
	fmt.Printf("4. Running evm t8n --trace verification on CheckpointProbe functions...\n")
	traceMap := verifyAllFunctionTraces(*evmBin, wallets, cs, standardAlloc, sstoreGasAlloc, errorsAlloc)
	printTraceTable(traceMap)

	// 5. Generate shared wallet/seed schedule for independent/early/late (fair comparison)
	fmt.Printf("5. Generating & verifying 6-block benchmark suites (independent, early, late)...\n")
	walletOrders, txSeeds := planFairBenchSchedule(suiteSeed)

	caseSummaries := make(map[string]map[string]any)
	caseSummaries["checkpoint-independent"] = generate6BlockCase(
		*evmBin, filepath.Join(outRoot, "checkpoint-independent"),
		"checkpoint-independent", wallets, walletOrders, txSeeds, cs, standardAlloc, traceMap["hashThenIncrementOwnSlot"],
	)
	caseSummaries["checkpoint-early"] = generate6BlockCase(
		*evmBin, filepath.Join(outRoot, "checkpoint-early"),
		"checkpoint-early", wallets, walletOrders, txSeeds, cs, standardAlloc, traceMap["readThenHashIncrement"],
	)
	caseSummaries["checkpoint-late"] = generate6BlockCase(
		*evmBin, filepath.Join(outRoot, "checkpoint-late"),
		"checkpoint-late", wallets, walletOrders, txSeeds, cs, standardAlloc, traceMap["hashThenIncrement"],
	)

	// 6. Generate checkpoint-sstore-gas (3 blocks)
	fmt.Printf("6. Generating & verifying checkpoint-sstore-gas (3 blocks)...\n")
	caseSummaries["checkpoint-sstore-gas"] = generateSstoreGasCase(
		*evmBin, filepath.Join(outRoot, "checkpoint-sstore-gas"),
		wallets, cs, sstoreGasAlloc, traceMap["hashThenDirectSstore"],
	)

	// 7. Generate checkpoint-errors (5 blocks, including exact OOG gas calibration via evm t8n)
	fmt.Printf("7. Calibrating OOG threshold & generating checkpoint-errors (5 blocks)...\n")
	oogInfo, errSummary := generateErrorsCase(
		*evmBin, filepath.Join(outRoot, "checkpoint-errors"),
		wallets, cs, errorsAlloc, traceMap,
	)
	caseSummaries["checkpoint-errors"] = errSummary
	fmt.Printf("   OOG threshold: gas(0->7)=%d, gas(5->7)=%d, diff=%d, chosenTxGasLimit=%d (standalone=%s, ordered=%s)\n",
		oogInfo.Gas0To7, oogInfo.Gas5To7, oogInfo.GasDiff, oogInfo.ChosenTxGasLimit, oogInfo.StandaloneStatus, oogInfo.OrderedStatus)

	// 8. Write top-level manifest.json (SHA-256 of top-level artifacts + all 5 case manifests)
	walletAddrs := make([]string, len(wallets))
	for i, w := range wallets {
		walletAddrs[i] = w.Addr.Hex()
	}
	topArtifacts := []string{
		"abi.json",
		"storage-layout.json",
		"runtime-bytecode.hex",
		"helper-abi.json",
		"helper-runtime-bytecode.hex",
	}
	topSha := make(map[string]string, len(topArtifacts)+5)
	for _, rel := range topArtifacts {
		b, err := os.ReadFile(filepath.Join(outRoot, rel))
		must(err)
		topSha[rel] = sha256Hex(b)
	}
	for _, cid := range []string{
		"checkpoint-independent",
		"checkpoint-early",
		"checkpoint-late",
		"checkpoint-sstore-gas",
		"checkpoint-errors",
	} {
		rel := cid + "/manifest.json"
		b, err := os.ReadFile(filepath.Join(outRoot, filepath.FromSlash(rel)))
		must(err)
		topSha[rel] = sha256Hex(b)
	}

	topManifest := map[string]any{
		"suite":              "phase2",
		"contract":           "CheckpointProbe",
		"contractSource":     "contracts/CheckpointProbe.sol",
		"probeAddress":       probeAddr.Hex(),
		"helperAddress":      helperAddr.Hex(),
		"coinbase":           coinbaseAddr.Hex(),
		"chainId":            chainID,
		"fork":               forkName,
		"seed":               suiteSeed,
		"hashRounds":         hashRounds,
		"initialCount":       initialCount,
		"walletCount":        walletCount,
		"compiler":           cs.Meta,
		"probeRuntimeHash":   cs.Probe.RuntimeCodeHash.Hex(),
		"probeRuntimeBytes":  len(cs.Probe.RuntimeBytecode),
		"helperRuntimeHash":  cs.Helper.RuntimeCodeHash.Hex(),
		"helperRuntimeBytes": len(cs.Helper.RuntimeBytecode),
		"selectors": map[string]string{
			"readThenHashIncrement":        "0x" + hex.EncodeToString(cs.SelReadThenHashIncrement[:]),
			"hashThenIncrement":            "0x" + hex.EncodeToString(cs.SelHashThenIncrement[:]),
			"hashThenIncrementOwnSlot":     "0x" + hex.EncodeToString(cs.SelHashThenIncrementOwnSlot[:]),
			"hashThenDirectSstore":         "0x" + hex.EncodeToString(cs.SelHashThenDirectSstore[:]),
			"hashThenWriteLogRevert":       "0x" + hex.EncodeToString(cs.SelHashThenWriteLogRevert[:]),
			"hashThenExpandMemoryAndStore": "0x" + hex.EncodeToString(cs.SelHashThenExpandMemoryStore[:]),
			"hashThenWarmColdRefund":       "0x" + hex.EncodeToString(cs.SelHashThenWarmColdRefund[:]),
			"hashThenExternalCall":         "0x" + hex.EncodeToString(cs.SelHashThenExternalCall[:]),
			"externalCallThenRead":         "0x" + hex.EncodeToString(cs.SelExternalCallThenRead[:]),
		},
		"eventTopics": map[string]string{
			"CountUpdated":     cs.TopicCountUpdated.Hex(),
			"SlotUpdated":      cs.TopicSlotUpdated.Hex(),
			"WarmColdChecked":  cs.TopicWarmColdChecked.Hex(),
			"ExternalCallDone": cs.TopicExternalCallDone.Hex(),
			"HelperPinged":     cs.TopicHelperPinged.Hex(),
		},
		"feeConfig": map[string]any{
			"baseFeeWei":    baseFeeWei,
			"feeCapWei":     feeCapWei,
			"tipWei":        tipWei,
			"blockGasLimit": blockGasLimit,
			"txGasLimit":    txGasLimit,
		},
		"oogThreshold":      oogInfo,
		"traceVerification": traceMap,
		"cases":             caseSummaries,
		"wallets":           walletAddrs,
		"sha256":            topSha,
	}
	topBytes, err := json.MarshalIndent(topManifest, "", "  ")
	must(err)
	topBytes = append(topBytes, '\n')
	must(os.WriteFile(filepath.Join(outRoot, "manifest.json"), topBytes, 0o644))

	fmt.Printf("ALL DONE! All 5 Phase 2 fixture suites generated and 100%% verified via evm t8n.\n")
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

func parseHexUint64(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"), 16, 64)
	must(err)
	return v
}

func buildWallets() []wallet {
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
	return wallets
}

func selector4(sig string) [4]byte {
	var out [4]byte
	copy(out[:], crypto.Keccak256([]byte(sig))[:4])
	return out
}

func compileCheckpointProbe(solcBin, contractPath string) (*compiledSuite, error) {
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
			"CheckpointProbe.sol": map[string]any{"content": string(srcBytes)},
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
					Object  string `json:"object"`
					Opcodes string `json:"opcodes"`
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

	extract := func(name string) (compiledUnit, error) {
		c, ok := out.Contracts["CheckpointProbe.sol"][name]
		if !ok {
			return compiledUnit{}, fmt.Errorf("contract %s missing from solc output", name)
		}
		creation, err := hex.DecodeString(c.EVM.Bytecode.Object)
		if err != nil {
			return compiledUnit{}, fmt.Errorf("decode %s creation bytecode: %w", name, err)
		}
		runtime, err := hex.DecodeString(c.EVM.DeployedBytecode.Object)
		if err != nil {
			return compiledUnit{}, fmt.Errorf("decode %s deployed bytecode: %w", name, err)
		}
		return compiledUnit{
			Name:             name,
			ABI:              c.ABI,
			StorageLayout:    c.StorageLayout,
			CreationBytecode: creation,
			RuntimeBytecode:  runtime,
			RuntimeCodeHash:  crypto.Keccak256Hash(runtime),
			Opcodes:          c.EVM.DeployedBytecode.Opcodes,
		}, nil
	}

	probeUnit, err := extract("CheckpointProbe")
	if err != nil {
		return nil, err
	}
	helperUnit, err := extract("HelperTarget")
	if err != nil {
		return nil, err
	}

	return &compiledSuite{
		Meta: compilerMeta{
			SolidityVersion:      solcReqVer,
			SolcFullVersion:      fullVer,
			EVMVersion:           "shanghai",
			OptimizerEnabled:     false,
			MetadataBytecodeHash: "none",
		},
		Probe:                        probeUnit,
		Helper:                       helperUnit,
		SelReadThenHashIncrement:     selector4("readThenHashIncrement(uint256,bytes32)"),
		SelHashThenIncrement:         selector4("hashThenIncrement(uint256,bytes32)"),
		SelHashThenIncrementOwnSlot:  selector4("hashThenIncrementOwnSlot(uint256,uint256,bytes32)"),
		SelHashThenDirectSstore:      selector4("hashThenDirectSstore(uint256,uint256,bytes32,uint256)"),
		SelHashThenWriteLogRevert:    selector4("hashThenWriteLogRevert(uint256,uint256,bytes32,uint256)"),
		SelHashThenExpandMemoryStore: selector4("hashThenExpandMemoryAndStore(uint256,uint256,bytes32,uint256,uint256)"),
		SelHashThenWarmColdRefund:    selector4("hashThenWarmColdRefund(uint256,uint256,bytes32,uint256,uint256)"),
		SelHashThenExternalCall:      selector4("hashThenExternalCall(address,uint256,bytes32)"),
		SelExternalCallThenRead:      selector4("externalCallThenRead(address,uint256,bytes32)"),
		TopicCountUpdated:            crypto.Keccak256Hash([]byte("CountUpdated(uint256,bytes32)")),
		TopicSlotUpdated:             crypto.Keccak256Hash([]byte("SlotUpdated(uint256,uint256,bytes32)")),
		TopicWarmColdChecked:         crypto.Keccak256Hash([]byte("WarmColdChecked(uint256,uint256,uint256,uint256,uint256,bytes32)")),
		TopicExternalCallDone:        crypto.Keccak256Hash([]byte("ExternalCallDone(address,uint256,uint256,bytes32)")),
		TopicHelperPinged:            crypto.Keccak256Hash([]byte("HelperPinged(address,uint256,bytes32)")),
	}, nil
}

func verifyDeploymentBytecode(evmBin string, unit compiledUnit) error {
	var pkBytes [32]byte
	pkBytes[31] = 1
	pk, err := crypto.ToECDSA(pkBytes[:])
	if err != nil {
		return err
	}
	deployer := crypto.PubkeyToAddress(pk.PublicKey)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(int64(chainID)),
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(1),
		Gas:       5_000_000,
		To:        nil,
		Value:     big.NewInt(0),
		Data:      bytes.Clone(unit.CreationBytecode),
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

	res, _, postAllocBytes, err := runT8N(evmBin, allocJSON, envJSON, txsJSON)
	if err != nil {
		return err
	}
	if len(res.Rejected) != 0 || len(res.Receipts) != 1 || res.Receipts[0].Status != "0x1" {
		return fmt.Errorf("deploy %s failed: rejected=%v receipts=%+v", unit.Name, res.Rejected, res.Receipts)
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
	if !bytes.Equal(deployedBytes, unit.RuntimeBytecode) {
		return fmt.Errorf("%s deployed bytecode (%d bytes) != solc runtime (%d bytes)", unit.Name, len(deployedBytes), len(unit.RuntimeBytecode))
	}
	return nil
}

func u64Hash(v uint64) common.Hash {
	var h common.Hash
	new(big.Int).SetUint64(v).FillBytes(h[:])
	return h
}

func marshalGenesisAlloc(wallets []wallet, cs *compiledSuite, probeInitSlots map[uint64]uint64, includeHelper bool) ([]byte, error) {
	alloc := make(map[string]allocAccount, len(wallets)+3)
	alloc[coinbaseAddr.Hex()] = allocAccount{Balance: coinbaseBal}

	var probeStorage map[string]string
	if len(probeInitSlots) > 0 {
		probeStorage = make(map[string]string, len(probeInitSlots))
		for k, v := range probeInitSlots {
			if v == 0 {
				continue
			}
			probeStorage[u64Hash(k).Hex()] = u64Hash(v).Hex()
		}
	}
	alloc[probeAddr.Hex()] = allocAccount{
		Balance: "0x0",
		Nonce:   "0x1",
		Code:    "0x" + hex.EncodeToString(cs.Probe.RuntimeBytecode),
		Storage: probeStorage,
	}
	if includeHelper {
		alloc[helperAddr.Hex()] = allocAccount{
			Balance: "0x0",
			Nonce:   "0x1",
			Code:    "0x" + hex.EncodeToString(cs.Helper.RuntimeBytecode),
		}
	}
	for _, w := range wallets {
		alloc[w.Addr.Hex()] = allocAccount{Balance: walletBal}
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
		CurrentRandom:    fmt.Sprintf("0x%x", 0x2000+height),
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

func encodeWordU64(v uint64) []byte {
	var b [32]byte
	new(big.Int).SetUint64(v).FillBytes(b[:])
	return b[:]
}

func encodeWordAddr(a common.Address) []byte {
	var b [32]byte
	copy(b[12:], a.Bytes())
	return b[:]
}

func encodeCall(sel [4]byte, words ...[]byte) []byte {
	out := make([]byte, 4+32*len(words))
	copy(out[0:4], sel[:])
	for i, w := range words {
		if len(w) != 32 {
			panic(fmt.Sprintf("ABI word %d has len %d != 32", i, len(w)))
		}
		copy(out[4+32*i:4+32*(i+1)], w)
	}
	return out
}

func signProbeTx(w wallet, nonce uint64, gasLimit uint64, calldata []byte, accessList types.AccessList) *types.Transaction {
	signer := types.NewLondonSigner(big.NewInt(int64(chainID)))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:    big.NewInt(int64(chainID)),
		Nonce:      nonce,
		GasTipCap:  new(big.Int).SetUint64(tipWei),
		GasFeeCap:  new(big.Int).SetUint64(feeCapWei),
		Gas:        gasLimit,
		To:         &probeAddr,
		Value:      big.NewInt(0),
		Data:       calldata,
		AccessList: accessList,
	})
	signed, err := types.SignTx(tx, signer, w.Key)
	must(err)
	return signed
}

func runT8N(evmBin string, allocJSON, envJSON, txsJSON []byte) (*t8nResult, []byte, []byte, error) {
	tmpDir, err := os.MkdirTemp("", "pluto-p2-t8n-*")
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

func traceTxCall(evmBin string, allocJSON []byte, tx *types.Transaction, fnName string) (*traceStats, *t8nResult, error) {
	tmpDir, err := os.MkdirTemp("", "pluto-p2-trace-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmpDir)

	allocPath := filepath.Join(tmpDir, "alloc.json")
	envPath := filepath.Join(tmpDir, "env.json")
	txsPath := filepath.Join(tmpDir, "txs.json")
	txsJSON, err := json.MarshalIndent([]*types.Transaction{tx}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	must(os.WriteFile(allocPath, allocJSON, 0o600))
	must(os.WriteFile(envPath, mustMarshalBlockEnv(1), 0o600))
	must(os.WriteFile(txsPath, txsJSON, 0o600))

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
		return nil, nil, fmt.Errorf("evm t8n --trace (%s) failed: %w (stderr=%s)", fnName, err, strings.TrimSpace(stderr.String()))
	}

	resBytes, err := os.ReadFile(filepath.Join(tmpDir, "result.json"))
	if err != nil {
		return nil, nil, err
	}
	var res t8nResult
	if err := json.Unmarshal(resBytes, &res); err != nil {
		return nil, nil, err
	}

	matches, err := filepath.Glob(filepath.Join(tmpDir, "trace-0-*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, nil, fmt.Errorf("no trace-0-*.jsonl generated for %s", fnName)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	stats := &traceStats{
		Function:        fnName,
		FirstSLOADStep:  -1,
		FirstSSTOREStep: -1,
		FirstLOGStep:    -1,
		FirstCALLStep:   -1,
	}
	extCalls := map[string]bool{
		"CALL": true, "STATICCALL": true, "DELEGATECALL": true,
		"CALLCODE": true, "CREATE": true, "CREATE2": true, "SELFDESTRUCT": true,
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
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
			continue
		}
		if rec.Depth > stats.MaxDepth {
			stats.MaxDepth = rec.Depth
		}
		if extCalls[rec.OpName] {
			stats.HasExternalCall = true
			stats.ExternalCallOps = append(stats.ExternalCallOps, rec.OpName)
			if stats.FirstCALLStep == -1 {
				stats.FirstCALLStep = step
			}
		}
		if rec.OpName == "SLOAD" {
			stats.SLOADCount++
			if stats.FirstSLOADStep == -1 {
				stats.FirstSLOADStep = step
				stats.FirstSLOADPC = rec.PC
			}
		}
		if rec.OpName == "SSTORE" {
			stats.SSTORECount++
			if stats.FirstSSTOREStep == -1 {
				stats.FirstSSTOREStep = step
				stats.FirstSSTOREPC = rec.PC
			}
		}
		if strings.HasPrefix(rec.OpName, "LOG") && stats.FirstLOGStep == -1 {
			stats.FirstLOGStep = step
		}
		if rec.OpName == "KECCAK256" || rec.OpName == "SHA3" {
			stats.KeccakTotal++
			if stats.FirstSLOADStep == -1 {
				stats.KeccakBeforeSLOAD++
			} else {
				stats.KeccakAfterSLOAD++
			}
			if stats.FirstSSTOREStep == -1 {
				stats.KeccakBeforeSSTORE++
			} else {
				stats.KeccakAfterSSTORE++
			}
		}
		step++
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	stats.TotalOpcodeSteps = step
	return stats, &res, nil
}

func verifyAllFunctionTraces(evmBin string, wallets []wallet, cs *compiledSuite, standardAlloc, sstoreGasAlloc, errorsAlloc []byte) map[string]*traceStats {
	seed := crypto.Keccak256Hash([]byte("phase2-trace-probe"))
	w := wallets[0]

	type probeSpec struct {
		name       string
		alloc      []byte
		calldata   []byte
		wantStatus string
		check      func(*traceStats) error
	}

	singleFrameNoExt := func(st *traceStats) error {
		if st.MaxDepth != 1 || st.HasExternalCall {
			return fmt.Errorf("%s: expected 1 call frame without external call, got maxDepth=%d ext=%v", st.Function, st.MaxDepth, st.ExternalCallOps)
		}
		return nil
	}

	specs := []probeSpec{
		{
			name:       "readThenHashIncrement",
			alloc:      standardAlloc,
			calldata:   encodeCall(cs.SelReadThenHashIncrement, encodeWordU64(hashRounds), seed.Bytes()),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.FirstSLOADStep < 0 || st.FirstSSTOREStep < 0 {
					return fmt.Errorf("missing SLOAD/SSTORE: %+v", st)
				}
				if st.KeccakBeforeSLOAD != 0 || st.KeccakAfterSLOAD != int(hashRounds) {
					return fmt.Errorf("readThenHashIncrement: want 0 keccak before SLOAD & %d after, got %d/%d", hashRounds, st.KeccakBeforeSLOAD, st.KeccakAfterSLOAD)
				}
				if st.KeccakBeforeSSTORE != int(hashRounds) || st.KeccakAfterSSTORE != 0 {
					return fmt.Errorf("readThenHashIncrement: want %d keccak before SSTORE, got %d/%d", hashRounds, st.KeccakBeforeSSTORE, st.KeccakAfterSSTORE)
				}
				return nil
			},
		},
		{
			name:       "hashThenIncrement",
			alloc:      standardAlloc,
			calldata:   encodeCall(cs.SelHashThenIncrement, encodeWordU64(hashRounds), seed.Bytes()),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.FirstSLOADStep < 0 || st.FirstSSTOREStep <= st.FirstSLOADStep {
					return fmt.Errorf("invalid SLOAD/SSTORE order: %+v", st)
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.KeccakAfterSLOAD != 0 {
					return fmt.Errorf("hashThenIncrement: want %d keccak before SLOAD & 0 after, got %d/%d", hashRounds, st.KeccakBeforeSLOAD, st.KeccakAfterSLOAD)
				}
				return nil
			},
		},
		{
			name:       "hashThenIncrementOwnSlot",
			alloc:      standardAlloc,
			calldata:   encodeCall(cs.SelHashThenIncrementOwnSlot, encodeWordU64(1), encodeWordU64(hashRounds), seed.Bytes()),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.FirstSLOADStep < 0 || st.FirstSSTOREStep <= st.FirstSLOADStep {
					return fmt.Errorf("invalid SLOAD/SSTORE order: %+v", st)
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.KeccakAfterSLOAD != 0 {
					return fmt.Errorf("hashThenIncrementOwnSlot: want %d keccak before SLOAD & 0 after, got %d/%d", hashRounds, st.KeccakBeforeSLOAD, st.KeccakAfterSLOAD)
				}
				return nil
			},
		},
		{
			name:       "hashThenDirectSstore",
			alloc:      sstoreGasAlloc,
			calldata:   encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotSstoreGasX), encodeWordU64(hashRounds), seed.Bytes(), encodeWordU64(7)),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				// YÊU CẦU BẮT BUỘC: tuyệt đối 0 SLOAD trước SSTORE (và 0 SLOAD trong cả tx)
				if st.SLOADCount != 0 || st.FirstSLOADStep != -1 {
					return fmt.Errorf("hashThenDirectSstore MUST have 0 SLOAD, got count=%d firstStep=%d", st.SLOADCount, st.FirstSLOADStep)
				}
				if st.SSTORECount != 1 || st.FirstSSTOREStep < 0 {
					return fmt.Errorf("hashThenDirectSstore expected 1 SSTORE, got count=%d", st.SSTORECount)
				}
				if st.KeccakBeforeSSTORE != int(hashRounds) || st.KeccakAfterSSTORE != 0 {
					return fmt.Errorf("hashThenDirectSstore: want %d keccak before SSTORE & 0 after, got %d/%d", hashRounds, st.KeccakBeforeSSTORE, st.KeccakAfterSSTORE)
				}
				return nil
			},
		},
		{
			name:       "hashThenWriteLogRevert",
			alloc:      errorsAlloc,
			calldata:   encodeCall(cs.SelHashThenWriteLogRevert, encodeWordU64(slotErrRevertY), encodeWordU64(hashRounds), seed.Bytes(), encodeWordU64(999)),
			wantStatus: "0x0",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.FirstSSTOREStep <= st.FirstSLOADStep || st.FirstLOGStep <= st.FirstSSTOREStep {
					return fmt.Errorf("hashThenWriteLogRevert unexpected opcode order: %+v", st)
				}
				return nil
			},
		},
		{
			name:       "hashThenExpandMemoryAndStore",
			alloc:      errorsAlloc,
			calldata:   encodeCall(cs.SelHashThenExpandMemoryStore, encodeWordU64(slotErrMem), encodeWordU64(hashRounds), seed.Bytes(), encodeWordU64(64), encodeWordU64(96)),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.FirstSSTOREStep <= st.FirstSLOADStep {
					return fmt.Errorf("hashThenExpandMemoryAndStore unexpected order: %+v", st)
				}
				return nil
			},
		},
		{
			name:       "hashThenWarmColdRefund",
			alloc:      errorsAlloc,
			calldata:   encodeCall(cs.SelHashThenWarmColdRefund, encodeWordU64(slotErrWarm0A), encodeWordU64(hashRounds), seed.Bytes(), encodeWordU64(42), encodeWordU64(0)),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if err := singleFrameNoExt(st); err != nil {
					return err
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.SLOADCount != 4 || st.SSTORECount != 2 {
					return fmt.Errorf("hashThenWarmColdRefund unexpected counts: %+v", st)
				}
				return nil
			},
		},
		{
			name:       "hashThenExternalCall",
			alloc:      errorsAlloc,
			calldata:   encodeCall(cs.SelHashThenExternalCall, encodeWordAddr(helperAddr), encodeWordU64(hashRounds), seed.Bytes()),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if st.MaxDepth != 2 || !st.HasExternalCall {
					return fmt.Errorf("hashThenExternalCall expected depth=2 & CALL: %+v", st)
				}
				if st.KeccakBeforeSLOAD != int(hashRounds) || st.FirstCALLStep <= st.FirstSSTOREStep {
					return fmt.Errorf("hashThenExternalCall expected CALL in suffix after SLOAD/SSTORE: %+v", st)
				}
				return nil
			},
		},
		{
			name:       "externalCallThenRead",
			alloc:      errorsAlloc,
			calldata:   encodeCall(cs.SelExternalCallThenRead, encodeWordAddr(helperAddr), encodeWordU64(hashRounds), seed.Bytes()),
			wantStatus: "0x1",
			check: func(st *traceStats) error {
				if st.MaxDepth != 2 || !st.HasExternalCall {
					return fmt.Errorf("externalCallThenRead expected depth=2 & CALL: %+v", st)
				}
				if st.FirstCALLStep < 0 || st.FirstCALLStep >= st.FirstSLOADStep {
					return fmt.Errorf("externalCallThenRead expected CALL before SLOAD: %+v", st)
				}
				return nil
			},
		},
	}

	out := make(map[string]*traceStats, len(specs))
	for _, sp := range specs {
		tx := signProbeTx(w, 0, txGasLimit, sp.calldata, nil)
		st, res, err := traceTxCall(evmBin, sp.alloc, tx, sp.name)
		must(err)
		if len(res.Rejected) != 0 || len(res.Receipts) != 1 || res.Receipts[0].Status != sp.wantStatus {
			must(fmt.Errorf("trace %s unexpected result: rejected=%v receipts=%+v", sp.name, res.Rejected, res.Receipts))
		}
		must(sp.check(st))
		out[sp.name] = st
	}
	return out
}

func printTraceTable(m map[string]*traceStats) {
	names := []string{
		"readThenHashIncrement",
		"hashThenIncrement",
		"hashThenIncrementOwnSlot",
		"hashThenDirectSstore",
		"hashThenWriteLogRevert",
		"hashThenExpandMemoryAndStore",
		"hashThenWarmColdRefund",
		"hashThenExternalCall",
		"externalCallThenRead",
	}
	fmt.Printf("   %-30s %6s %5s %5s %10s %10s %8s %8s %8s\n",
		"Function", "Steps", "Depth", "SLOAD", "FirstSLOAD", "FirstSSTR", "K<SLOAD", "K>SLOAD", "K<SSTR")
	for _, n := range names {
		st := m[n]
		fmt.Printf("   %-30s %6d %5d %5d %10d %10d %8d %8d %8d\n",
			st.Function, st.TotalOpcodeSteps, st.MaxDepth, st.SLOADCount,
			st.FirstSLOADStep, st.FirstSSTOREStep,
			st.KeccakBeforeSLOAD, st.KeccakAfterSLOAD, st.KeccakBeforeSSTORE)
	}
}

func planFairBenchSchedule(seed uint64) ([][]int, [][]common.Hash) {
	rng := &splitMix64{state: seed ^ 0x5048415345320001}
	orders := make([][]int, blockCount)
	seeds := make([][]common.Hash, blockCount)
	for h := 0; h < blockCount; h++ {
		perm := make([]int, txPerBlock)
		for i := range perm {
			perm[i] = i
		}
		rng.shuffleInts(perm)
		orders[h] = perm

		blockSeeds := make([]common.Hash, txPerBlock)
		for i := 0; i < txPerBlock; i++ {
			var buf [24]byte
			new(big.Int).SetUint64(seed).FillBytes(buf[0:8])
			new(big.Int).SetUint64(uint64(h + 1)).FillBytes(buf[8:16])
			new(big.Int).SetUint64(uint64(i)).FillBytes(buf[16:24])
			blockSeeds[i] = crypto.Keccak256Hash(buf[:])
		}
		seeds[h] = blockSeeds
	}
	return orders, seeds
}

func readAccountStorage(postAllocBytes []byte, target common.Address) map[common.Hash]common.Hash {
	var postAlloc map[string]allocAccount
	must(json.Unmarshal(postAllocBytes, &postAlloc))
	out := make(map[common.Hash]common.Hash)
	for addrStr, acc := range postAlloc {
		if strings.EqualFold(addrStr, target.Hex()) {
			for k, v := range acc.Storage {
				out[common.HexToHash(k)] = common.HexToHash(v)
			}
			break
		}
	}
	return out
}

func assertSlotU64(label string, storage map[common.Hash]common.Hash, slot, want uint64) {
	got := storage[u64Hash(slot)]
	wantH := u64Hash(want)
	if got != wantH {
		must(fmt.Errorf("%s: slot %d mismatch: got %s want %s (%d)", label, slot, got.Hex(), wantH.Hex(), want))
	}
}

func generate6BlockCase(
	evmBin, caseDir, caseID string,
	wallets []wallet,
	walletOrders [][]int,
	txSeeds [][]common.Hash,
	cs *compiledSuite,
	allocBytes []byte,
	fnTrace *traceStats,
) map[string]any {
	must(os.RemoveAll(caseDir))
	must(os.MkdirAll(caseDir, 0o755))

	nonces := make([]uint64, walletCount)
	curAlloc := allocBytes
	var blockRels []string
	var schedule []map[string]any
	var totalGas uint64

	wantTopic := cs.TopicCountUpdated
	if caseID == "checkpoint-independent" {
		wantTopic = cs.TopicSlotUpdated
	}

	for h := 0; h < blockCount; h++ {
		height := uint64(h + 1)
		rel := fmt.Sprintf("blocks/block-%02d", height)
		blockDir := filepath.Join(caseDir, filepath.FromSlash(rel))
		must(os.MkdirAll(blockDir, 0o755))
		blockRels = append(blockRels, rel)

		txs := make([]*types.Transaction, 0, txPerBlock)
		for i := 0; i < txPerBlock; i++ {
			wIdx := walletOrders[h][i]
			w := wallets[wIdx]
			txSeed := txSeeds[h][i]
			var calldata []byte
			switch caseID {
			case "checkpoint-independent":
				calldata = encodeCall(cs.SelHashThenIncrementOwnSlot, encodeWordU64(uint64(wIdx+1)), encodeWordU64(hashRounds), txSeed.Bytes())
			case "checkpoint-early":
				calldata = encodeCall(cs.SelReadThenHashIncrement, encodeWordU64(hashRounds), txSeed.Bytes())
			case "checkpoint-late":
				calldata = encodeCall(cs.SelHashThenIncrement, encodeWordU64(hashRounds), txSeed.Bytes())
			default:
				must(fmt.Errorf("unknown 6-block case %q", caseID))
			}
			txs = append(txs, signProbeTx(w, nonces[wIdx], txGasLimit, calldata, nil))
			nonces[wIdx]++
		}

		envBytes := mustMarshalBlockEnv(height)
		txsBytes, err := json.MarshalIndent(txs, "", "  ")
		must(err)
		txsBytes = append(txsBytes, '\n')

		res, resBytes, postAllocBytes, err := runT8N(evmBin, curAlloc, envBytes, txsBytes)
		must(err)
		if len(res.Rejected) != 0 {
			must(fmt.Errorf("%s block %d: rejected txs %+v", caseID, height, res.Rejected))
		}
		if len(res.Receipts) != txPerBlock {
			must(fmt.Errorf("%s block %d: expected %d receipts, got %d", caseID, height, txPerBlock, len(res.Receipts)))
		}
		for i, r := range res.Receipts {
			if r.Status != "0x1" {
				must(fmt.Errorf("%s block %d tx %d: status=%s (want 0x1)", caseID, height, i, r.Status))
			}
			if len(r.Logs) != 1 || !strings.EqualFold(r.Logs[0].Topics[0], wantTopic.Hex()) {
				must(fmt.Errorf("%s block %d tx %d: unexpected logs %+v", caseID, height, i, r.Logs))
			}
			if parseHexUint64(r.GasUsed) >= txGasLimit {
				must(fmt.Errorf("%s block %d tx %d: gasUsed %d >= txGasLimit", caseID, height, i, parseHexUint64(r.GasUsed)))
			}
		}

		// verify post-state storage
		st := readAccountStorage(postAllocBytes, probeAddr)
		if caseID == "checkpoint-independent" {
			assertSlotU64(caseID, st, 0, initialCount)
			for wIdx := 0; wIdx < walletCount; wIdx++ {
				assertSlotU64(caseID, st, uint64(wIdx+1), height)
			}
		} else {
			assertSlotU64(caseID, st, 0, initialCount+height*uint64(txPerBlock))
		}

		blkGas := parseHexUint64(res.GasUsed)
		if blkGas > blockGasLimit {
			must(fmt.Errorf("%s block %d: block gasUsed %d > limit", caseID, height, blkGas))
		}
		totalGas += blkGas

		must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))
		curAlloc = postAllocBytes

		schedule = append(schedule, map[string]any{
			"dir":     rel,
			"height":  height,
			"txCount": txPerBlock,
			"gasUsed": blkGas,
		})
	}

	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cs.Probe.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cs.Probe.StorageLayout))

	seedCopy := suiteSeed
	mHash := writeCaseManifest(caseDir, caseID, &seedCopy, blockRels, schedule, cs, map[string]any{
		"function":     fnTrace.Function,
		"hashRounds":   hashRounds,
		"initialCount": initialCount,
		"trace":        fnTrace,
	}, false)

	fmt.Printf("   %-24s: %d blocks, %d txs, all status=0x1, totalGas=%d\n", caseID, blockCount, blockCount*txPerBlock, totalGas)
	return map[string]any{
		"dir":            caseID,
		"manifestSha256": mHash,
		"blocks":         blockCount,
		"totalTxs":       blockCount * txPerBlock,
		"totalGasUsed":   totalGas,
	}
}

func generateSstoreGasCase(
	evmBin, caseDir string,
	wallets []wallet,
	cs *compiledSuite,
	allocBytes []byte,
	directTrace *traceStats,
) map[string]any {
	must(os.RemoveAll(caseDir))
	must(os.MkdirAll(caseDir, 0o755))

	type blockPlan struct {
		height   uint64
		desc     string
		slot     uint64
		aWallet  int
		aVal     uint64
		bWallet  int
		bVal     uint64
		wantX    uint64
		wantY    uint64
	}
	plans := []blockPlan{
		{
			height:  1,
			desc:    "Block 1: A writes slot X (0->5), B hashes 256 rounds then direct SSTORE(X, 7) (0 SLOAD)",
			slot:    slotSstoreGasX,
			aWallet: 0, aVal: 5,
			bWallet: 1, bVal: 7,
			wantX: 7, wantY: 0,
		},
		{
			height:  2,
			desc:    "Block 2: repeat contention on slot X (A: 7->0, B: hash 256 then direct SSTORE(X, 9)) so history learns & captures before SSTORE",
			slot:    slotSstoreGasX,
			aWallet: 2, aVal: 0,
			bWallet: 3, bVal: 9,
			wantX: 9, wantY: 0,
		},
		{
			height:  3,
			desc:    "Block 3: switch contention to slot Y (A: 0->5, B: hash 256 then direct SSTORE(Y, 7)) to verify 1-block history lag",
			slot:    slotSstoreGasY,
			aWallet: 4, aVal: 5,
			bWallet: 5, bVal: 7,
			wantX: 9, wantY: 7,
		},
	}

	curAlloc := allocBytes
	var blockRels []string
	var schedule []map[string]any
	var totalGas uint64

	for _, p := range plans {
		rel := fmt.Sprintf("blocks/block-%02d", p.height)
		blockDir := filepath.Join(caseDir, filepath.FromSlash(rel))
		must(os.MkdirAll(blockDir, 0o755))
		blockRels = append(blockRels, rel)

		seedA := crypto.Keccak256Hash([]byte(fmt.Sprintf("sstore-gas-b%d-A", p.height)))
		seedB := crypto.Keccak256Hash([]byte(fmt.Sprintf("sstore-gas-b%d-B", p.height)))
		// A chạy 0 vòng băm để hoàn tất nhanh hơn B trong OCC, buộc B đọc nền cũ khi chạy song song
		txA := signProbeTx(wallets[p.aWallet], 0, txGasLimit,
			encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(p.slot), encodeWordU64(0), seedA.Bytes(), encodeWordU64(p.aVal)), nil)
		txB := signProbeTx(wallets[p.bWallet], 0, txGasLimit,
			encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(p.slot), encodeWordU64(hashRounds), seedB.Bytes(), encodeWordU64(p.bVal)), nil)

		txs := []*types.Transaction{txA, txB}
		envBytes := mustMarshalBlockEnv(p.height)
		txsBytes, err := json.MarshalIndent(txs, "", "  ")
		must(err)
		txsBytes = append(txsBytes, '\n')

		res, resBytes, postAllocBytes, err := runT8N(evmBin, curAlloc, envBytes, txsBytes)
		must(err)
		if len(res.Rejected) != 0 || len(res.Receipts) != 2 {
			must(fmt.Errorf("checkpoint-sstore-gas block %d: rejected=%v receipts=%d", p.height, res.Rejected, len(res.Receipts)))
		}
		for i, r := range res.Receipts {
			if r.Status != "0x1" || len(r.Logs) != 1 || !strings.EqualFold(r.Logs[0].Topics[0], cs.TopicSlotUpdated.Hex()) {
				must(fmt.Errorf("checkpoint-sstore-gas block %d tx %d: unexpected receipt %+v", p.height, i, r))
			}
		}

		st := readAccountStorage(postAllocBytes, probeAddr)
		assertSlotU64("checkpoint-sstore-gas", st, slotSstoreGasX, p.wantX)
		assertSlotU64("checkpoint-sstore-gas", st, slotSstoreGasY, p.wantY)

		blkGas := parseHexUint64(res.GasUsed)
		totalGas += blkGas

		must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))
		curAlloc = postAllocBytes

		schedule = append(schedule, map[string]any{
			"dir":         rel,
			"height":      p.height,
			"description": p.desc,
			"slot":        p.slot,
			"txCount":     2,
			"gasUsed":     blkGas,
			"txGasUsed":   []uint64{parseHexUint64(res.Receipts[0].GasUsed), parseHexUint64(res.Receipts[1].GasUsed)},
		})
	}

	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cs.Probe.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cs.Probe.StorageLayout))

	mHash := writeCaseManifest(caseDir, "checkpoint-sstore-gas", nil, blockRels, schedule, cs, map[string]any{
		"slotX":      slotSstoreGasX,
		"slotY":      slotSstoreGasY,
		"hashRounds": hashRounds,
		"trace":      directTrace,
	}, false)

	fmt.Printf("   %-24s: 3 blocks, 6 txs, all status=0x1, totalGas=%d\n", "checkpoint-sstore-gas", totalGas)
	return map[string]any{
		"dir":            "checkpoint-sstore-gas",
		"manifestSha256": mHash,
		"blocks":         3,
		"totalTxs":       6,
		"totalGasUsed":   totalGas,
	}
}

func calibrateOOGThreshold(evmBin string, wallets []wallet, cs *compiledSuite, errorsAlloc []byte) (oogThresholdInfo, common.Hash, common.Hash) {
	seedA := crypto.Keccak256Hash([]byte("errors-b1-oog-A"))
	seedB := crypto.Keccak256Hash([]byte("errors-b1-oog-B"))
	callA := encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrOOG), encodeWordU64(0), seedA.Bytes(), encodeWordU64(5))
	callB := encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrOOG), encodeWordU64(hashRounds), seedB.Bytes(), encodeWordU64(7))

	// 1. Measure gas for B standalone on slot=0 (0 -> 7)
	txB0 := signProbeTx(wallets[1], 0, txGasLimit, callB, nil)
	txsB0, err := json.MarshalIndent([]*types.Transaction{txB0}, "", "  ")
	must(err)
	resB0, _, _, err := runT8N(evmBin, errorsAlloc, mustMarshalBlockEnv(1), txsB0)
	must(err)
	if len(resB0.Rejected) != 0 || len(resB0.Receipts) != 1 || resB0.Receipts[0].Status != "0x1" {
		must(fmt.Errorf("probe 0->7 failed: %+v", resB0))
	}
	gas0To7 := parseHexUint64(resB0.Receipts[0].GasUsed)

	// 2. Measure gas for B after A (5 -> 7)
	txA := signProbeTx(wallets[0], 0, txGasLimit, callA, nil)
	txsAB, err := json.MarshalIndent([]*types.Transaction{txA, txB0}, "", "  ")
	must(err)
	resAB, _, _, err := runT8N(evmBin, errorsAlloc, mustMarshalBlockEnv(1), txsAB)
	must(err)
	if len(resAB.Rejected) != 0 || len(resAB.Receipts) != 2 || resAB.Receipts[1].Status != "0x1" {
		must(fmt.Errorf("probe 5->7 failed: %+v", resAB))
	}
	gas5To7 := parseHexUint64(resAB.Receipts[1].GasUsed)

	if gas0To7 <= gas5To7+4000 {
		must(fmt.Errorf("unexpectedly small gas gap between 0->7 (%d) and 5->7 (%d)", gas0To7, gas5To7))
	}
	// chọn ngưỡng sát phía trên gas5To7 (dư 2000 gas) nhưng thấp hơn hẳn gas0To7 (chênh ~17100 gas)
	chosenLimit := gas5To7 + 2000

	// 3. Double-verify standalone B with chosenLimit -> MUST OOG (status = 0x0, 0 logs)
	txBExact := signProbeTx(wallets[1], 0, chosenLimit, callB, nil)
	txsBExact, err := json.MarshalIndent([]*types.Transaction{txBExact}, "", "  ")
	must(err)
	resStandalone, _, postStandalone, err := runT8N(evmBin, errorsAlloc, mustMarshalBlockEnv(1), txsBExact)
	must(err)
	if len(resStandalone.Rejected) != 0 || len(resStandalone.Receipts) != 1 {
		must(fmt.Errorf("standalone OOG check rejected: %+v", resStandalone))
	}
	if resStandalone.Receipts[0].Status != "0x0" || len(resStandalone.Receipts[0].Logs) != 0 {
		must(fmt.Errorf("expected standalone 0->7 with gasLimit=%d to OOG (0x0), got status=%s logs=%d",
			chosenLimit, resStandalone.Receipts[0].Status, len(resStandalone.Receipts[0].Logs)))
	}
	assertSlotU64("standalone-oog-check", readAccountStorage(postStandalone, probeAddr), slotErrOOG, 0)

	// 4. Double-verify ordered [A, B] with chosenLimit -> MUST succeed (status = 0x1, 1 log, slot=7)
	txsOrdered, err := json.MarshalIndent([]*types.Transaction{txA, txBExact}, "", "  ")
	must(err)
	resOrdered, _, postOrdered, err := runT8N(evmBin, errorsAlloc, mustMarshalBlockEnv(1), txsOrdered)
	must(err)
	if len(resOrdered.Rejected) != 0 || len(resOrdered.Receipts) != 2 {
		must(fmt.Errorf("ordered 5->7 check rejected: %+v", resOrdered))
	}
	if resOrdered.Receipts[1].Status != "0x1" || len(resOrdered.Receipts[1].Logs) != 1 {
		must(fmt.Errorf("expected ordered 5->7 with gasLimit=%d to succeed (0x1), got status=%s logs=%d",
			chosenLimit, resOrdered.Receipts[1].Status, len(resOrdered.Receipts[1].Logs)))
	}
	assertSlotU64("ordered-5to7-check", readAccountStorage(postOrdered, probeAddr), slotErrOOG, 7)

	return oogThresholdInfo{
		Gas0To7:          gas0To7,
		Gas5To7:          gas5To7,
		GasDiff:          gas0To7 - gas5To7,
		ChosenTxGasLimit: chosenLimit,
		StandaloneStatus: resStandalone.Receipts[0].Status,
		OrderedStatus:    resOrdered.Receipts[1].Status,
	}, seedA, seedB
}

func generateErrorsCase(
	evmBin, caseDir string,
	wallets []wallet,
	cs *compiledSuite,
	allocBytes []byte,
	traceMap map[string]*traceStats,
) (oogThresholdInfo, map[string]any) {
	must(os.RemoveAll(caseDir))
	must(os.MkdirAll(caseDir, 0o755))

	oogInfo, oogSeedA, oogSeedB := calibrateOOGThreshold(evmBin, wallets, cs, allocBytes)
	oogSeedC := crypto.Keccak256Hash([]byte("errors-b1-oog-only"))

	curAlloc := allocBytes
	var blockRels []string
	var schedule []map[string]any
	var totalGas uint64
	var totalTxs int

	runAndSaveBlock := func(
		height uint64,
		desc string,
		txs []*types.Transaction,
		wantStatuses []string,
		wantLogs []int,
		postCheck func(postAlloc []byte, res *t8nResult),
	) {
		rel := fmt.Sprintf("blocks/block-%02d", height)
		blockDir := filepath.Join(caseDir, filepath.FromSlash(rel))
		must(os.MkdirAll(blockDir, 0o755))
		blockRels = append(blockRels, rel)

		envBytes := mustMarshalBlockEnv(height)
		txsBytes, err := json.MarshalIndent(txs, "", "  ")
		must(err)
		txsBytes = append(txsBytes, '\n')

		res, resBytes, postAllocBytes, err := runT8N(evmBin, curAlloc, envBytes, txsBytes)
		must(err)
		if len(res.Rejected) != 0 {
			must(fmt.Errorf("checkpoint-errors block %d: rejected txs %+v", height, res.Rejected))
		}
		if len(res.Receipts) != len(txs) {
			must(fmt.Errorf("checkpoint-errors block %d: expected %d receipts, got %d", height, len(txs), len(res.Receipts)))
		}
		txGasUsed := make([]uint64, len(txs))
		for i, r := range res.Receipts {
			if r.Status != wantStatuses[i] {
				must(fmt.Errorf("checkpoint-errors block %d tx %d: status=%s want=%s", height, i, r.Status, wantStatuses[i]))
			}
			if len(r.Logs) != wantLogs[i] {
				must(fmt.Errorf("checkpoint-errors block %d tx %d: logs=%d want=%d", height, i, len(r.Logs), wantLogs[i]))
			}
			txGasUsed[i] = parseHexUint64(r.GasUsed)
		}
		postCheck(postAllocBytes, res)

		blkGas := parseHexUint64(res.GasUsed)
		totalGas += blkGas
		totalTxs += len(txs)

		must(os.WriteFile(filepath.Join(blockDir, "env.json"), envBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "txs.json"), txsBytes, 0o644))
		must(os.WriteFile(filepath.Join(blockDir, "reference-result.json"), resBytes, 0o644))
		curAlloc = postAllocBytes

		schedule = append(schedule, map[string]any{
			"dir":              rel,
			"height":           height,
			"description":      desc,
			"txCount":          len(txs),
			"expectedStatuses": wantStatuses,
			"expectedLogCount": wantLogs,
			"gasUsed":          blkGas,
			"txGasUsed":        txGasUsed,
		})
	}

	// Block 1: OOG on speculative attempt (0->7 OOG, 5->7 OK) + genuine OOG tx on untouched slot
	runAndSaveBlock(
		1,
		"OOG on speculative first attempt: A writes slot 300 (0->5), B writes 7 with exact gasLimit between G(5->7) and G(0->7) -> succeeds; C writes untouched slot 301 with same gasLimit -> genuine OOG",
		[]*types.Transaction{
			signProbeTx(wallets[0], 0, txGasLimit,
				encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrOOG), encodeWordU64(0), oogSeedA.Bytes(), encodeWordU64(5)), nil),
			signProbeTx(wallets[1], 0, oogInfo.ChosenTxGasLimit,
				encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrOOG), encodeWordU64(hashRounds), oogSeedB.Bytes(), encodeWordU64(7)), nil),
			signProbeTx(wallets[2], 0, oogInfo.ChosenTxGasLimit,
				encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrOOGOnly), encodeWordU64(hashRounds), oogSeedC.Bytes(), encodeWordU64(7)), nil),
		},
		[]string{"0x1", "0x1", "0x0"},
		[]int{1, 1, 0},
		func(postAlloc []byte, _ *t8nResult) {
			st := readAccountStorage(postAlloc, probeAddr)
			assertSlotU64("errors-b1", st, slotErrOOG, 7)
			assertSlotU64("errors-b1", st, slotErrOOGOnly, 0)
		},
	)

	// Block 2: Warm/cold & original storage refund (with and without EIP-2930 access list)
	alSlot311 := types.AccessList{{Address: probeAddr, StorageKeys: []common.Hash{u64Hash(slotErrWarm0B)}}}
	alSlot312 := types.AccessList{{Address: probeAddr, StorageKeys: []common.Hash{u64Hash(slotErrWarmNZ)}}}
	runAndSaveBlock(
		2,
		"Warm/cold & EIP-2200/3529 original-storage refund with and without EIP-2930 access list (0->42->0 cold & warm, contended 0->10 then 10->20->10 & 10->20->0)",
		[]*types.Transaction{
			signProbeTx(wallets[3], 0, txGasLimit,
				encodeCall(cs.SelHashThenWarmColdRefund, encodeWordU64(slotErrWarm0A), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b2-t0")).Bytes(), encodeWordU64(42), encodeWordU64(0)), nil),
			signProbeTx(wallets[4], 0, txGasLimit,
				encodeCall(cs.SelHashThenWarmColdRefund, encodeWordU64(slotErrWarm0B), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b2-t1")).Bytes(), encodeWordU64(42), encodeWordU64(0)), alSlot311),
			signProbeTx(wallets[5], 0, txGasLimit,
				encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrWarmNZ), encodeWordU64(0), crypto.Keccak256Hash([]byte("errors-b2-t2")).Bytes(), encodeWordU64(10)), nil),
			signProbeTx(wallets[6], 0, txGasLimit,
				encodeCall(cs.SelHashThenWarmColdRefund, encodeWordU64(slotErrWarmNZ), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b2-t3")).Bytes(), encodeWordU64(20), encodeWordU64(10)), nil),
			signProbeTx(wallets[7], 0, txGasLimit,
				encodeCall(cs.SelHashThenWarmColdRefund, encodeWordU64(slotErrWarmNZ), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b2-t4")).Bytes(), encodeWordU64(20), encodeWordU64(0)), alSlot312),
		},
		[]string{"0x1", "0x1", "0x1", "0x1", "0x1"},
		[]int{1, 1, 1, 1, 1},
		func(postAlloc []byte, res *t8nResult) {
			st := readAccountStorage(postAlloc, probeAddr)
			assertSlotU64("errors-b2", st, slotErrWarm0A, 0)
			assertSlotU64("errors-b2", st, slotErrWarm0B, 0)
			assertSlotU64("errors-b2", st, slotErrWarmNZ, 0)
			// sanity: access list adds 2400+1900 intrinsic vs saves 2000 cold SLOAD -> tx1 gas must differ from tx0
			g0 := parseHexUint64(res.Receipts[0].GasUsed)
			g1 := parseHexUint64(res.Receipts[1].GasUsed)
			if g0 == g1 {
				must(fmt.Errorf("errors-b2: expected different gas between non-AL (%d) and AL (%d) txs", g0, g1))
			}
		},
	)

	// Block 3: Memory expansion before AND after SLOAD/SSTORE
	runAndSaveBlock(
		3,
		"Memory expansion before and after SLOAD/SSTORE (verifies memory.lastGasCost preservation across checkpoint resume)",
		[]*types.Transaction{
			signProbeTx(wallets[8], 0, txGasLimit,
				encodeCall(cs.SelHashThenDirectSstore, encodeWordU64(slotErrMem), encodeWordU64(0), crypto.Keccak256Hash([]byte("errors-b3-t0")).Bytes(), encodeWordU64(10)), nil),
			signProbeTx(wallets[9], 0, txGasLimit,
				encodeCall(cs.SelHashThenExpandMemoryStore, encodeWordU64(slotErrMem), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b3-t1")).Bytes(), encodeWordU64(64), encodeWordU64(96)), nil),
			signProbeTx(wallets[10], 0, txGasLimit,
				encodeCall(cs.SelHashThenExpandMemoryStore, encodeWordU64(slotErrMem), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b3-t2")).Bytes(), encodeWordU64(128), encodeWordU64(128)), nil),
		},
		[]string{"0x1", "0x1", "0x1"},
		[]int{1, 1, 1},
		func(postAlloc []byte, _ *t8nResult) {
			st := readAccountStorage(postAlloc, probeAddr)
			assertSlotU64("errors-b3", st, slotErrMem, 12)
		},
	)

	// Block 4: A updates count (slot 0), B hashes -> reads count -> writes slot Y + emits log -> reverts; C reads count & increments
	runAndSaveBlock(
		4,
		"Post-checkpoint write to slot Y + log + revert after A updates slot 0 (count): A's write persists, B's slot Y and log roll back",
		[]*types.Transaction{
			signProbeTx(wallets[11], 0, txGasLimit,
				encodeCall(cs.SelHashThenIncrement, encodeWordU64(0), crypto.Keccak256Hash([]byte("errors-b4-t0")).Bytes()), nil),
			signProbeTx(wallets[12], 0, txGasLimit,
				encodeCall(cs.SelHashThenWriteLogRevert, encodeWordU64(slotErrRevertY), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b4-t1")).Bytes(), encodeWordU64(999)), nil),
			signProbeTx(wallets[13], 0, txGasLimit,
				encodeCall(cs.SelHashThenIncrement, encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b4-t2")).Bytes()), nil),
		},
		[]string{"0x1", "0x0", "0x1"},
		[]int{1, 0, 1},
		func(postAlloc []byte, _ *t8nResult) {
			st := readAccountStorage(postAlloc, probeAddr)
			assertSlotU64("errors-b4", st, 0, initialCount+2) // 10 -> 11 -> (revert) -> 12
			assertSlotU64("errors-b4", st, slotErrRevertY, 0)
		},
	)

	// Block 5: External CALL in suffix (hashThenExternalCall) + external CALL in prefix (externalCallThenRead)
	runAndSaveBlock(
		5,
		"External CALL in suffix (cancels resume -> full replay fallback) and external CALL in prefix (stops checkpointing for remainder of attempt)",
		[]*types.Transaction{
			signProbeTx(wallets[14], 0, txGasLimit,
				encodeCall(cs.SelHashThenIncrement, encodeWordU64(0), crypto.Keccak256Hash([]byte("errors-b5-t0")).Bytes()), nil),
			signProbeTx(wallets[15], 0, txGasLimit,
				encodeCall(cs.SelHashThenExternalCall, encodeWordAddr(helperAddr), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b5-t1")).Bytes()), nil),
			signProbeTx(wallets[16], 0, txGasLimit,
				encodeCall(cs.SelExternalCallThenRead, encodeWordAddr(helperAddr), encodeWordU64(hashRounds), crypto.Keccak256Hash([]byte("errors-b5-t2")).Bytes()), nil),
			signProbeTx(wallets[17], 0, txGasLimit,
				encodeCall(cs.SelHashThenIncrement, encodeWordU64(64), crypto.Keccak256Hash([]byte("errors-b5-t3")).Bytes()), nil),
		},
		[]string{"0x1", "0x1", "0x1", "0x1"},
		[]int{1, 2, 2, 1},
		func(postAlloc []byte, _ *t8nResult) {
			stProbe := readAccountStorage(postAlloc, probeAddr)
			stHelper := readAccountStorage(postAlloc, helperAddr)
			assertSlotU64("errors-b5-probe", stProbe, 0, initialCount+6) // 12 -> 13 -> 14 -> 15 -> 16
			assertSlotU64("errors-b5-helper", stHelper, 0, 2)            // ping called twice
		},
	)

	must(os.WriteFile(filepath.Join(caseDir, "alloc.json"), allocBytes, 0o644))
	must(writeFormattedJSON(filepath.Join(caseDir, "abi.json"), cs.Probe.ABI))
	must(writeFormattedJSON(filepath.Join(caseDir, "storage-layout.json"), cs.Probe.StorageLayout))
	must(writeFormattedJSON(filepath.Join(caseDir, "helper-abi.json"), cs.Helper.ABI))

	mHash := writeCaseManifest(caseDir, "checkpoint-errors", nil, blockRels, schedule, cs, map[string]any{
		"helperAddress": helperAddr.Hex(),
		"oogThreshold":  oogInfo,
		"traces": map[string]*traceStats{
			"hashThenDirectSstore":         traceMap["hashThenDirectSstore"],
			"hashThenWarmColdRefund":       traceMap["hashThenWarmColdRefund"],
			"hashThenExpandMemoryAndStore": traceMap["hashThenExpandMemoryAndStore"],
			"hashThenWriteLogRevert":       traceMap["hashThenWriteLogRevert"],
			"hashThenExternalCall":         traceMap["hashThenExternalCall"],
			"externalCallThenRead":         traceMap["externalCallThenRead"],
		},
	}, true)

	fmt.Printf("   %-24s: 5 blocks, %d txs, statuses verified, totalGas=%d\n", "checkpoint-errors", totalTxs, totalGas)
	return oogInfo, map[string]any{
		"dir":            "checkpoint-errors",
		"manifestSha256": mHash,
		"blocks":         5,
		"totalTxs":       totalTxs,
		"totalGasUsed":   totalGas,
		"oogThreshold":   oogInfo,
	}
}

func writeCaseManifest(
	caseDir, caseID string,
	seed *uint64,
	blockRels []string,
	schedule []map[string]any,
	cs *compiledSuite,
	extra map[string]any,
	hasHelper bool,
) string {
	relFiles := []string{
		"abi.json",
		"alloc.json",
		"storage-layout.json",
	}
	if hasHelper {
		relFiles = append(relFiles, "helper-abi.json")
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
		"contractAddress":      probeAddr.Hex(),
		"runtimeCodeHash":      cs.Probe.RuntimeCodeHash.Hex(),
		"solidityVersion":      cs.Meta.SolidityVersion,
		"solcFullVersion":      cs.Meta.SolcFullVersion,
		"evmVersion":           cs.Meta.EVMVersion,
		"optimizerEnabled":     cs.Meta.OptimizerEnabled,
		"metadataBytecodeHash": cs.Meta.MetadataBytecodeHash,
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
		manifest["prng"] = "splitmix64"
	}
	for k, v := range extra {
		manifest[k] = v
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	must(err)
	b = append(b, '\n')
	must(os.WriteFile(filepath.Join(caseDir, "manifest.json"), b, 0o644))
	return sha256Hex(b)
}
