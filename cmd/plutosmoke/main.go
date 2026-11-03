package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/plutonetwork"
)

const (
	genesisAccount = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	transferWei    = 1
)

type networkFile struct {
	Nodes []plutonetwork.NodeInfo `json:"nodes"`
}

type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	root := flag.String("root", ".pluto-network", "Pluto validator network root created by setup-pluto-network.ps1")
	keyHex := flag.String("key", os.Getenv("PLUTO_TEST_KEY"), "local test private key (or PLUTO_TEST_KEY)")
	flag.Parse()
	if err := run(*root, *keyHex); err != nil {
		fmt.Fprintln(os.Stderr, "transfer smoke test failed:", err)
		os.Exit(1)
	}
}

func run(root, keyHex string) error {
	if keyHex == "" {
		return errors.New("provide a local test key with -key or PLUTO_TEST_KEY")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(keyHex, "0x"))
	if err != nil {
		return fmt.Errorf("parse test key: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	if from != common.HexToAddress(genesisAccount) {
		return fmt.Errorf("test key address %s is not the funded genesis test account %s", from, genesisAccount)
	}
	encodedNetwork, err := os.ReadFile(filepath.Join(root, "network.json"))
	if err != nil {
		return fmt.Errorf("read Pluto validator network metadata: %w", err)
	}
	var network networkFile
	if err := json.Unmarshal(encodedNetwork, &network); err != nil {
		return fmt.Errorf("decode Pluto validator network metadata: %w", err)
	}
	if len(network.Nodes) != plutonetwork.DefaultNodeCount {
		return fmt.Errorf("network has %d nodes, expected %d", len(network.Nodes), plutonetwork.DefaultNodeCount)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	var nonce hexutil.Uint64
	if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_getTransactionCount", []any{from, "latest"}, &nonce); err != nil {
		return fmt.Errorf("read sender nonce: %w", err)
	}
	to, err := randomAddress()
	if err != nil {
		return fmt.Errorf("create fresh recipient: %w", err)
	}
	gasPrice, err := callGasPrice(ctx, client, network.Nodes[0].EthereumRPC)
	if err != nil {
		return fmt.Errorf("read gas price: %w", err)
	}
	transaction := types.NewTx(&types.LegacyTx{
		Nonce: uint64(nonce), GasPrice: gasPrice, Gas: 21000,
		To: &to, Value: big.NewInt(transferWei),
	})
	transaction, err = types.SignTx(transaction, types.LatestSignerForChainID(big.NewInt(config.EVMChainID)), key)
	if err != nil {
		return fmt.Errorf("sign transfer: %w", err)
	}
	raw, err := transaction.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode transfer: %w", err)
	}
	var txHash common.Hash
	if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_sendRawTransaction", []any{hexutil.Bytes(raw)}, &txHash); err != nil {
		return fmt.Errorf("broadcast transfer: %w", err)
	}

	var receipt struct {
		Status      hexutil.Uint64 `json:"status"`
		BlockNumber hexutil.Uint64 `json:"blockNumber"`
	}
	if err := waitForReceipt(ctx, client, network.Nodes[0].EthereumRPC, txHash, &receipt); err != nil {
		return err
	}
	if uint64(receipt.Status) != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transfer %s was included but failed (receipt status %d)", txHash, receipt.Status)
	}
	if err := waitForReplicatedTransfer(ctx, client, network.Nodes, txHash, to); err != nil {
		return err
	}

	var metrics struct {
		Height           uint64 `json:"height"`
		Executor         string `json:"executor"`
		Workers          string `json:"workers"`
		TransactionCount uint64 `json:"transactionCount"`
		RetryCount       uint64 `json:"retryCount"`
		ExecutionNanos   uint64 `json:"executionNanos"`
	}
	metricHeight := hexutil.EncodeUint64(uint64(receipt.BlockNumber))
	if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "pluto_getBlockMetrics", []any{metricHeight}, &metrics); err != nil {
		return fmt.Errorf("read committed block metrics: %w", err)
	}
	if metrics.TransactionCount == 0 {
		return fmt.Errorf("block %d metrics report no transactions", receipt.BlockNumber)
	}
	var balance hexutil.Big
	if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_getBalance", []any{to, "latest"}, &balance); err != nil {
		return fmt.Errorf("read recipient balance from validator1: %w", err)
	}
	if (*big.Int)(&balance).Cmp(big.NewInt(transferWei)) != 0 {
		return fmt.Errorf("recipient balance is %s wei, want %d", (*big.Int)(&balance), transferWei)
	}

	statuses, err := queryCommonCommit(ctx, client, network.Nodes)
	if err != nil {
		return err
	}
	for _, node := range network.Nodes {
		var replicatedBalance hexutil.Big
		if err := callJSONRPC(ctx, client, node.EthereumRPC, "eth_getBalance", []any{to, "latest"}, &replicatedBalance); err != nil {
			return fmt.Errorf("read recipient balance from %s: %w", node.Name, err)
		}
		if (*big.Int)(&replicatedBalance).Cmp(big.NewInt(transferWei)) != 0 {
			return fmt.Errorf("%s has recipient balance %s wei; transaction state did not replicate", node.Name, (*big.Int)(&replicatedBalance))
		}
	}
	sameHeight, sameHash, err := plutonetwork.CompareAppHashes(statuses)
	if err != nil {
		return err
	}
	if !sameHeight || !sameHash {
		return fmt.Errorf("validators have not converged to the same height and AppHash: %+v", statuses)
	}
	shortHash := statuses[0].AppHash
	if len(shortHash) > 16 {
		shortHash = shortHash[:16]
	}
	fmt.Printf("Confirmed transfer on 4 validators\n  tx: %s\n  EVM block: %d\n  sender: %s\n  recipient: %s\n  replicated recipient balance: %s wei\n  executor: %s (%s workers), tx in block: %d, retries: %d\n  common CometBFT height: %d\n  common AppHash: %s...\n",
		txHash, receipt.BlockNumber, from, to, (*big.Int)(&balance), metrics.Executor, metrics.Workers,
		metrics.TransactionCount, metrics.RetryCount, statuses[0].Height, shortHash)
	return nil
}

