package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtflags "github.com/cometbft/cometbft/libs/cli/flags"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmtnode "github.com/cometbft/cometbft/node"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmtproxy "github.com/cometbft/cometbft/proxy"
	cmtrpclocal "github.com/cometbft/cometbft/rpc/client/local"

	dbm "github.com/cometbft/cometbft-db"

	"github.com/phamhaian2508/project-pluto/internal/app"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/ethrpc"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/lab"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

var (
	homeDir               string
	executor              string
	workerSetting         string
	logLevel              string
	timeoutCommit         string
	ethRPCAddress         string
	evmChainID            int64
	checkpointFlag        string
	checkpointPolicy      string
	checkpointInterval    uint64
	checkpointBudgetBytes int64
	checkpointModelPath   string
	selectionPath         string
)

func init() {
	flag.StringVar(&homeDir, "home", "", "Path to the home directory")
	flag.StringVar(&executor, "executor", "sequential", "Block executor: sequential, parallel, occ, grouped or predicted-occ")
	flag.StringVar(&workerSetting, "workers", "auto", "Parallel executor workers: auto or a positive integer")
	flag.StringVar(&logLevel, "log-level", "", "Override CometBFT log level (debug, info, error, none)")
	flag.StringVar(&timeoutCommit, "timeout-commit", "", "Override CometBFT timeout_commit (for example 1s)")
	flag.StringVar(&ethRPCAddress, "eth-rpc", plutoconfig.EthereumRPCListenAddr, "Ethereum JSON-RPC listen address")
	flag.Int64Var(&evmChainID, "chain-id", plutoconfig.EVMChainID, "EVM chain ID (default 31337; use 1337 for Phase 4 verification)")
	flag.StringVar(&checkpointFlag, "checkpoint", "", "Checkpoint policy: off, fixed, history, adaptive (overrides --checkpoint-policy)")
	flag.StringVar(&checkpointPolicy, "checkpoint-policy", "p0", "Legacy alias for checkpoint policy: p0, p1, p2, p3, off, fixed, history or adaptive")
	flag.Uint64Var(&checkpointInterval, "checkpoint-interval", 0, "Opcode interval for checkpoint policy fixed/p2 (0 = use model.CheckpointEvery for adaptive, or 100 default)")
	flag.Int64Var(&checkpointBudgetBytes, "checkpoint-budget-bytes", -1, "Hard RAM budget in bytes for checkpoint store (-1 = unset, 0 = behaves like off)")
	flag.StringVar(&checkpointModelPath, "checkpoint-model", "", "Path to calibrated Phase 3 model.json (required when --checkpoint=adaptive)")
	flag.StringVar(&selectionPath, "selection", "", "Optional path to Phase 4 selection.json to load winning executor/checkpoint config")
}

func parseWorkerSetting(setting string) (int, bool, error) {
	setting = strings.TrimSpace(setting)
	if strings.EqualFold(setting, "auto") {
		return 0, true, nil
	}
	workers, err := strconv.Atoi(setting)
	if err != nil || workers < 1 {
		return 0, false, fmt.Errorf("workers must be 'auto' or a positive integer, got %q", setting)
	}
	return workers, false, nil
}

func parseCheckpointSetting(policy string, interval uint64) (evm.CheckpointConfig, error) {
	return parseCheckpointConfig(policy, interval, -1)
}

func parseCheckpointConfig(policy string, interval uint64, budgetBytes int64) (evm.CheckpointConfig, error) {
	return parseCheckpointConfigWithModel(policy, interval, budgetBytes, "")
}

