package evm

import (
	"context"
	"crypto/sha256"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/phamhaian2508/project-pluto/internal/store"
)

type failingStateKV struct {
	err error
}

func (f failingStateKV) Get([]byte) ([]byte, error)            { return nil, f.err }
func (f failingStateKV) Has([]byte) (bool, error)              { return false, f.err }
func (f failingStateKV) NewBatch() dbm.Batch                   { return nil }
func (f failingStateKV) Iterator([]byte, []byte) (dbm.Iterator, error) { return nil, f.err }

func TestPebbleStateDBPropagatesDBReadErrors(t *testing.T) {
	ioErr := errors.New("simulated pebble disk I/O error")
	addr := common.HexToAddress("0x1111")
	slot := common.BigToHash(big.NewInt(1))

	newFailingState := func() *PebbleStateDB {
		s := NewPebbleStateDB(nil)
		s.kv = failingStateKV{err: ioErr}
		return s
	}

	t.Run("GetBalance records error and fails PendingWrites Commit Export", func(t *testing.T) {
		s := newFailingState()
		_ = s.GetBalance(addr)
		if !errors.Is(s.Err(), ioErr) {
			t.Fatalf("expected %v after GetBalance, got %v", ioErr, s.Err())
		}
		if _, err := s.PendingWrites(); !errors.Is(err, ioErr) {
			t.Fatalf("expected PendingWrites to propagate %v, got %v", ioErr, err)
		}
		if err := s.Commit(); !errors.Is(err, ioErr) {
			t.Fatalf("expected Commit to propagate %v, got %v", ioErr, err)
		}
		if _, err := s.ExportLogicalState(); !errors.Is(err, ioErr) {
			t.Fatalf("expected ExportLogicalState to propagate %v, got %v", ioErr, err)
		}
	})

	t.Run("GetCode and GetState and overlay record db errors", func(t *testing.T) {
		s1 := newFailingState()
		_ = s1.GetCode(addr)
		if !errors.Is(s1.Err(), ioErr) {
			t.Fatalf("expected %v after GetCode, got %v", ioErr, s1.Err())
		}
		s2 := newFailingState()
		_ = s2.GetState(addr, slot)
		if !errors.Is(s2.Err(), ioErr) {
			t.Fatalf("expected %v after GetState, got %v", ioErr, s2.Err())
		}
		// kiểm tra lỗi truyền qua overlayBase -> child overlay
		ov := newFailingState().NewOverlay()
		_ = ov.GetState(addr, slot)
		if !errors.Is(ov.Err(), ioErr) {
			t.Fatalf("expected %v via overlay GetState, got %v", ioErr, ov.Err())
		}
	})

	t.Run("SequentialExecutor fails on db read error", func(t *testing.T) {
		chainID := big.NewInt(1337)
		cfg := overlayTestChainConfig(chainID)
		digest := sha256.Sum256([]byte("db-err-sender"))
		key, _ := crypto.ToECDSA(digest[:])
		tx, err := types.SignNewTx(key, types.MakeSigner(cfg, big.NewInt(1), 100), &types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     0,
			To:        &addr,
			Gas:       21_000,
			GasFeeCap: big.NewInt(1),
			GasTipCap: big.NewInt(0),
			Value:     big.NewInt(0),
		})
		if err != nil {
			t.Fatal(err)
		}
		s := newFailingState()
		env := BlockEnv{Height: 1, Time: 100, Coinbase: common.HexToAddress("0xfee1"), GasLimit: 1_000_000, BaseFee: big.NewInt(1)}
		if _, err := NewSequentialExecutor(cfg, chainID).ExecuteBlock(s, env, []*types.Transaction{tx}); !errors.Is(err, ioErr) {
			t.Fatalf("expected SequentialExecutor to return %v, got %v", ioErr, err)
		}
	})
}

