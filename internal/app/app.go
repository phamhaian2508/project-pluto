package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const (
	AppVersion         uint64 = 1
	blockGasLimit      uint64 = 30000000
	maxTransactionSize        = 128 * 1024

	genesisDigestKey  = "genesis_digest"
	genesisAppHashKey = "genesis_app_hash"
	evmChainIDKey     = "evm_chain_id"
)

// Mã định danh chuỗi mặc định
var chainID = big.NewInt(plutoconfig.EVMChainID)

// newChainConfig bật mọi EIP đến Shanghai từ khối 0.
// Chưa bật Cancun vì trạng thái tự hủy (EIP-6780) chưa được cài đặt đúng.
func newChainConfig() *params.ChainConfig {
	return newChainConfigForID(chainID)
}

func newChainConfigForID(id *big.Int) *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	cid := new(big.Int).Set(chainID)
	if id != nil && id.Sign() > 0 {
		cid.Set(id)
	}
	return &params.ChainConfig{
		ChainID:                 cid,
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

type checkpointConfigurable interface {
	ConfigureCheckpoints(evm.CheckpointConfig) error
}

type App struct {
	abci.BaseApplication
	db             *store.PebbleDB
	logger         *slog.Logger
	metaMu         sync.RWMutex
	currentHeight  int64
	appHash        []byte
	genesisAppHash []byte
	chainID        *big.Int
	pendingBlock   *pendingBlock
	txProcessor    *evm.TxProcessor
	executor       evm.Executor
	executorMode   string
	workers        int
	autoWorkers    bool

	// test seam: chặn ngay sau batch.WriteSync() trước khi cập nhật RAM để test crash recovery
	afterCommitWriteSyncHook func(height int64, appHash []byte) error
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

type genesisEntry struct {
	addr    common.Address
	balance *big.Int
}

func parseCanonicalGenesis(req *abci.InitChainRequest, evmChainID *big.Int) ([]genesisEntry, []byte, error) {
	var genesisState GenesisState
	if len(req.AppStateBytes) > 0 {
		if err := json.Unmarshal(req.AppStateBytes, &genesisState); err != nil {
			return nil, nil, fmt.Errorf("failed to parse genesis state: %w", err)
		}
	}

	entries := make([]genesisEntry, 0, len(genesisState.Alloc))
	seen := make(map[common.Address]struct{}, len(genesisState.Alloc))
	for addrHex, acc := range genesisState.Alloc {
		addr := common.HexToAddress(addrHex)
		if _, dup := seen[addr]; dup {
			return nil, nil, fmt.Errorf("duplicate genesis address %s", addr.Hex())
		}
		seen[addr] = struct{}{}

		balance, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok || balance.Sign() < 0 {
			return nil, nil, fmt.Errorf("invalid balance %q for genesis address %s", acc.Balance, addrHex)
		}
		entries = append(entries, genesisEntry{addr: addr, balance: balance})
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].addr.Bytes(), entries[j].addr.Bytes()) < 0
	})

	h := sha256.New()
	h.Write([]byte("pluto-genesis-digest-v2"))
	h.Write([]byte{0})
	h.Write([]byte(req.ChainId))
	h.Write([]byte{0})
	if evmChainID != nil {
		h.Write([]byte(evmChainID.String()))
	}
	h.Write([]byte{0})
	for _, entry := range entries {
		h.Write(entry.addr.Bytes())
		h.Write([]byte("="))
		h.Write([]byte(entry.balance.String()))
		h.Write([]byte{0})
	}
	return entries, h.Sum(nil), nil
}

