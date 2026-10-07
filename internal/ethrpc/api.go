package ethrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/huyCuong73/pluto/internal/app"
	plutoconfig "github.com/huyCuong73/pluto/internal/config"
)

var rpcChainID = big.NewInt(plutoconfig.EVMChainID)

type EthAPI struct {
	app         *app.App
	broadcaster TxBroadcaster
}

type NetAPI struct{}
type Web3API struct{}

func (*NetAPI) Version() string           { return rpcChainID.String() }
func (*NetAPI) Listening() bool           { return true }
func (*NetAPI) PeerCount() hexutil.Uint   { return 0 }
func (*Web3API) ClientVersion() string    { return "pluto/v1" }
func (api *EthAPI) ChainId() *hexutil.Big { return (*hexutil.Big)(new(big.Int).Set(rpcChainID)) }
func (api *EthAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(api.app.CurrentHeight())
}
func (api *EthAPI) GasPrice() *hexutil.Big { return (*hexutil.Big)(new(big.Int)) }
func (api *EthAPI) MaxPriorityFeePerGas() *hexutil.Big {
	return (*hexutil.Big)(new(big.Int))
}
func (api *EthAPI) Syncing() bool              { return false }
func (api *EthAPI) Mining() bool               { return false }
func (api *EthAPI) Hashrate() hexutil.Uint64   { return 0 }
func (api *EthAPI) Accounts() []common.Address { return []common.Address{} }

// SendRawTransaction submits the exact Ethereum transaction bytes to the
// CometBFT mempool and returns the Ethereum transaction hash after CheckTx.
func (api *EthAPI) SendRawTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	if api.broadcaster == nil {
		return common.Hash{}, errors.New("transaction broadcasting is unavailable")
	}
	result, err := api.broadcaster.BroadcastTxSync(ctx, cmttypes.Tx(raw))
	if err != nil {
		return common.Hash{}, fmt.Errorf("broadcast transaction: %w", err)
	}
	if result.Code != 0 {
		if result.Log == "" {
			result.Log = fmt.Sprintf("CheckTx rejected transaction with code %d", result.Code)
		}
		return common.Hash{}, errors.New(result.Log)
	}
	return crypto.Keccak256Hash(raw), nil
}

type FeeHistoryResult struct {
	OldestBlock  *hexutil.Big     `json:"oldestBlock"`
	Reward       [][]*hexutil.Big `json:"reward,omitempty"`
	BaseFee      []*hexutil.Big   `json:"baseFeePerGas"`
	GasUsedRatio []float64        `json:"gasUsedRatio"`
}

func (api *EthAPI) FeeHistory(blockCount hexutil.Uint64, newestBlock gethrpc.BlockNumber, rewardPercentiles []float64) (*FeeHistoryResult, error) {
	count := uint64(blockCount)
	if count > 1024 {
		return nil, errors.New("block count exceeds maximum of 1024")
	}
	for i, percentile := range rewardPercentiles {
		if percentile < 0 || percentile > 100 || (i > 0 && percentile <= rewardPercentiles[i-1]) {
			return nil, errors.New("reward percentiles must be strictly increasing and between 0 and 100")
		}
	}
	newest, err := api.resolveBlockNumber(newestBlock)
	if err != nil {
		return nil, err
	}
	if count > newest+1 {
		count = newest + 1
	}
	oldest := newest + 1 - count
	response := &FeeHistoryResult{
		OldestBlock:  (*hexutil.Big)(new(big.Int).SetUint64(oldest)),
		BaseFee:      make([]*hexutil.Big, count+1),
		GasUsedRatio: make([]float64, count),
	}
	if len(rewardPercentiles) > 0 {
		response.Reward = make([][]*hexutil.Big, count)
	}
	for i := uint64(0); i <= count; i++ {
		response.BaseFee[i] = (*hexutil.Big)(new(big.Int))
		if i == count {
			continue
		}
		if block, found, err := api.app.EthereumBlockByNumber(oldest + i); err != nil {
			return nil, err
		} else if found && block.GasLimit > 0 {
			response.GasUsedRatio[i] = float64(block.GasUsed) / float64(block.GasLimit)
		}
		if len(rewardPercentiles) > 0 {
			response.Reward[i] = make([]*hexutil.Big, len(rewardPercentiles))
			for j := range response.Reward[i] {
				response.Reward[i][j] = (*hexutil.Big)(new(big.Int))
			}
		}
	}
	return response, nil
}

