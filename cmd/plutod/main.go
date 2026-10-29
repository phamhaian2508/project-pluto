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
	"github.com/phamhaian2508/project-pluto/internal/store"
)

var (
	homeDir       string
	executor      string
	workerSetting string
)

func init() {
	flag.StringVar(&homeDir, "home", "", "Path to the home directory")
	flag.StringVar(&executor, "executor", "sequential", "Block executor: sequential or parallel")
	flag.StringVar(&workerSetting, "workers", "auto", "Parallel executor workers: auto or a positive integer")
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

// Bộ cung cấp cơ sở dữ liệu Pebble.
func PebbleDBProvider(ctx *cfg.DBContext) (dbm.DB, error) {
	return store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
}

func main() {
	flag.Parse()
	workers, autoWorkers, err := parseWorkerSetting(workerSetting)
	if err != nil {
		panic(err)
	}
	if homeDir == "" {
		homeDir = os.ExpandEnv("$HOME/.plutod")
	}

	// Thiết lập cấu hình.
	config := cfg.DefaultConfig()
	config.SetRoot(homeDir)

	// Đảm bảo thư mục tồn tại
	for _, dir := range []string{config.DBDir(), filepath.Dir(config.NodeKeyFile()), filepath.Dir(config.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to create directory %s: %w", dir, err))
		}
	}

	// Thiết lập bộ ghi nhật ký CometBFT.
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
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

	myApp, err := app.NewApp(appDBDir, appLogger)
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
	defer myApp.Close()

	// Tạo ngữ cảnh cho nút.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Khởi tạo Node
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
	ethereumRPC, err := ethrpc.New(myApp, cmtrpclocal.New(node))
	if err != nil {
		panic(fmt.Errorf("failed to create Ethereum JSON-RPC server: %w", err))
	}
	if err := ethereumRPC.Start(plutoconfig.EthereumRPCListenAddr); err != nil {
		panic(fmt.Errorf("failed to start Ethereum JSON-RPC server: %w", err))
	}

	logger.Info("Node Started", "home", homeDir)
	logger.Info("Ethereum JSON-RPC Started", "address", plutoconfig.EthereumRPCListenAddr)

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