// InitChain idempotent tại height=0: ghi nguyên tử alloc + genesis_digest + genesis_app_hash + evm_chain_id + height=0.
func (app *App) InitChain(ctx context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	app.logger.Info("Initializing chain with genesis data...")

	// luôn chặn InitChain nếu DB đã tiến triển qua khối 0, bất kể genesis có trùng hay không
	if app.CurrentHeight() > 0 {
		return nil, fmt.Errorf("cannot run InitChain on already progressed db at height %d", app.CurrentHeight())
	}

	evmCID := app.effectiveChainID()
	entries, digest, err := parseCanonicalGenesis(req, evmCID)
	if err != nil {
		return nil, err
	}

	existingDigest, err := app.db.Get([]byte(genesisDigestKey))
	if err != nil {
		return nil, fmt.Errorf("read genesis digest: %w", err)
	}
	if len(existingDigest) > 0 {
		if !bytes.Equal(existingDigest, digest) {
			return nil, fmt.Errorf("genesis mismatch: db already initialized with digest %x, got %x", existingDigest, digest)
		}
		savedGenesisHash, err := app.db.Get([]byte(genesisAppHashKey))
		if err != nil {
			return nil, fmt.Errorf("read genesis app hash: %w", err)
		}
		if len(savedGenesisHash) == 0 {
			savedGenesisHash, err = app.db.Get([]byte("appHash"))
			if err != nil {
				return nil, fmt.Errorf("read app hash: %w", err)
			}
		}
		app.metaMu.Lock()
		app.genesisAppHash = append([]byte(nil), savedGenesisHash...)
		if app.currentHeight == 0 && len(app.appHash) == 0 {
			app.appHash = append([]byte(nil), savedGenesisHash...)
		}
		app.metaMu.Unlock()
		return &abci.InitChainResponse{
			AppHash: append([]byte(nil), savedGenesisHash...),
		}, nil
	}

	stateDB := evm.NewPebbleStateDB(app.db)
	for _, entry := range entries {
		stateDB.AddBalanceBig(entry.addr, entry.balance)
		app.logger.Info("Genesis Alloc",
			"address", entry.addr.Hex(),
			"balance_wei", entry.balance.String(),
			"balance_eth", new(big.Int).Div(entry.balance, big.NewInt(1e18)).String(),
		)
	}

	writes, err := stateDB.PendingWrites()
	if err != nil {
		return nil, fmt.Errorf("collect genesis state writes: %w", err)
	}
	genesisAppHash := stateDB.ComputeAppHash()

	batch := app.db.NewBatch()
	defer batch.Close()

	keys := make([]string, 0, len(writes))
	for k := range writes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := writes[k]
		if val == nil {
			if err := batch.Delete([]byte(k)); err != nil {
				return nil, fmt.Errorf("stage genesis deletion: %w", err)
			}
			continue
		}
		if err := batch.Set([]byte(k), val); err != nil {
			return nil, fmt.Errorf("stage genesis write: %w", err)
		}
	}
	if err := batch.Set([]byte(genesisDigestKey), digest); err != nil {
		return nil, fmt.Errorf("stage genesis digest: %w", err)
	}
	if err := batch.Set([]byte(genesisAppHashKey), genesisAppHash); err != nil {
		return nil, fmt.Errorf("stage genesis app hash: %w", err)
	}
	if err := batch.Set([]byte(evmChainIDKey), []byte(evmCID.String())); err != nil {
		return nil, fmt.Errorf("stage evm chain id: %w", err)
	}
	if err := batch.Set([]byte("height"), []byte("0")); err != nil {
		return nil, fmt.Errorf("stage genesis height: %w", err)
	}
	if err := batch.Set([]byte("appHash"), genesisAppHash); err != nil {
		return nil, fmt.Errorf("stage app hash: %w", err)
	}
	if err := batch.Set(blockHashKey(0), genesisAppHash); err != nil {
		return nil, fmt.Errorf("stage genesis block hash: %w", err)
	}

	app.metaMu.Lock()
	if err := batch.WriteSync(); err != nil {
		app.metaMu.Unlock()
		return nil, fmt.Errorf("commit genesis state: %w", err)
	}
	app.currentHeight = 0
	app.appHash = append([]byte(nil), genesisAppHash...)
	app.genesisAppHash = append([]byte(nil), genesisAppHash...)
	app.metaMu.Unlock()

	return &abci.InitChainResponse{
		AppHash: append([]byte(nil), genesisAppHash...),
	}, nil
}

func NewApp(dbPath string, logger *slog.Logger) (*App, error) {
	return NewAppWithChainID(dbPath, logger, plutoconfig.EVMChainID)
}

