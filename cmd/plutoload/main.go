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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/plutonetwork"
)

type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type receipt struct {
	Status      hexutil.Uint64 `json:"status"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
}

type blockMetrics struct {
	Height           uint64 `json:"height"`
	Executor         string `json:"executor"`
	Workers          string `json:"workers"`
	TransactionCount uint64 `json:"transactionCount"`
	RetryCount       uint64 `json:"retryCount"`
	ExecutionNanos   uint64 `json:"executionNanos"`
}

type report struct {
	Network          string         `json:"network"`
	EVMChainID       int64          `json:"evmChainId"`
	Transactions     int            `json:"transactions"`
	SenderCount      int            `json:"senderCount"`
	Executor         string         `json:"executor"`
	Workers          string         `json:"workers"`
	FirstEVMBlock    uint64         `json:"firstEvmBlock"`
	LastEVMBlock     uint64         `json:"lastEvmBlock"`
	ElapsedMillis    int64          `json:"elapsedMillis"`
	TransactionsPerS float64        `json:"transactionsPerSecond"`
	Retries          uint64         `json:"retries"`
	ExecutionNanos   uint64         `json:"executionNanos"`
	Blocks           []blockMetrics `json:"blocks"`
}

func main() {
	root := flag.String("root", ".pluto-network", "Pluto validator network root created by setup-pluto-network.ps1")
	count := flag.Int("count", 50, "number of native coin transfer transactions (1..64; one funded sender per transaction)")
	out := flag.String("out", "", "optional JSON report output path")
	flag.Parse()
	if err := run(*root, *count, *out); err != nil {
		fmt.Fprintln(os.Stderr, "validator-network load failed:", err)
		os.Exit(1)
	}
}

func run(root string, count int, out string) error {
	if count < 1 || count > plutonetwork.DefaultAccountCount {
		return fmt.Errorf("count must be between 1 and %d (the number of funded test accounts)", plutonetwork.DefaultAccountCount)
	}
	encodedNetwork, err := os.ReadFile(filepath.Join(root, "network.json"))
	if err != nil {
		return fmt.Errorf("read Pluto validator network metadata: %w", err)
	}
	var network plutonetwork.NetworkInfo
	if err := json.Unmarshal(encodedNetwork, &network); err != nil {
		return fmt.Errorf("decode Pluto validator network metadata: %w", err)
	}
	if len(network.Nodes) != plutonetwork.DefaultNodeCount {
		return fmt.Errorf("network has %d validators, expected %d", len(network.Nodes), plutonetwork.DefaultNodeCount)
	}
	encodedAccounts, err := os.ReadFile(filepath.Join(root, "accounts.json"))
	if err != nil {
		return fmt.Errorf("read funded test accounts: %w", err)
	}
	var accounts []plutonetwork.TestAccount
	if err := json.Unmarshal(encodedAccounts, &accounts); err != nil {
		return fmt.Errorf("decode funded test accounts: %w", err)
	}
	if len(accounts) < count {
		return fmt.Errorf("network has only %d funded test accounts; requested %d", len(accounts), count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	var gasPrice hexutil.Big
	if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_gasPrice", []any{}, &gasPrice); err != nil {
		return fmt.Errorf("read gas price: %w", err)
	}
	start := time.Now()
	hashes := make([]common.Hash, 0, count)
	for i := 0; i < count; i++ {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(accounts[i].PrivateKey, "0x"))
		if err != nil {
			return fmt.Errorf("parse local test account %d key: %w", i+1, err)
		}
		from := crypto.PubkeyToAddress(key.PublicKey)
		if from != accounts[i].Address {
			return fmt.Errorf("local test account %d address does not match its private key", i+1)
		}
		var nonce hexutil.Uint64
		if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_getTransactionCount", []any{from, "latest"}, &nonce); err != nil {
			return fmt.Errorf("read nonce for local test account %d: %w", i+1, err)
		}
		recipientKey, err := crypto.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate fresh recipient %d: %w", i, err)
		}
		to := crypto.PubkeyToAddress(recipientKey.PublicKey)
		tx := types.NewTx(&types.LegacyTx{
			Nonce: uint64(nonce), GasPrice: new(big.Int).Set((*big.Int)(&gasPrice)),
			Gas: 21000, To: &to, Value: big.NewInt(1),
		})
		tx, err = types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(network.EVMChainID)), key)
		if err != nil {
			return fmt.Errorf("sign transaction %d: %w", i+1, err)
		}
		raw, err := tx.MarshalBinary()
		if err != nil {
			return fmt.Errorf("encode transaction %d: %w", i+1, err)
		}
		var returnedHash common.Hash
		if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "eth_sendRawTransaction", []any{hexutil.Bytes(raw)}, &returnedHash); err != nil {
			return fmt.Errorf("submit transaction %d/%d: %w", i+1, count, err)
		}
		if returnedHash != tx.Hash() {
			return fmt.Errorf("validator returned unexpected transaction hash for transaction %d", i+1)
		}
		hashes = append(hashes, returnedHash)
	}

	receipts := make([]receipt, len(hashes))
	firstBlock := uint64(^uint64(0))
	lastBlock := uint64(0)
	for i, hash := range hashes {
		if err := waitForReceipt(ctx, client, network.Nodes[0].EthereumRPC, hash, &receipts[i]); err != nil {
			return err
		}
		if uint64(receipts[i].Status) != types.ReceiptStatusSuccessful {
			return fmt.Errorf("transaction %s was included with failed status %d", hash, receipts[i].Status)
		}
		block := uint64(receipts[i].BlockNumber)
		if block < firstBlock {
			firstBlock = block
		}
		if block > lastBlock {
			lastBlock = block
		}
	}
	elapsed := time.Since(start)
	blockSet := make(map[uint64]struct{})
	for _, item := range receipts {
		blockSet[uint64(item.BlockNumber)] = struct{}{}
	}
	blocks := make([]blockMetrics, 0, len(blockSet))
	var retryTotal, executionTotal uint64
	for height := firstBlock; height <= lastBlock; height++ {
		if _, included := blockSet[height]; !included {
			continue
		}
		var metrics blockMetrics
		if err := callJSONRPC(ctx, client, network.Nodes[0].EthereumRPC, "pluto_getBlockMetrics", []any{hexutil.EncodeUint64(height)}, &metrics); err != nil {
			return fmt.Errorf("read metrics for EVM block %d: %w", height, err)
		}
		blocks = append(blocks, metrics)
		retryTotal += metrics.RetryCount
		executionTotal += metrics.ExecutionNanos
	}
	if len(blocks) == 0 {
		return errors.New("no committed block metrics were returned")
	}
	result := report{
		Network: network.ChainID, EVMChainID: network.EVMChainID, Transactions: count, SenderCount: count,
		Executor: blocks[0].Executor, Workers: blocks[0].Workers, FirstEVMBlock: firstBlock,
		LastEVMBlock: lastBlock, ElapsedMillis: elapsed.Milliseconds(),
		TransactionsPerS: float64(count) / elapsed.Seconds(), Retries: retryTotal,
		ExecutionNanos: executionTotal, Blocks: blocks,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode load report: %w", err)
	}
	fmt.Printf("Pluto validator network transfer load report\n  Network: %s (EVM chain ID %d)\n  Transactions: %d from %d funded senders\n  Executor: %s, workers: %s\n  EVM blocks: %d-%d (%d blocks with this load)\n  Time until all receipts committed: %s\n  Observed throughput: %.2f tx/s\n  Executor retries: %d\n  Executor time summed across included blocks: %s\n",
		result.Network, result.EVMChainID, result.Transactions, result.SenderCount, result.Executor, result.Workers,
		firstBlock, lastBlock, len(blocks), elapsed.Round(time.Millisecond), result.TransactionsPerS,
		retryTotal, time.Duration(executionTotal))
	if out != "" {
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
			return fmt.Errorf("write report %s: %w", out, err)
		}
		fmt.Printf("  JSON report: %s\n", out)
	}
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

func waitForReceipt(ctx context.Context, client *http.Client, endpoint string, hash common.Hash, out *receipt) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result json.RawMessage
		err := callJSONRPC(ctx, client, endpoint, "eth_getTransactionReceipt", []any{hash}, &result)
		if err == nil && len(result) > 0 && string(result) != "null" {
			if err := json.Unmarshal(result, out); err != nil {
				return fmt.Errorf("decode receipt %s: %w", hash, err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for receipt %s: %w", hash, ctx.Err())
		case <-ticker.C:
		}
	}
}
