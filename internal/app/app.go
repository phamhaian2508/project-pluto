package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/huyCuong73/pluto/internal/evm"
	"github.com/huyCuong73/pluto/internal/store"
)

const (
	AppVersion    uint64 = 1
	blockGasLimit uint64 = 30000000
)

// Chain ID
var chainID = big.NewInt(1)

// newChainConfig: bật mọi EIP đến Shanghai từ block 0.
// Chưa bật Cancun vì SelfDestruct (EIP-6780) trong state chưa làm đúng.
func newChainConfig() *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	return &params.ChainConfig{
		ChainID:                 chainID,
		HomesteadBlock:          zero,
		EIP150Block:             zero,
		EIP155Block:             zero,
		EIP158Block:             zero,
		ByzantiumBlock:          zero,
		ConstantinopleBlock:     zero,
		PetersburgBlock:         zero,
		IstanbulBlock:           zero,
		MuirGlacierBlock:        zero,
		BerlinBlock:             zero,
		LondonBlock:             zero,
		ArrowGlacierBlock:       zero,
		GrayGlacierBlock:        zero,
		MergeNetsplitBlock:      zero,
		TerminalTotalDifficulty: zero,
		ShanghaiTime:            &shanghai,
	}
}

type App struct {
	abci.BaseApplication
	db            *store.PebbleDB
	logger        *slog.Logger
	currentHeight int64
	appHash       []byte
	txProcessor   *evm.TxProcessor
	executor      evm.Executor
}

type GenesisState struct {
	Alloc map[string]GenesisAccount `json:"alloc"`
}

type GenesisAccount struct {
	Balance string `json:"balance"` // Dùng string cho BigInt
}

// Chạy một lần khi block height = 0
func (app *App) InitChain(ctx context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	app.logger.Info("Initializing chain with genesis data...")

	var genesisState GenesisState

	if len(req.AppStateBytes) == 0 {
		app.logger.Info("No app_state in genesis, skipping genesis allocation")
		return &abci.InitChainResponse{
			AppHash: app.appHash,
		}, nil
	}

	if err := json.Unmarshal(req.AppStateBytes, &genesisState); err != nil {
		return nil, fmt.Errorf("failed to parse genesis state: %w", err)
	}

	stateDB := evm.NewPebbleStateDB(app.db)

	for addrHex, acc := range genesisState.Alloc {
		addr := common.HexToAddress(addrHex)

		balance, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok {
			app.logger.Error("Invalid balance in genesis", "address", addrHex)
			continue
		}

		stateDB.AddBalanceBig(addr, balance)
		app.logger.Info("Genesis Alloc",
			"address", addrHex,
			"balance_wei", balance.String(),
			"balance_eth", new(big.Int).Div(balance, big.NewInt(1e18)).String(),
		)
	}

	if err := stateDB.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit genesis state: %w", err)
	}

	// Tính AppHash cho genesis
	app.appHash = stateDB.ComputeAppHash()

	return &abci.InitChainResponse{
		AppHash: app.appHash,
	}, nil
}

func NewApp(dbPath string, logger *slog.Logger) (*App, error) {
	db, err := store.NewPebbleDB("pluto", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create PebbleDB: %w", err)
	}

	// Khôi phục state khi restart
	currentHeight := int64(0)
	appHash := []byte{}

	// Đọc height
	heightBytes, err := db.Get([]byte("height"))
	if err == nil && len(heightBytes) > 0 {
		if h, err := strconv.ParseInt(string(heightBytes), 10, 64); err == nil {
			currentHeight = h
			logger.Info("Restored state", "height", currentHeight)
		}
	}

	// Đọc appHash
	savedHash, err := db.Get([]byte("appHash"))
	if err == nil && len(savedHash) > 0 {
		appHash = savedHash
	}

	return &App{
		db:            db,
		logger:        logger,
		currentHeight: currentHeight,
		appHash:       appHash,
		txProcessor:   evm.NewTxProcessor(chainID.Int64()),
		executor:      evm.NewSequentialExecutor(newChainConfig(), chainID),
	}, nil
}

// Info: trả về state hiện tại khi restart node
// Handshake
func (app *App) Info(ctx context.Context, req *abci.InfoRequest) (*abci.InfoResponse, error) {
	app.logger.Info("CometBFT Handshake received",
		"comet_version", req.Version,
		"p2p_version", req.P2PVersion,
		"block_version", req.BlockVersion)
	return &abci.InfoResponse{
		Data:             "pluto-v1",
		Version:          req.Version,
		AppVersion:       AppVersion,
		LastBlockHeight:  app.currentHeight,
		LastBlockAppHash: app.appHash,
	}, nil
}

// Gọi khi bắt đầu tạo block mới (chọn lọc/sắp xếp tx)
func (app *App) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	return &abci.PrepareProposalResponse{
		Txs: req.Txs,
	}, nil
}

// Validator kiểm tra block trước khi vote precommit
func (app *App) ProcessProposal(_ context.Context, req *abci.ProcessProposalRequest) (*abci.ProcessProposalResponse, error) {
	for _, tx := range req.Txs {
		if len(tx) == 0 {
			app.logger.Error("Rejecting block: contains empty transaction")
			return &abci.ProcessProposalResponse{
				Status: abci.PROCESS_PROPOSAL_STATUS_REJECT,
			}, nil
		}
	}

	return &abci.ProcessProposalResponse{
		Status: abci.PROCESS_PROPOSAL_STATUS_ACCEPT,
	}, nil
}

