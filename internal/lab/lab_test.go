package lab

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

func testdataPhase1Dir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "phase1")
}

func TestComparatorNegativeDetectsSwappedLogsGasStateAndOrphans(t *testing.T) {
	addrA := common.HexToAddress("0x000000000000000000000000000000000000a001")
	addrB := common.HexToAddress("0x000000000000000000000000000000000000b002")
	slot0 := common.BigToHash(big.NewInt(0))
	txHash := common.HexToHash("0x1234")

	baseLogs := func() []*types.Log {
		return []*types.Log{
			{Address: addrA, Topics: []common.Hash{common.HexToHash("0x01")}, Data: []byte{0x0b}, TxHash: txHash, TxIndex: 0, Index: 0, BlockNumber: 1, BlockTimestamp: 100},
			{Address: addrA, Topics: []common.Hash{common.HexToHash("0x02")}, Data: []byte{0x0c}, TxHash: txHash, TxIndex: 0, Index: 1, BlockNumber: 1, BlockTimestamp: 100},
		}
	}
	makeReceipt := func(gas uint64, logs []*types.Log) *TxReceipt {
		return &TxReceipt{
			Type:              2,
			Status:            1,
			GasUsed:           gas,
			CumulativeGasUsed: gas,
			TxHash:            txHash,
			TransactionIndex:  0,
			Logs:              logs,
		}
	}
	makeState := func(slotVal int64) *evm.LogicalState {
		st := evm.NewLogicalState()
		st.Accounts[addrA] = &evm.LogicalAccount{
			Nonce:   1,
			Balance: uint256.NewInt(100),
			Code:    []byte{0x60, 0x00},
			Storage: map[common.Hash]common.Hash{slot0: common.BigToHash(big.NewInt(slotVal))},
		}
		return st
	}

	gethRes := &T8NBlockResult{
		GasUsed:   50_000,
		Receipts:  []*TxReceipt{makeReceipt(50_000, baseLogs())},
		PostState: makeState(12),
	}
	goodPluto := &BlockResult{
		BlockNumber: 1,
		GasUsed:     50_000,
		Receipts:    []*TxReceipt{makeReceipt(50_000, baseLogs())},
	}
	if rep := CompareBlockWithGeth(1, 1, goodPluto, makeState(12), gethRes); !rep.OK() {
		t.Fatalf("expected identical result to pass, got %v", rep.Error())
	}

	t.Run("swapped log order", func(t *testing.T) {
		logs := baseLogs()
		logs[0], logs[1] = logs[1], logs[0]
		logs[0].Index, logs[1].Index = 0, 1
		bad := &BlockResult{BlockNumber: 1, GasUsed: 50_000, Receipts: []*TxReceipt{makeReceipt(50_000, logs)}}
		rep := CompareBlockWithGeth(1, 1, bad, makeState(12), gethRes)
		if rep.OK() {
			t.Fatal("expected comparator to detect swapped log order")
		}
		if !strings.Contains(rep.Error().Error(), "scope=log") {
			t.Fatalf("expected log mismatch in report, got: %v", rep.Error())
		}
	})

	t.Run("modified gas", func(t *testing.T) {
		bad := &BlockResult{BlockNumber: 1, GasUsed: 50_001, Receipts: []*TxReceipt{makeReceipt(50_001, baseLogs())}}
		rep := CompareBlockWithGeth(1, 1, bad, makeState(12), gethRes)
		if rep.OK() || !strings.Contains(rep.Error().Error(), "field=gasUsed") {
			t.Fatalf("expected gasUsed mismatch, got: %v", rep.Error())
		}
	})

	t.Run("modified storage state", func(t *testing.T) {
		rep := CompareBlockWithGeth(1, 1, goodPluto, makeState(11), gethRes)
		if rep.OK() || !strings.Contains(rep.Error().Error(), "field=storage") {
			t.Fatalf("expected storage mismatch, got: %v", rep.Error())
		}
	})

	t.Run("extra account in pluto post-state", func(t *testing.T) {
		st := makeState(12)
		st.Accounts[addrB] = &evm.LogicalAccount{Nonce: 0, Balance: uint256.NewInt(0), Storage: map[common.Hash]common.Hash{}}
		rep := CompareBlockWithGeth(1, 1, goodPluto, st, gethRes)
		if rep.OK() || !strings.Contains(rep.Error().Error(), "field=account") {
			t.Fatalf("expected extra account mismatch, got: %v", rep.Error())
		}
	})

	t.Run("orphan key in pluto post-state", func(t *testing.T) {
		st := makeState(12)
		st.OrphanKeys = [][]byte{[]byte("storage-orphan")}
		rep := CompareBlockWithGeth(1, 1, goodPluto, st, gethRes)
		if rep.OK() || !strings.Contains(rep.Error().Error(), "field=orphanKey") {
			t.Fatalf("expected orphanKey mismatch, got: %v", rep.Error())
		}
	})

	t.Run("missing receipt", func(t *testing.T) {
		bad := &BlockResult{BlockNumber: 1, GasUsed: 50_000, Receipts: nil}
		rep := CompareBlockWithGeth(1, 1, bad, makeState(12), gethRes)
		if rep.OK() || !strings.Contains(rep.Error().Error(), "field=count") {
			t.Fatalf("expected receipt count mismatch, got: %v", rep.Error())
		}
	})
}

