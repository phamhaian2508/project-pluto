package app

import (
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

func TestParallelExecutorAppCommitPersistsStaticTransfers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbPath := t.TempDir()
	application, err := NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.ConfigureExecutor("parallel", 4); err != nil {
		t.Fatal(err)
	}

	keys := make([][]byte, 2)
	senders := make([]common.Address, 2)
	recipients := []common.Address{common.HexToAddress("0x9001"), common.HexToAddress("0x9002")}
	seed := evm.NewPebbleStateDB(application.db)
	for i := range keys {
		key, err := crypto.HexToECDSA([]string{
			"0000000000000000000000000000000000000000000000000000000000000001",
			"0000000000000000000000000000000000000000000000000000000000000002",
		}[i])
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = crypto.FromECDSA(key)
		senders[i] = crypto.PubkeyToAddress(key.PublicKey)
		seed.AddBalanceBig(senders[i], big.NewInt(10))
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	application.appHash = seed.ComputeAppHash()

	txs := make([]*types.Transaction, 2)
	for i := range txs {
		key, err := crypto.ToECDSA(keys[i])
		if err != nil {
			t.Fatal(err)
		}
		tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 21_000, To: &recipients[i], Value: big.NewInt(3)})
		txs[i], err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	response := runBlock(t, application, 1, txs...)
	for i, txResult := range response.TxResults {
		if txResult.Code != 0 || txResult.GasUsed != 21_000 {
			t.Fatalf("parallel tx[%d] result = %+v", i, txResult)
		}
	}
	for _, addr := range senders {
		if got := evm.NewPebbleStateDB(application.db).GetBalance(addr); got.Cmp(uint256.NewInt(7)) != 0 {
			t.Fatalf("sender %s balance = %s, want 7", addr, got)
		}
	}
	for _, addr := range recipients {
		if got := evm.NewPebbleStateDB(application.db).GetBalance(addr); got.Cmp(uint256.NewInt(3)) != 0 {
			t.Fatalf("recipient %s balance = %s, want 3", addr, got)
		}
	}
	if application.currentHeight != 1 {
		t.Fatalf("committed height = %d, want 1", application.currentHeight)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := evm.NewPebbleStateDB(application.db).GetBalance(recipients[0]); got.Cmp(uint256.NewInt(3)) != 0 {
		t.Fatalf("recipient balance after restart = %s, want 3", got)
	}
}
