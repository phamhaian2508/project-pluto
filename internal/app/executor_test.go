package app

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/evm"
)

func testApp(t *testing.T) (*App, common.Address, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := NewApp(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	from := crypto.PubkeyToAddress(key.PublicKey)
	state := evm.NewPebbleStateDB(app.db)
	state.AddBalanceBig(from, new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18)))
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	app.appHash = state.ComputeAppHash()
	return app, from, key
}

func signedTx(t *testing.T, keyHex string, nonce, gas uint64, to *common.Address, value *big.Int, data []byte) *types.Transaction {
	t.Helper()
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int), Gas: gas, To: to, Value: value, Data: data})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func runBlock(t *testing.T, app *App, height int64, txs ...*types.Transaction) *abci.FinalizeBlockResponse {
	t.Helper()
	encoded := make([][]byte, len(txs))
	for i, tx := range txs {
		encoded[i], _ = tx.MarshalBinary()
	}
	resp, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height, Time: time.Unix(height, 0), Hash: []byte{byte(height)}, Txs: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFinalizeBlockStagesStateUntilAtomicCommit(t *testing.T) {
	app, from, key := testApp(t)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: new(big.Int),
		Gas:      21000,
		To:       &to,
		Value:    big.NewInt(5),
	})
	tx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	previousHash := append([]byte(nil), app.appHash...)
	resp, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1,
		Time:   time.Unix(1, 0),
		Hash:   []byte{1},
		Txs:    [][]byte{encoded},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TxResults[0].Code != 0 {
		t.Fatalf("transaction failed: %s", resp.TxResults[0].Log)
	}
	if app.currentHeight != 0 || common.BytesToHash(app.appHash) != common.BytesToHash(previousHash) {
		t.Fatalf("committed metadata changed during FinalizeBlock: height=%d appHash=%x", app.currentHeight, app.appHash)
	}
	state := evm.NewPebbleStateDB(app.db)
	if got := state.GetBalance(to).ToBig(); got.Sign() != 0 {
		t.Fatalf("recipient balance before Commit = %s, want 0", got)
	}
	if got := state.GetNonce(from); got != 0 {
		t.Fatalf("sender nonce before Commit = %d, want 0", got)
	}
	if height, err := app.db.Get([]byte("height")); err != nil || len(height) != 0 {
		t.Fatalf("height persisted before Commit: value=%q err=%v", height, err)
	}
	if hash, err := app.db.Get([]byte("appHash")); err != nil || len(hash) != 0 {
		t.Fatalf("app hash persisted before Commit: value=%x err=%v", hash, err)
	}
	if hash, err := app.db.Get(blockHashKey(1)); err != nil || len(hash) != 0 {
		t.Fatalf("block hash persisted before Commit: value=%x err=%v", hash, err)
	}

	if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetBalance(to).ToBig(); got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("recipient balance after Commit = %s, want 5", got)
	}
	if got := state.GetNonce(from); got != 1 {
		t.Fatalf("sender nonce after Commit = %d, want 1", got)
	}
	if app.currentHeight != 1 {
		t.Fatalf("current height after Commit = %d, want 1", app.currentHeight)
	}
	if string(app.appHash) != string(resp.AppHash) {
		t.Fatalf("committed app hash = %x, response app hash = %x", app.appHash, resp.AppHash)
	}
	if hash, err := app.db.Get([]byte("appHash")); err != nil || string(hash) != string(resp.AppHash) {
		t.Fatalf("persisted app hash = %x, response app hash = %x, err=%v", hash, resp.AppHash, err)
	}
	if hash, err := app.db.Get(blockHashKey(1)); err != nil || common.BytesToHash(hash) != common.BytesToHash([]byte{1}) {
		t.Fatalf("block hash after Commit: value=%x err=%v", hash, err)
	}
}