func TestGenSwapPoolsMatchesManifest19Hashes(t *testing.T) {
	srcDir := filepath.Join(testdataPhase1Dir(t), "swap-pools")
	outDir := t.TempDir()
	man, err := GenerateSwapPoolsFixture(srcDir, outDir, DefaultSwapPoolsSeed)
	if err != nil {
		t.Fatalf("GenerateSwapPoolsFixture: %v", err)
	}
	if err := VerifyGeneratedHashesAgainstManifest(man.InputSHA256, filepath.Join(srcDir, "manifest.json")); err != nil {
		t.Fatalf("VerifyGeneratedHashesAgainstManifest: %v", err)
	}
}

func TestGroupedApproveIndependentRunsInParallelAndTipSplits(t *testing.T) {
	c, err := LoadFixtureCase(testdataPhase1Dir(t), "approve-independent")
	if err != nil {
		t.Fatal(err)
	}
	blk := c.Blocks[0]

	db, err := store.NewPebbleDB("grp-approve-par", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	runner, err := NewChainRunner(db, ModeGrouped, 2, true, c.GenesisAlloc)
	if err != nil {
		t.Fatal(err)
	}
	grpExec := runner.Executor.(*evm.GroupedExecutor)

	// điểm đồng bộ bắt buộc 2 tx approve cùng đang chạy đồng thời trong một đợt
	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	bothStarted := make(chan struct{})
	grpExec.Hooks = &evm.GroupedHooks{
		OnAttemptStart: func(ctx context.Context, txIndex, attempt int) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			if inFlight == 2 {
				close(bothStarted)
			}
			mu.Unlock()
			select {
			case <-bothStarted:
			case <-time.After(3 * time.Second):
			}
		},
		OnAttemptBeforeDeliver: func(ctx context.Context, txIndex, attempt int) {
			mu.Lock()
			inFlight--
			mu.Unlock()
		},
	}

	res, _, err := runner.ExecuteBlock(context.Background(), blk.Env, blk.Txs)
	if err != nil {
		t.Fatalf("grouped approve-independent failed: %v", err)
	}
	if maxInFlight < 2 {
		t.Fatalf("expected 2 approve txs running concurrently, observed maxInFlight=%d", maxInFlight)
	}
	if res.GroupedStats == nil || res.GroupedStats.GroupedTxs != 2 || res.GroupedStats.MaxGroupWidth != 2 || res.GroupedStats.AbortedGroups != 0 {
		t.Fatalf("unexpected grouped stats: %+v", res.GroupedStats)
	}
}

