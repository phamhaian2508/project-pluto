package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/huyCuong73/pluto/internal/evm"
)

const maxRPCRequestBytes = 1 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcCallArgs struct {
	From     common.Address  `json:"from"`
	To       *common.Address `json:"to"`
	Gas      string          `json:"gas"`
	GasPrice string          `json:"gasPrice"`
	Value    string          `json:"value"`
	Data     string          `json:"data"`
	Input    string          `json:"input"`
}

// NewEthereumRPCHandler exposes the committed chain state through a small
// Ethereum JSON-RPC surface suitable for Remix and browser wallets.
func NewEthereumRPCHandler(app *App, cometRPC string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setRPCCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32600, Message: "JSON-RPC requires POST"}})
			return
		}
		defer r.Body.Close()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCRequestBytes))
		if err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: "request body too large or unreadable"}})
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil || req.Method == "" || req.JSONRPC != "2.0" {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32600, Message: "invalid JSON-RPC request"}})
			return
		}
		result, callErr := app.handleEthereumRPC(r.Context(), cometRPC, req.Method, req.Params)
		response := rpcResponse{JSONRPC: "2.0", ID: requestID(req.ID), Result: result}
		if callErr != nil {
			response.Result = nil
			response.Error = callErr
		} else if result == nil {
			response.Result = json.RawMessage("null")
		}
		writeRPC(w, response)
	})
}

func setRPCCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "https://remix.ethereum.org" || strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.Header().Set("Content-Type", "application/json")
}

func writeRPC(w http.ResponseWriter, response rpcResponse) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func requestID(raw json.RawMessage) any {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return nil
	}
	return id
}

func rpcParams(raw json.RawMessage) ([]json.RawMessage, *rpcError) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &rpcError{Code: -32602, Message: "params must be an array"}
	}
	return params, nil
}

func param(params []json.RawMessage, index int, target any) *rpcError {
	if index >= len(params) {
		return &rpcError{Code: -32602, Message: fmt.Sprintf("missing parameter %d", index)}
	}
	if err := json.Unmarshal(params[index], target); err != nil {
		return &rpcError{Code: -32602, Message: fmt.Sprintf("invalid parameter %d: %v", index, err)}
	}
	return nil
}

