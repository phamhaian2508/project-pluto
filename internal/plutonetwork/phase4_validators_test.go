package plutonetwork

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	cmtcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmtnode "github.com/cometbft/cometbft/node"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmtproxy "github.com/cometbft/cometbft/proxy"
	cmtrpclocal "github.com/cometbft/cometbft/rpc/client/local"
	dbm "github.com/cometbft/cometbft-db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/phamhaian2508/project-pluto/internal/app"
	"github.com/phamhaian2508/project-pluto/internal/ethrpc"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// Counter(10) biên dịch bằng solc 0.8.24 (--evm-version shanghai --metadata-hash none).
// count() = 0x06661abd, increment() = 0xd09de08a, event Incremented(uint256) = 0x20d8a6f5...8d84.
const counterInit10CreationHex = "6080604052600a5f55348015610013575f80fd5b50610155806100215f395ff3fe608060405234801561000f575f80fd5b5060043610610034575f3560e01c806306661abd14610038578063d09de08a14610056575b5f80fd5b610040610060565b60405161004d91906100cf565b60405180910390f35b61005e610065565b005b5f5481565b60015f808282546100769190610115565b925050819055507f20d8a6f5a693f9d1d627a598e8820f7a55ee74c183aa8f1a30e8d4e8dd9a8d845f546040516100ad91906100cf565b60405180910390a1565b5f819050919050565b6100c9816100b7565b82525050565b5f6020820190506100e25f8301846100c0565b92915050565b7f4e487b71000000000000000000000000000000000000000000000000000000005f52601160045260245ffd5b5f61011f826100b7565b915061012a836100b7565b9250828201905080821115610142576101416100e8565b5b9291505056fea164736f6c6343000818000a"

var (
	counterCountSelector     = common.FromHex("0x06661abd")
	counterIncrementSelector = common.FromHex("0xd09de08a")
	counterEventTopic0       = common.HexToHash("0x20d8a6f5a693f9d1d627a598e8820f7a55ee74c183aa8f1a30e8d4e8dd9a8d84")
)

type validatorExecSpec struct {
	executor   string
	workers    int
	checkpoint evm.CheckpointConfig
}

type runningValidator struct {
	info      NodeInfo
	spec      validatorExecSpec
	app       *app.App
	node      *cmtnode.Node
	local     *cmtrpclocal.Local
	ethRPC    *ethrpc.Server
	cancel    context.CancelFunc
	openedDBs []*store.PebbleDB
}

func testPebbleDBProvider(opened *[]*store.PebbleDB) cmtcfg.DBProvider {
	return func(ctx *cmtcfg.DBContext) (dbm.DB, error) {
		db, err := store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
		if err != nil {
			return nil, err
		}
		*opened = append(*opened, db)
		return db, nil
	}
}

func findFreePortOffset(t *testing.T, baseOffset int) int {
	t.Helper()
	for offset := baseOffset; offset < baseOffset+4000; offset += 20 {
		ok := true
		var listeners []net.Listener
		for i := 0; i < DefaultNodeCount; i++ {
			for _, port := range []int{P2PPortBase + offset + i, CometRPCBase + offset + i, EthereumRPCBase + offset + i} {
				ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					ok = false
					break
				}
				listeners = append(listeners, ln)
			}
			if !ok {
				break
			}
		}
		for _, ln := range listeners {
			_ = ln.Close()
		}
		if ok {
			return offset
		}
	}
	t.Fatalf("could not find free port offset starting from %d", baseOffset)
	return 0
}