func TestPredictedOCCWaitsForHotPoolPredecessorWhileOtherPoolRuns(t *testing.T) {
	c, err := LoadFixtureCase(testdataPhase1Dir(t), "swap-pools")
	if err != nil {
		t.Fatal(err)
	}
	// chọn 3 tx từ block 1: tx0 -> poolA, tx1 -> poolB, tx2 -> poolA
	blk := c.Blocks[0]
	var poolATxs, poolBTxs []SignedTx
	for _, stx := range blk.Txs {
		pool := common.BytesToAddress(stx.Tx.Data()[16:36])
		if pool == evm.DefaultSwapPoolAddrs[0] && len(poolATxs) < 2 {
			poolATxs = append(poolATxs, stx)
		} else if pool == evm.DefaultSwapPoolAddrs[1] && len(poolBTxs) < 1 {
			poolBTxs = append(poolBTxs, stx)
		}
	}
	if len(poolATxs) < 2 || len(poolBTxs) < 1 {
		t.Fatalf("failed to find required poolA/poolB txs")
	}
	miniTxs := []SignedTx{poolATxs[0], poolBTxs[0], poolATxs[1]}

	seqDB, err := store.NewPebbleDB("pred-seq", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer seqDB.Close()
	seqRunner, err := NewChainRunner(seqDB, ModeSequential, 1, false, c.GenesisAlloc)
	if err != nil {
		t.Fatal(err)
	}
	seqRes, _, err := seqRunner.ExecuteBlock(context.Background(), blk.Env, miniTxs)
	if err != nil {
		t.Fatal(err)
	}

	predDB, err := store.NewPebbleDB("pred-wait", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer predDB.Close()
	predRunner, err := NewChainRunner(predDB, ModePredictedOCC, 3, true, c.GenesisAlloc)
	if err != nil {
		t.Fatal(err)
	}
	predExec := predRunner.Executor.(*evm.PredictedOCCExecutor)
	predExec.SetHotPools([]common.Address{evm.DefaultSwapPoolAddrs[0]})

	var mu sync.Mutex
	tx2WaitedOnTx0 := false
	tx1RanBeforeTx2 := false
	tx1Started := false
	tx2Started := false
	predExec.Hooks = &evm.PredictedOCCHooks{
		OnTxWaitPredecessor: func(ctx context.Context, txIndex, predTxIndex int, pool common.Address) {
			mu.Lock()
			if txIndex == 2 && predTxIndex == 0 && pool == evm.DefaultSwapPoolAddrs[0] {
				tx2WaitedOnTx0 = true
			}
			mu.Unlock()
		},
		OnAttemptStart: func(ctx context.Context, txIndex, attempt int) {
			mu.Lock()
			if txIndex == 1 {
				tx1Started = true
				if !tx2Started {
					tx1RanBeforeTx2 = true
				}
			}
			if txIndex == 2 {
				tx2Started = true
			}
			mu.Unlock()
		},
	}

	predRes, _, err := predRunner.ExecuteBlock(context.Background(), blk.Env, miniTxs)
	if err != nil {
		t.Fatal(err)
	}
	if predRes.AppHash != seqRes.AppHash || predRes.GasUsed != seqRes.GasUsed {
		t.Fatalf("predicted-occ diverged from seq: appHash=%s/%s gas=%d/%d", predRes.AppHash.Hex(), seqRes.AppHash.Hex(), predRes.GasUsed, seqRes.GasUsed)
	}
	if !tx2WaitedOnTx0 || !tx1Started || !tx1RanBeforeTx2 {
		t.Fatalf("expected tx2 to wait on tx0 while tx1(poolB) runs: waited=%v tx1Started=%v tx1BeforeTx2=%v", tx2WaitedOnTx0, tx1Started, tx1RanBeforeTx2)
	}
	if predRes.PredictedStats == nil || predRes.PredictedStats.WaitedTxs != 1 || predRes.PredictedStats.RetriedTxs != 0 {
		t.Fatalf("expected waitedTxs=1 retriedTxs=0, got %+v", predRes.PredictedStats)
	}
}

func TestPredictedOCCSharedRecipientDifferentPoolsConflictCaughtByOCC(t *testing.T) {
	c, err := LoadFixtureCase(testdataPhase1Dir(t), "swap-pools")
	if err != nil {
		t.Fatal(err)
	}
	blk := c.Blocks[0]
	chainCfg := ShanghaiChainConfig(DefaultChainID)
	signer := types.MakeSigner(chainCfg, new(big.Int).SetUint64(blk.Env.Number), blk.Env.Timestamp)

	// 2 trader khác nhau swap ở 2 pool khác nhau (poolA và poolB) nhưng cùng recipient -> đụng balance token1 ngoài dự đoán pool
	k1, err := deterministicGenKey(1)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := deterministicGenKey(2)
	if err != nil {
		t.Fatal(err)
	}
	sharedRecipient := common.HexToAddress("0x000000000000000000000000000000000000f00d")

	signSwap := func(k *ecdsa.PrivateKey, pool common.Address, idx int) SignedTx {
		data := genEncodeSwapExactIn(pool, true, 10_000, 1, sharedRecipient)
		tx, err := types.SignNewTx(k, signer, &types.DynamicFeeTx{
			ChainID:   new(big.Int).SetUint64(DefaultChainID),
			Nonce:     0,
			To:        &evm.DefaultSwapRouterAddr,
			Gas:       300_000,
			GasFeeCap: big.NewInt(1),
			GasTipCap: big.NewInt(0),
			Value:     big.NewInt(0),
			Data:      data,
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := tx.MarshalBinary()
		return SignedTx{Index: idx, Tx: tx, Raw: raw, Hash: tx.Hash(), Sender: crypto.PubkeyToAddress(k.PublicKey)}
	}

	txs := []SignedTx{
		signSwap(k1, evm.DefaultSwapPoolAddrs[0], 0),
		signSwap(k2, evm.DefaultSwapPoolAddrs[1], 1),
	}

	seqDB, _ := store.NewPebbleDB("shared-rcpt-seq", t.TempDir())
	defer seqDB.Close()
	seqRunner, err := NewChainRunner(seqDB, ModeSequential, 1, false, c.GenesisAlloc)
	if err != nil {
		t.Fatal(err)
	}
	seqRes, _, err := seqRunner.ExecuteBlock(context.Background(), blk.Env, txs)
	if err != nil {
		t.Fatal(err)
	}

	predDB, _ := store.NewPebbleDB("shared-rcpt-pred", t.TempDir())
	defer predDB.Close()
	predRunner, err := NewChainRunner(predDB, ModePredictedOCC, 2, true, c.GenesisAlloc)
	if err != nil {
		t.Fatal(err)
	}
	predExec := predRunner.Executor.(*evm.PredictedOCCExecutor)
	predExec.SetHotPools([]common.Address{evm.DefaultSwapPoolAddrs[0]})

	// ép cả 2 tx cùng bắt đầu đồng thời trước khi tx 0 chốt để chắc chắn gặp xung đột ngoài dự đoán trên sharedRecipient
	var mu sync.Mutex
	started := 0
	both := make(chan struct{})
	predExec.Hooks = &evm.PredictedOCCHooks{
		OnAttemptStart: func(ctx context.Context, txIndex, attempt int) {
			if attempt != 1 {
				return
			}
			mu.Lock()
			started++
			if started == 2 {
				close(both)
			}
			mu.Unlock()
			select {
			case <-both:
			case <-time.After(3 * time.Second):
			}
		},
	}

	predRes, _, err := predRunner.ExecuteBlock(context.Background(), blk.Env, txs)
	if err != nil {
		t.Fatal(err)
	}
	if predRes.AppHash != seqRes.AppHash || predRes.GasUsed != seqRes.GasUsed {
		t.Fatalf("shared recipient conflict diverged from seq: appHash=%s/%s gas=%d/%d",
			predRes.AppHash.Hex(), seqRes.AppHash.Hex(), predRes.GasUsed, seqRes.GasUsed)
	}
	if predRes.PredictedStats == nil || predRes.PredictedStats.RetriedTxs < 1 {
		t.Fatalf("expected OCC to catch unpredicted shared-recipient conflict and retry, got %+v", predRes.PredictedStats)
	}
}