func (app *App) handleEthereumRPC(ctx context.Context, cometRPC, method string, rawParams json.RawMessage) (any, *rpcError) {
	params, paramErr := rpcParams(rawParams)
	if paramErr != nil {
		return nil, paramErr
	}
	switch method {
	case "web3_clientVersion":
		return "Pluto/v0.2.0", nil
	case "net_version":
		return strconv.FormatInt(chainID.Int64(), 10), nil
	case "eth_chainId":
		return hexutil.EncodeBig(chainID), nil
	case "eth_blockNumber":
		return hexutil.EncodeUint64(uint64(app.currentHeight)), nil
	case "eth_gasPrice", "eth_maxPriorityFeePerGas":
		return "0x0", nil
	case "eth_feeHistory":
		return app.feeHistory(params)
	case "eth_syncing":
		return false, nil
	case "eth_getBalance", "eth_getTransactionCount", "eth_getCode":
		var address common.Address
		if err := param(params, 0, &address); err != nil {
			return nil, err
		}
		if len(params) > 1 {
			var tag string
			if err := param(params, 1, &tag); err != nil {
				return nil, err
			}
			if !supportedBlockTag(tag) {
				return nil, &rpcError{Code: -32602, Message: "historical state is not available; use latest or pending"}
			}
		}
		state := evm.NewPebbleStateDB(app.db)
		switch method {
		case "eth_getBalance":
			return hexutil.EncodeBig(state.GetBalance(address).ToBig()), nil
		case "eth_getTransactionCount":
			return hexutil.EncodeUint64(state.GetNonce(address)), nil
		default:
			return hexutil.Encode(state.GetCode(address)), nil
		}
	case "eth_getStorageAt":
		var address common.Address
		var slot string
		if err := param(params, 0, &address); err != nil {
			return nil, err
		}
		if err := param(params, 1, &slot); err != nil {
			return nil, err
		}
		if len(params) > 2 {
			var tag string
			if err := param(params, 2, &tag); err != nil {
				return nil, err
			}
			if !supportedBlockTag(tag) {
				return nil, &rpcError{Code: -32602, Message: "historical state is not available; use latest or pending"}
			}
		}
		return evm.NewPebbleStateDB(app.db).GetState(address, common.HexToHash(slot)).Hex(), nil
	case "eth_call":
		args, err := parseCallArgs(params)
		if err != nil {
			return nil, err
		}
		callResult, callErr := app.Call(args)
		if callErr != nil {
			return nil, &rpcError{Code: -32000, Message: callErr.Error()}
		}
		if callResult.Err != nil {
			return nil, &rpcError{Code: 3, Message: "execution reverted", Data: hexutil.Encode(callResult.RevertData)}
		}
		return hexutil.Encode(callResult.ReturnData), nil
	case "eth_estimateGas":
		args, err := parseCallArgs(params)
		if err != nil {
			return nil, err
		}
		gas, estimateErr := app.EstimateGas(args)
		if estimateErr != nil {
			return nil, &rpcError{Code: -32000, Message: estimateErr.Error()}
		}
		return hexutil.EncodeUint64(gas), nil
	case "eth_sendRawTransaction":
		var rawTx hexutil.Bytes
		if err := param(params, 0, &rawTx); err != nil {
			return nil, err
		}
		if len(rawTx) == 0 || len(rawTx) > maxTransactionSize {
			return nil, &rpcError{Code: -32602, Message: "raw transaction is empty or exceeds size limit"}
		}
		return broadcastRawTransaction(ctx, cometRPC, rawTx)
	case "eth_getTransactionReceipt":
		var hash common.Hash
		if err := param(params, 0, &hash); err != nil {
			return nil, err
		}
		receipt, found, err := app.EthereumReceiptByHash(hash)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return formatReceipt(receipt), nil
	case "eth_getTransactionByHash":
		var hash common.Hash
		if err := param(params, 0, &hash); err != nil {
			return nil, err
		}
		transaction, found, err := app.EthereumTransactionByHash(hash)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return app.formatTransaction(transaction)
	case "eth_getBlockByNumber":
		var tag string
		var full bool
		if err := param(params, 0, &tag); err != nil {
			return nil, err
		}
		if err := param(params, 1, &full); err != nil {
			return nil, err
		}
		number, ok := app.parseBlockTag(tag)
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unsupported block tag"}
		}
		block, found, err := app.EthereumBlockByNumber(number)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return app.formatBlock(block, full)
	case "eth_getBlockByHash":
		var hash common.Hash
		var full bool
		if err := param(params, 0, &hash); err != nil {
			return nil, err
		}
		if err := param(params, 1, &full); err != nil {
			return nil, err
		}
		block, found, err := app.EthereumBlockByHash(hash)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return app.formatBlock(block, full)
	case "eth_getBlockTransactionCountByNumber":
		var tag string
		if err := param(params, 0, &tag); err != nil {
			return nil, err
		}
		number, ok := app.parseBlockTag(tag)
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unsupported block tag"}
		}
		block, found, err := app.EthereumBlockByNumber(number)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return hexutil.EncodeUint64(uint64(len(block.Transactions))), nil
	case "eth_getBlockTransactionCountByHash":
		var hash common.Hash
		if err := param(params, 0, &hash); err != nil {
			return nil, err
		}
		block, found, err := app.EthereumBlockByHash(hash)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, nil
		}
		return hexutil.EncodeUint64(uint64(len(block.Transactions))), nil
	case "eth_getTransactionByBlockNumberAndIndex":
		var tag string
		var index string
		if err := param(params, 0, &tag); err != nil {
			return nil, err
		}
		if err := param(params, 1, &index); err != nil {
			return nil, err
		}
		number, ok := app.parseBlockTag(tag)
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unsupported block tag"}
		}
		idx, err := hexutil.DecodeUint64(index)
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid transaction index"}
		}
		block, found, err := app.EthereumBlockByNumber(number)
		if err != nil || !found || idx >= uint64(len(block.Transactions)) {
			return nil, nil
		}
		tx, found, err := app.EthereumTransactionByHash(block.Transactions[idx])
		if err != nil || !found {
			return nil, nil
		}
		return app.formatTransaction(tx)
	case "eth_getTransactionByBlockHashAndIndex":
		var hash common.Hash
		var index string
		if err := param(params, 0, &hash); err != nil {
			return nil, err
		}
		if err := param(params, 1, &index); err != nil {
			return nil, err
		}
		idx, err := hexutil.DecodeUint64(index)
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid transaction index"}
		}
		block, found, err := app.EthereumBlockByHash(hash)
		if err != nil || !found || idx >= uint64(len(block.Transactions)) {
			return nil, nil
		}
		tx, found, err := app.EthereumTransactionByHash(block.Transactions[idx])
		if err != nil || !found {
			return nil, nil
		}
		return app.formatTransaction(tx)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func parseCallArgs(params []json.RawMessage) (CallArgs, *rpcError) {
	if len(params) == 0 {
		return CallArgs{}, &rpcError{Code: -32602, Message: "missing call arguments"}
	}
	var raw rpcCallArgs
	if err := param(params, 0, &raw); err != nil {
		return CallArgs{}, err
	}
	args := CallArgs{From: raw.From, To: raw.To}
	if raw.Gas != "" {
		gas, err := hexutil.DecodeUint64(raw.Gas)
		if err != nil {
			return CallArgs{}, &rpcError{Code: -32602, Message: "invalid gas quantity"}
		}
		args.Gas = gas
	}
	if raw.GasPrice != "" {
		value, err := hexutil.DecodeBig(raw.GasPrice)
		if err != nil {
			return CallArgs{}, &rpcError{Code: -32602, Message: "invalid gasPrice quantity"}
		}
		args.GasPrice = value
	}
	if raw.Value != "" {
		value, err := hexutil.DecodeBig(raw.Value)
		if err != nil {
			return CallArgs{}, &rpcError{Code: -32602, Message: "invalid value quantity"}
		}
		args.Value = value
	}
	data := raw.Data
	if data == "" {
		data = raw.Input
	}
	if data != "" {
		decoded, err := hexutil.Decode(data)
		if err != nil {
			return CallArgs{}, &rpcError{Code: -32602, Message: "invalid call data"}
		}
		args.Data = decoded
	}
	return args, nil
}

