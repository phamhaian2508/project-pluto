package evm

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// TestLookupProbColdSingleSampleFallsBackToHotSlot kiểm chứng mẫu đơn (seen=1, useful=0) fallback sang BySlot thay vì gán p=0.
func TestLookupProbColdSingleSampleFallsBackToHotSlot(t *testing.T) {
	slot := MVKey{Address: common.HexToAddress("0x1111"), Kind: MVStorage, Slot: common.BigToHash(big.NewInt(5))}
	loc := AdaptiveLocationKey{
		CodeHash: common.BigToHash(big.NewInt(1)),
		Selector: [4]byte{0xde, 0x2a, 0xfb, 0x9c},
		PC:       1173,
		Slot:     slot,
	}
	snap := &AdaptiveHistorySnapshot{
		HasHistory: true,
		ByLocation: map[AdaptiveLocationKey]AdaptiveSampleCounts{
			loc: {Seen: 1, Useful: 0},
		},
		BySlot: map[MVKey]AdaptiveSampleCounts{
			slot: {Seen: 16, Useful: 12},
		},
	}
	p, seen, useful, src, ok := snap.LookupProb(loc)
	if !ok || src != "slot_fallback" || seen != 16 || useful != 12 || p < 0.74 || p > 0.76 {
		t.Fatalf("LookupProb(cold single sample) = (p=%f seen=%d useful=%d src=%q ok=%v), want slot_fallback 12/16", p, seen, useful, src, ok)
	}
}

// TestOCCWindowGasLimitDoesNotPreemptNonceError đảm bảo tx.Gas() > remainingGas không trả nhầm gas_limit_reached khi tx lỗi nonce_too_high trước.
func TestOCCWindowGasLimitDoesNotPreemptNonceError(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := newTestShanghaiConfig(chainID)
	signer := types.LatestSignerForChainID(chainID)
	key1, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000061")
	key2, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000062")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	recv := common.HexToAddress("0x0000000000000000000000000000000000009999")

	tx0, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 21_000, To: &recv, Value: big.NewInt(1),
	}), signer, key1)
	// tx1 vừa sai nonce (nonce=5 > 0) vừa có Gas=50_000 > remainingGas (30_000 - 21_000 = 9_000) -> Geth preCheck báo nonce_too_high trước
	tx1, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 5, GasPrice: big.NewInt(0), Gas: 50_000, To: &recv, Value: big.NewInt(1),
	}), signer, key2)

	for _, name := range []string{"sequential", "occ", "predicted-occ"} {
		db, err := store.NewPebbleDB("state", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(sender1, big.NewInt(1e18))
		seed.AddBalanceBig(sender2, big.NewInt(1e18))
		seed.Finalise(true)
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}

		var exec Executor
		switch name {
		case "sequential":
			e := NewSequentialExecutor(cfg, chainID)
			e.StrictInvalidTx = true
			exec = e
		case "occ":
			e := NewParallelExecutor(cfg, chainID, 2)
			e.SlidingWindow = true
			e.StrictInvalidTx = true
			exec = e
		case "predicted-occ":
			e := NewPredictedOCCExecutor(cfg, chainID, 2)
			e.StrictInvalidTx = true
			exec = e
		}
		_, err = exec.ExecuteBlock(NewPebbleStateDB(db), BlockEnv{Height: 1, Time: 1, GasLimit: 30_000, BaseFee: big.NewInt(0)}, []*types.Transaction{tx0, tx1})
		if err == nil {
			t.Fatalf("%s unexpectedly accepted invalid tx1", name)
		}
		if !strings.Contains(err.Error(), "nonce_too_high") {
			t.Fatalf("%s error = %q, want category nonce_too_high (must not preempt preCheck with gas_limit_reached)", name, err.Error())
		}
	}
}

// TestExportLogicalStateDetectsCodeHashMismatch kiểm chứng ExportLogicalState bắt lệch giữa acc.CodeHash và keccak256(code).
func TestExportLogicalStateDetectsCodeHashMismatch(t *testing.T) {
	s := testStateDB(t)
	addr := common.HexToAddress("0x000000000000000000000000000000000000f00d")
	s.CreateAccount(addr)
	s.SetNonce(addr, 1, 0)
	s.SetCode(addr, []byte{0x60, 0x00, 0x00}, 0)
	// cố ý làm sai CodeHash trong bộ đệm để kiểm tra bộ phát hiện
	s.state[addr].CodeHash = crypto.Keccak256([]byte{0xff})
	if _, err := s.ExportLogicalState(); err == nil || !strings.Contains(err.Error(), "CodeHash mismatch") {
		t.Fatalf("ExportLogicalState err = %v, want CodeHash mismatch error", err)
	}
}