func NewAppWithChainID(dbPath string, logger *slog.Logger, evmChainID int64) (*App, error) {
	if evmChainID <= 0 {
		return nil, fmt.Errorf("EVM chain ID must be positive, got %d", evmChainID)
	}
	db, err := store.NewPebbleDB("pluto", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create PebbleDB: %w", err)
	}

	// Khôi phục trạng thái bền vững khi khởi động lại.
	currentHeight := int64(0)
	appHash := []byte{}
	genesisAppHash := []byte{}

	heightBytes, err := db.Get([]byte("height"))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read persisted height: %w", err)
	}
	if len(heightBytes) > 0 {
		h, parseErr := strconv.ParseInt(string(heightBytes), 10, 64)
		if parseErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("parse persisted height %q: %w", string(heightBytes), parseErr)
		}
		currentHeight = h
		if logger != nil {
			logger.Info("Restored state", "height", currentHeight)
		}
	}

	savedHash, err := db.Get([]byte("appHash"))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read persisted appHash: %w", err)
	}
	if len(savedHash) > 0 {
		appHash = append([]byte(nil), savedHash...)
	}

	savedGenesisHash, err := db.Get([]byte(genesisAppHashKey))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read persisted genesis appHash: %w", err)
	}
	if len(savedGenesisHash) > 0 {
		genesisAppHash = append([]byte(nil), savedGenesisHash...)
	}

	savedChainID, err := db.Get([]byte(evmChainIDKey))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read persisted evm chain id: %w", err)
	}
	if len(savedChainID) > 0 && string(savedChainID) != strconv.FormatInt(evmChainID, 10) {
		_ = db.Close()
		return nil, fmt.Errorf("evm chain id mismatch: db has %s, got %d", string(savedChainID), evmChainID)
	}

	cid := big.NewInt(evmChainID)
	return &App{
		db:             db,
		logger:         logger,
		currentHeight:  currentHeight,
		appHash:        appHash,
		genesisAppHash: genesisAppHash,
		chainID:        cid,
		txProcessor:    evm.NewTxProcessor(evmChainID),
		executor:       evm.NewSequentialExecutor(newChainConfigForID(cid), cid),
		executorMode:   "sequential",
		workers:        4,
		autoWorkers:    false,
	}, nil
}

func (app *App) effectiveChainID() *big.Int {
	if app != nil && app.chainID != nil && app.chainID.Sign() > 0 {
		return new(big.Int).Set(app.chainID)
	}
	return new(big.Int).Set(chainID)
}

func (app *App) chainConfig() *params.ChainConfig {
	return newChainConfigForID(app.effectiveChainID())
}

func (app *App) getTxProcessor() *evm.TxProcessor {
	if app.txProcessor != nil {
		return app.txProcessor
	}
	return evm.NewTxProcessor(app.effectiveChainID().Int64())
}

// ConfigureExecutor chọn bộ thực thi khối có tính tất định.
func (app *App) ConfigureExecutor(mode string, workers int) error {
	if workers < 1 {
		return fmt.Errorf("worker count must be at least 1")
	}
	cfg := app.chainConfig()
	cid := app.effectiveChainID()
	switch mode {
	case "sequential":
		app.executor = evm.NewSequentialExecutor(cfg, cid)
	case "parallel":
		exec := evm.NewParallelExecutor(cfg, cid, workers)
		exec.SlidingWindow = true
		app.executor = exec
	case "occ":
		exec := evm.NewParallelExecutor(cfg, cid, workers)
		exec.SlidingWindow = true
		app.executor = exec
	case "grouped":
		app.executor = evm.NewGroupedExecutor(cfg, cid, workers)
	case "predicted-occ":
		app.executor = evm.NewPredictedOCCExecutor(cfg, cid, workers)
	default:
		return fmt.Errorf("unknown executor %q (want sequential, parallel, occ, grouped or predicted-occ)", mode)
	}
	app.executorMode = mode
	app.workers = workers
	app.autoWorkers = false
	return nil
}

