package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

func loadCounterCreationBytecode(t *testing.T) []byte {
	t.Helper()
	rawHex, err := os.ReadFile("../../contracts/Counter.bin")
	if err != nil {
		t.Fatalf("read contracts/Counter.bin: %v", err)
	}
	code, err := hex.DecodeString(strings.TrimSpace(string(rawHex)))
	if err != nil {
		t.Fatalf("decode Counter.bin: %v", err)
	}
	return code
}

func makeGenesisJSON(t *testing.T, alloc map[common.Address]*big.Int) []byte {
	t.Helper()
	state := GenesisState{Alloc: make(map[string]GenesisAccount, len(alloc))}
	for addr, bal := range alloc {
		state.Alloc[addr.Hex()] = GenesisAccount{Balance: bal.String()}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal genesis: %v", err)
	}
	return data
}

func mustSignLegacyTx(t *testing.T, key *ecdsa.PrivateKey, cid *big.Int, nonce, gas uint64, gasPrice *big.Int, to *common.Address, value *big.Int, data []byte) (*types.Transaction, []byte) {
	t.Helper()
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: gasPrice,
		Gas:      gas,
		To:       to,
		Value:    value,
		Data:     data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(cid), key)
	if err != nil {
		t.Fatalf("sign legacy tx: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal legacy tx: %v", err)
	}
	return signed, raw
}

func TestInitChainIdempotenceAndGenesisMismatch(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewApp(dir, logger)
	if err != nil {
		t.Fatal(err)
	}

	addrA := common.HexToAddress("0x000000000000000000000000000000000000a001")
	addrB := common.HexToAddress("0x000000000000000000000000000000000000b002")
	genesisA := makeGenesisJSON(t, map[common.Address]*big.Int{addrA: big.NewInt(100)})

	resp1, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-test",
		AppStateBytes: genesisA,
	})
	if err != nil {
		t.Fatalf("first InitChain failed: %v", err)
	}
	if len(resp1.AppHash) != 32 {
		t.Fatalf("genesis AppHash len = %d, want 32", len(resp1.AppHash))
	}
	if got := application.Balance(addrA); got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("balance after first InitChain = %s, want 100", got)
	}

	info, err := application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.LastBlockHeight != 0 || !bytes.Equal(info.LastBlockAppHash, resp1.AppHash) {
		t.Fatalf("Info after InitChain = (%d, %x), want (0, %x)", info.LastBlockHeight, info.LastBlockAppHash, resp1.AppHash)
	}

	// gọi lại cùng genesis -> idempotent, số dư giữ nguyên 100 và AppHash không đổi
	resp2, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-test",
		AppStateBytes: genesisA,
	})
	if err != nil {
		t.Fatalf("repeated InitChain with identical genesis failed: %v", err)
	}
	if !bytes.Equal(resp2.AppHash, resp1.AppHash) {
		t.Fatalf("repeated InitChain AppHash = %x, want %x", resp2.AppHash, resp1.AppHash)
	}
	if got := application.Balance(addrA); got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("balance doubled on repeated InitChain: got %s, want 100", got)
	}

	// đóng và mở lại trên cùng data dir -> kiểm tra dấu khởi tạo bền vững
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApp(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	infoReopened, err := reopened.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if infoReopened.LastBlockHeight != 0 || !bytes.Equal(infoReopened.LastBlockAppHash, resp1.AppHash) {
		t.Fatalf("Info after reopen = (%d, %x), want (0, %x)", infoReopened.LastBlockHeight, infoReopened.LastBlockAppHash, resp1.AppHash)
	}

	// gọi lại cùng genesis sau restart -> vẫn idempotent
	resp3, err := reopened.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-test",
		AppStateBytes: genesisA,
	})
	if err != nil {
		t.Fatalf("InitChain after reopen with same genesis failed: %v", err)
	}
	if !bytes.Equal(resp3.AppHash, resp1.AppHash) {
		t.Fatalf("InitChain after reopen AppHash = %x, want %x", resp3.AppHash, resp1.AppHash)
	}
	if got := reopened.Balance(addrA); got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("balance after reopen InitChain = %s, want 100", got)
	}

	// genesis khác nội dung -> phải trả lỗi rõ ràng và không sửa state
	genesisDiffBalance := makeGenesisJSON(t, map[common.Address]*big.Int{addrA: big.NewInt(200)})
	if _, err := reopened.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-test",
		AppStateBytes: genesisDiffBalance,
	}); err == nil {
		t.Fatal("InitChain with different balance unexpectedly succeeded")
	}
	genesisDiffAlloc := makeGenesisJSON(t, map[common.Address]*big.Int{addrA: big.NewInt(100), addrB: big.NewInt(50)})
	if _, err := reopened.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-test",
		AppStateBytes: genesisDiffAlloc,
	}); err == nil {
		t.Fatal("InitChain with extra account unexpectedly succeeded")
	}
	if _, err := reopened.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "other-chain",
		AppStateBytes: genesisA,
	}); err == nil {
		t.Fatal("InitChain with different chain ID unexpectedly succeeded")
	}
	if got := reopened.Balance(addrA); got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("balance mutated after rejected InitChain: got %s, want 100", got)
	}

	// FinalizeBlock(1) trước Commit(1): Info vẫn phải trả H-1 (height=0, genesis AppHash)
	if _, err := reopened.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(10, 0), Hash: common.HexToHash("0x01").Bytes(),
	}); err != nil {
		t.Fatalf("FinalizeBlock(1) failed: %v", err)
	}
	infoPending, err := reopened.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if infoPending.LastBlockHeight != 0 || !bytes.Equal(infoPending.LastBlockAppHash, resp1.AppHash) {
		t.Fatalf("Info before Commit(1) = (%d, %x), want (0, %x)", infoPending.LastBlockHeight, infoPending.LastBlockAppHash, resp1.AppHash)
	}
}