func parseCheckpointConfigWithModel(policy string, interval uint64, budgetBytes int64, modelPath string) (evm.CheckpointConfig, error) {
	trimmed := strings.TrimSpace(policy)
	parsed, err := evm.ParseCheckpointPolicy(trimmed)
	if err != nil {
		return evm.CheckpointConfig{}, err
	}
	config := evm.CheckpointConfig{Policy: parsed}
	if parsed == evm.CheckpointPolicyP2 || parsed == evm.CheckpointPolicyP3 || parsed == evm.CheckpointPolicyP4 {
		config.Interval = interval
	}
	if budgetBytes >= 0 {
		config.UseBudget = true
		config.BudgetBytes = uint64(budgetBytes)
	}
	if parsed == evm.CheckpointPolicyAdaptive {
		if strings.TrimSpace(modelPath) == "" {
			return evm.CheckpointConfig{}, fmt.Errorf("checkpoint policy %q requires --checkpoint-model <path/to/model.json>", trimmed)
		}
		model, err := evm.LoadAdaptiveModel(modelPath)
		if err != nil {
			return evm.CheckpointConfig{}, fmt.Errorf("load --checkpoint-model %q: %w", modelPath, err)
		}
		config.AdaptiveModel = model
		if !config.UseBudget && model.TightBudgetBytes > 0 {
			config.UseBudget = true
			config.BudgetBytes = model.TightBudgetBytes
		}
		if config.Interval == 0 && model.CheckpointEvery > 0 {
			config.Interval = model.CheckpointEvery
		}
	}
	if err := config.Validate(); err != nil {
		return evm.CheckpointConfig{}, err
	}
	return config, nil
}

func resolveCheckpointFlag(checkpoint, legacyPolicy string) string {
	if strings.TrimSpace(checkpoint) != "" {
		return checkpoint
	}
	return legacyPolicy
}

// Bộ cung cấp cơ sở dữ liệu Pebble.
func PebbleDBProvider(ctx *cfg.DBContext) (dbm.DB, error) {
	return store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
}