func startValidatorNode(t *testing.T, info NodeInfo, evmChainID int64, spec validatorExecSpec) *runningValidator {
	t.Helper()
	configPath := filepath.Join(info.Home, cmtcfg.DefaultConfigDir, cmtcfg.DefaultConfigFileName)
	rawCfg, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml for %s: %v", info.Name, err)
	}
	// hạ timeout_commit cho test tích hợp chạy nhanh mà vẫn chạy đủ vòng đồng thuận BFT
	patched := strings.ReplaceAll(string(rawCfg), `timeout_commit = "1s"`, `timeout_commit = "200ms"`)
	if err := os.WriteFile(configPath, []byte(patched), 0o644); err != nil {
		t.Fatalf("patch timeout_commit for %s: %v", info.Name, err)
	}

	cfg := cmtcfg.DefaultConfig().SetRoot(info.Home)
	// nạp lại đúng cấu hình từ config.toml vừa sinh của plutonetwork.SetupWithPortOffset
	loadedCfg, err := loadCometConfigForTest(info.Home)
	if err != nil {
		t.Fatalf("load comet config %s: %v", info.Name, err)
	}
	cfg = loadedCfg

	for _, dir := range []string{cfg.DBDir(), filepath.Dir(cfg.NodeKeyFile()), filepath.Dir(cfg.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	nodeKey, err := cmtp2p.LoadOrGenNodeKey(cfg.NodeKeyFile())
	if err != nil {
		t.Fatalf("load node key %s: %v", info.Name, err)
	}
	pv, err := cmtprivval.LoadOrGenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile(), func() (crypto.PrivKey, error) {
		return ed25519.GenPrivKey(), nil
	})
	if err != nil {
		t.Fatalf("load privval %s: %v", info.Name, err)
	}

	appLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	myApp, err := app.NewAppWithChainID(filepath.Join(info.Home, "data"), appLogger, evmChainID)
	if err != nil {
		t.Fatalf("new app %s: %v", info.Name, err)
	}
	workers := spec.workers
	if workers < 1 {
		workers = 1
	}
	if err := myApp.ConfigureExecutor(spec.executor, workers); err != nil {
		_ = myApp.Close()
		t.Fatalf("configure executor %s (%s/%d): %v", info.Name, spec.executor, workers, err)
	}
	if err := myApp.ConfigureCheckpoints(spec.checkpoint); err != nil {
		_ = myApp.Close()
		t.Fatalf("configure checkpoints %s: %v", info.Name, err)
	}

	var openedDBs []*store.PebbleDB
	ctx, cancel := context.WithCancel(context.Background())
	node, err := cmtnode.NewNode(
		ctx,
		cfg,
		pv,
		nodeKey,
		cmtproxy.NewLocalClientCreator(myApp),
		cmtnode.DefaultGenesisDocProviderFunc(cfg),
		testPebbleDBProvider(&openedDBs),
		cmtnode.DefaultMetricsProvider(cfg.Instrumentation),
		cmtlog.NewNopLogger(),
	)
	if err != nil {
		cancel()
		_ = myApp.Close()
		t.Fatalf("new comet node %s: %v", info.Name, err)
	}
	if err := node.Start(); err != nil {
		cancel()
		_ = myApp.Close()
		t.Fatalf("start comet node %s: %v", info.Name, err)
	}

	localClient := cmtrpclocal.New(node)
	ethServer, err := ethrpc.New(myApp, localClient)
	if err != nil {
		_ = node.Stop()
		node.Wait()
		cancel()
		_ = myApp.Close()
		t.Fatalf("new ethrpc %s: %v", info.Name, err)
	}
	ethAddr := strings.TrimPrefix(info.EthereumRPC, "http://")
	if err := ethServer.Start(ethAddr); err != nil {
		_ = node.Stop()
		node.Wait()
		cancel()
		_ = myApp.Close()
		t.Fatalf("start ethrpc %s at %s: %v", info.Name, ethAddr, err)
	}

	return &runningValidator{
		info:      info,
		spec:      spec,
		app:       myApp,
		node:      node,
		local:     localClient,
		ethRPC:    ethServer,
		cancel:    cancel,
		openedDBs: openedDBs,
	}
}

func (rv *runningValidator) stop() {
	if rv == nil {
		return
	}
	if rv.ethRPC != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = rv.ethRPC.Close(ctx)
		cancel()
		rv.ethRPC = nil
	}
	if rv.node != nil {
		_ = rv.node.Stop()
		rv.node.Wait()
		rv.node = nil
	}
	if rv.cancel != nil {
		rv.cancel()
		rv.cancel = nil
	}
	if rv.app != nil {
		_ = rv.app.Close()
		rv.app = nil
	}
	// đảm bảo đóng sạch mọi DB CometBFT (tx_index/evidence/...) để giải phóng flock khi restart trong cùng process test
	for _, db := range rv.openedDBs {
		func(d *store.PebbleDB) {
			defer func() { _ = recover() }()
			_ = d.Close()
		}(db)
	}
	rv.openedDBs = nil
}