func TestProcessProposalRejectsProtocolInvalidAndAcceptsRevert(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x0000000000000000000000000000000000005555")
	poorKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	application, err := NewApp(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if err := application.ConfigureExecutor("occ", 4); err != nil {
		t.Fatal(err)
	}
	if err := application.ConfigureCheckpoints(evm.CheckpointConfig{Policy: evm.CheckpointPolicyHistory}); err != nil {
		t.Fatal(err)
	}

	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{
		sender: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)),
	})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-proposal-test",
		AppStateBytes: genesis,
	}); err != nil {
		t.Fatal(err)
	}

	// cài đặt 2 contract: revertContract ghi slot 0 rồi REVERT; oogContract chạy vòng lặp vô hạn
	revertContract := common.HexToAddress("0x000000000000000000000000000000000000bad1")
	oogContract := common.HexToAddress("0x000000000000000000000000000000000000bad2")
	seed := evm.NewPebbleStateDB(application.db)
	seed.CreateAccount(revertContract)
	seed.SetCode(revertContract, common.FromHex("0x600160005560006000fd"), 0)
	seed.CreateAccount(oogContract)
	seed.SetCode(oogContract, common.FromHex("0x600056"), 0)
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	cid := application.ChainID()
	gasPrice := big.NewInt(1_000_000_000)

	_, validTransfer0 := mustSignLegacyTx(t, key, cid, 0, 21000, gasPrice, &recipient, big.NewInt(7), nil)
	_, validTransfer1 := mustSignLegacyTx(t, key, cid, 1, 21000, gasPrice, &recipient, big.NewInt(9), nil)
	revertTx, revertRaw := mustSignLegacyTx(t, key, cid, 0, 100000, gasPrice, &revertContract, big.NewInt(0), nil)
	oogTx, oogRaw := mustSignLegacyTx(t, key, cid, 1, 50000, gasPrice, &oogContract, big.NewInt(0), nil)

	_, wrongChainRaw := mustSignLegacyTx(t, key, big.NewInt(1), 0, 21000, gasPrice, &recipient, big.NewInt(1), nil)
	_, wrongNonceRaw := mustSignLegacyTx(t, key, cid, 5, 21000, gasPrice, &recipient, big.NewInt(1), nil)
	_, poorRaw := mustSignLegacyTx(t, poorKey, cid, 0, 21000, gasPrice, &recipient, big.NewInt(1), nil)
	_, overGasRaw := mustSignLegacyTx(t, key, cid, 0, blockGasLimit+1, gasPrice, &recipient, big.NewInt(0), nil)
	_, lowFeeRaw := mustSignLegacyTx(t, key, cid, 0, 21000, big.NewInt(1), &recipient, big.NewInt(1), nil)
	invalidSigTx := types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: gasPrice, Gas: 21000, To: &recipient, Value: big.NewInt(0),
		V: new(big.Int).Add(new(big.Int).Mul(cid, big.NewInt(2)), big.NewInt(35)), R: big.NewInt(0), S: big.NewInt(0),
	})
	invalidSigRaw, err := invalidSigTx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	// 1. ProcessProposal phải REJECT mọi lỗi protocol
	invalidCases := []struct {
		name string
		txs  [][]byte
	}{
		{"empty tx", [][]byte{{}}},
		{"malformed 0xff", [][]byte{{0xff}}},
		{"wrong chain ID", [][]byte{wrongChainRaw}},
		{"invalid signature", [][]byte{invalidSigRaw}},
		{"wrong nonce", [][]byte{wrongNonceRaw}},
		{"duplicate nonce", [][]byte{validTransfer0, validTransfer0}},
		{"insufficient balance", [][]byte{poorRaw}},
		{"exceeds block gas limit", [][]byte{overGasRaw}},
		{"below base fee", [][]byte{lowFeeRaw}},
		{"valid followed by 0xff", [][]byte{validTransfer0, {0xff}}},
	}
	for _, tc := range invalidCases {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			resp, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
				Height: 1, Time: time.Unix(100, 0), Hash: common.HexToHash("0x11").Bytes(), Txs: tc.txs,
			})
			if err != nil {
				t.Fatalf("ProcessProposal returned error: %v", err)
			}
			if resp.Status != abci.PROCESS_PROPOSAL_STATUS_REJECT {
				t.Fatalf("ProcessProposal(%s) status = %v, want REJECT", tc.name, resp.Status)
			}
		})
	}

	// 2. PrepareProposal lọc sạch tx lỗi protocol và tôn trọng MaxTxBytes
	prepResp, err := application.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{
		Height:     1,
		Time:       time.Unix(100, 0),
		MaxTxBytes: 1024 * 1024,
		Txs:        [][]byte{{0xff}, wrongChainRaw, validTransfer0, validTransfer0, wrongNonceRaw, poorRaw, validTransfer1},
	})
	if err != nil {
		t.Fatalf("PrepareProposal failed: %v", err)
	}
	if len(prepResp.Txs) != 2 || !bytes.Equal(prepResp.Txs[0], validTransfer0) || !bytes.Equal(prepResp.Txs[1], validTransfer1) {
		t.Fatalf("PrepareProposal kept %d txs, want [validTransfer0, validTransfer1]", len(prepResp.Txs))
	}

	// kiểm tra giới hạn MaxTxBytes chỉ đủ chứa 1 tx
	prepLimited, err := application.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{
		Height:     1,
		Time:       time.Unix(100, 0),
		MaxTxBytes: int64(len(validTransfer0)),
		Txs:        [][]byte{validTransfer0, validTransfer1},
	})
	if err != nil {
		t.Fatalf("PrepareProposal(MaxTxBytes) failed: %v", err)
	}
	if len(prepLimited.Txs) != 1 || !bytes.Equal(prepLimited.Txs[0], validTransfer0) {
		t.Fatalf("PrepareProposal(MaxTxBytes) kept %d txs, want 1", len(prepLimited.Txs))
	}

	// 3. Nhiều vòng đề xuất tại cùng độ cao H không làm bẩn trạng thái chốt hay lịch sử checkpoint
	abandonedResp, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
		Height: 1, Time: time.Unix(100, 0), Hash: common.HexToHash("0x12").Bytes(), Txs: [][]byte{validTransfer0, validTransfer1},
	})
	if err != nil || abandonedResp.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
		t.Fatalf("valid proposal A status=%v err=%v, want ACCEPT", abandonedResp, err)
	}
	if application.CurrentHeight() != 0 || application.Nonce(sender) != 0 || application.Balance(recipient).Sign() != 0 {
		t.Fatalf("abandoned proposal mutated committed state: height=%d nonce=%d recipBal=%s",
			application.CurrentHeight(), application.Nonce(sender), application.Balance(recipient))
	}
	if parallelExec, ok := application.executor.(*evm.ParallelExecutor); ok && len(parallelExec.HotSlots()) != 0 {
		t.Fatalf("proposal dirtied checkpoint history tracker: %v", parallelExec.HotSlots())
	}

	// 4. Proposal chứa giao dịch hợp lệ nhưng EVM revert và OOG phải được ACCEPT và sinh receipt status=0
	proposalB := [][]byte{revertRaw, oogRaw}
	acceptRevert, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
		Height: 1, Time: time.Unix(100, 0), Hash: common.HexToHash("0x13").Bytes(), Txs: proposalB,
	})
	if err != nil || acceptRevert.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
		t.Fatalf("ProcessProposal(revert+oog) status=%v err=%v, want ACCEPT", acceptRevert, err)
	}

	if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(100, 0), Hash: common.HexToHash("0x13").Bytes(), Txs: proposalB,
	}); err != nil {
		t.Fatalf("FinalizeBlock(revert+oog) failed: %v", err)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("Commit(revert+oog) failed: %v", err)
	}

	for name, txHash := range map[string]common.Hash{"revert": revertTx.Hash(), "oog": oogTx.Hash()} {
		receipt, found, err := application.EthereumReceiptByHash(txHash)
		if err != nil || !found {
			t.Fatalf("%s receipt missing: found=%v err=%v", name, found, err)
		}
		if receipt.Status != types.ReceiptStatusFailed {
			t.Fatalf("%s receipt status = %d, want 0", name, receipt.Status)
		}
		if receipt.GasUsed == 0 {
			t.Fatalf("%s receipt gasUsed = 0, want > 0", name)
		}
	}
	if got := application.Storage(revertContract, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("reverted storage leaked: %s", got)
	}
	if got := application.Nonce(sender); got != 2 {
		t.Fatalf("sender nonce after revert+oog = %d, want 2", got)
	}
}