func TestCommitStoresEthereumBlockTransactionReceiptLogsAndLocation(t *testing.T) {
	app, from, key := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000cafe")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	state.SetCode(contract, common.FromHex("0x60006000a000"), 0) // LOG0 then STOP
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: new(big.Int),
		Gas:      100000,
		To:       &contract,
	})
	tx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	blockHash := common.BytesToHash([]byte{0xab})
	resp, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height:          1,
		Time:            time.Unix(1234, 0),
		Hash:            blockHash.Bytes(),
		ProposerAddress: common.HexToAddress("0x000000000000000000000000000000000000beef").Bytes(),
		Txs:             [][]byte{raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TxResults[0].Code != 0 {
		t.Fatalf("contract call failed: %s", resp.TxResults[0].Log)
	}

	for name, storageKey := range map[string][]byte{
		"block":       ethereumBlockKey(1),
		"transaction": ethereumTransactionKey(tx.Hash()),
		"receipt":     ethereumReceiptKey(tx.Hash()),
		"location":    ethereumTransactionLocationKey(tx.Hash()),
	} {
		if value, err := app.db.Get(storageKey); err != nil || len(value) != 0 {
			t.Fatalf("%s persisted before Commit: value=%x err=%v", name, value, err)
		}
	}

	if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	block, found, err := app.EthereumBlockByNumber(1)
	if err != nil || !found {
		t.Fatalf("load block: found=%v err=%v", found, err)
	}
	if block.Hash != blockHash || block.Number != 1 || block.Timestamp != 1234 {
		t.Fatalf("unexpected block metadata: %+v", block)
	}
	if len(block.Transactions) != 1 || block.Transactions[0] != tx.Hash() {
		t.Fatalf("block transactions = %v, want [%s]", block.Transactions, tx.Hash())
	}
	if block.GasUsed != uint64(resp.TxResults[0].GasUsed) || block.StateRoot != common.BytesToHash(resp.AppHash) {
		t.Fatalf("block execution metadata: gasUsed=%d stateRoot=%s", block.GasUsed, block.StateRoot)
	}
	if block.LogsBloom == (types.Bloom{}) {
		t.Fatal("block logs bloom is empty")
	}
	byHash, found, err := app.EthereumBlockByHash(blockHash)
	if err != nil || !found || byHash.Number != 1 {
		t.Fatalf("load block by hash: block=%+v found=%v err=%v", byHash, found, err)
	}

	storedTx, found, err := app.EthereumTransactionByHash(tx.Hash())
	if err != nil || !found {
		t.Fatalf("load transaction: found=%v err=%v", found, err)
	}
	var decoded types.Transaction
	if err := decoded.UnmarshalBinary(storedTx.Raw); err != nil {
		t.Fatal(err)
	}
	if decoded.Hash() != tx.Hash() || storedTx.BlockHash != blockHash || storedTx.TransactionIndex != 0 {
		t.Fatalf("unexpected stored transaction: %+v", storedTx)
	}

	receipt, found, err := app.EthereumReceiptByHash(tx.Hash())
	if err != nil || !found {
		t.Fatalf("load receipt: found=%v err=%v", found, err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful || receipt.GasUsed == 0 || receipt.CumulativeGasUsed != receipt.GasUsed {
		t.Fatalf("unexpected receipt execution result: %+v", receipt)
	}
	if receipt.From != from || receipt.To == nil || *receipt.To != contract {
		t.Fatalf("unexpected receipt addresses: from=%s to=%v", receipt.From, receipt.To)
	}
	if len(receipt.Logs) != 1 {
		t.Fatalf("receipt logs = %d, want 1", len(receipt.Logs))
	}
	if receipt.LogsBloom == (types.Bloom{}) {
		t.Fatal("receipt logs bloom is empty")
	}
	if receipt.Logs[0].Address != contract || receipt.Logs[0].TxHash != tx.Hash() || receipt.Logs[0].BlockHash != blockHash {
		t.Fatalf("unexpected stored log: %+v", receipt.Logs[0])
	}

	location, found, err := app.EthereumTransactionLocationByHash(tx.Hash())
	if err != nil || !found {
		t.Fatalf("load transaction location: found=%v err=%v", found, err)
	}
	if location.BlockHash != blockHash || location.BlockNumber != 1 || location.TransactionIndex != 0 {
		t.Fatalf("unexpected transaction location: %+v", location)
	}
}

func TestSequentialExecutorTransferAndNonceValidation(t *testing.T) {
	app, from, _ := testApp(t)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	wrongNonce := signedTx(t, "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80", 1, 21000, &to, big.NewInt(9), nil)
	transfer := signedTx(t, "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80", 0, 21000, &to, big.NewInt(5), nil)
	resp := runBlock(t, app, 1, wrongNonce, transfer)
	if resp.TxResults[0].Code == 0 {
		t.Fatal("wrong nonce transaction unexpectedly succeeded")
	}
	if resp.TxResults[1].Code != 0 {
		t.Fatalf("valid transfer failed: %s", resp.TxResults[1].Log)
	}
	if got := resp.TxResults[1].GasUsed; got != 21000 {
		t.Fatalf("transfer gasUsed = %d, want 21000", got)
	}
	failedReceipt, found, err := app.EthereumReceiptByHash(wrongNonce.Hash())
	if err != nil || !found || failedReceipt.Status != types.ReceiptStatusFailed {
		t.Fatalf("failed transaction receipt: receipt=%+v found=%v err=%v", failedReceipt, found, err)
	}
	successReceipt, found, err := app.EthereumReceiptByHash(transfer.Hash())
	if err != nil || !found || successReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("successful transaction receipt: receipt=%+v found=%v err=%v", successReceipt, found, err)
	}
	state := evm.NewPebbleStateDB(app.db)
	if got := state.GetNonce(from); got != 1 {
		t.Fatalf("sender nonce = %d, want 1", got)
	}
	if got := state.GetBalance(to).ToBig(); got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("recipient balance = %s, want 5", got)
	}
}