func callJSONRPC(ctx context.Context, client *http.Client, endpoint, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s: %s", response.Status, string(encoded))
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return fmt.Errorf("decode JSON-RPC response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("JSON-RPC %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return errors.New("JSON-RPC returned null result")
	}
	return json.Unmarshal(envelope.Result, out)
}

func waitForReceipt(ctx context.Context, client *http.Client, endpoint string, hash common.Hash, out any) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result json.RawMessage
		err := callJSONRPC(ctx, client, endpoint, "eth_getTransactionReceipt", []any{hash}, &result)
		if err == nil && len(result) > 0 && string(result) != "null" {
			return json.Unmarshal(result, out)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for receipt %s: %w", hash, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForReplicatedTransfer(ctx context.Context, client *http.Client, nodes []plutonetwork.NodeInfo, hash common.Hash, recipient common.Address) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		allReplicated := true
		for _, node := range nodes {
			var receipt struct {
				Status hexutil.Uint64 `json:"status"`
			}
			if err := callJSONRPC(ctx, client, node.EthereumRPC, "eth_getTransactionReceipt", []any{hash}, &receipt); err != nil || uint64(receipt.Status) != types.ReceiptStatusSuccessful {
				allReplicated = false
				break
			}
			var balance hexutil.Big
			if err := callJSONRPC(ctx, client, node.EthereumRPC, "eth_getBalance", []any{recipient, "latest"}, &balance); err != nil || (*big.Int)(&balance).Cmp(big.NewInt(transferWei)) != 0 {
				allReplicated = false
				break
			}
		}
		if allReplicated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for all validator RPCs to report the committed transaction %s: %w", hash, ctx.Err())
		case <-ticker.C:
		}
	}
}

func queryCommonCommit(ctx context.Context, client *http.Client, nodes []plutonetwork.NodeInfo) ([]plutonetwork.AppInfo, error) {
	infos := make([]plutonetwork.AppInfo, 0, len(nodes))
	minimumHeight := int64(^uint64(0) >> 1)
	for _, node := range nodes {
		info, err := queryAppInfo(ctx, client, node.CometRPC)
		if err != nil {
			return nil, fmt.Errorf("read %s CometBFT height: %w", node.Name, err)
		}
		infos = append(infos, info)
		if info.Height < minimumHeight {
			minimumHeight = info.Height
		}
	}
	if minimumHeight < 1 {
		return nil, fmt.Errorf("validators have no committed block yet")
	}
	for i, node := range nodes {
		info, err := queryCommitAtHeight(ctx, client, node.CometRPC, minimumHeight)
		if err != nil {
			return nil, fmt.Errorf("read %s commit at common height %d: %w", node.Name, minimumHeight, err)
		}
		infos[i] = info
	}
	return infos, nil
}

func queryCommitAtHeight(ctx context.Context, client *http.Client, endpoint string, height int64) (plutonetwork.AppInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/commit?height="+url.QueryEscape(fmt.Sprint(height)), nil)
	if err != nil {
		return plutonetwork.AppInfo{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return plutonetwork.AppInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return plutonetwork.AppInfo{}, fmt.Errorf("CometBFT returned HTTP %s", response.Status)
	}
	var envelope struct {
		Result struct {
			SignedHeader struct {
				Header struct {
					Height  string `json:"height"`
					AppHash string `json:"app_hash"`
				} `json:"header"`
			} `json:"signed_header"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return plutonetwork.AppInfo{}, err
	}
	var gotHeight int64
	if _, err := fmt.Sscan(envelope.Result.SignedHeader.Header.Height, &gotHeight); err != nil {
		return plutonetwork.AppInfo{}, fmt.Errorf("decode commit height: %w", err)
	}
	if gotHeight != height || envelope.Result.SignedHeader.Header.AppHash == "" {
		return plutonetwork.AppInfo{}, fmt.Errorf("commit response has height %d or empty AppHash; expected height %d", gotHeight, height)
	}
	return plutonetwork.AppInfo{Node: endpoint, Height: gotHeight, AppHash: envelope.Result.SignedHeader.Header.AppHash}, nil
}

func queryAppInfo(ctx context.Context, client *http.Client, endpoint string) (plutonetwork.AppInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/abci_info", nil)
	if err != nil {
		return plutonetwork.AppInfo{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return plutonetwork.AppInfo{}, err
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Response struct {
				LastBlockHeight  string `json:"last_block_height"`
				LastBlockAppHash string `json:"last_block_app_hash"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return plutonetwork.AppInfo{}, err
	}
	height, err := strconv.ParseInt(envelope.Result.Response.LastBlockHeight, 10, 64)
	if err != nil {
		return plutonetwork.AppInfo{}, fmt.Errorf("decode CometBFT height: %w", err)
	}
	return plutonetwork.AppInfo{Node: endpoint, Height: height, AppHash: envelope.Result.Response.LastBlockAppHash}, nil
}

func randomAddress() (common.Address, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(key.PublicKey), nil
}

func callGasPrice(ctx context.Context, client *http.Client, endpoint string) (*big.Int, error) {
	var price hexutil.Big
	if err := callJSONRPC(ctx, client, endpoint, "eth_gasPrice", []any{}, &price); err != nil {
		return nil, err
	}
	return new(big.Int).Set((*big.Int)(&price)), nil
}
