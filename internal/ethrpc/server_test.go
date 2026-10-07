package ethrpc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/app"
	plutoconfig "github.com/huyCuong73/pluto/internal/config"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestHTTPJSONRPCReadMethods(t *testing.T) {
	application, key, from := rpcTestApp(t)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 21000, To: &to, Value: big.NewInt(5)})
	tx, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(plutoconfig.EVMChainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(1234, 0), Hash: common.HexToHash("0xabc").Bytes(), Txs: [][]byte{raw},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	broadcaster := &recordingBroadcaster{}
	server, err := New(application, broadcaster)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	tests := []struct {
		name   string
		method string
		params any
		check  func(t *testing.T, result json.RawMessage)
	}{
		{"chain ID", "eth_chainId", []any{}, expectJSON("0x7a69")},
		{"network ID", "net_version", []any{}, expectJSON("31337")},
		{"block number", "eth_blockNumber", []any{}, expectJSON("0x1")},
		{"gas price", "eth_gasPrice", []any{}, expectJSON("0x0")},
		{"priority fee", "eth_maxPriorityFeePerGas", []any{}, expectJSON("0x0")},
		{"balance", "eth_getBalance", []any{to, "latest"}, expectJSON("0x5")},
		{"nonce", "eth_getTransactionCount", []any{from, "latest"}, expectJSON("0x1")},
		{"code", "eth_getCode", []any{to, "latest"}, expectJSON("0x")},
		{"storage", "eth_getStorageAt", []any{to, common.Hash{}, "latest"}, expectJSON(common.Hash{}.Hex())},
		{"call", "eth_call", []any{map[string]any{"from": from, "to": to}, "latest"}, expectJSON("0x")},
		{"estimate gas", "eth_estimateGas", []any{map[string]any{"from": from, "to": to}}, expectJSON("0x5208")},
		{"block", "eth_getBlockByNumber", []any{"0x1", false}, func(t *testing.T, result json.RawMessage) {
			var block struct {
				Hash         common.Hash   `json:"hash"`
				Transactions []common.Hash `json:"transactions"`
			}
			if err := json.Unmarshal(result, &block); err != nil {
				t.Fatal(err)
			}
			if len(block.Transactions) != 1 || block.Transactions[0] != tx.Hash() {
				t.Fatalf("unexpected block: %+v", block)
			}
		}},
		{"transaction", "eth_getTransactionByHash", []any{tx.Hash()}, func(t *testing.T, result json.RawMessage) {
			var transaction RPCTransaction
			if err := json.Unmarshal(result, &transaction); err != nil {
				t.Fatal(err)
			}
			if transaction.Hash != tx.Hash() || transaction.From != from {
				t.Fatalf("unexpected transaction: %+v", transaction)
			}
		}},
		{"receipt", "eth_getTransactionReceipt", []any{tx.Hash()}, func(t *testing.T, result json.RawMessage) {
			var receipt struct {
				Status  string `json:"status"`
				GasUsed string `json:"gasUsed"`
			}
			if err := json.Unmarshal(result, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Status != "0x1" || receipt.GasUsed != "0x5208" {
				t.Fatalf("unexpected receipt: %+v", receipt)
			}
		}},
		{"syncing", "eth_syncing", []any{}, func(t *testing.T, result json.RawMessage) {
			if string(result) != "false" {
				t.Fatalf("syncing = %s, want false", result)
			}
		}},
		{"fee history", "eth_feeHistory", []any{"0x1", "latest", []float64{50}}, func(t *testing.T, result json.RawMessage) {
			var history struct {
				OldestBlock  string     `json:"oldestBlock"`
				BaseFee      []string   `json:"baseFeePerGas"`
				GasUsedRatio []float64  `json:"gasUsedRatio"`
				Reward       [][]string `json:"reward"`
			}
			if err := json.Unmarshal(result, &history); err != nil {
				t.Fatal(err)
			}
			if history.OldestBlock != "0x1" || len(history.BaseFee) != 2 || len(history.GasUsedRatio) != 1 || len(history.Reward) != 1 {
				t.Fatalf("unexpected fee history: %+v", history)
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := rpcRequest(t, httpServer.URL, test.method, test.params)
			if response.Error != nil {
				t.Fatalf("RPC error %d: %s", response.Error.Code, response.Error.Message)
			}
			test.check(t, response.Result)
		})
	}

	response := rpcRequest(t, httpServer.URL, "eth_sendRawTransaction", []any{"0x" + common.Bytes2Hex(raw)})
	if response.Error != nil {
		t.Fatalf("send raw transaction RPC error: %+v", response.Error)
	}
	var returnedHash common.Hash
	if err := json.Unmarshal(response.Result, &returnedHash); err != nil {
		t.Fatal(err)
	}
	if returnedHash != tx.Hash() {
		t.Fatalf("transaction hash = %s, want %s", returnedHash, tx.Hash())
	}
	if !bytes.Equal(broadcaster.raw, raw) {
		t.Fatalf("mempool received %x, want %x", broadcaster.raw, raw)
	}
}

func TestSendRawTransactionReturnsCheckTxRejection(t *testing.T) {
	api := &EthAPI{broadcaster: &recordingBroadcaster{result: &coretypes.ResultBroadcastTx{Code: 1, Log: "rejected by CheckTx"}}}
	if _, err := api.SendRawTransaction(context.Background(), []byte{1, 2, 3}); err == nil || err.Error() != "rejected by CheckTx" {
		t.Fatalf("unexpected rejection error: %v", err)
	}
}

func TestFreshChainReturnsLatestGenesisBlock(t *testing.T) {
	application, _, _ := rpcTestApp(t)
	server, err := New(application)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	response := rpcRequest(t, httpServer.URL, "eth_getBlockByNumber", []any{"latest", false})
	if response.Error != nil {
		t.Fatalf("RPC error %d: %s", response.Error.Code, response.Error.Message)
	}
	var block struct {
		Number  string      `json:"number"`
		Hash    common.Hash `json:"hash"`
		MixHash common.Hash `json:"mixHash"`
	}
	if err := json.Unmarshal(response.Result, &block); err != nil {
		t.Fatal(err)
	}
	if block.Number != "0x0" || block.Hash == (common.Hash{}) {
		t.Fatalf("unexpected genesis block: %+v", block)
	}
}

func TestHTTPHandlerAllowsMetaMaskCORS(t *testing.T) {
	application, _, _ := rpcTestApp(t)
	server, err := New(application)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	request, err := http.NewRequest(http.MethodOptions, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "content-type")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("unexpected CORS preflight response: status=%d headers=%v", response.StatusCode, response.Header)
	}
	if response.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatal("private-network access was not allowed")
	}

	payload := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	request, err = http.NewRequest(http.MethodPost, httpServer.URL, payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "chrome-extension://nkbihfbeogaeaoehlefnkodbefgpgknn")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("JSON-RPC response missing CORS header: %v", response.Header)
	}
}