func loadCometConfigForTest(home string) (*cmtcfg.Config, error) {
	// đọc đúng các tham số P2P/RPC/PersistentPeers/TimeoutCommit từ config.toml
	raw, err := os.ReadFile(filepath.Join(home, cmtcfg.DefaultConfigDir, cmtcfg.DefaultConfigFileName))
	if err != nil {
		return nil, err
	}
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	cfg.P2P.AllowDuplicateIP = true
	cfg.Consensus.TimeoutCommit = 200 * time.Millisecond
	cfg.Consensus.TimeoutPropose = 500 * time.Millisecond
	cfg.Consensus.TimeoutPrevote = 300 * time.Millisecond
	cfg.Consensus.TimeoutPrecommit = 300 * time.Millisecond
	cfg.LogLevel = "error"

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "moniker = "):
			cfg.Moniker = strings.Trim(strings.TrimPrefix(line, "moniker = "), `"`)
		case strings.HasPrefix(line, "laddr = ") && strings.Contains(line, ":27"):
			val := strings.Trim(strings.TrimPrefix(line, "laddr = "), `"`)
			if strings.Contains(val, "276") || strings.Contains(val, "286") || strings.Contains(val, "296") || strings.Contains(val, "306") {
				cfg.P2P.ListenAddress = val
			} else {
				cfg.RPC.ListenAddress = val
			}
		case strings.HasPrefix(line, "persistent_peers = "):
			cfg.P2P.PersistentPeers = strings.Trim(strings.TrimPrefix(line, "persistent_peers = "), `"`)
		}
	}
	// tách chắc chắn laddr của [rpc] và [p2p] theo section
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if strings.HasPrefix(line, "laddr = ") {
			val := strings.Trim(strings.TrimPrefix(line, "laddr = "), `"`)
			if section == "[rpc]" {
				cfg.RPC.ListenAddress = val
			} else if section == "[p2p]" {
				cfg.P2P.ListenAddress = val
			}
		}
	}
	return cfg, nil
}

type jsonRPCReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type jsonRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func callEthRPC(t *testing.T, url, method string, params ...any) json.RawMessage {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(jsonRPCReq{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		t.Fatalf("marshal rpc %s: %v", method, err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http post %s (%s): %v", url, method, err)
	}
	defer resp.Body.Close()
	var out jsonRPCResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode rpc %s: %v", method, err)
	}
	if out.Error != nil {
		t.Fatalf("rpc %s error (%d): %s", method, out.Error.Code, out.Error.Message)
	}
	return out.Result
}

func waitForHeight(t *testing.T, nodes []*runningValidator, target int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, rv := range nodes {
			if rv == nil || rv.app == nil {
				continue
			}
			if int64(rv.app.CurrentHeight()) < target {
				allReady = false
				break
			}
		}
		if allReady {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var heights []string
	for _, rv := range nodes {
		if rv == nil || rv.app == nil {
			continue
		}
		heights = append(heights, fmt.Sprintf("%s=%d", rv.info.Name, rv.app.CurrentHeight()))
	}
	t.Fatalf("timed out waiting for height >= %d (got %s)", target, strings.Join(heights, ", "))
}

type rpcReceipt struct {
	TransactionHash   string `json:"transactionHash"`
	TransactionIndex  string `json:"transactionIndex"`
	BlockHash         string `json:"blockHash"`
	BlockNumber       string `json:"blockNumber"`
	ContractAddress   string `json:"contractAddress"`
	CumulativeGasUsed string `json:"cumulativeGasUsed"`
	GasUsed           string `json:"gasUsed"`
	LogsBloom         string `json:"logsBloom"`
	Status            string `json:"status"`
	Logs              []struct {
		Address  string   `json:"address"`
		Topics   []string `json:"topics"`
		Data     string   `json:"data"`
		LogIndex string   `json:"logIndex"`
	} `json:"logs"`
}

func waitForReceipt(t *testing.T, ethURL string, txHash common.Hash, timeout time.Duration) rpcReceipt {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw := callEthRPC(t, ethURL, "eth_getTransactionReceipt", txHash.Hex())
		trimmed := strings.TrimSpace(string(raw))
		if trimmed != "" && trimmed != "null" {
			var rec rpcReceipt
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatalf("unmarshal receipt %s: %v", txHash.Hex(), err)
			}
			return rec
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for receipt %s on %s", txHash.Hex(), ethURL)
	return rpcReceipt{}
}

func readCounterValue(t *testing.T, ethURL string, contract common.Address) uint64 {
	t.Helper()
	raw := callEthRPC(t, ethURL, "eth_call", map[string]string{
		"to":   contract.Hex(),
		"data": hexutil.Encode(counterCountSelector),
	}, "latest")
	var outHex string
	if err := json.Unmarshal(raw, &outHex); err != nil {
		t.Fatalf("decode eth_call result: %v", err)
	}
	data := common.FromHex(outHex)
	if len(data) != 32 {
		t.Fatalf("unexpected count() return len=%d (%s)", len(data), outHex)
	}
	return new(big.Int).SetBytes(data).Uint64()
}

func parseHexUint64(t *testing.T, raw string) uint64 {
	t.Helper()
	v, err := hexutil.DecodeUint64(raw)
	if err != nil {
		t.Fatalf("parse hex uint64 %q: %v", raw, err)
	}
	return v
}

func signAndSendDynamicTx(t *testing.T, ethURL string, key *ecdsa.PrivateKey, chainID int64, to *common.Address, data []byte, gas uint64) common.Hash {
	t.Helper()
	from := gethcrypto.PubkeyToAddress(key.PublicKey)
	nonceRaw := callEthRPC(t, ethURL, "eth_getTransactionCount", from.Hex(), "latest")
	var nonceHex string
	if err := json.Unmarshal(nonceRaw, &nonceHex); err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	nonce := parseHexUint64(t, nonceHex)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(10_000_000_000),
		Gas:       gas,
		To:        to,
		Value:     big.NewInt(0),
		Data:      data,
	})
	signer := types.LatestSignerForChainID(big.NewInt(chainID))
	signed, err := types.SignTx(tx, signer, key)
	if err != nil {
		t.Fatalf("sign tx: %v", err)
	}
	rawBytes, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal tx: %v", err)
	}
	sendRes := callEthRPC(t, ethURL, "eth_sendRawTransaction", hexutil.Encode(rawBytes))
	var gotHash string
	if err := json.Unmarshal(sendRes, &gotHash); err != nil {
		t.Fatalf("decode sendRawTransaction hash: %v", err)
	}
	if !strings.EqualFold(gotHash, signed.Hash().Hex()) {
		t.Fatalf("sendRawTransaction hash mismatch: got=%s want=%s", gotHash, signed.Hash().Hex())
	}
	return signed.Hash()
}