func (api *EthAPI) GetBalance(address common.Address, block *gethrpc.BlockNumberOrHash) (*hexutil.Big, error) {
	if err := api.requireLatest(block); err != nil {
		return nil, err
	}
	return (*hexutil.Big)(api.app.Balance(address)), nil
}

func (api *EthAPI) GetTransactionCount(address common.Address, block *gethrpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	if err := api.requireLatest(block); err != nil {
		return 0, err
	}
	return hexutil.Uint64(api.app.Nonce(address)), nil
}

func (api *EthAPI) GetCode(address common.Address, block *gethrpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if err := api.requireLatest(block); err != nil {
		return nil, err
	}
	return hexutil.Bytes(api.app.Code(address)), nil
}

func (api *EthAPI) GetStorageAt(address common.Address, slot common.Hash, block *gethrpc.BlockNumberOrHash) (common.Hash, error) {
	if err := api.requireLatest(block); err != nil {
		return common.Hash{}, err
	}
	return api.app.Storage(address, slot), nil
}

func (api *EthAPI) GetBlockByNumber(number gethrpc.BlockNumber, fullTransactions bool) (map[string]any, error) {
	resolved, err := api.resolveBlockNumber(number)
	if err != nil {
		return nil, err
	}
	block, found, err := api.app.EthereumBlockByNumber(resolved)
	if err != nil {
		return nil, err
	}
	if !found && resolved == 0 && api.app.CurrentHeight() == 0 {
		block = api.genesisBlock()
		found = true
	}
	if !found {
		return nil, nil
	}
	return api.rpcBlock(block, fullTransactions)
}

func (api *EthAPI) GetBlockByHash(hash common.Hash, fullTransactions bool) (map[string]any, error) {
	block, found, err := api.app.EthereumBlockByHash(hash)
	if err != nil {
		return nil, err
	}
	if !found && api.app.CurrentHeight() == 0 && hash == api.genesisBlock().Hash {
		block = api.genesisBlock()
		found = true
	}
	if !found {
		return nil, nil
	}
	return api.rpcBlock(block, fullTransactions)
}

func (api *EthAPI) GetBlockTransactionCountByNumber(number gethrpc.BlockNumber) (*hexutil.Uint64, error) {
	block, err := api.GetBlockByNumber(number, false)
	if err != nil || block == nil {
		return nil, err
	}
	count := hexutil.Uint64(len(block["transactions"].([]any)))
	return &count, nil
}

func (api *EthAPI) GetBlockTransactionCountByHash(hash common.Hash) (*hexutil.Uint64, error) {
	block, found, err := api.app.EthereumBlockByHash(hash)
	if err != nil || !found {
		return nil, err
	}
	count := hexutil.Uint64(len(block.Transactions))
	return &count, nil
}

func (api *EthAPI) GetTransactionByHash(hash common.Hash) (*RPCTransaction, error) {
	stored, found, err := api.app.EthereumTransactionByHash(hash)
	if err != nil || !found {
		return nil, err
	}
	return decodeRPCTransaction(stored)
}

func (api *EthAPI) GetTransactionByBlockHashAndIndex(hash common.Hash, index hexutil.Uint64) (*RPCTransaction, error) {
	block, found, err := api.app.EthereumBlockByHash(hash)
	if err != nil || !found || uint64(index) >= uint64(len(block.Transactions)) {
		return nil, err
	}
	return api.GetTransactionByHash(block.Transactions[index])
}

func (api *EthAPI) GetTransactionByBlockNumberAndIndex(number gethrpc.BlockNumber, index hexutil.Uint64) (*RPCTransaction, error) {
	resolved, err := api.resolveBlockNumber(number)
	if err != nil {
		return nil, err
	}
	block, found, err := api.app.EthereumBlockByNumber(resolved)
	if err != nil || !found || uint64(index) >= uint64(len(block.Transactions)) {
		return nil, err
	}
	return api.GetTransactionByHash(block.Transactions[index])
}