func TestShanghaiFinaliseEmptyAccountCoinbaseAndSelfDestructExport(t *testing.T) {
	db, err := store.NewPebbleDB("shanghai-state", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	absentCB := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	emptyCB := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	nonEmptyCB := common.HexToAddress("0x00000000000000000000000000000000000000cc")
	destructAddr := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	storageSlot := common.BigToHash(big.NewInt(7))

	genesis := NewLogicalState()
	genesis.Accounts[emptyCB] = &LogicalAccount{Nonce: 0, Balance: uint256.NewInt(0), Storage: map[common.Hash]common.Hash{}}
	genesis.Accounts[nonEmptyCB] = &LogicalAccount{Nonce: 0, Balance: uint256.NewInt(1), Storage: map[common.Hash]common.Hash{}}
	genesis.Accounts[destructAddr] = &LogicalAccount{
		Nonce:   1,
		Balance: uint256.NewInt(50),
		Code:    []byte{0x60, 0x00},
		Storage: map[common.Hash]common.Hash{storageSlot: common.BigToHash(big.NewInt(42))},
	}

	seed := NewPebbleStateDB(db)
	if err := seed.SeedLogicalState(genesis); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	s := NewPebbleStateDB(db)
	if got := s.GetStorageRoot(destructAddr); got == (common.Hash{}) || got == types.EmptyRootHash {
		t.Fatalf("expected non-empty storage root sentinel for %s, got %s", destructAddr.Hex(), got.Hex())
	}
	if got := s.GetStorageRoot(nonEmptyCB); got != types.EmptyRootHash {
		t.Fatalf("expected EmptyRootHash for EOA %s, got %s", nonEmptyCB.Hex(), got.Hex())
	}
	if got := s.GetStorageRoot(absentCB); got != (common.Hash{}) {
		t.Fatalf("expected zero storage root for absent %s, got %s", absentCB.Hex(), got.Hex())
	}

	// AddBalance(0) trên absentCB, emptyCB và nonEmptyCB theo quy tắc EIP-161 / Shanghai
	s.AddBalance(absentCB, uint256.NewInt(0), tracing.BalanceIncreaseRewardTransactionFee)
	s.AddBalance(emptyCB, uint256.NewInt(0), tracing.BalanceIncreaseRewardTransactionFee)
	s.AddBalance(nonEmptyCB, uint256.NewInt(0), tracing.BalanceIncreaseRewardTransactionFee)
	s.SelfDestruct(destructAddr)
	s.Finalise(true)

	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	exported, err := NewPebbleStateDB(db).ExportLogicalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.OrphanKeys) != 0 {
		t.Fatalf("expected zero orphan keys after selfdestruct/finalise, got %d", len(exported.OrphanKeys))
	}
	if _, ok := exported.Accounts[absentCB]; ok {
		t.Fatalf("absent coinbase should not exist after AddBalance(0) + Finalise(true)")
	}
	if _, ok := exported.Accounts[emptyCB]; ok {
		t.Fatalf("touched empty coinbase should be deleted by EIP-161 Finalise(true)")
	}
	if acc, ok := exported.Accounts[nonEmptyCB]; !ok || acc.Balance.Uint64() != 1 {
		t.Fatalf("non-empty coinbase must remain untouched with balance 1, got ok=%v acc=%+v", ok, acc)
	}
	if _, ok := exported.Accounts[destructAddr]; ok {
		t.Fatalf("self-destructed account must be absent after Finalise(true) + Commit")
	}
}

func TestOCCWindowForcedCounterConflictMatchesSequential(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := overlayTestChainConfig(chainID)
	contract := common.HexToAddress("0x000000000000000000000000000000000000c010")
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee1")
	code := productionResumeCode(0)

	buildDBAndTxs := func(name string) (*store.PebbleDB, []*types.Transaction) {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1))
		seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
		seed.SetNonce(contract, 1, tracing.NonceChangeUnspecified)
		seed.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(10)))
		txs := make([]*types.Transaction, 2)
		for i := 0; i < 2; i++ {
			digest := sha256.Sum256([]byte{byte(i + 1), 0xaa})
			key, _ := crypto.ToECDSA(digest[:])
			seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
			tx, err := types.SignNewTx(key, types.MakeSigner(cfg, big.NewInt(1), 100), &types.DynamicFeeTx{
				ChainID:   chainID,
				Nonce:     0,
				To:        &contract,
				Gas:       100_000,
				GasFeeCap: big.NewInt(1),
				GasTipCap: big.NewInt(0),
			})
			if err != nil {
				t.Fatal(err)
			}
			txs[i] = tx
		}
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db, txs
	}

	env := BlockEnv{Height: 1, Time: 100, Coinbase: coinbase, GasLimit: 1_000_000, BaseFee: big.NewInt(1)}

	seqDB, seqTxs := buildDBAndTxs("occ-seq")
	seqState := NewPebbleStateDB(seqDB)
	seqRes, err := NewSequentialExecutor(cfg, chainID).ExecuteBlock(seqState, env, seqTxs)
	if err != nil {
		t.Fatal(err)
	}

	// ép cả A (tx 0) và B (tx 1) cùng đọc giá trị 10 ở attempt 1 trước khi A chốt
	occDB, occTxs := buildDBAndTxs("occ-par")
	var barrierMu sync.Mutex
	seen10 := 0
	bothRead := make(chan struct{})
	var tx1Values []uint64

	occExec := NewParallelExecutor(cfg, chainID, 2)
	occExec.SlidingWindow = true
	occExec.StrictInvalidTx = true
	occExec.RecordTrace = true
	occExec.OCCHooks = &OCCHooks{
		OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
			if addr != contract || slot != (common.Hash{}) {
				return
			}
			if txIndex == 1 {
				barrierMu.Lock()
				tx1Values = append(tx1Values, new(big.Int).SetBytes(val.Bytes()).Uint64())
				barrierMu.Unlock()
			}
			if attempt == 1 && new(big.Int).SetBytes(val.Bytes()).Uint64() == 10 {
				barrierMu.Lock()
				seen10++
				if seen10 == 2 {
					close(bothRead)
				}
				barrierMu.Unlock()
				select {
				case <-bothRead:
				case <-time.After(3 * time.Second):
				}
			}
		},
	}

	occState := NewPebbleStateDB(occDB)
	occRes, err := occExec.ExecuteBlock(occState, env, occTxs)
	if err != nil {
		t.Fatal(err)
	}

	if occRes.StateRoot != seqRes.StateRoot || occRes.GasUsed != seqRes.GasUsed {
		t.Fatalf("OCC mismatch vs seq: root=%x/%x gas=%d/%d", occRes.StateRoot, seqRes.StateRoot, occRes.GasUsed, seqRes.GasUsed)
	}
	if occRes.OCCStats == nil || occRes.OCCStats.RetriedTxs != 1 || occRes.OCCStats.TotalRetries != 1 {
		t.Fatalf("expected exactly 1 retry for tx B, got stats=%+v", occRes.OCCStats)
	}
	if got := occState.GetState(contract, common.Hash{}); got != common.BigToHash(big.NewInt(12)) {
		t.Fatalf("final counter = %s, want 12", got.Hex())
	}
	// tx 1 phải thấy 10 ở attempt 1 rồi 11 ở attempt 2
	has10, has11 := false, false
	for _, v := range tx1Values {
		if v == 10 {
			has10 = true
		}
		if v == 11 {
			has11 = true
		}
	}
	if !has10 || !has11 {
		t.Fatalf("expected tx 1 to read 10 then 11, observed reads=%v", tx1Values)
	}
}