func supportedBlockTag(tag string) bool {
	return tag == "latest" || tag == "pending" || tag == "safe" || tag == "finalized" || tag == "earliest"
}

func (app *App) parseBlockTag(tag string) (uint64, bool) {
	switch tag {
	case "latest", "pending", "safe", "finalized":
		return uint64(app.currentHeight), true
	case "earliest":
		return 0, true
	default:
		number, err := hexutil.DecodeUint64(tag)
		return number, err == nil
	}
}

func (app *App) feeHistory(params []json.RawMessage) (any, *rpcError) {
	var countHex, newestTag string
	if err := param(params, 0, &countHex); err != nil {
		return nil, err
	}
	if err := param(params, 1, &newestTag); err != nil {
		return nil, err
	}
	count, err := hexutil.DecodeUint64(countHex)
	if err != nil || count == 0 || count > 1024 {
		return nil, &rpcError{Code: -32602, Message: "block count must be between 1 and 1024"}
	}
	newest, ok := app.parseBlockTag(newestTag)
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "unsupported newest block tag"}
	}
	if newest > uint64(app.currentHeight) {
		newest = uint64(app.currentHeight)
	}
	oldest := uint64(0)
	if count <= newest+1 {
		oldest = newest - count + 1
	}
	actualCount := newest - oldest + 1
	baseFees := make([]string, actualCount+1)
	gasUsedRatios := make([]float64, actualCount)
	rewards := make([][]string, actualCount)
	var percentiles []float64
	if len(params) > 2 && string(params[2]) != "null" {
		if err := param(params, 2, &percentiles); err != nil {
			return nil, err
		}
		for i, percentile := range percentiles {
			if percentile < 0 || percentile > 100 || (i > 0 && percentile < percentiles[i-1]) {
				return nil, &rpcError{Code: -32602, Message: "reward percentiles must be sorted and between 0 and 100"}
			}
		}
	}
	for i := uint64(0); i < actualCount; i++ {
		baseFees[i] = "0x0"
		rewards[i] = make([]string, len(percentiles))
		for j := range rewards[i] {
			rewards[i][j] = "0x0"
		}
		block, found, err := app.EthereumBlockByNumber(oldest + i)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if found && block.GasLimit > 0 {
			gasUsedRatios[i] = float64(block.GasUsed) / float64(block.GasLimit)
		}
	}
	baseFees[actualCount] = "0x0"
	return map[string]any{
		"oldestBlock": hexutil.EncodeUint64(oldest), "baseFeePerGas": baseFees,
		"gasUsedRatio": gasUsedRatios, "reward": rewards,
	}, nil
}

