package app

import (
	"encoding/json"
	"fmt"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/evm"
)

// EthereumBlock stores the Ethereum-facing metadata for a committed block.
type EthereumBlock struct {
	Number       uint64         `json:"number"`
	Hash         common.Hash    `json:"hash"`
	ParentHash   common.Hash    `json:"parentHash"`
	Timestamp    uint64         `json:"timestamp"`
	Coinbase     common.Address `json:"coinbase"`
	GasLimit     uint64         `json:"gasLimit"`
	GasUsed      uint64         `json:"gasUsed"`
	StateRoot    common.Hash    `json:"stateRoot"`
	LogsBloom    types.Bloom    `json:"logsBloom"`
	Transactions []common.Hash  `json:"transactions"`
}

// EthereumTransaction stores canonical transaction bytes and their block position.
type EthereumTransaction struct {
	Hash             common.Hash `json:"hash"`
	Raw              []byte      `json:"raw"`
	BlockHash        common.Hash `json:"blockHash"`
	BlockNumber      uint64      `json:"blockNumber"`
	TransactionIndex uint64      `json:"transactionIndex"`
}

// EthereumReceipt stores the execution result required by Ethereum clients.
type EthereumReceipt struct {
	TransactionHash   common.Hash     `json:"transactionHash"`
	TransactionIndex  uint64          `json:"transactionIndex"`
	BlockHash         common.Hash     `json:"blockHash"`
	BlockNumber       uint64          `json:"blockNumber"`
	From              common.Address  `json:"from"`
	To                *common.Address `json:"to,omitempty"`
	ContractAddress   common.Address  `json:"contractAddress"`
	Status            uint64          `json:"status"`
	GasUsed           uint64          `json:"gasUsed"`
	CumulativeGasUsed uint64          `json:"cumulativeGasUsed"`
	LogsBloom         types.Bloom     `json:"logsBloom"`
	Logs              []*types.Log    `json:"logs"`
}

// EthereumTransactionLocation is the tx-hash index into a block.
type EthereumTransactionLocation struct {
	BlockHash        common.Hash `json:"blockHash"`
	BlockNumber      uint64      `json:"blockNumber"`
	TransactionIndex uint64      `json:"transactionIndex"`
}

func ethereumBlockKey(number uint64) []byte {
	return []byte(fmt.Sprintf("eth:block:%020d", number))
}

func ethereumBlockHashKey(hash common.Hash) []byte {
	return append([]byte("eth:block-hash:"), hash.Bytes()...)
}

func ethereumTransactionKey(hash common.Hash) []byte {
	return append([]byte("eth:tx:"), hash.Bytes()...)
}

func ethereumReceiptKey(hash common.Hash) []byte {
	return append([]byte("eth:receipt:"), hash.Bytes()...)
}

func ethereumTransactionLocationKey(hash common.Hash) []byte {
	return append([]byte("eth:tx-location:"), hash.Bytes()...)
}

func putJSON(writes map[string][]byte, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writes[string(key)] = encoded
	return nil
}

