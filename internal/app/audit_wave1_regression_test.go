package app

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// TestPrevRandaoConsistentAcrossPrepareProcessAndFinalize kiểm chứng PREVRANDAO đồng nhất giữa PrepareProposal, ProcessProposal và FinalizeBlock.
func TestPrevRandaoConsistentAcrossPrepareProcessAndFinalize(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewApp(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })

	key, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000077")
	sender := crypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{sender: new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId: "prevrandao-test", AppStateBytes: genesis,
	}); err != nil {
		t.Fatal(err)
	}

	// Contract lưu PREVRANDAO vào slot 0: PREVRANDAO (0x44), PUSH1 0x00, SSTORE (0x55), STOP (0x00)
	// Kèm 1 contract trả về PREVRANDAO để so sánh trực tiếp.
	contract := common.HexToAddress("0x000000000000000000000000000000000000d001")
	seed := evm.NewPebbleStateDB(application.db)
	seed.CreateAccount(contract)
	seed.SetNonce(contract, 1, 0)
	seed.SetCode(contract, []byte{byte(vm.PREVRANDAO), byte(vm.PUSH1), 0x00, byte(vm.SSTORE), byte(vm.STOP)}, 0)
	seed.Finalise(true)
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	blockTime := time.Unix(1_700_000_001, 0)
	proposer := common.HexToAddress("0x000000000000000000000000000000000000cafe").Bytes()
	wantRand := derivePrevRandao(1, blockTime, proposer, application.appHash)

	_, rawTx := mustSignLegacyTx(t, key, application.ChainID(), 0, 100_000, big.NewInt(1e9), &contract, big.NewInt(0), nil)

	prepResp, err := application.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{
		Height: 1, Time: blockTime, ProposerAddress: proposer, MaxTxBytes: 1 << 20, Txs: [][]byte{rawTx},
	})
	if err != nil || len(prepResp.Txs) != 1 {
		t.Fatalf("PrepareProposal = (%v, %v), want 1 tx", prepResp, err)
	}

	// ProcessProposal nhận req.Hash khác rỗng (khác với PrepareProposal) -> vẫn phải có cùng PREVRANDAO
	procResp, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
		Height: 1, Time: blockTime, Hash: []byte{0xaa, 0xbb, 0xcc, 0xdd}, ProposerAddress: proposer, Txs: prepResp.Txs,
	})
	if err != nil || procResp.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
		t.Fatalf("ProcessProposal = (%v, %v), want ACCEPT", procResp, err)
	}

	finResp, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: blockTime, Hash: []byte{0xaa, 0xbb, 0xcc, 0xdd}, ProposerAddress: proposer, Txs: prepResp.Txs,
	})
	if err != nil || len(finResp.TxResults) != 1 || finResp.TxResults[0].Code != 0 {
		t.Fatalf("FinalizeBlock = (%+v, %v), want Code=0", finResp, err)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	gotRand := application.Storage(contract, common.Hash{})
	if gotRand != wantRand {
		t.Fatalf("stored PREVRANDAO = %s, want %s (Prepare/Process/Finalize must agree)", gotRand.Hex(), wantRand.Hex())
	}
}

// TestInitChainRejectedAfterProgressAndChainIDMismatch kiểm chứng InitChain bị chặn khi height>0 và reopen khác evmChainID bị từ chối.
func TestInitChainRejectedAfterProgressAndChainIDMismatch(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewAppWithChainID(dir, logger, 9001)
	if err != nil {
		t.Fatal(err)
	}

	addr := common.HexToAddress("0x000000000000000000000000000000000000a001")
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{addr: big.NewInt(500)})
	initReq := &abci.InitChainRequest{ChainId: "cid-test", AppStateBytes: genesis}
	if _, err := application.InitChain(context.Background(), initReq); err != nil {
		t.Fatalf("initial InitChain failed: %v", err)
	}

	// Chốt block 1 -> CurrentHeight() == 1
	if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(10, 0), Hash: []byte{1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	// Gọi lại InitChain với CÙNG genesis khi height=1 vẫn phải bị từ chối
	if _, err := application.InitChain(context.Background(), initReq); err == nil {
		t.Fatal("InitChain unexpectedly succeeded on progressed DB at height=1")
	}

	// Kiểm tra chốt đơn điệu chiều cao: height=5 khi đang ở height=1 phải lỗi/REJECT
	if _, err := application.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{Height: 5, Time: time.Unix(20, 0)}); err == nil {
		t.Fatal("PrepareProposal unexpectedly accepted non-monotonic height 5 at height 1")
	}
	procResp, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{Height: 5, Time: time.Unix(20, 0)})
	if err != nil || procResp.Status != abci.PROCESS_PROPOSAL_STATUS_REJECT {
		t.Fatalf("ProcessProposal(height=5) = (%v, %v), want REJECT", procResp, err)
	}
	if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 5, Time: time.Unix(20, 0)}); err == nil {
		t.Fatal("FinalizeBlock unexpectedly accepted non-monotonic height 5 at height 1")
	}

	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	// Mở lại cùng DB với EVM chain ID khác phải bị từ chối
	if wrong, err := NewAppWithChainID(dir, logger, 9002); err == nil {
		_ = wrong.Close()
		t.Fatal("NewAppWithChainID unexpectedly allowed reopening DB with different EVM chain ID")
	}
}