func (api *EthAPI) GetTransactionReceipt(hash common.Hash) (map[string]any, error) {
	receipt, found, err := api.app.EthereumReceiptByHash(hash)
	if err != nil || !found {
		return nil, err
	}
	transaction, found, err := api.app.EthereumTransactionByHash(hash)
	if err != nil || !found {
		return nil, err
	}
	var decoded types.Transaction
	if err := decoded.UnmarshalBinary(transaction.Raw); err != nil {
		return nil, err
	}
	var contractAddress any
	if receipt.ContractAddress != (common.Address{}) {
		contractAddress = receipt.ContractAddress
	}
	logs := receipt.Logs
	if logs == nil {
		logs = []*types.Log{}
	}
	return map[string]any{
		"transactionHash":   receipt.TransactionHash,
		"transactionIndex":  hexutil.Uint64(receipt.TransactionIndex),
		"blockHash":         receipt.BlockHash,
		"blockNumber":       hexutil.Uint64(receipt.BlockNumber),
		"from":              receipt.From,
		"to":                receipt.To,
		"cumulativeGasUsed": hexutil.Uint64(receipt.CumulativeGasUsed),
		"gasUsed":           hexutil.Uint64(receipt.GasUsed),
		"contractAddress":   contractAddress,
		"logs":              logs,
		"logsBloom":         receipt.LogsBloom,
		"status":            hexutil.Uint64(receipt.Status),
		"type":              hexutil.Uint64(decoded.Type()),
		"effectiveGasPrice": (*hexutil.Big)(decoded.GasPrice()),
	}, nil
}

func (api *EthAPI) Call(_ context.Context, args TransactionArgs, block *gethrpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if err := api.requireLatest(block); err != nil {
		return nil, err
	}
	result, err := api.app.Call(args.callArgs())
	if err != nil {
		return nil, err
	}
	if result.Err != nil {
		return nil, &revertError{cause: result.Err, data: result.RevertData}
	}
	return hexutil.Bytes(result.ReturnData), nil
}

func (api *EthAPI) EstimateGas(_ context.Context, args TransactionArgs, block *gethrpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	if err := api.requireLatest(block); err != nil {
		return 0, err
	}
	gas, err := api.app.EstimateGas(args.callArgs())
	return hexutil.Uint64(gas), err
}