func (app *App) stageEthereumBlock(
	req *abci.FinalizeBlockRequest,
	blockHash common.Hash,
	stateRoot [32]byte,
	transactions []*types.Transaction,
	results []evm.TxResult,
	writes map[string][]byte,
) error {
	blockNumber := uint64(req.Height)
	parentHash := common.Hash{}
	if blockNumber > 1 {
		encoded, err := app.db.Get(blockHashKey(blockNumber - 1))
		if err != nil {
			return fmt.Errorf("load parent block hash: %w", err)
		}
		parentHash = common.BytesToHash(encoded)
	}

	block := EthereumBlock{
		Number:       blockNumber,
		Hash:         blockHash,
		ParentHash:   parentHash,
		Timestamp:    uint64(req.Time.Unix()),
		Coinbase:     common.BytesToAddress(req.ProposerAddress),
		GasLimit:     blockGasLimit,
		StateRoot:    common.BytesToHash(stateRoot[:]),
		Transactions: make([]common.Hash, 0, len(transactions)),
	}

	var cumulativeGas uint64
	var logIndex uint
	for originalIndex, transaction := range transactions {
		if transaction == nil {
			continue
		}

		result := results[originalIndex]
		transactionIndex := uint64(len(block.Transactions))
		transactionHash := transaction.Hash()
		block.Transactions = append(block.Transactions, transactionHash)
		cumulativeGas += result.GasUsed
		block.GasUsed += result.GasUsed

		from, _ := app.txProcessor.RecoverSender(transaction)
		var to *common.Address
		if destination := transaction.To(); destination != nil {
			copy := *destination
			to = &copy
		}
		contractAddress := common.Address{}
		status := types.ReceiptStatusFailed
		if !result.Invalid && !result.Failed {
			status = types.ReceiptStatusSuccessful
			if to == nil {
				contractAddress = crypto.CreateAddress(from, transaction.Nonce())
			}
		}

		logs := make([]*types.Log, 0, len(result.Logs))
		for _, entry := range result.Logs {
			logCopy := *entry
			logCopy.BlockNumber = blockNumber
			logCopy.BlockHash = blockHash
			logCopy.BlockTimestamp = block.Timestamp
			logCopy.TxHash = transactionHash
			logCopy.TxIndex = uint(transactionIndex)
			logCopy.Index = logIndex
			logCopy.Removed = false
			logs = append(logs, &logCopy)
			logIndex++
		}
		logsBloom := types.CreateBloom(&types.Receipt{Logs: logs})
		for i := range block.LogsBloom {
			block.LogsBloom[i] |= logsBloom[i]
		}

		storedTransaction := EthereumTransaction{
			Hash:             transactionHash,
			Raw:              append([]byte(nil), req.Txs[originalIndex]...),
			BlockHash:        blockHash,
			BlockNumber:      blockNumber,
			TransactionIndex: transactionIndex,
		}
		receipt := EthereumReceipt{
			TransactionHash:   transactionHash,
			TransactionIndex:  transactionIndex,
			BlockHash:         blockHash,
			BlockNumber:       blockNumber,
			From:              from,
			To:                to,
			ContractAddress:   contractAddress,
			Status:            status,
			GasUsed:           result.GasUsed,
			CumulativeGasUsed: cumulativeGas,
			LogsBloom:         logsBloom,
			Logs:              logs,
		}
		location := EthereumTransactionLocation{
			BlockHash:        blockHash,
			BlockNumber:      blockNumber,
			TransactionIndex: transactionIndex,
		}
		if err := putJSON(writes, ethereumTransactionKey(transactionHash), &storedTransaction); err != nil {
			return fmt.Errorf("encode transaction %s: %w", transactionHash, err)
		}
		if err := putJSON(writes, ethereumReceiptKey(transactionHash), &receipt); err != nil {
			return fmt.Errorf("encode receipt %s: %w", transactionHash, err)
		}
		if err := putJSON(writes, ethereumTransactionLocationKey(transactionHash), &location); err != nil {
			return fmt.Errorf("encode transaction location %s: %w", transactionHash, err)
		}
	}

	if err := putJSON(writes, ethereumBlockKey(blockNumber), &block); err != nil {
		return fmt.Errorf("encode block %d: %w", blockNumber, err)
	}
	writes[string(ethereumBlockHashKey(blockHash))] = []byte(strconv.FormatUint(blockNumber, 10))
	return nil
}

func (app *App) loadJSON(key []byte, target any) (bool, error) {
	encoded, err := app.db.Get(key)
	if err != nil || len(encoded) == 0 {
		return false, err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return false, err
	}
	return true, nil
}

func (app *App) EthereumBlockByNumber(number uint64) (*EthereumBlock, bool, error) {
	var block EthereumBlock
	found, err := app.loadJSON(ethereumBlockKey(number), &block)
	return &block, found, err
}

func (app *App) EthereumBlockByHash(hash common.Hash) (*EthereumBlock, bool, error) {
	encoded, err := app.db.Get(ethereumBlockHashKey(hash))
	if err != nil || len(encoded) == 0 {
		return &EthereumBlock{}, false, err
	}
	number, err := strconv.ParseUint(string(encoded), 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("decode block number for hash %s: %w", hash, err)
	}
	return app.EthereumBlockByNumber(number)
}

func (app *App) EthereumTransactionByHash(hash common.Hash) (*EthereumTransaction, bool, error) {
	var transaction EthereumTransaction
	found, err := app.loadJSON(ethereumTransactionKey(hash), &transaction)
	return &transaction, found, err
}

func (app *App) EthereumReceiptByHash(hash common.Hash) (*EthereumReceipt, bool, error) {
	var receipt EthereumReceipt
	found, err := app.loadJSON(ethereumReceiptKey(hash), &receipt)
	return &receipt, found, err
}

func (app *App) EthereumTransactionLocationByHash(hash common.Hash) (*EthereumTransactionLocation, bool, error) {
	var location EthereumTransactionLocation
	found, err := app.loadJSON(ethereumTransactionLocationKey(hash), &location)
	return &location, found, err
}