// TestProcessProposalStaticPrePassRejectsMalformedTail kiểm chứng Pass 1 tĩnh loại ngay proposal có tx rác ở cuối mà không cần chạy EVM.
func TestProcessProposalStaticPrePassRejectsMalformedTail(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewApp(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })

	key, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000088")
	sender := crypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{sender: new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId: "prepass-test", AppStateBytes: genesis,
	}); err != nil {
		t.Fatal(err)
	}

	to := common.HexToAddress("0x0000000000000000000000000000000000009001")
	_, validTx := mustSignLegacyTx(t, key, application.ChainID(), 0, 21_000, big.NewInt(1e9), &to, big.NewInt(1), nil)
	resp, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
		Height: 1,
		Time:   time.Unix(10, 0),
		Hash:   []byte{1},
		Txs:    [][]byte{validTx, {0xff}},
	})
	if err != nil {
		t.Fatalf("ProcessProposal returned error: %v", err)
	}
	if resp.Status != abci.PROCESS_PROPOSAL_STATUS_REJECT {
		t.Fatalf("ProcessProposal status = %v, want REJECT", resp.Status)
	}
}

// TestCommittedSnapshotPreventsTornReadsDuringConcurrentCommit kiểm chứng RPC Call/EstimateGas không bao giờ thấy torn read giữa 2 slot khi Commit đồng thời.
func TestCommittedSnapshotPreventsTornReadsDuringConcurrentCommit(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewApp(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })

	key, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000099")
	sender := crypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{sender: new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId: "snapshot-test", AppStateBytes: genesis,
	}); err != nil {
		t.Fatal(err)
	}

	// Contract: nếu calldata rỗng -> đọc slot0 và slot1, trả về 64 byte (slot0 || slot1);
	// nếu calldata không rỗng -> ghi NUMBER vào cả slot0 và slot1 (bất biến: slot0 == slot1 ở mọi block đã commit).
	// CALLDATASIZE ISZERO PUSH1 0x0e JUMPI
	// NUMBER PUSH1 0x00 SSTORE NUMBER PUSH1 0x01 SSTORE STOP
	// JUMPDEST(0x0e): PUSH1 0x00 SLOAD PUSH1 0x00 MSTORE PUSH1 0x01 SLOAD PUSH1 0x20 MSTORE PUSH1 0x40 PUSH1 0x00 RETURN
	code := []byte{
		0x36, 0x15, 0x60, 0x0e, 0x57,
		0x43, 0x60, 0x00, 0x55, 0x43, 0x60, 0x01, 0x55, 0x00,
		0x5b, 0x60, 0x00, 0x54, 0x60, 0x00, 0x52, 0x60, 0x01, 0x54, 0x60, 0x20, 0x52, 0x60, 0x40, 0x60, 0x00, 0xf3,
	}
	contract := common.HexToAddress("0x000000000000000000000000000000000000d002")
	seed := evm.NewPebbleStateDB(application.db)
	seed.CreateAccount(contract)
	seed.SetNonce(contract, 1, 0)
	seed.SetCode(contract, code, 0)
	seed.Finalise(true)
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var tornCount atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				res, callErr := application.Call(CallArgs{From: sender, To: &contract, Gas: 200_000})
				if callErr != nil || res.Err != nil || len(res.ReturnData) != 64 {
					tornCount.Add(1)
					return
				}
				v0 := binary.BigEndian.Uint64(res.ReturnData[24:32])
				v1 := binary.BigEndian.Uint64(res.ReturnData[56:64])
				if v0 != v1 {
					tornCount.Add(1)
					return
				}
			}
		}()
	}

	for h := int64(1); h <= 20; h++ {
		_, rawTx := mustSignLegacyTx(t, key, application.ChainID(), uint64(h-1), 100_000, big.NewInt(1e9), &contract, big.NewInt(0), []byte{1})
		if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
			Height: h, Time: time.Unix(100+h, 0), Hash: []byte{byte(h)}, Txs: [][]byte{rawTx},
		}); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("FinalizeBlock(%d) failed: %v", h, err)
		}
		if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("Commit(%d) failed: %v", h, err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if tornCount.Load() != 0 {
		t.Fatalf("detected %d torn or failed RPC reads during concurrent Commit", tornCount.Load())
	}
}