// FinalizeBlock xử lý các tx trong block (tuần tự)
func (app *App) FinalizeBlock(ctx context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// StateDB mới cho block. Transactions are decoded here so malformed input
	// stays attached to its original index in the executor result.
	stateDB := evm.NewPebbleStateDB(app.db)
	chainConfig := newChainConfig()
	transactions := make([]*types.Transaction, len(req.Txs))
	preResults := make([]*abci.ExecTxResult, len(req.Txs))
	for i, txBytes := range req.Txs {
		ethTx, err := app.txProcessor.DecodeTx(txBytes)
		if err != nil {
			preResults[i] = &abci.ExecTxResult{Code: 1, Log: fmt.Sprintf("decode error: %v", err)}
			continue
		}
		transactions[i] = ethTx
	}

	blockHash := common.BytesToHash(req.Hash)
	if len(req.Hash) == 0 {
		seed := append([]byte("pluto-block-v1"), []byte(fmt.Sprintf("%d:%d", req.Height, req.Time.Unix()))...)
		blockHash = sha256.Sum256(seed)
	}
	rand := sha256.Sum256(append([]byte("pluto-random-v1"), blockHash[:]...))
	getHash := func(n uint64) common.Hash {
		if n >= uint64(req.Height) || req.Height-int64(n) > 256 {
			return common.Hash{}
		}
		b, err := app.db.Get(blockHashKey(n))
		if err != nil {
			return common.Hash{}
		}
		return common.BytesToHash(b)
	}
	blockEnv := evm.BlockEnv{
		Height: req.Height, Time: uint64(req.Time.Unix()),
		Coinbase: common.BytesToAddress(req.ProposerAddress), GasLimit: blockGasLimit,
		Random: common.Hash(rand), GetHash: getHash, BaseFee: new(big.Int), PreviousRoot: app.appHash,
	}
	app.executor = evm.NewSequentialExecutor(chainConfig, chainID)
	blockResult, err := app.executor.ExecuteBlock(stateDB, blockEnv, transactions)
	if err != nil {
		return nil, fmt.Errorf("execute block: %w", err)
	}
	for i, result := range blockResult.Results {
		if preResults[i] != nil {
			txResults[i] = preResults[i]
			continue
		}
		code := uint32(0)
		if result.Invalid || result.Failed {
			code = 1
		}
		txResults[i] = &abci.ExecTxResult{Code: code, Log: result.ErrMsg, GasUsed: int64(result.GasUsed)}
		app.logger.Info("EVM executed", "height", req.Height, "txIndex", i, "gasUsed", result.GasUsed, "err", result.ErrMsg)
	}

	// Commit state
	if err := stateDB.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit stateDB: %w", err)
	}

	// Tính AppHash
	app.appHash = append(app.appHash[:0], blockResult.StateRoot[:]...)
	if err := app.db.Set(blockHashKey(uint64(req.Height)), blockHash.Bytes()); err != nil {
		return nil, fmt.Errorf("save block hash: %w", err)
	}

	app.currentHeight = req.Height

	return &abci.FinalizeBlockResponse{
		TxResults: txResults,
		AppHash:   app.appHash,
	}, nil
}

func blockHashKey(height uint64) []byte { return []byte(fmt.Sprintf("block-hash-%020d", height)) }

// Commit metadata (height + appHash)
func (app *App) Commit(ctx context.Context, req *abci.CommitRequest) (*abci.CommitResponse, error) {
	// Lưu height
	k := []byte("height")
	v := []byte(fmt.Sprintf("%d", app.currentHeight))
	if err := app.db.Set(k, v); err != nil {
		return nil, fmt.Errorf("failed to commit block height: %w", err)
	}

	// Lưu appHash
	if len(app.appHash) > 0 {
		if err := app.db.Set([]byte("appHash"), app.appHash); err != nil {
			return nil, fmt.Errorf("failed to commit appHash: %w", err)
		}
	}

	return &abci.CommitResponse{}, nil
}

// CheckTx kiểm tra tx trước khi vào Mempool
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	// Decode tx
	ethTx, err := app.txProcessor.DecodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid tx encoding: %v", err),
		}, nil
	}

	// Verify signature
	if _, err = app.txProcessor.RecoverSender(ethTx); err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid signature: %v", err),
		}, nil
	}

	// TODO (M2.2): kiểm tra nonce, số dư, gas limit

	return &abci.CheckTxResponse{Code: 0}, nil
}

func (app *App) Query(ctx context.Context, req *abci.QueryRequest) (*abci.QueryResponse, error) {
	stateDB := evm.NewPebbleStateDB(app.db)
	addr := common.BytesToAddress(req.Data)

	switch req.Path {
	case "balance":
		return &abci.QueryResponse{Code: 0, Value: []byte(stateDB.GetBalance(addr).ToBig().String())}, nil
	case "nonce":
		return &abci.QueryResponse{Code: 0, Value: []byte(strconv.FormatUint(stateDB.GetNonce(addr), 10))}, nil
	case "storage":
		if len(req.Data) != common.AddressLength+common.HashLength {
			return &abci.QueryResponse{Code: 1, Log: "storage query data must contain a 20-byte address and 32-byte slot"}, nil
		}
		storageAddr := common.BytesToAddress(req.Data[:common.AddressLength])
		storageKey := common.BytesToHash(req.Data[common.AddressLength:])
		value := stateDB.GetState(storageAddr, storageKey)
		return &abci.QueryResponse{Code: 0, Value: value.Bytes()}, nil
	default:
		return &abci.QueryResponse{Code: 1, Log: "unknown path: " + req.Path}, nil
	}
}

func (app *App) Close() error {
	return app.db.Close()
}