func TestSequentialExecutorInsufficientBalance(t *testing.T) {
	app, _, _ := testApp(t)
	poorKey, _ := crypto.GenerateKey()
	poorAddr := crypto.PubkeyToAddress(poorKey.PublicKey)
	state := evm.NewPebbleStateDB(app.db)
	state.AddBalanceBig(poorAddr, big.NewInt(1))
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	keyHex := hex.EncodeToString(crypto.FromECDSA(poorKey))
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	tx := signedTx(t, keyHex, 0, 21000, &to, big.NewInt(2), nil)
	resp := runBlock(t, app, 1, tx)
	if resp.TxResults[0].Code == 0 {
		t.Fatal("underfunded transaction unexpectedly succeeded")
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetNonce(poorAddr); got != 0 {
		t.Fatalf("invalid transaction changed nonce to %d", got)
	}
	if got := state.GetBalance(poorAddr).ToBig(); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("invalid transaction changed balance to %s", got)
	}
}

func TestSequentialExecutorOutOfGasAndRevert(t *testing.T) {
	app, _, key := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000cafe")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	// Infinite jump exhausts the call's gas; the second runtime writes slot 0
	// and reverts, proving reverted storage does not escape the EVM frame.
	state.SetCode(contract, common.FromHex("0x600056"), 0)
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	call := func(nonce uint64, gas uint64) *types.Transaction {
		tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int), Gas: gas, To: &contract})
		signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	resp := runBlock(t, app, 1, call(0, 50000))
	if resp.TxResults[0].Code == 0 || uint64(resp.TxResults[0].GasUsed) != 50000 {
		t.Fatalf("out-of-gas result = %+v", resp.TxResults[0])
	}
	state = evm.NewPebbleStateDB(app.db)
	state.SetCode(contract, common.FromHex("0x600160005560006000fd"), 0)
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	resp = runBlock(t, app, 2, call(1, 100000))
	if resp.TxResults[0].Code == 0 {
		t.Fatal("reverting contract call unexpectedly succeeded")
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetState(contract, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("reverted SSTORE persisted: %s", got)
	}
}