func TestJSONRPCStateAndReceiptSurviveRestart(t *testing.T) {
	dbPath := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := app.NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	genesis, _ := json.Marshal(app.GenesisState{Alloc: map[string]app.GenesisAccount{
		from.Hex(): {Balance: "1000000000000000000"},
	}})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 21000, To: &to, Value: big.NewInt(7)})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(plutoconfig.EVMChainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.MarshalBinary()
	if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(1, 0), Hash: common.HexToHash("0x01").Bytes(), Txs: [][]byte{raw},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	application, err = app.NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	server, err := New(application)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	for method, test := range map[string]struct {
		params any
		check  func(*testing.T, json.RawMessage)
	}{
		"eth_blockNumber": {[]any{}, expectJSON("0x1")},
		"eth_getBalance":  {[]any{to, "latest"}, expectJSON("0x7")},
		"eth_getTransactionReceipt": {[]any{tx.Hash()}, func(t *testing.T, result json.RawMessage) {
			if string(result) == "null" || !bytes.Contains(result, []byte(`"status":"0x1"`)) {
				t.Fatalf("receipt after restart = %s", result)
			}
		}},
	} {
		response := rpcRequest(t, httpServer.URL, method, test.params)
		if response.Error != nil {
			t.Fatalf("%s RPC error: %+v", method, response.Error)
		}
		test.check(t, response.Result)
	}
}

type recordingBroadcaster struct {
	raw    []byte
	result *coretypes.ResultBroadcastTx
}

func (b *recordingBroadcaster) BroadcastTxSync(_ context.Context, tx cmttypes.Tx) (*coretypes.ResultBroadcastTx, error) {
	b.raw = append([]byte(nil), tx...)
	if b.result != nil {
		return b.result, nil
	}
	return &coretypes.ResultBroadcastTx{}, nil
}

func rpcTestApp(t *testing.T) (*app.App, *ecdsa.PrivateKey, common.Address) {
	t.Helper()
	application, err := app.NewApp(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	genesis, _ := json.Marshal(app.GenesisState{Alloc: map[string]app.GenesisAccount{
		from.Hex(): {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18)).String()},
	}})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	return application, key, from
}

func rpcRequest(t *testing.T, endpoint, method string, params any) rpcResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded rpcResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func expectJSON(want string) func(*testing.T, json.RawMessage) {
	return func(t *testing.T, result json.RawMessage) {
		t.Helper()
		var got string
		if err := json.Unmarshal(result, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("result = %q, want %q", got, want)
		}
	}
}