func TestGroupedMissingDeclaredKeyAbortsBatchAndMatchesSequential(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := overlayTestChainConfig(chainID)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000fee1")

	build := func(name string) (*store.PebbleDB, []*types.Transaction) {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1))
		txs := make([]*types.Transaction, 2)
		for i := 0; i < 2; i++ {
			digest := sha256.Sum256([]byte{byte(i + 10), 0xbb})
			key, _ := crypto.ToECDSA(digest[:])
			seed.AddBalanceBig(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1_000_000_000))
			to := common.BigToAddress(big.NewInt(int64(0x9000 + i)))
			tx, err := types.SignNewTx(key, types.MakeSigner(cfg, big.NewInt(1), 100), &types.DynamicFeeTx{
				ChainID:   chainID,
				Nonce:     0,
				To:        &to,
				Gas:       21_000,
				GasFeeCap: big.NewInt(1),
				GasTipCap: big.NewInt(0),
				Value:     big.NewInt(100),
			})
			if err != nil {
				t.Fatal(err)
			}
			txs[i] = tx
		}
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db, txs
	}

	env := BlockEnv{Height: 1, Time: 100, Coinbase: coinbase, GasLimit: 1_000_000, BaseFee: big.NewInt(1)}

	seqDB, seqTxs := build("grp-seq")
	seqRes, err := NewSequentialExecutor(cfg, chainID).ExecuteBlock(NewPebbleStateDB(seqDB), env, seqTxs)
	if err != nil {
		t.Fatal(err)
	}

	// cố ý xóa 1 write-key khỏi tập khai báo của tx 1 để ép hủy cả đợt và chạy lại tuần tự
	grpDB, grpTxs := build("grp-abort")
	grpExec := NewGroupedExecutor(cfg, chainID, 2)
	grpExec.StrictInvalidTx = true
	grpExec.RecordTrace = true
	grpExec.Hooks = &GroupedHooks{
		OverrideDeclaredAccess: func(txIndex int, declared *DeclaredAccess) {
			if txIndex == 1 {
				for k := range declared.Writes {
					if k.Kind == MVBalance {
						delete(declared.Writes, k)
						break
					}
				}
			}
		},
	}

	grpRes, err := grpExec.ExecuteBlock(NewPebbleStateDB(grpDB), env, grpTxs)
	if err != nil {
		t.Fatal(err)
	}
	if grpRes.StateRoot != seqRes.StateRoot || grpRes.GasUsed != seqRes.GasUsed {
		t.Fatalf("grouped abort-fallback diverged from seq: root=%x/%x gas=%d/%d", grpRes.StateRoot, seqRes.StateRoot, grpRes.GasUsed, seqRes.GasUsed)
	}
	if grpRes.GroupedStats == nil || grpRes.GroupedStats.AbortedGroups != 1 || grpRes.GroupedStats.FallbackTxs != 2 {
		t.Fatalf("expected 1 aborted group with 2 fallback txs, got %+v", grpRes.GroupedStats)
	}
}