// ConfigureAdaptiveExecutor bật chế độ tự điều chỉnh số worker cho executor song song.
// Chế độ tuần tự bỏ qua bộ điều khiển worker; grouped/predicted-occ dùng GOMAXPROCS.
func (app *App) ConfigureAdaptiveExecutor(mode string) error {
	cfg := app.chainConfig()
	cid := app.effectiveChainID()
	defaultWorkers := runtime.GOMAXPROCS(0)
	if defaultWorkers < 1 {
		defaultWorkers = 1
	}
	switch mode {
	case "sequential":
		app.executorMode = mode
		app.autoWorkers = false
		app.workers = 1
		app.executor = evm.NewSequentialExecutor(cfg, cid)
	case "parallel":
		app.executorMode = mode
		app.autoWorkers = true
		app.workers = 0
		exec := evm.NewAdaptiveParallelExecutor(cfg, cid, 0)
		exec.SlidingWindow = true
		app.executor = exec
	case "occ":
		app.executorMode = mode
		app.autoWorkers = true
		app.workers = 0
		exec := evm.NewAdaptiveParallelExecutor(cfg, cid, 0)
		exec.SlidingWindow = true
		app.executor = exec
	case "grouped":
		app.executorMode = mode
		app.autoWorkers = false
		app.workers = defaultWorkers
		app.executor = evm.NewGroupedExecutor(cfg, cid, defaultWorkers)
	case "predicted-occ":
		app.executorMode = mode
		app.autoWorkers = false
		app.workers = defaultWorkers
		app.executor = evm.NewPredictedOCCExecutor(cfg, cid, defaultWorkers)
	default:
		return fmt.Errorf("unknown executor %q (want sequential, parallel, occ, grouped or predicted-occ)", mode)
	}
	return nil
}

// ConfigureCheckpoints gắn cấu hình checkpoint/ngân sách RAM vào executor hỗ trợ.
// P0/off là no-op cho sequential, grouped và predicted-occ.
func (app *App) ConfigureCheckpoints(config evm.CheckpointConfig) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid checkpoint configuration: %w", err)
	}
	switch app.executorMode {
	case "sequential", "grouped", "":
		if config.Policy == evm.CheckpointPolicyP0 {
			return nil
		}
		return fmt.Errorf("checkpoint policy %s requires parallel, occ or predicted-occ executor", config.Policy)
	case "parallel", "occ", "predicted-occ":
		if configurable, ok := app.executor.(checkpointConfigurable); ok {
			if parallel, isParallel := app.executor.(*evm.ParallelExecutor); isParallel && config.UseBudget {
				// ngân sách cứng M byte chạy trên đường sliding-window OCC
				parallel.SlidingWindow = true
			}
			return configurable.ConfigureCheckpoints(config)
		}
		if config.Policy == evm.CheckpointPolicyP0 {
			return nil
		}
		return fmt.Errorf("executor %s does not support active checkpoint policy %s", app.executorMode, config.Policy)
	default:
		return fmt.Errorf("unsupported executor %q for checkpoint configuration", app.executorMode)
	}
}

// Info trả về trạng thái bền vững gần nhất để CometBFT bắt tay khi mở lại.
func (app *App) Info(ctx context.Context, req *abci.InfoRequest) (*abci.InfoResponse, error) {
	// CometBFT gọi Info khi bắt tay hoặc khôi phục; Pluto trả chiều cao và AppHash đã lưu.
	if app.logger != nil {
		app.logger.Info("CometBFT Handshake received",
			"comet_version", req.Version,
			"p2p_version", req.P2PVersion,
			"block_version", req.BlockVersion)
	}
	app.metaMu.RLock()
	height := app.currentHeight
	hash := append([]byte(nil), app.appHash...)
	app.metaMu.RUnlock()

	return &abci.InfoResponse{
		Data:             "pluto-v1",
		Version:          req.Version,
		AppVersion:       AppVersion,
		LastBlockHeight:  height,
		LastBlockAppHash: hash,
	}, nil
}

// derivePrevRandao tính PREVRANDAO tất định từ các đầu vào đã biết trước khi đóng header,
// đảm bảo PrepareProposal, ProcessProposal và FinalizeBlock nhận cùng 1 giá trị.
func derivePrevRandao(height int64, blockTime time.Time, proposer []byte, prevAppHash []byte) common.Hash {
	h := sha256.New()
	h.Write([]byte("pluto-prevrandao-v1"))
	h.Write([]byte{0})
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(height))
	binary.BigEndian.PutUint64(buf[8:], uint64(blockTime.Unix()))
	h.Write(buf[:])
	h.Write(proposer)
	h.Write(prevAppHash)
	return common.BytesToHash(h.Sum(nil))
}