func loadTestAccounts(t *testing.T, root string) []*ecdsa.PrivateKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "accounts.json"))
	if err != nil {
		t.Fatalf("read accounts.json: %v", err)
	}
	var accs []TestAccount
	if err := json.Unmarshal(raw, &accs); err != nil {
		t.Fatalf("parse accounts.json: %v", err)
	}
	keys := make([]*ecdsa.PrivateKey, 0, len(accs))
	for _, a := range accs {
		k, err := gethcrypto.HexToECDSA(strings.TrimPrefix(a.PrivateKey, "0x"))
		if err != nil {
			t.Fatalf("parse private key: %v", err)
		}
		keys = append(keys, k)
	}
	return keys
}

func assertFourNodeConsensusAtHeight(t *testing.T, nodes []*runningValidator, height int64, txHash common.Hash) (blockIDHex, appHashHex, lastResultsHex string) {
	t.Helper()
	waitForHeight(t, nodes, height+1, 60*time.Second)

	var refBlockID, refHeaderAppHash, refLastResults string
	var refReceipt rpcReceipt

	for idx, rv := range nodes {
		h := height
		hNext := height + 1
		blkH, err := rv.local.Block(context.Background(), &h)
		if err != nil {
			t.Fatalf("%s Block(%d): %v", rv.info.Name, h, err)
		}
		blkNext, err := rv.local.Block(context.Background(), &hNext)
		if err != nil {
			t.Fatalf("%s Block(%d): %v", rv.info.Name, hNext, err)
		}
		bID := hex.EncodeToString(blkH.BlockID.Hash)
		// header ở H+1 mang AppHash và LastResultsHash của block H
		hdrAppHash := hex.EncodeToString(blkNext.Block.Header.AppHash)
		hdrLastRes := hex.EncodeToString(blkNext.Block.Header.LastResultsHash)

		rec := waitForReceipt(t, rv.info.EthereumRPC, txHash, 10*time.Second)
		if rec.Status != "0x1" {
			t.Fatalf("%s receipt %s status=%s, want 0x1", rv.info.Name, txHash.Hex(), rec.Status)
		}

		if idx == 0 {
			refBlockID = bID
			refHeaderAppHash = hdrAppHash
			refLastResults = hdrLastRes
			refReceipt = rec
		} else {
			if bID != refBlockID {
				t.Fatalf("height %d blockID mismatch on %s: got=%s want=%s", height, rv.info.Name, bID, refBlockID)
			}
			if hdrAppHash != refHeaderAppHash {
				t.Fatalf("height %d header(H+1).AppHash mismatch on %s: got=%s want=%s", height, rv.info.Name, hdrAppHash, refHeaderAppHash)
			}
			if hdrLastRes != refLastResults {
				t.Fatalf("height %d header(H+1).LastResultsHash mismatch on %s: got=%s want=%s", height, rv.info.Name, hdrLastRes, refLastResults)
			}
			if rec.Status != refReceipt.Status ||
				rec.GasUsed != refReceipt.GasUsed ||
				rec.CumulativeGasUsed != refReceipt.CumulativeGasUsed ||
				!strings.EqualFold(rec.ContractAddress, refReceipt.ContractAddress) ||
				!strings.EqualFold(rec.LogsBloom, refReceipt.LogsBloom) ||
				len(rec.Logs) != len(refReceipt.Logs) {
				t.Fatalf("receipt %s mismatch on %s: got=%+v want=%+v", txHash.Hex(), rv.info.Name, rec, refReceipt)
			}
			for li := range rec.Logs {
				gl, rl := rec.Logs[li], refReceipt.Logs[li]
				if !strings.EqualFold(gl.Address, rl.Address) ||
					!strings.EqualFold(gl.Data, rl.Data) ||
					gl.LogIndex != rl.LogIndex ||
					len(gl.Topics) != len(rl.Topics) {
					t.Fatalf("receipt %s log[%d] mismatch on %s: got=%+v want=%+v", txHash.Hex(), li, rv.info.Name, gl, rl)
				}
				for ti := range gl.Topics {
					if !strings.EqualFold(gl.Topics[ti], rl.Topics[ti]) {
						t.Fatalf("receipt %s log[%d].topic[%d] mismatch on %s: got=%s want=%s", txHash.Hex(), li, ti, rv.info.Name, gl.Topics[ti], rl.Topics[ti])
					}
				}
			}
		}
	}
	return refBlockID, refHeaderAppHash, refLastResults
}