func TestSequentialExecutorContractCreationAndShanghai(t *testing.T) {
	app, _, key := testApp(t)
	from := crypto.PubkeyToAddress(key.PublicKey)
	// Constructor returns PUSH0/PUSH0/REVERT, proving Shanghai opcodes are enabled.
	initCode := common.FromHex("0x6003600c60003960036000f35f5ffd")
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 100000, Data: initCode})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	resp := runBlock(t, app, 1, signed)
	if resp.TxResults[0].Code != 0 {
		t.Fatalf("contract creation failed: %s", resp.TxResults[0].Log)
	}
	addr := crypto.CreateAddress(from, 0)
	state := evm.NewPebbleStateDB(app.db)
	if got := state.GetCode(addr); hex.EncodeToString(got) != "5f5ffd" {
		t.Fatalf("deployed runtime code = %x, want 5f5ffd", got)
	}
	if got := state.GetNonce(addr); got != 1 {
		t.Fatalf("new contract nonce = %d, want 1", got)
	}
}

func TestSequentialExecutorNestedContractCall(t *testing.T) {
	app, _, key := testApp(t)
	child := common.HexToAddress("0x000000000000000000000000000000000000c001")
	parent := common.HexToAddress("0x000000000000000000000000000000000000c002")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(child)
	state.SetCode(child, common.FromHex("0x00"), 0) // STOP
	state.CreateAccount(parent)
	parentCode := append(common.FromHex("0x6000600060006000600073"), child.Bytes()...)
	parentCode = append(parentCode, common.FromHex("0x61fffff15000")...)
	state.SetCode(parent, parentCode, 0)
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 200000, To: &parent})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	resp := runBlock(t, app, 1, signed)
	if resp.TxResults[0].Code != 0 {
		t.Fatalf("nested call failed: %s", resp.TxResults[0].Log)
	}
	if resp.TxResults[0].GasUsed == 0 {
		t.Fatal("nested call used no gas")
	}
}