func (app *App) buildProposalEnv(height int64, blockTime time.Time, proposer []byte) (
	*evm.PebbleStateDB, *vm.EVM, types.Signer, *big.Int, *core.GasPool, error,
) {
	stateDB := evm.NewPebbleStateDB(app.db)
	chainConfig := app.chainConfig()
	baseFeePerGas, err := app.BaseFeePerGasForBlock(uint64(height))
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("calculate block base fee: %w", err)
	}

	app.metaMu.RLock()
	prevAppHash := append([]byte(nil), app.appHash...)
	app.metaMu.RUnlock()
	rand := derivePrevRandao(height, blockTime, proposer, prevAppHash)
	getHash := func(n uint64) common.Hash {
		if n >= uint64(height) || height-int64(n) > 256 {
			return common.Hash{}
		}
		b, err := app.db.Get(blockHashKey(n))
		if err != nil {
			return common.Hash{}
		}
		return common.BytesToHash(b)
	}
	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     getHash,
		Coinbase:    common.BytesToAddress(proposer),
		BlockNumber: big.NewInt(height),
		Time:        uint64(blockTime.Unix()),
		Difficulty:  new(big.Int),
		Random:      &rand,
		BaseFee:     new(big.Int).Set(baseFeePerGas),
		BlobBaseFee: new(big.Int),
		GasLimit:    blockGasLimit,
	}
	evmEnv := vm.NewEVM(blockContext, stateDB, chainConfig, vm.Config{})
	signer := types.MakeSigner(chainConfig, big.NewInt(height), uint64(blockTime.Unix()))
	gasPool := new(core.GasPool).AddGas(blockGasLimit)
	return stateDB, evmEnv, signer, baseFeePerGas, gasPool, nil
}

// validateProposalTx kiểm tra mức protocol trên stateDB tạm (KHÔNG commit, KHÔNG chạm executor history).
// Giao dịch hợp lệ nhưng EVM revert/OOG vẫn trả valid=true (sinh receipt status=0 khi FinalizeBlock).
func (app *App) validateProposalTxStatic(
	txBytes []byte,
	signer types.Signer,
	baseFeePerGas *big.Int,
) (*core.Message, bool, string) {
	if len(txBytes) == 0 {
		return nil, false, "empty transaction"
	}
	if len(txBytes) > maxTransactionSize {
		return nil, false, fmt.Sprintf("transaction too large: %d bytes", len(txBytes))
	}
	processor := app.getTxProcessor()
	ethTx, err := processor.DecodeTx(txBytes)
	if err != nil {
		return nil, false, fmt.Sprintf("invalid tx encoding: %v", err)
	}
	if ethTx.Type() == types.BlobTxType || ethTx.Type() == types.SetCodeTxType {
		return nil, false, fmt.Sprintf("unsupported tx type %d", ethTx.Type())
	}
	expectedChainID := app.effectiveChainID()
	if !ethTx.Protected() || ethTx.ChainId().Cmp(expectedChainID) != 0 {
		return nil, false, fmt.Sprintf("wrong chain ID: got %s, want %s", ethTx.ChainId(), expectedChainID)
	}
	if _, err := processor.RecoverSender(ethTx); err != nil {
		return nil, false, fmt.Sprintf("invalid signature: %v", err)
	}
	if ethTx.Gas() > blockGasLimit {
		return nil, false, fmt.Sprintf("gas limit %d exceeds block gas limit %d", ethTx.Gas(), blockGasLimit)
	}
	if ethTx.GasFeeCap().Cmp(baseFeePerGas) < 0 {
		return nil, false, fmt.Sprintf("max fee per gas %s below base fee %s", ethTx.GasFeeCap(), baseFeePerGas)
	}
	if ethTx.GasTipCap().Cmp(ethTx.GasFeeCap()) > 0 {
		return nil, false, "max priority fee per gas exceeds max fee per gas"
	}
	msg, err := core.TransactionToMessage(ethTx, signer, new(big.Int).Set(baseFeePerGas))
	if err != nil {
		return nil, false, fmt.Sprintf("invalid message: %v", err)
	}
	return msg, true, ""
}

// validateProposalTxExec thực thi xác thực protocol trên stateDB tạm (KHÔNG commit).
func (app *App) validateProposalTxExec(
	msg *core.Message,
	stateDB *evm.PebbleStateDB,
	evmEnv *vm.EVM,
	gasPool *core.GasPool,
) (bool, string, error) {
	savedGas := gasPool.Gas()
	snap := stateDB.Snapshot()
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))
	_, err := core.ApplyMessage(evmEnv, msg, gasPool)
	if dbErr := stateDB.Err(); dbErr != nil {
		return false, "", fmt.Errorf("proposal state db error: %w", dbErr)
	}
	if err != nil {
		stateDB.RevertToSnapshot(snap)
		stateDB.Finalise(true)
		*gasPool = core.GasPool(savedGas)
		return false, err.Error(), nil
	}
	stateDB.Finalise(true)
	if dbErr := stateDB.Err(); dbErr != nil {
		return false, "", fmt.Errorf("proposal state db error after finalise: %w", dbErr)
	}
	return true, "", nil
}

