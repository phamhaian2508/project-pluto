package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const (
	AppVersion         uint64 = 1
	blockGasLimit      uint64 = 30000000
	maxTransactionSize        = 128 * 1024
)

// Mã định danh chuỗi
var chainID = big.NewInt(plutoconfig.EVMChainID)

// newChainConfig bật mọi EIP đến Shanghai từ khối 0.
// Chưa bật Cancun vì trạng thái tự hủy (EIP-6780) chưa được cài đặt đúng.
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
	pendingBlock  *pendingBlock
	txProcessor   *evm.TxProcessor
	executor      evm.Executor
	executorMode  string
	workers       int
	autoWorkers   bool
}

type pendingBlock struct {
	height    int64
	appHash   []byte
	blockHash common.Hash
	writes    map[string][]byte
}

type GenesisState struct {
	Alloc map[string]GenesisAccount `json:"alloc"`
}

type GenesisAccount struct {
	Balance string `json:"balance"` // Dùng chuỗi để biểu diễn số nguyên lớn.
}

// Chạy một lần khi chiều cao khối bằng 0.
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

	// Tính AppHash cho khối khởi nguyên.
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

	// Khôi phục trạng thái khi khởi động lại.
	currentHeight := int64(0)
	appHash := []byte{}

	// Đọc chiều cao khối.
	heightBytes, err := db.Get([]byte("height"))
	if err == nil && len(heightBytes) > 0 {
		if h, err := strconv.ParseInt(string(heightBytes), 10, 64); err == nil {
			currentHeight = h
			logger.Info("Restored state", "height", currentHeight)
		}
	}

	// Đọc AppHash.
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
		executorMode:  "sequential",
		workers:       4,
		autoWorkers:   false,
	}, nil
}

// ConfigureExecutor chọn bộ thực thi khối có tính tất định. Mặc định là tuần tự;
// chế độ song song chỉ được bật khi cần vì phạm vi tải được hỗ trợ còn hẹp.
func (app *App) ConfigureExecutor(mode string, workers int) error {
	if mode != "sequential" && mode != "parallel" {
		return fmt.Errorf("unknown executor %q (want sequential or parallel)", mode)
	}
	if workers < 1 {
		return fmt.Errorf("worker count must be at least 1")
	}
	app.executorMode = mode
	app.workers = workers
	app.autoWorkers = false
	if mode == "parallel" {
		app.executor = evm.NewParallelExecutor(newChainConfig(), chainID, workers)
	} else {
		app.executor = evm.NewSequentialExecutor(newChainConfig(), chainID)
	}
	return nil
}

// ConfigureAdaptiveExecutor bật chế độ tự điều chỉnh số worker cho executor song song.
// Chế độ tuần tự bỏ qua bộ điều khiển worker.
func (app *App) ConfigureAdaptiveExecutor(mode string) error {
	if mode != "sequential" && mode != "parallel" {
		return fmt.Errorf("unknown executor %q (want sequential or parallel)", mode)
	}
	app.executorMode = mode
	app.autoWorkers = mode == "parallel"
	if app.autoWorkers {
		app.workers = 0
		app.executor = evm.NewAdaptiveParallelExecutor(newChainConfig(), chainID, 0)
	} else {
		app.workers = 1
		app.executor = evm.NewSequentialExecutor(newChainConfig(), chainID)
	}
	return nil
}

// Info trả về trạng thái hiện tại khi nút khởi động lại.
// Đây là bước bắt tay giữa ứng dụng và CometBFT.
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

// Được gọi khi bắt đầu tạo khối mới để chọn và sắp xếp giao dịch.
func (app *App) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	return &abci.PrepareProposalResponse{
		Txs: req.Txs,
	}, nil
}

// Validator kiểm tra khối trước khi bỏ phiếu precommit.
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