func TestSequentialExecutorSSTORERefundDoesNotLeakBetweenTransactions(t *testing.T) {
	app, _, key := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000cafe")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	state.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(1)))
	state.SetCode(contract, common.FromHex("0x600060005500"), 0) // clear slot 0
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	clear := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 100000, To: &contract})
	clear, err := types.SignTx(clear, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	first := runBlock(t, app, 1, clear).TxResults[0]
	if first.Code != 0 || first.GasUsed == 0 || first.GasUsed >= 100000 {
		t.Fatalf("SSTORE clear result: %+v", first)
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetState(contract, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("cleared slot = %s", got)
	}
	state.SetCode(contract, common.FromHex("0x600260005500"), 0) // set slot 0 to 2
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	set := types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: new(big.Int), Gas: 100000, To: &contract})
	set, err = types.SignTx(set, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	second := runBlock(t, app, 2, set).TxResults[0]
	if second.Code != 0 || second.GasUsed <= first.GasUsed {
		t.Fatalf("refund leaked into next tx: clear gas=%d, set result=%+v", first.GasUsed, second)
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetState(contract, common.Hash{}); got != common.BigToHash(big.NewInt(2)) {
		t.Fatalf("SSTORE set slot = %s, want 2", got)
	}
}

func TestCounterStorageAcrossThreeCallsAndRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbPath := t.TempDir()
	app, err := NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	state := evm.NewPebbleStateDB(app.db)
	state.AddBalanceBig(from, new(big.Int).Mul(big.NewInt(1), big.NewInt(1e18)))
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	app.appHash = state.ComputeAppHash()
	creationBytecode, err := os.ReadFile("../../contracts/Counter.bin")
	if err != nil {
		t.Fatal(err)
	}
	creation, err := hex.DecodeString(strings.TrimSpace(string(creationBytecode)))
	if err != nil {
		t.Fatal(err)
	}
	deploy := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 3000000, Data: creation})
	deploy, err = types.SignTx(deploy, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	contract := crypto.CreateAddress(from, 0)
	txs := make([]*types.Transaction, 4)
	txs[0] = deploy
	for i := 1; i < len(txs); i++ {
		txs[i] = types.NewTx(&types.LegacyTx{Nonce: uint64(i), GasPrice: new(big.Int), Gas: 100000, To: &contract, Data: common.FromHex("0xd09de08a")})
		txs[i], err = types.SignTx(txs[i], types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	resp := runBlock(t, app, 1, txs...)
	for i, result := range resp.TxResults {
		if result.Code != 0 {
			t.Fatalf("increment tx %d failed: %s", i, result.Log)
		}
	}
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetState(contract, common.Hash{}); got != common.BigToHash(big.NewInt(3)) {
		t.Fatalf("counter storage = %s, want 3", got)
	}
	if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	app, err = NewApp(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	state = evm.NewPebbleStateDB(app.db)
	if got := state.GetState(contract, common.Hash{}); got != common.BigToHash(big.NewInt(3)) {
		t.Fatalf("counter storage after restart = %s, want 3", got)
	}
	if got := app.currentHeight; got != 1 {
		t.Fatalf("restored height = %d, want 1", got)
	}
}

func TestStateRootIncludesStorageChanges(t *testing.T) {
	app, _, _ := testApp(t)
	state := evm.NewPebbleStateDB(app.db)
	contract := common.HexToAddress("0x000000000000000000000000000000000000cafe")
	before := state.ComputeAppHash(app.appHash)
	state.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(7)))
	after := state.ComputeAppHash(app.appHash)
	if string(before) == string(after) {
		t.Fatal("state root did not change for a storage-only update")
	}
	if got := len(after); got != 32 {
		t.Fatalf("state root length = %d, want 32", got)
	}
}

func TestSequentialExecutorUsesDeterministicBlockContext(t *testing.T) {
	app, _, key := testApp(t)
	runBlock(t, app, 1) // stores this block hash for a later BLOCKHASH opcode
	ancestor := common.BytesToHash([]byte{0xa1, 0xb2, 0xc3})
	if err := app.db.Set(blockHashKey(1), ancestor.Bytes()); err != nil {
		t.Fatal(err)
	}
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000bEEF")
	random := common.HexToHash("0xfeed")
	cases := []struct {
		address common.Address
		code    string
		want    common.Hash
	}{
		{common.HexToAddress("0x1001"), "0x6001405f5260205ff3", ancestor},                         // BLOCKHASH(1)
		{common.HexToAddress("0x1002"), "0x415f5260205ff3", common.BytesToHash(coinbase.Bytes())}, // COINBASE
		{common.HexToAddress("0x1003"), "0x445f5260205ff3", random},                               // PREVRANDAO
	}
	state := evm.NewPebbleStateDB(app.db)
	txs := make([]*types.Transaction, len(cases))
	for i, tc := range cases {
		state.CreateAccount(tc.address)
		state.SetCode(tc.address, common.FromHex(tc.code), 0)
		txs[i] = types.NewTx(&types.LegacyTx{Nonce: uint64(i), GasPrice: new(big.Int), Gas: 100000, To: &tc.address})
		var err error
		txs[i], err = types.SignTx(txs[i], types.LatestSignerForChainID(chainID), key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	env := evm.BlockEnv{
		Height: 2, Time: 2, Coinbase: coinbase, GasLimit: blockGasLimit,
		Random: random, PreviousRoot: app.appHash, BaseFee: new(big.Int),
		GetHash: func(height uint64) common.Hash {
			if height == 1 {
				return ancestor
			}
			return common.Hash{}
		},
	}
	result, err := evm.NewSequentialExecutor(newChainConfig(), chainID).ExecuteBlock(state, env, txs)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range result.Results {
		if got.Invalid || got.Failed {
			t.Fatalf("context tx %d failed: %s", i, got.ErrMsg)
		}
		if len(got.ReturnData) != common.HashLength || common.BytesToHash(got.ReturnData) != cases[i].want {
			t.Fatalf("context tx %d returned %x, want %s", i, got.ReturnData, cases[i].want)
		}
	}
}