// PrepareProposal lọc tx hợp lệ mức protocol trên bản state tạm từ H-1 và tôn trọng giới hạn MaxTxBytes.
func (app *App) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	// Mô phỏng lần lượt trên state tạm để chỉ chọn giao dịch hợp lệ theo protocol và vừa giới hạn byte.
	// Đây là bước chuẩn bị đề xuất; các thay đổi chưa được ghi vào Pebble.
	expectedHeight := int64(app.CurrentHeight()) + 1
	if req.Height != expectedHeight {
		return nil, fmt.Errorf("unexpected PrepareProposal height %d, want %d", req.Height, expectedHeight)
	}
	stateDB, evmEnv, signer, baseFeePerGas, gasPool, err := app.buildProposalEnv(req.Height, req.Time, req.ProposerAddress)
	if err != nil {
		return nil, err
	}

	accepted := make([][]byte, 0, len(req.Txs))
	var totalBytes int64
	for _, txBytes := range req.Txs {
		if req.MaxTxBytes > 0 && totalBytes+int64(len(txBytes)) > req.MaxTxBytes {
			continue
		}
		msg, ok, reason := app.validateProposalTxStatic(txBytes, signer, baseFeePerGas)
		if !ok {
			if app.logger != nil {
				app.logger.Debug("Skipping statically invalid tx in PrepareProposal", "height", req.Height, "reason", reason)
			}
			continue
		}
		valid, reason, err := app.validateProposalTxExec(msg, stateDB, evmEnv, gasPool)
		if err != nil {
			return nil, err
		}
		if !valid {
			if app.logger != nil {
				app.logger.Debug("Skipping protocol-invalid tx in PrepareProposal", "height", req.Height, "reason", reason)
			}
			continue
		}
		accepted = append(accepted, txBytes)
		totalBytes += int64(len(txBytes))
	}
	return &abci.PrepareProposalResponse{
		Txs: accepted,
	}, nil
}