func broadcastRawTransaction(ctx context.Context, cometRPC string, raw []byte) (any, *rpcError) {
	endpoint, err := url.Parse(cometRPC)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, &rpcError{Code: -32603, Message: "invalid CometBFT RPC endpoint"}
	}
	query := endpoint.Query()
	query.Set("tx", "0x"+common.Bytes2Hex(raw))
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/broadcast_tx_sync"
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: err.Error()}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("broadcast failed: %v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCRequestBytes))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &rpcError{Code: -32000, Message: "CometBFT broadcast request failed"}
	}
	var result struct {
		Result struct {
			Code uint32 `json:"code"`
			Log  string `json:"log"`
			Hash string `json:"hash"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, &rpcError{Code: -32000, Message: "invalid CometBFT response"}
	}
	if result.Error != nil {
		return nil, &rpcError{Code: -32000, Message: result.Error.Message}
	}
	if result.Result.Code != 0 {
		return nil, &rpcError{Code: -32000, Message: result.Result.Log}
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid signed transaction"}
	}
	return tx.Hash(), nil
}

func (app *App) formatTransaction(stored *EthereumTransaction) (map[string]any, *rpcError) {
	var tx types.Transaction
	err := tx.UnmarshalBinary(stored.Raw)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "stored transaction is corrupt"}
	}
	from, err := types.Sender(types.LatestSignerForChainID(chainID), &tx)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "stored transaction has invalid signature"}
	}
	var to any
	if tx.To() != nil {
		to = *tx.To()
	}
	v, r, s := tx.RawSignatureValues()
	return map[string]any{
		"hash": tx.Hash(), "nonce": hexutil.EncodeUint64(tx.Nonce()), "blockHash": stored.BlockHash,
		"blockNumber": hexutil.EncodeUint64(stored.BlockNumber), "transactionIndex": hexutil.EncodeUint64(stored.TransactionIndex),
		"from": from, "to": to, "value": hexutil.EncodeBig(tx.Value()), "gas": hexutil.EncodeUint64(tx.Gas()),
		"gasPrice": hexutil.EncodeBig(tx.GasPrice()), "input": hexutil.Encode(tx.Data()), "type": hexutil.EncodeUint64(uint64(tx.Type())),
		"v": hexutil.EncodeBig(v), "r": hexutil.EncodeBig(r), "s": hexutil.EncodeBig(s),
	}, nil
}

func formatReceipt(receipt *EthereumReceipt) map[string]any {
	var to any
	if receipt.To != nil {
		to = *receipt.To
	}
	var contractAddress any
	if receipt.ContractAddress != (common.Address{}) {
		contractAddress = receipt.ContractAddress
	}
	logs := make([]map[string]any, 0, len(receipt.Logs))
	for _, entry := range receipt.Logs {
		topics := make([]common.Hash, len(entry.Topics))
		copy(topics, entry.Topics)
		logs = append(logs, map[string]any{
			"address": entry.Address, "topics": topics, "data": hexutil.Encode(entry.Data),
			"blockNumber": hexutil.EncodeUint64(entry.BlockNumber), "blockHash": entry.BlockHash,
			"transactionHash": entry.TxHash, "transactionIndex": hexutil.EncodeUint64(uint64(entry.TxIndex)),
			"logIndex": hexutil.EncodeUint64(uint64(entry.Index)), "removed": entry.Removed,
		})
	}
	return map[string]any{
		"transactionHash": receipt.TransactionHash, "transactionIndex": hexutil.EncodeUint64(receipt.TransactionIndex),
		"blockHash": receipt.BlockHash, "blockNumber": hexutil.EncodeUint64(receipt.BlockNumber),
		"from": receipt.From, "to": to, "contractAddress": contractAddress,
		"cumulativeGasUsed": hexutil.EncodeUint64(receipt.CumulativeGasUsed), "gasUsed": hexutil.EncodeUint64(receipt.GasUsed),
		"logsBloom": receipt.LogsBloom, "logs": logs, "status": hexutil.EncodeUint64(receipt.Status),
		"type": "0x0", "effectiveGasPrice": "0x0",
	}
}

func (app *App) formatBlock(block *EthereumBlock, full bool) (map[string]any, *rpcError) {
	transactions := make([]any, 0, len(block.Transactions))
	for _, hash := range block.Transactions {
		if !full {
			transactions = append(transactions, hash)
			continue
		}
		stored, found, err := app.EthereumTransactionByHash(hash)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !found {
			return nil, &rpcError{Code: -32000, Message: "block references a missing transaction"}
		}
		formatted, rpcErr := app.formatTransaction(stored)
		if rpcErr != nil {
			return nil, rpcErr
		}
		transactions = append(transactions, formatted)
	}
	return map[string]any{
		"number": hexutil.EncodeUint64(block.Number), "hash": block.Hash, "parentHash": block.ParentHash,
		"nonce": "0x0000000000000000", "sha3Uncles": types.EmptyUncleHash,
		"logsBloom": block.LogsBloom, "transactionsRoot": types.EmptyRootHash,
		"stateRoot": block.StateRoot, "receiptsRoot": types.EmptyRootHash, "miner": block.Coinbase,
		"difficulty": "0x0", "totalDifficulty": "0x0", "extraData": "0x",
		"size": "0x0", "gasLimit": hexutil.EncodeUint64(block.GasLimit), "gasUsed": hexutil.EncodeUint64(block.GasUsed),
		"timestamp": hexutil.EncodeUint64(block.Timestamp), "transactions": transactions, "uncles": []common.Hash{},
		"baseFeePerGas": "0x0", "mixHash": common.Hash{},
	}, nil
}

func (app *App) ethereumHeaderByNumber(number uint64) (map[string]any, bool, *rpcError) {
	block, found, err := app.EthereumBlockByNumber(number)
	if err != nil {
		return nil, false, &rpcError{Code: -32000, Message: err.Error()}
	}
	if !found {
		return nil, false, nil
	}
	return map[string]any{"hash": block.Hash, "number": hexutil.EncodeUint64(number), "parentHash": block.ParentHash}, true, nil
}