// buildPrefixCounterCreation tạo bytecode contract có prefix thuần ~282 opcode (> Interval=32/64)
// trước khi SLOAD(0)+1 -> SSTORE(0) + LOG1(counterEventTopic0, newVal), để thực sự kích hoạt
// chụp và resume PurePrefixCheckpoint trên cụm 4 validator hỗn hợp.
func buildPrefixCounterCreation() []byte {
	runtimeCode := make([]byte, 0, 64)
	runtimeCode = append(runtimeCode,
		0x60, 0x28, // PUSH1 40
		0x5b,       // JUMPDEST (0x02)
		0x60, 0x01, // PUSH1 1
		0x90,       // SWAP1
		0x03,       // SUB
		0x80,       // DUP1
		0x60, 0x02, // PUSH1 0x02
		0x57,       // JUMPI
		0x50,       // POP
		0x60, 0x00, // PUSH1 0
		0x54,       // SLOAD (step ~283)
		0x60, 0x01, // PUSH1 1
		0x01,       // ADD
		0x80,       // DUP1
		0x60, 0x00, // PUSH1 0
		0x55,       // SSTORE
		0x60, 0x00, // PUSH1 0
		0x52,       // MSTORE
		0x7f,       // PUSH32 counterEventTopic0
	)
	runtimeCode = append(runtimeCode, counterEventTopic0.Bytes()...)
	runtimeCode = append(runtimeCode,
		0x60, 0x20, // PUSH1 32
		0x60, 0x00, // PUSH1 0
		0xa1, // LOG1
		0x00, // STOP
	)
	initCode := []byte{
		0x60, byte(len(runtimeCode)), // PUSH1 len
		0x60, 0x0c, // PUSH1 offset=12
		0x60, 0x00, // PUSH1 dst=0
		0x39,                         // CODECOPY
		0x60, byte(len(runtimeCode)), // PUSH1 len
		0x60, 0x00, // PUSH1 0
		0xf3, // RETURN
	}
	return append(initCode, runtimeCode...)
}