// ProcessProposal kiểm tra tĩnh toàn bộ req.Txs trước (chống DoS CPU), rồi mới chạy EVM xác định theo thứ tự.
func (app *App) ProcessProposal(_ context.Context, req *abci.ProcessProposalRequest) (*abci.ProcessProposalResponse, error) {
	// Mọi validator kiểm tra cùng đề xuất: kiểm tra tĩnh trước, rồi mô phỏng EVM theo thứ tự giao dịch.
	// Cách làm này giúp các validator quyết định accept/reject dựa trên cùng quy tắc và state nền.
	expectedHeight := int64(app.CurrentHeight()) + 1
	if req.Height != expectedHeight {
		if app.logger != nil {
			app.logger.Error("Rejecting proposal with unexpected height", "height", req.Height, "want", expectedHeight)
		}
		return &abci.ProcessProposalResponse{
			Status: abci.PROCESS_PROPOSAL_STATUS_REJECT,
		}, nil
	}
	stateDB, evmEnv, signer, baseFeePerGas, gasPool, err := app.buildProposalEnv(req.Height, req.Time, req.ProposerAddress)
	if err != nil {
		return nil, err
	}

	// Pass 1: lọc tĩnh toàn bộ req.Txs trước khi chạy EVM để đề xuất Byzantine có tx rác ở cuối bị từ chối tức thì
	msgs := make([]*core.Message, len(req.Txs))
	for i, txBytes := range req.Txs {
		msg, ok, reason := app.validateProposalTxStatic(txBytes, signer, baseFeePerGas)
		if !ok {
			if app.logger != nil {
				app.logger.Error("Rejecting proposal with statically invalid transaction", "height", req.Height, "txIndex", i, "reason", reason)
			}
			return &abci.ProcessProposalResponse{
				Status: abci.PROCESS_PROPOSAL_STATUS_REJECT,
			}, nil
		}
		msgs[i] = msg
	}

	// Pass 2: kiểm tra trạng thái/nonce/số dư/gasPool theo đúng thứ tự
	for i, msg := range msgs {
		valid, reason, err := app.validateProposalTxExec(msg, stateDB, evmEnv, gasPool)
		if err != nil {
			return nil, err
		}
		if !valid {
			if app.logger != nil {
				app.logger.Error("Rejecting proposal with protocol-invalid transaction", "height", req.Height, "txIndex", i, "reason", reason)
			}
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
	// CometBFT chỉ gọi đến đây sau khi đã chọn block; kết quả EVM được giữ pending đến Commit.
	if app.pendingBlock != nil {
		return nil, fmt.Errorf("cannot finalize height %d: height %d is not committed", req.Height, app.pendingBlock.height)
	}
	expectedHeight := int64(app.CurrentHeight()) + 1
	if req.Height != expectedHeight {
		return nil, fmt.Errorf("unexpected FinalizeBlock height %d, want %d", req.Height, expectedHeight)
	}

	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// Tạo StateDB mới cho khối. Giao dịch được giải mã tại đây để dữ liệu đầu vào
	// sai định dạng vẫn gắn với đúng chỉ số trong kết quả của bộ thực thi.
	stateDB := evm.NewPebbleStateDB(app.db)
	chainConfig := app.chainConfig()
	cid := app.effectiveChainID()
	processor := app.getTxProcessor()
	transactions := make([]*types.Transaction, len(req.Txs))
	preResults := make([]*abci.ExecTxResult, len(req.Txs))
	// Giải mã theo chỉ số trong block để lỗi của một giao dịch vẫn gắn đúng ExecTxResult.
	for i, txBytes := range req.Txs {
		ethTx, err := processor.DecodeTx(txBytes)
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
	baseFeePerGas, err := app.BaseFeePerGasForBlock(uint64(req.Height))
	if err != nil {
		return nil, fmt.Errorf("calculate block base fee: %w", err)
	}
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
	app.metaMu.RLock()
	prevRoot := append([]byte(nil), app.appHash...)
	app.metaMu.RUnlock()
	rand := derivePrevRandao(req.Height, req.Time, req.ProposerAddress, prevRoot)

	blockEnv := evm.BlockEnv{
		Height: req.Height, Time: uint64(req.Time.Unix()),
		Coinbase: common.BytesToAddress(req.ProposerAddress), GasLimit: blockGasLimit,
		Random: rand, GetHash: getHash, BaseFee: baseFeePerGas, PreviousRoot: prevRoot,
	}
	if app.executor == nil {
		switch app.executorMode {
		case "parallel", "occ":
			var exec *evm.ParallelExecutor
			if app.autoWorkers {
				exec = evm.NewAdaptiveParallelExecutor(chainConfig, cid, 0)
			} else {
				exec = evm.NewParallelExecutor(chainConfig, cid, app.workers)
			}
			exec.SlidingWindow = true
			app.executor = exec
		case "grouped":
			app.executor = evm.NewGroupedExecutor(chainConfig, cid, app.workers)
		case "predicted-occ":
			app.executor = evm.NewPredictedOCCExecutor(chainConfig, cid, app.workers)
		default:
			app.executor = evm.NewSequentialExecutor(chainConfig, cid)
		}
	}
	executionStarted := time.Now()
	// Cả executor tuần tự và song song chỉ tính kết quả/state delta; chúng không tự tạo block.
	blockResult, err := app.executor.ExecuteBlock(stateDB, blockEnv, transactions)
	if err != nil {
		return nil, fmt.Errorf("execute block: %w", err)
	}
	retryCount := blockResult.Metrics["total_retries"]
	if retryCount == 0 {
		retryCount = blockResult.Metrics["retry_total"]
	}
	if retryCount == 0 {
		retryCount = blockResult.Metrics["abort_total"]
	}
	workers := strconv.Itoa(app.workers)
	if app.autoWorkers {
		workers = "auto"
	} else if app.executorMode == "sequential" {
		workers = "1"
	}
	executionMetrics := BlockExecutionMetrics{
		Height:         uint64(req.Height),
		Executor:       app.executorMode,
		Workers:        workers,
		RetryCount:     uint64(retryCount),
		ExecutionNanos: uint64(time.Since(executionStarted).Nanoseconds()),
	}
	if len(blockResult.Metrics) > 0 && app.logger != nil {
		workerMode := fmt.Sprint(app.workers)
		if app.autoWorkers {
			workerMode = "auto"
		}
		app.logger.Info("block executor metrics", "height", req.Height, "executor", app.executorMode, "workers", workerMode, "metrics", blockResult.Metrics)
	}
	// Chuyển kết quả EVM thành định dạng ABCI mà CometBFT dùng cho các giao dịch trong block.
	for i, result := range blockResult.Results {
		if preResults[i] != nil {
			txResults[i] = preResults[i]
			continue
		}
		code := uint32(0)
		if result.Invalid || result.Failed {
			code = 1
		}
		var gasWanted int64
		if transactions[i] != nil {
			gasWanted = int64(transactions[i].Gas())
		}
		txResults[i] = &abci.ExecTxResult{Code: code, Log: result.ErrMsg, GasWanted: gasWanted, GasUsed: int64(result.GasUsed)}
		if app.logger != nil {
			app.logger.Info("EVM executed", "height", req.Height, "txIndex", i, "gasUsed", result.GasUsed, "err", result.ErrMsg)
		}
	}
	if err := app.stageEthereumBlock(req, blockHash, blockResult.StateRoot, baseFeePerGas, transactions, blockResult.Results, executionMetrics, blockResult.Writes); err != nil {
		return nil, fmt.Errorf("stage ethereum block metadata: %w", err)
	}

	// Chuẩn bị trạng thái và siêu dữ liệu. Commit lưu tất cả trong cùng một thao tác.
	pendingAppHash := append([]byte(nil), blockResult.StateRoot[:]...)
	// Giữ writes và AppHash ở pending; chỉ Commit mới ghi chúng thành trạng thái bền vững.
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
	// giữ metaMu.Lock() bọc trọn WriteSync + cập nhật head để đóng cửa sổ torn-read với RPC/NewSnapshot
	// Khóa xuyên suốt WriteSync và cập nhật cache để RPC không thấy chiều cao/AppHash lệch nhau.
	app.metaMu.Lock()
	if err := batch.WriteSync(); err != nil {
		app.metaMu.Unlock()
		return nil, fmt.Errorf("commit finalized block: %w", err)
	}
	if app.afterCommitWriteSyncHook != nil {
		if err := app.afterCommitWriteSyncHook(pending.height, append([]byte(nil), pending.appHash...)); err != nil {
			app.metaMu.Unlock()
			return nil, err
		}
	}
	app.currentHeight = pending.height
	app.appHash = append([]byte(nil), pending.appHash...)
	app.metaMu.Unlock()
	app.pendingBlock = nil

	return &abci.CommitResponse{}, nil
}

// CheckTx kiểm tra giao dịch trước khi đưa vào vùng chờ giao dịch.
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	// CheckTx sàng lọc giao dịch gửi vào mempool; giao dịch vẫn sẽ được kiểm tra khi tạo/kiểm tra block.
	if len(req.Tx) == 0 {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  "empty transaction",
		}, nil
	}
	if len(req.Tx) > maxTransactionSize {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("transaction too large: %d bytes, maximum %d", len(req.Tx), maxTransactionSize),
		}, nil
	}

	processor := app.getTxProcessor()
	// Giải mã giao dịch.
	ethTx, err := processor.DecodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid tx encoding: %v", err),
		}, nil
	}
	if ethTx.Type() == types.BlobTxType || ethTx.Type() == types.SetCodeTxType {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("unsupported tx type %d", ethTx.Type()),
		}, nil
	}

	expectedChainID := app.effectiveChainID()
	if !ethTx.Protected() || ethTx.ChainId().Cmp(expectedChainID) != 0 {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("wrong chain ID: got %s, want %s", ethTx.ChainId(), expectedChainID),
		}, nil
	}

	// Xác minh chữ ký.
	sender, err := processor.RecoverSender(ethTx)
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
	baseFeePerGas, err := app.NextBaseFeePerGas()
	if err != nil {
		return nil, fmt.Errorf("calculate next block base fee: %w", err)
	}
	if ethTx.GasFeeCap().Cmp(baseFeePerGas) < 0 {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("max fee per gas %s is below next block base fee %s", ethTx.GasFeeCap(), baseFeePerGas),
		}, nil
	}
	if ethTx.GasTipCap().Cmp(ethTx.GasFeeCap()) > 0 {
		return &abci.CheckTxResponse{Code: 1, Log: "max priority fee per gas exceeds max fee per gas"}, nil
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
	// Query đọc dữ liệu đã commit từ state; nó không thực thi giao dịch và không ghi thay đổi.
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