type TransactionArgs struct {
	From                 *common.Address   `json:"from"`
	To                   *common.Address   `json:"to"`
	Gas                  *hexutil.Uint64   `json:"gas"`
	GasPrice             *hexutil.Big      `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big      `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big      `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big      `json:"value"`
	Data                 *hexutil.Bytes    `json:"data"`
	Input                *hexutil.Bytes    `json:"input"`
	AccessList           *types.AccessList `json:"accessList"`
}

func (args TransactionArgs) callArgs() app.CallArgs {
	var from common.Address
	if args.From != nil {
		from = *args.From
	}
	var gas uint64
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	gasPrice := new(big.Int)
	if args.GasPrice != nil {
		gasPrice.Set((*big.Int)(args.GasPrice))
	} else if args.MaxFeePerGas != nil {
		gasPrice.Set((*big.Int)(args.MaxFeePerGas))
	}
	value := new(big.Int)
	if args.Value != nil {
		value.Set((*big.Int)(args.Value))
	}
	data := args.Data
	if args.Input != nil {
		data = args.Input
	}
	var accessList types.AccessList
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	return app.CallArgs{From: from, To: args.To, Gas: gas, GasPrice: gasPrice, Value: value, Data: common.CopyBytes(*dataOrEmpty(data)), AccessList: accessList}
}

func dataOrEmpty(data *hexutil.Bytes) *hexutil.Bytes {
	if data != nil {
		return data
	}
	empty := hexutil.Bytes{}
	return &empty
}

type RPCTransaction struct {
	BlockHash        *common.Hash    `json:"blockHash"`
	BlockNumber      *hexutil.Uint64 `json:"blockNumber"`
	From             common.Address  `json:"from"`
	Gas              hexutil.Uint64  `json:"gas"`
	GasPrice         *hexutil.Big    `json:"gasPrice"`
	Hash             common.Hash     `json:"hash"`
	Input            hexutil.Bytes   `json:"input"`
	Nonce            hexutil.Uint64  `json:"nonce"`
	To               *common.Address `json:"to"`
	TransactionIndex *hexutil.Uint64 `json:"transactionIndex"`
	Value            *hexutil.Big    `json:"value"`
	Type             hexutil.Uint64  `json:"type"`
	ChainID          *hexutil.Big    `json:"chainId,omitempty"`
	V                *hexutil.Big    `json:"v"`
	R                *hexutil.Big    `json:"r"`
	S                *hexutil.Big    `json:"s"`
}

func decodeRPCTransaction(stored *app.EthereumTransaction) (*RPCTransaction, error) {
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(stored.Raw); err != nil {
		return nil, err
	}
	from, err := types.Sender(types.LatestSignerForChainID(rpcChainID), &transaction)
	if err != nil {
		return nil, err
	}
	v, r, s := transaction.RawSignatureValues()
	blockNumber := hexutil.Uint64(stored.BlockNumber)
	index := hexutil.Uint64(stored.TransactionIndex)
	blockHash := stored.BlockHash
	return &RPCTransaction{
		BlockHash: &blockHash, BlockNumber: &blockNumber, TransactionIndex: &index,
		From: from, Gas: hexutil.Uint64(transaction.Gas()), GasPrice: (*hexutil.Big)(transaction.GasPrice()),
		Hash: transaction.Hash(), Input: transaction.Data(), Nonce: hexutil.Uint64(transaction.Nonce()),
		To: transaction.To(), Value: (*hexutil.Big)(transaction.Value()), Type: hexutil.Uint64(transaction.Type()),
		ChainID: (*hexutil.Big)(transaction.ChainId()), V: (*hexutil.Big)(v), R: (*hexutil.Big)(r), S: (*hexutil.Big)(s),
	}, nil
}

func (api *EthAPI) rpcBlock(block *app.EthereumBlock, fullTransactions bool) (map[string]any, error) {
	transactions := make([]any, 0, len(block.Transactions))
	for _, hash := range block.Transactions {
		if !fullTransactions {
			transactions = append(transactions, hash)
			continue
		}
		transaction, err := api.GetTransactionByHash(hash)
		if err != nil {
			return nil, err
		}
		transactions = append(transactions, transaction)
	}
	zeroHash := common.Hash{}
	return map[string]any{
		"number": hexutil.Uint64(block.Number), "hash": block.Hash, "parentHash": block.ParentHash,
		"nonce": types.BlockNonce{}, "sha3Uncles": zeroHash, "logsBloom": block.LogsBloom,
		"transactionsRoot": zeroHash, "stateRoot": block.StateRoot, "receiptsRoot": zeroHash,
		"mixHash": zeroHash,
		"miner":   block.Coinbase, "difficulty": (*hexutil.Big)(new(big.Int)), "totalDifficulty": (*hexutil.Big)(new(big.Int)),
		"extraData": hexutil.Bytes{}, "size": hexutil.Uint64(0), "gasLimit": hexutil.Uint64(block.GasLimit),
		"gasUsed": hexutil.Uint64(block.GasUsed), "timestamp": hexutil.Uint64(block.Timestamp),
		"transactions": transactions, "uncles": []common.Hash{}, "baseFeePerGas": (*hexutil.Big)(new(big.Int)),
	}, nil
}

func (api *EthAPI) genesisBlock() *app.EthereumBlock {
	stateRoot := api.app.CurrentAppHash()
	return &app.EthereumBlock{
		Number: 0, Hash: crypto.Keccak256Hash([]byte("pluto-genesis-v1"), stateRoot.Bytes()),
		GasLimit: 30000000, StateRoot: stateRoot, Transactions: []common.Hash{},
	}
}

func (api *EthAPI) resolveBlockNumber(number gethrpc.BlockNumber) (uint64, error) {
	switch number {
	case gethrpc.LatestBlockNumber, gethrpc.PendingBlockNumber, gethrpc.SafeBlockNumber, gethrpc.FinalizedBlockNumber:
		return api.app.CurrentHeight(), nil
	case gethrpc.EarliestBlockNumber:
		return 0, nil
	default:
		if number < 0 {
			return 0, fmt.Errorf("unsupported block tag %s", number)
		}
		return uint64(number), nil
	}
}

func (api *EthAPI) requireLatest(block *gethrpc.BlockNumberOrHash) error {
	if block == nil {
		return nil
	}
	if number, ok := block.Number(); ok {
		resolved, err := api.resolveBlockNumber(number)
		if err != nil {
			return err
		}
		if resolved != api.app.CurrentHeight() {
			return errors.New("historical state is not available")
		}
		return nil
	}
	if hash, ok := block.Hash(); ok {
		latest, found, err := api.app.EthereumBlockByNumber(api.app.CurrentHeight())
		if err != nil {
			return err
		}
		if !found || latest.Hash != hash {
			return errors.New("historical state is not available")
		}
	}
	return nil
}

type revertError struct {
	cause error
	data  []byte
}

func (err *revertError) Error() string {
	if errors.Is(err.cause, vm.ErrExecutionReverted) {
		return "execution reverted"
	}
	return err.cause.Error()
}
func (*revertError) ErrorCode() int     { return 3 }
func (err *revertError) ErrorData() any { return "0x" + hex.EncodeToString(err.data) }