// FinalizeBlock thực thi các giao dịch trong khối bằng bộ thực thi đã cấu hình.
func (app *App) FinalizeBlock(ctx context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	if app.pendingBlock != nil {
		return nil, fmt.Errorf("cannot finalize height %d: height %d is not committed", req.Height, app.pendingBlock.height)
	}

	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// Tạo StateDB mới cho khối. Giao dịch được giải mã tại đây để dữ liệu đầu vào
	// sai định dạng vẫn gắn với đúng chỉ số trong kết quả của bộ thực thi.
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
	if app.executor == nil {
		if app.executorMode == "parallel" {
			if app.autoWorkers {
				app.executor = evm.NewAdaptiveParallelExecutor(chainConfig, chainID, 0)
			} else {
				app.executor = evm.NewParallelExecutor(chainConfig, chainID, app.workers)
			}
		} else {
			app.executor = evm.NewSequentialExecutor(chainConfig, chainID)
		}
	}
	blockResult, err := app.executor.ExecuteBlock(stateDB, blockEnv, transactions)
	if err != nil {
		return nil, fmt.Errorf("execute block: %w", err)
	}
	if len(blockResult.Metrics) > 0 {
		workerMode := fmt.Sprint(app.workers)
		if app.autoWorkers {
			workerMode = "auto"
		}
		app.logger.Info("block executor metrics", "height", req.Height, "executor", app.executorMode, "workers", workerMode, "metrics", blockResult.Metrics)
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
	if err := app.stageEthereumBlock(req, blockHash, blockResult.StateRoot, transactions, blockResult.Results, blockResult.Writes); err != nil {
		return nil, fmt.Errorf("stage ethereum block metadata: %w", err)
	}

	// Chuẩn bị trạng thái và siêu dữ liệu. Commit lưu tất cả trong cùng một thao tác.
	pendingAppHash := append([]byte(nil), blockResult.StateRoot[:]...)
	app.pendingBlock = &pendingBlock{
		height:    req.Height,
		appHash:   pendingAppHash,
		blockHash: blockHash,
		writes:    blockResult.Writes,
	}

	return &abci.FinalizeBlockResponse{
		TxResults: txResults,
		AppHash:   pendingAppHash,
	}, nil
}

func blockHashKey(height uint64) []byte { return []byte(fmt.Sprintf("block-hash-%020d", height)) }

// Commit lưu trạng thái khối đã hoàn tất cùng siêu dữ liệu trong một lô nguyên tử.
func (app *App) Commit(ctx context.Context, req *abci.CommitRequest) (*abci.CommitResponse, error) {
	if app.pendingBlock == nil {
		return &abci.CommitResponse{}, nil
	}

	pending := app.pendingBlock
	batch := app.db.NewBatch()
	defer batch.Close()

	keys := make([]string, 0, len(pending.writes))
	for key := range pending.writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := pending.writes[key]
		if value == nil {
			if err := batch.Delete([]byte(key)); err != nil {
				return nil, fmt.Errorf("stage state deletion: %w", err)
			}
			continue
		}
		if err := batch.Set([]byte(key), value); err != nil {
			return nil, fmt.Errorf("stage state write: %w", err)
		}
	}
	if err := batch.Set([]byte("height"), []byte(strconv.FormatInt(pending.height, 10))); err != nil {
		return nil, fmt.Errorf("stage block height: %w", err)
	}
	if err := batch.Set([]byte("appHash"), pending.appHash); err != nil {
		return nil, fmt.Errorf("stage app hash: %w", err)
	}
	if err := batch.Set(blockHashKey(uint64(pending.height)), pending.blockHash.Bytes()); err != nil {
		return nil, fmt.Errorf("stage block hash: %w", err)
	}
	if err := batch.WriteSync(); err != nil {
		return nil, fmt.Errorf("commit finalized block: %w", err)
	}

	app.currentHeight = pending.height
	app.appHash = append(app.appHash[:0], pending.appHash...)
	app.pendingBlock = nil

	return &abci.CommitResponse{}, nil
}

// CheckTx kiểm tra giao dịch trước khi đưa vào vùng chờ giao dịch.
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	if len(req.Tx) > maxTransactionSize {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("transaction too large: %d bytes, maximum %d", len(req.Tx), maxTransactionSize),
		}, nil
	}

	// Giải mã giao dịch.
	ethTx, err := app.txProcessor.DecodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid tx encoding: %v", err),
		}, nil
	}

	if !ethTx.Protected() || ethTx.ChainId().Cmp(chainID) != 0 {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("wrong chain ID: got %s, want %s", ethTx.ChainId(), chainID),
		}, nil
	}

	// Xác minh chữ ký.
	sender, err := app.txProcessor.RecoverSender(ethTx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid signature: %v", err),
		}, nil
	}

	if ethTx.Gas() > blockGasLimit {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("gas limit %d exceeds block gas limit %d", ethTx.Gas(), blockGasLimit),
		}, nil
	}

	// Chuỗi bật Homestead, Istanbul (EIP-2028) và Shanghai (EIP-3860)
	// ngay từ khối khởi nguyên.
	intrinsicGas, err := core.IntrinsicGas(
		ethTx.Data(),
		ethTx.AccessList(),
		ethTx.SetCodeAuthorizations(),
		ethTx.To() == nil,
		true,
		true,
		true,
	)
	if err != nil {
		return &abci.CheckTxResponse{Code: 1, Log: fmt.Sprintf("invalid intrinsic gas: %v", err)}, nil
	}
	if ethTx.Gas() < intrinsicGas {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("intrinsic gas too low: got %d, need %d", ethTx.Gas(), intrinsicGas),
		}, nil
	}

	stateDB := evm.NewPebbleStateDB(app.db)
	expectedNonce := stateDB.GetNonce(sender)
	if ethTx.Nonce() != expectedNonce {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid nonce: got %d, want %d", ethTx.Nonce(), expectedNonce),
		}, nil
	}

	balance := stateDB.GetBalance(sender).ToBig()
	required := ethTx.Cost()
	if balance.Cmp(required) < 0 {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("insufficient balance: have %s, need %s", balance, required),
		}, nil
	}

	return &abci.CheckTxResponse{Code: 0, GasWanted: int64(ethTx.Gas())}, nil
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