func TestConcurrentCommitAndMetadataReadsRaceFree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x0000000000000000000000000000000000007777")

	application, err := NewApp(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })

	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{
		sender: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)),
	})
	if _, err := application.InitChain(context.Background(), &abci.InitChainRequest{
		ChainId:       "pluto-race-test",
		AppStateBytes: genesis,
	}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastHeight uint64
			for {
				select {
				case <-stop:
					return
				default:
					h, hash := application.CommittedHead()
					if h < lastHeight {
						errCh <- errors.New("height moved backwards")
						return
					}
					lastHeight = h
					if h > 0 && hash == (common.Hash{}) {
						errCh <- errors.New("empty app hash at non-zero height")
						return
					}
					_ = application.CurrentHeight()
					_ = application.CurrentAppHash()
					_ = application.GenesisAppHash()
					if _, err := application.Info(context.Background(), &abci.InfoRequest{}); err != nil {
						errCh <- err
						return
					}
					if _, err := application.NextBaseFeePerGas(); err != nil {
						errCh <- err
						return
					}
					if _, err := application.CurrentBaseFeePerGas(); err != nil {
						errCh <- err
						return
					}
					if h > 0 {
						if _, _, err := application.EthereumBlockByNumber(h); err != nil {
							errCh <- err
							return
						}
					}
					_ = application.Balance(recipient)
					_ = application.Nonce(sender)
					if _, err := application.Call(CallArgs{From: sender, To: &recipient, Gas: 21000}); err != nil {
						errCh <- err
						return
					}
				}
			}
		}()
	}

	cid := application.ChainID()
	gasPrice := big.NewInt(1_000_000_000)
	const totalBlocks = 20
	for h := int64(1); h <= totalBlocks; h++ {
		_, raw := mustSignLegacyTx(t, key, cid, uint64(h-1), 21000, gasPrice, &recipient, big.NewInt(1), nil)
		if _, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
			Height: h, Time: time.Unix(1000+h, 0), Hash: common.BigToHash(big.NewInt(h)).Bytes(), Txs: [][]byte{raw},
		}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("FinalizeBlock(%d) failed: %v", h, err)
		}
		if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Commit(%d) failed: %v", h, err)
		}
	}
	close(stop)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent reader error: %v", err)
	default:
	}

	if got := application.CurrentHeight(); got != totalBlocks {
		t.Fatalf("final height = %d, want %d", got, totalBlocks)
	}
	if got := application.Balance(recipient); got.Cmp(big.NewInt(totalBlocks)) != 0 {
		t.Fatalf("recipient balance = %s, want %d", got, totalBlocks)
	}
}

func TestCrashRecoveryBeforeWriteSyncBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x0000000000000000000000000000000000008888")
	proposer := common.HexToAddress("0x000000000000000000000000000000000000c01b")
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{
		sender: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)),
	})

	creation := loadCounterCreationBytecode(t)
	contractAddr := crypto.CreateAddress(sender, 0)
	gasPrice := big.NewInt(1_000_000_000)

	_, deployRaw := mustSignLegacyTx(t, key, chainID, 0, 3_000_000, gasPrice, nil, big.NewInt(0), creation)
	incTx, incRaw := mustSignLegacyTx(t, key, chainID, 1, 100_000, gasPrice, &contractAddr, big.NewInt(0), common.FromHex("0xd09de08a"))
	xferTx, xferRaw := mustSignLegacyTx(t, key, chainID, 2, 21_000, gasPrice, &recipient, big.NewInt(12345), nil)

	block1Req := &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(2001, 0), Hash: common.HexToHash("0xb1").Bytes(), ProposerAddress: proposer.Bytes(),
		Txs: [][]byte{deployRaw},
	}
	block2Req := &abci.FinalizeBlockRequest{
		Height: 2, Time: time.Unix(2002, 0), Hash: common.HexToHash("0xb2").Bytes(), ProposerAddress: proposer.Bytes(),
		Txs: [][]byte{incRaw, xferRaw},
	}

	// chuỗi đối chiếu chạy liên tục không crash
	refApp, err := NewApp(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = refApp.Close() })
	if _, err := refApp.InitChain(context.Background(), &abci.InitChainRequest{ChainId: "crash-before", AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	respRef1, err := refApp.FinalizeBlock(context.Background(), block1Req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refApp.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := refApp.FinalizeBlock(context.Background(), block2Req); err != nil {
		t.Fatal(err)
	}
	if _, err := refApp.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	// nút crash sau FinalizeBlock(2), trước Commit(2)/WriteSync
	crashDir := t.TempDir()
	crashApp, err := NewApp(crashDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.InitChain(context.Background(), &abci.InitChainRequest{ChainId: "crash-before", AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.FinalizeBlock(context.Background(), block1Req); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.FinalizeBlock(context.Background(), block2Req); err != nil {
		t.Fatal(err)
	}
	// giả lập crash: đóng DB mà không gọi Commit(2)
	if err := crashApp.Close(); err != nil {
		t.Fatal(err)
	}

	// mở lại trên cùng data dir -> Info phải trả H-1 (height=1) và AppHash(1)
	recovered, err := NewApp(crashDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })

	infoBeforeReplay, err := recovered.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if infoBeforeReplay.LastBlockHeight != 1 || !bytes.Equal(infoBeforeReplay.LastBlockAppHash, respRef1.AppHash) {
		t.Fatalf("Info after pre-WriteSync crash = (%d, %x), want (1, %x)",
			infoBeforeReplay.LastBlockHeight, infoBeforeReplay.LastBlockAppHash, respRef1.AppHash)
	}
	if _, found, err := recovered.EthereumReceiptByHash(incTx.Hash()); err != nil || found {
		t.Fatalf("block 2 receipt leaked before Commit: found=%v err=%v", found, err)
	}
	if got := recovered.Storage(contractAddr, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("counter mutated before Commit: %s", got)
	}

	// CometBFT phát lại FinalizeBlock(2) + Commit(2)
	if _, err := recovered.FinalizeBlock(context.Background(), block2Req); err != nil {
		t.Fatalf("replay FinalizeBlock(2) failed: %v", err)
	}
	if _, err := recovered.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("replay Commit(2) failed: %v", err)
	}

	assertAppsMatchAtHeight(t, refApp, recovered, 2, sender, recipient, contractAddr, incTx.Hash(), xferTx.Hash())
}

func TestCrashRecoveryAfterWriteSyncBeforeCommitReturn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x0000000000000000000000000000000000009999")
	proposer := common.HexToAddress("0x000000000000000000000000000000000000c01b")
	genesis := makeGenesisJSON(t, map[common.Address]*big.Int{
		sender: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)),
	})

	creation := loadCounterCreationBytecode(t)
	contractAddr := crypto.CreateAddress(sender, 0)
	gasPrice := big.NewInt(1_000_000_000)

	_, deployRaw := mustSignLegacyTx(t, key, chainID, 0, 3_000_000, gasPrice, nil, big.NewInt(0), creation)
	inc1Tx, inc1Raw := mustSignLegacyTx(t, key, chainID, 1, 100_000, gasPrice, &contractAddr, big.NewInt(0), common.FromHex("0xd09de08a"))
	xferTx, xferRaw := mustSignLegacyTx(t, key, chainID, 2, 21_000, gasPrice, &recipient, big.NewInt(54321), nil)
	inc2Tx, inc2Raw := mustSignLegacyTx(t, key, chainID, 3, 100_000, gasPrice, &contractAddr, big.NewInt(0), common.FromHex("0xd09de08a"))

	block1Req := &abci.FinalizeBlockRequest{
		Height: 1, Time: time.Unix(3001, 0), Hash: common.HexToHash("0xc1").Bytes(), ProposerAddress: proposer.Bytes(),
		Txs: [][]byte{deployRaw},
	}
	block2Req := &abci.FinalizeBlockRequest{
		Height: 2, Time: time.Unix(3002, 0), Hash: common.HexToHash("0xc2").Bytes(), ProposerAddress: proposer.Bytes(),
		Txs: [][]byte{inc1Raw, xferRaw},
	}
	block3Req := &abci.FinalizeBlockRequest{
		Height: 3, Time: time.Unix(3003, 0), Hash: common.HexToHash("0xc3").Bytes(), ProposerAddress: proposer.Bytes(),
		Txs: [][]byte{inc2Raw},
	}

	// chuỗi đối chiếu chạy liên tục 1 -> 2 -> 3
	refApp, err := NewApp(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = refApp.Close() })
	if _, err := refApp.InitChain(context.Background(), &abci.InitChainRequest{ChainId: "crash-after", AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*abci.FinalizeBlockRequest{block1Req, block2Req} {
		if _, err := refApp.FinalizeBlock(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if _, err := refApp.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	refHashAt2 := refApp.CurrentAppHash()

	// nút crash ngay sau batch.WriteSync() hoàn tất nhưng trước khi cập nhật RAM / trả về Commit(2)
	crashDir := t.TempDir()
	crashApp, err := NewApp(crashDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.InitChain(context.Background(), &abci.InitChainRequest{ChainId: "crash-after", AppStateBytes: genesis}); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.FinalizeBlock(context.Background(), block1Req); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}

	crashErr := errors.New("simulated crash after WriteSync before Commit return")
	crashApp.afterCommitWriteSyncHook = func(height int64, appHash []byte) error {
		return crashErr
	}
	if _, err := crashApp.FinalizeBlock(context.Background(), block2Req); err != nil {
		t.Fatal(err)
	}
	if _, err := crashApp.Commit(context.Background(), &abci.CommitRequest{}); !errors.Is(err, crashErr) {
		t.Fatalf("Commit(2) err = %v, want %v", err, crashErr)
	}
	if got := crashApp.CurrentHeight(); got != 1 {
		t.Fatalf("RAM height updated despite crash before Commit return: got %d, want 1", got)
	}
	if err := crashApp.Close(); err != nil {
		t.Fatal(err)
	}

	// mở lại trên cùng data dir -> Info phải trả đúng H=2 & AppHash(2) để CometBFT không phát lại block 2
	recovered, err := NewApp(crashDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })

	infoRecovered, err := recovered.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if infoRecovered.LastBlockHeight != 2 || common.BytesToHash(infoRecovered.LastBlockAppHash) != refHashAt2 {
		t.Fatalf("Info after post-WriteSync crash = (%d, %x), want (2, %x)",
			infoRecovered.LastBlockHeight, infoRecovered.LastBlockAppHash, refHashAt2)
	}
	assertAppsMatchAtHeight(t, refApp, recovered, 2, sender, recipient, contractAddr, inc1Tx.Hash(), xferTx.Hash())

	// tiếp tục tiến lên block 3 trên cả 2 nút -> khớp 100%, không áp lại giao dịch 2 lần
	for _, a := range []*App{refApp, recovered} {
		if _, err := a.FinalizeBlock(context.Background(), block3Req); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	assertAppsMatchAtHeight(t, refApp, recovered, 3, sender, recipient, contractAddr, inc1Tx.Hash(), xferTx.Hash(), inc2Tx.Hash())
	if got := recovered.Storage(contractAddr, common.Hash{}); got != common.BigToHash(big.NewInt(2)) {
		t.Fatalf("counter storage after block 3 = %s, want 2", got)
	}
}

func assertAppsMatchAtHeight(t *testing.T, want, got *App, height uint64, sender, recipient, contract common.Address, txHashes ...common.Hash) {
	t.Helper()
	if got.CurrentHeight() != height || want.CurrentHeight() != height {
		t.Fatalf("height mismatch: got=%d want=%d expected=%d", got.CurrentHeight(), want.CurrentHeight(), height)
	}
	if got.CurrentAppHash() != want.CurrentAppHash() {
		t.Fatalf("AppHash mismatch at height %d: got=%s want=%s", height, got.CurrentAppHash(), want.CurrentAppHash())
	}
	if got.Nonce(sender) != want.Nonce(sender) {
		t.Fatalf("sender nonce mismatch: got=%d want=%d", got.Nonce(sender), want.Nonce(sender))
	}
	if got.Balance(sender).Cmp(want.Balance(sender)) != 0 {
		t.Fatalf("sender balance mismatch (fee accounting): got=%s want=%s", got.Balance(sender), want.Balance(sender))
	}
	if got.Balance(recipient).Cmp(want.Balance(recipient)) != 0 {
		t.Fatalf("recipient balance mismatch: got=%s want=%s", got.Balance(recipient), want.Balance(recipient))
	}
	if got.Storage(contract, common.Hash{}) != want.Storage(contract, common.Hash{}) {
		t.Fatalf("counter storage mismatch: got=%s want=%s", got.Storage(contract, common.Hash{}), want.Storage(contract, common.Hash{}))
	}
	for _, txHash := range txHashes {
		rGot, foundGot, errGot := got.EthereumReceiptByHash(txHash)
		rWant, foundWant, errWant := want.EthereumReceiptByHash(txHash)
		if errGot != nil || errWant != nil || !foundGot || !foundWant {
			t.Fatalf("receipt lookup %s: got=(%v,%v) want=(%v,%v)", txHash, foundGot, errGot, foundWant, errWant)
		}
		if rGot.Status != rWant.Status || rGot.GasUsed != rWant.GasUsed || rGot.CumulativeGasUsed != rWant.CumulativeGasUsed || len(rGot.Logs) != len(rWant.Logs) {
			t.Fatalf("receipt mismatch for %s: got=%+v want=%+v", txHash, rGot, rWant)
		}
		for i := range rGot.Logs {
			if rGot.Logs[i].Address != rWant.Logs[i].Address || !bytes.Equal(rGot.Logs[i].Data, rWant.Logs[i].Data) || len(rGot.Logs[i].Topics) != len(rWant.Logs[i].Topics) {
				t.Fatalf("receipt log[%d] mismatch for %s: got=%+v want=%+v", i, txHash, rGot.Logs[i], rWant.Logs[i])
			}
		}
	}
}