func main() {
	flag.Parse()
	intervalExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "checkpoint-interval" {
			intervalExplicit = true
		}
	})
	if strings.TrimSpace(selectionPath) != "" {
		sel, err := lab.LoadPhase4Selection(selectionPath)
		if err != nil {
			panic(err)
		}
		executor = sel.Mode
		workerSetting = strconv.Itoa(sel.Workers)
		checkpointFlag = sel.CheckpointPolicy
		if sel.CheckpointInterval > 0 {
			checkpointInterval = sel.CheckpointInterval
			intervalExplicit = true
		}
		if sel.BudgetBytes > 0 && sel.CheckpointPolicy != lab.CheckpointOff {
			checkpointBudgetBytes = int64(sel.BudgetBytes)
		}
		if sel.ModelPath != "" && strings.TrimSpace(checkpointModelPath) == "" {
			checkpointModelPath = sel.ModelPath
		}
	}
	workers, autoWorkers, err := parseWorkerSetting(workerSetting)
	if err != nil {
		panic(err)
	}
	effectivePolicy := resolveCheckpointFlag(checkpointFlag, checkpointPolicy)
	if !intervalExplicit && checkpointInterval == 0 {
		if p, parseErr := evm.ParseCheckpointPolicy(effectivePolicy); parseErr == nil && p != evm.CheckpointPolicyAdaptive {
			checkpointInterval = 100
		}
	}
	checkpointConfig, err := parseCheckpointConfigWithModel(effectivePolicy, checkpointInterval, checkpointBudgetBytes, checkpointModelPath)
	if err != nil {
		panic(err)
	}
	modelSHA := ""
	if checkpointConfig.AdaptiveModel != nil {
		modelSHA = checkpointConfig.AdaptiveModel.SHA256()
	}
	if evmChainID <= 0 {
		panic(fmt.Errorf("chain-id must be positive, got %d", evmChainID))
	}
	if homeDir == "" {
		homeDir = os.ExpandEnv("$HOME/.plutod")
	}

	// Đọc cấu hình đã lưu trong config.toml; cờ dòng lệnh chỉ ghi đè giá trị được nêu.
	config, err := plutoconfig.LoadCometConfig(homeDir, logLevel, timeoutCommit)
	if err != nil {
		panic(err)
	}

	// Đảm bảo thư mục tồn tại
	for _, dir := range []string{config.DBDir(), filepath.Dir(config.NodeKeyFile()), filepath.Dir(config.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to create directory %s: %w", dir, err))
		}
	}

	// Thiết lập bộ ghi nhật ký CometBFT.
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
	logger, err = cmtflags.ParseLogLevel(config.LogLevel, logger, "info")
	if err != nil {
		panic(fmt.Errorf("parse log level %q: %w", config.LogLevel, err))
	}
	logger = logger.With("module", "main")

	// Thiết lập bộ ghi nhật ký cho ứng dụng (Slog).
	appLogger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Tải khóa nút và khóa validator.
	nodeKey, err := cmtp2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		panic(fmt.Errorf("failed to load node key: %w", err))
	}

	// Tải hoặc tạo khóa validator (CometBFT v1.0.0).
	pv, err := cmtprivval.LoadOrGenFilePV(
		config.PrivValidatorKeyFile(),
		config.PrivValidatorStateFile(),
		func() (crypto.PrivKey, error) {
			return ed25519.GenPrivKey(), nil
		},
	)
	if err != nil {
		panic(fmt.Errorf("failed to load or generate priv validator: %w", err))
	}

	// Khởi tạo App (DB nằm trong data/)
	appDBDir := filepath.Join(homeDir, "data")

	myApp, err := app.NewAppWithChainID(appDBDir, appLogger, evmChainID)
	if err != nil {
		panic(fmt.Errorf("failed to create app: %w", err))
	}
	if autoWorkers {
		err = myApp.ConfigureAdaptiveExecutor(executor)
	} else {
		err = myApp.ConfigureExecutor(executor, workers)
	}
	if err != nil {
		panic(fmt.Errorf("configure block executor: %w", err))
	}
	if err := myApp.ConfigureCheckpoints(checkpointConfig); err != nil {
		panic(fmt.Errorf("configure checkpoints: %w", err))
	}
	defer myApp.Close()

	// Tạo ngữ cảnh cho nút.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Nối CometBFT với ứng dụng Pluto qua ABCI:
	// NewLocalClientCreator(myApp) làm cho CometBFT gọi các phương thức ABCI trên myApp
	// như CheckTx, PrepareProposal, ProcessProposal, FinalizeBlock và Commit.
	node, err := cmtnode.NewNode(
		ctx,
		config,
		pv,
		nodeKey,
		cmtproxy.NewLocalClientCreator(myApp),
		cmtnode.DefaultGenesisDocProviderFunc(config),
		PebbleDBProvider,
		cmtnode.DefaultMetricsProvider(config.Instrumentation),
		logger,
	)

	if err != nil {
		panic(fmt.Errorf("failed to create node: %v", err))
	}

	// Khởi chạy nút.
	if err := node.Start(); err != nil {
		panic(fmt.Errorf("failed to start node: %v", err))
	}
	// RPC Ethereum đọc trạng thái từ myApp; khi gửi giao dịch, broadcaster này
	// chuyển raw transaction vào chính CometBFT node thay vì tự thực thi giao dịch.
	ethereumRPC, err := ethrpc.New(myApp, cmtrpclocal.New(node))
	if err != nil {
		panic(fmt.Errorf("failed to create Ethereum JSON-RPC server: %w", err))
	}
	if err := ethereumRPC.Start(ethRPCAddress); err != nil {
		panic(fmt.Errorf("failed to start Ethereum JSON-RPC server: %w", err))
	}

	logger.Info("Node Started",
		"home", homeDir,
		"chain_id", evmChainID,
		"executor", executor,
		"workers", workers,
		"auto_workers", autoWorkers,
		"checkpoint_policy", checkpointConfig.Policy.Phase2Name(),
		"checkpoint_interval", checkpointConfig.Interval,
		"checkpoint_use_budget", checkpointConfig.UseBudget,
		"checkpoint_budget_bytes", checkpointConfig.BudgetBytes,
		"checkpoint_model_sha256", modelSHA,
		"selection", selectionPath,
	)
	logger.Info("Ethereum JSON-RPC Started", "address", ethRPCAddress)

	// Chờ tín hiệu shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Info("Stopping Node...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := ethereumRPC.Close(shutdownCtx); err != nil {
		logger.Error("Failed to stop Ethereum JSON-RPC", "err", err)
	}
	shutdownCancel()
	cancel() // Dừng nút.
	node.Stop()
	node.Wait()
}