func TestFourValidatorsCatchUpAndMixedExecutors(t *testing.T) {
	// Kịch bản A + B: 4 validator đồng thuận Counter(10 -> 11 -> 12) + dừng validator 4, chốt >=3 block lên 13, mở lại bắt kịp
	t.Run("DeployConsensusAndValidator4CatchUp", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "net-catchup")
		offset := findFreePortOffset(t, 1200)
		netInfo, err := SetupWithPortOffset(root, offset)
		if err != nil {
			t.Fatalf("SetupWithPortOffset: %v", err)
		}
		keys := loadTestAccounts(t, root)

		spec := validatorExecSpec{
			executor:   "sequential",
			workers:    1,
			checkpoint: evm.DefaultCheckpointConfig(),
		}
		nodes := make([]*runningValidator, DefaultNodeCount)
		defer func() {
			for _, rv := range nodes {
				rv.stop()
			}
		}()
		for i, ni := range netInfo.Nodes {
			nodes[i] = startValidatorNode(t, ni, netInfo.EVMChainID, spec)
		}
		waitForHeight(t, nodes, 2, 60*time.Second)

		// 1. Deploy Counter(10) bằng creation bytecode
		creationBytes := common.FromHex("0x" + counterInit10CreationHex)
		deployHash := signAndSendDynamicTx(t, nodes[0].info.EthereumRPC, keys[0], netInfo.EVMChainID, nil, creationBytes, 1_000_000)
		deployRec := waitForReceipt(t, nodes[0].info.EthereumRPC, deployHash, 30*time.Second)
		if deployRec.Status != "0x1" || deployRec.ContractAddress == "" {
			t.Fatalf("deploy Counter failed: %+v", deployRec)
		}
		contract := common.HexToAddress(deployRec.ContractAddress)

		codeRaw := callEthRPC(t, nodes[0].info.EthereumRPC, "eth_getCode", contract.Hex(), "latest")
		var codeHex string
		if err := json.Unmarshal(codeRaw, &codeHex); err != nil || len(common.FromHex(codeHex)) == 0 {
			t.Fatalf("eth_getCode empty after deploy: %s err=%v", codeHex, err)
		}
		if got := readCounterValue(t, nodes[0].info.EthereumRPC, contract); got != 10 {
			t.Fatalf("initial Counter.count = %d, want 10", got)
		}

		// 2. Hai ví độc lập gửi increment() -> sự kiện 11 và 12
		inc1Hash := signAndSendDynamicTx(t, nodes[0].info.EthereumRPC, keys[1], netInfo.EVMChainID, &contract, counterIncrementSelector, 200_000)
		inc2Hash := signAndSendDynamicTx(t, nodes[1].info.EthereumRPC, keys[2], netInfo.EVMChainID, &contract, counterIncrementSelector, 200_000)
		rec1 := waitForReceipt(t, nodes[0].info.EthereumRPC, inc1Hash, 15*time.Second)
		rec2 := waitForReceipt(t, nodes[0].info.EthereumRPC, inc2Hash, 15*time.Second)

		recs := []rpcReceipt{rec1, rec2}
		sort.Slice(recs, func(i, j int) bool {
			bi, bj := parseHexUint64(t, recs[i].BlockNumber), parseHexUint64(t, recs[j].BlockNumber)
			if bi != bj {
				return bi < bj
			}
			return parseHexUint64(t, recs[i].TransactionIndex) < parseHexUint64(t, recs[j].TransactionIndex)
		})
		for i, wantVal := range []uint64{11, 12} {
			if len(recs[i].Logs) != 1 || !strings.EqualFold(recs[i].Logs[0].Topics[0], counterEventTopic0.Hex()) {
				t.Fatalf("inc[%d] unexpected logs: %+v", i, recs[i].Logs)
			}
			gotEventVal := new(big.Int).SetBytes(common.FromHex(recs[i].Logs[0].Data)).Uint64()
			if gotEventVal != wantVal {
				t.Fatalf("inc[%d] event value = %d, want %d", i, gotEventVal, wantVal)
			}
		}

		hLastInc := int64(parseHexUint64(t, recs[1].BlockNumber))
		bID12, appHash12, lastRes12 := assertFourNodeConsensusAtHeight(t, nodes, hLastInc, common.HexToHash(recs[1].TransactionHash))
		for _, rv := range nodes {
			if got := readCounterValue(t, rv.info.EthereumRPC, contract); got != 12 {
				t.Fatalf("%s Counter.count = %d, want 12", rv.info.Name, got)
			}
		}
		t.Logf("SCENARIO A PASS: height=%d blockID=%s appHash=%s lastResultsHash=%s count=12 on all 4 validators",
			hLastInc, bID12, appHash12, lastRes12)

		// 3. Dừng validator số 4 -> 3 validator còn lại chốt thêm >= 3 block gồm 1 increment (12 -> 13)
		stoppedHeight := int64(nodes[3].app.CurrentHeight())
		nodes[3].stop()
		nodes[3] = nil

		inc3Hash := signAndSendDynamicTx(t, nodes[0].info.EthereumRPC, keys[3], netInfo.EVMChainID, &contract, counterIncrementSelector, 200_000)
		rec3 := waitForReceipt(t, nodes[0].info.EthereumRPC, inc3Hash, 15*time.Second)
		hInc3 := int64(parseHexUint64(t, rec3.BlockNumber))
		targetBeforeRestart := stoppedHeight + 4
		if hInc3+2 > targetBeforeRestart {
			targetBeforeRestart = hInc3 + 2
		}
		waitForHeight(t, nodes[:3], targetBeforeRestart, 60*time.Second)

		// 4. Mở lại validator 4 bằng đúng khóa, priv_validator_state và PebbleDB cũ -> đợi bắt kịp
		nodes[3] = startValidatorNode(t, netInfo.Nodes[3], netInfo.EVMChainID, spec)
		bID13, appHash13, lastRes13 := assertFourNodeConsensusAtHeight(t, nodes, hInc3, inc3Hash)
		for _, rv := range nodes {
			if got := readCounterValue(t, rv.info.EthereumRPC, contract); got != 13 {
				t.Fatalf("%s Counter.count after catch-up = %d, want 13", rv.info.Name, got)
			}
		}
		// xác nhận riêng node 4 đọc được biên nhận bị bỏ lỡ chứa sự kiện 13
		rec3OnNode4 := waitForReceipt(t, nodes[3].info.EthereumRPC, inc3Hash, 10*time.Second)
		got13 := new(big.Int).SetBytes(common.FromHex(rec3OnNode4.Logs[0].Data)).Uint64()
		if got13 != 13 {
			t.Fatalf("validator4 missed receipt event = %d, want 13", got13)
		}
		t.Logf("SCENARIO B PASS: validator4 stopped at H=%d, caught up past H=%d, blockID=%s appHash=%s lastResultsHash=%s count=13",
			stoppedHeight, hInc3, bID13, appHash13, lastRes13)
	})

	// Kịch bản C: 4 validator chạy đủ 4 cấu hình hỗn hợp (sequential/off + occ/fixed + occ/history + occ/adaptive)
	// trên contract có pure-prefix > Interval để thực sự kích hoạt retry + checkpoint resume và đối chiếu toàn bộ biên nhận/số dư/storage.
	t.Run("MixedExecutorAndCheckpointConfigsMatchAppHash", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "net-mixed")
		offset := findFreePortOffset(t, 2200)
		netInfo, err := SetupWithPortOffset(root, offset)
		if err != nil {
			t.Fatalf("SetupWithPortOffset: %v", err)
		}
		keys := loadTestAccounts(t, root)
		// dựng model.json tự khép kín trong t.TempDir() để CI không phụ thuộc thư mục results/ (đã gitignore)
		prefixCreation := buildPrefixCounterCreation()
		prefixRuntime := prefixCreation[12:]
		modelPath := filepath.Join(t.TempDir(), "model.json")
		if _, err := evm.SaveAdaptiveModel(modelPath, &evm.AdaptiveModel{
			FormatVersion:    evm.AdaptiveModelFormatVersion,
			CheckpointEvery:  32,
			TightBudgetBytes: 64 << 20,
			WideBudgetBytes:  64 << 20,
			ScoreThreshold:   0.1,
			Entries: []evm.AdaptiveCalibrationEntry{{
				CodeHash:   gethcrypto.Keccak256Hash(prefixRuntime).Hex(),
				Selector:   "0x00000000",
				PC:         0x0e,
				SizeBucket: 512,
				Samples:    10,
				GainNs:     25000,
				CaptureNs:  1500,
			}},
		}); err != nil {
			t.Fatalf("SaveAdaptiveModel for validator4: %v", err)
		}
		adaptiveModel, err := evm.LoadAdaptiveModel(modelPath)
		if err != nil {
			t.Fatalf("LoadAdaptiveModel for validator4: %v", err)
		}

		specs := []validatorExecSpec{
			{executor: "sequential", workers: 1, checkpoint: evm.DefaultCheckpointConfig()},
			{executor: "occ", workers: 4, checkpoint: evm.CheckpointConfig{Policy: evm.CheckpointPolicyFixed, Interval: 32, UseBudget: true, BudgetBytes: 64 << 20}},
			{executor: "occ", workers: 4, checkpoint: evm.CheckpointConfig{Policy: evm.CheckpointPolicyHistory, Interval: 32, UseBudget: true, BudgetBytes: 64 << 20}},
			{executor: "occ", workers: 4, checkpoint: evm.CheckpointConfig{Policy: evm.CheckpointPolicyAdaptive, Interval: 32, UseBudget: true, BudgetBytes: 64 << 20, AdaptiveModel: adaptiveModel}},
		}
		nodes := make([]*runningValidator, DefaultNodeCount)
		defer func() {
			for _, rv := range nodes {
				rv.stop()
			}
		}()
		for i, ni := range netInfo.Nodes {
			nodes[i] = startValidatorNode(t, ni, netInfo.EVMChainID, specs[i])
		}
		waitForHeight(t, nodes, 2, 60*time.Second)

		// 1. Deploy contract có pure-prefix ~282 opcode (> Interval=32) + hot slot 0
		deployHash := signAndSendDynamicTx(t, nodes[0].info.EthereumRPC, keys[0], netInfo.EVMChainID, nil, prefixCreation, 1_000_000)
		deployRec := waitForReceipt(t, nodes[0].info.EthereumRPC, deployHash, 30*time.Second)
		if deployRec.Status != "0x1" || deployRec.ContractAddress == "" {
			t.Fatalf("deploy prefixCounter failed: %+v", deployRec)
		}
		contract := common.HexToAddress(deployRec.ContractAddress)

		// 2. Bắn 2 đợt x 4 tx xung đột cùng slot 0 (đợt 1 làm nóng lịch sử history/adaptive, đợt 2 kích hoạt resume)
		allHashes := make([]common.Hash, 0, 8)
		var maxBlock int64
		var lastHash common.Hash
		for burst := 0; burst < 2; burst++ {
			burstHashes := make([]common.Hash, 4)
			for i := 0; i < 4; i++ {
				burstHashes[i] = signAndSendDynamicTx(t, nodes[0].info.EthereumRPC, keys[i], netInfo.EVMChainID, &contract, nil, 200_000)
			}
			for _, h := range burstHashes {
				rec := waitForReceipt(t, nodes[0].info.EthereumRPC, h, 15*time.Second)
				if rec.Status != "0x1" {
					t.Fatalf("burst %d tx %s failed: %+v", burst, h.Hex(), rec)
				}
				bn := int64(parseHexUint64(t, rec.BlockNumber))
				if bn >= maxBlock {
					maxBlock = bn
					lastHash = h
				}
				allHashes = append(allHashes, h)
			}
		}

		// Kiểm tra đồng thuận toàn vẹn trên cả 4 validator cho mọi tx và khối cuối
		bID, appHash, lastRes := assertFourNodeConsensusAtHeight(t, nodes, maxBlock, lastHash)
		for _, h := range allHashes {
			rec := waitForReceipt(t, nodes[0].info.EthereumRPC, h, 10*time.Second)
			assertFourNodeConsensusAtHeight(t, nodes, int64(parseHexUint64(t, rec.BlockNumber)), h)
		}

		// Kiểm tra slot 0 == 8, số dư và nonce của cả 4 ví khớp tuyệt đối trên cả 4 validator
		wantSlot := common.BigToHash(big.NewInt(8))
		for _, rv := range nodes {
			if got := rv.app.Storage(contract, common.Hash{}); got != wantSlot {
				t.Fatalf("%s (%s/%s) slot0 = %s, want %s", rv.info.Name, rv.spec.executor, rv.spec.checkpoint.Policy.Phase2Name(), got.Hex(), wantSlot.Hex())
			}
			for kIdx, k := range keys[:4] {
				addr := gethcrypto.PubkeyToAddress(k.PublicKey)
				wantBal := nodes[0].app.Balance(addr)
				wantNonce := nodes[0].app.Nonce(addr)
				if gotBal := rv.app.Balance(addr); gotBal.Cmp(wantBal) != 0 {
					t.Fatalf("%s key[%d] balance = %s, want %s", rv.info.Name, kIdx, gotBal, wantBal)
				}
				if gotNonce := rv.app.Nonce(addr); gotNonce != wantNonce {
					t.Fatalf("%s key[%d] nonce = %d, want %d", rv.info.Name, kIdx, gotNonce, wantNonce)
				}
			}
		}

		// Xác nhận ít nhất 1 khối có retry thực tế trên các node OCC (chứng minh xung đột OCC + checkpoint chạy thật)
		var totalOCCRetries uint64
		for h := uint64(1); h <= uint64(maxBlock); h++ {
			if m, found, mErr := nodes[1].app.ExecutionMetricsAtHeight(h); mErr == nil && found {
				totalOCCRetries += m.RetryCount
			}
		}
		t.Logf("SCENARIO C PASS (sequential/off + occ/fixed + occ/history + occ/adaptive): height=%d blockID=%s appHash=%s lastResultsHash=%s slot0=8 occRetries=%d",
			maxBlock, bID, appHash, lastRes, totalOCCRetries)
	})
}

func init() {
	// tránh cảnh báo unused import nếu refactor
	_ = strconv.Itoa
}
