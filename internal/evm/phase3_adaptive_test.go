package evm

import (
	"context"
	"math"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// buildSelectorEarlyLateContract dựng 1 hợp đồng duy nhất có 2 hàm phân biệt bằng 4 byte selector:
// - selector 0x11111111 (early): SLOAD(slot) -> băm pure rounds lần -> +1 -> SSTORE(slot) -> LOG0 -> RETURN
// - selector 0x22222222 (late):  băm pure rounds lần -> SLOAD(slot) -> +1 -> SSTORE(slot) -> LOG0 -> RETURN
func buildSelectorEarlyLateContract(rounds int, slot byte) []byte {
	earlyBody := buildEarlyProbeCode(rounds, slot)
	lateBody := buildLateProbeCode(rounds, slot)

	// header dispatch:
	// PUSH0 CALLDATALOAD PUSH1 224 SHR (lấy selector 4 byte ở 32 bit thấp)
	// DUP1 PUSH4 0x11111111 EQ PUSH2 <earlyDest> JUMPI
	// PUSH4 0x22222222 EQ PUSH2 <lateDest> JUMPI
	// PUSH0 PUSH0 REVERT
	// tổng độ dài header cố định = 4 + 10 + 9 + 3 = 26 byte
	const headerLen = 26
	earlyDest := uint16(headerLen)
	lateDest := uint16(headerLen + 1 + len(earlyBody)) // +1 cho JUMPDEST

	code := make([]byte, 0, headerLen+2+len(earlyBody)+len(lateBody))
	code = append(code,
		0x5f, 0x35, 0x60, 0xe0, 0x1c, // selector = calldata[0:4] (5 byte)
	)
	// cập nhật lại headerLen chính xác = 5 + (1+5+1+3+1) + (5+1+3+1) + 3 = 5 + 11 + 10 + 3 = 29
	code = code[:0]
	const exactHeaderLen = 29
	earlyDest = uint16(exactHeaderLen)
	lateDest = uint16(exactHeaderLen + 1 + len(earlyBody))

	code = append(code,
		0x5f, 0x35, 0x60, 0xe0, 0x1c, // 5 byte: selector
		0x80,                         // 1 byte: DUP1
		0x63, 0x11, 0x11, 0x11, 0x11, // 5 byte: PUSH4 early
		0x14,                                     // 1 byte: EQ
		0x61, byte(earlyDest>>8), byte(earlyDest), // 3 byte: PUSH2 earlyDest
		0x57,                         // 1 byte: JUMPI
		0x63, 0x22, 0x22, 0x22, 0x22, // 5 byte: PUSH4 late
		0x14,                                   // 1 byte: EQ
		0x61, byte(lateDest>>8), byte(lateDest), // 3 byte: PUSH2 lateDest
		0x57,             // 1 byte: JUMPI
		0x5f, 0x5f, 0xfd, // 3 byte: PUSH0 PUSH0 REVERT
	)
	if len(code) != exactHeaderLen {
		panic("unexpected selector dispatcher header length")
	}
	code = append(code, 0x5b) // JUMPDEST early
	code = append(code, earlyBody...)
	code = append(code, 0x5b) // JUMPDEST late
	code = append(code, lateBody...)
	return code
}

func makeSelectorCalldata(sel [4]byte, seed byte) []byte {
	data := make([]byte, 36)
	copy(data[:4], sel[:])
	data[35] = seed
	return data
}

func TestAdaptiveScoreTableAndEdgeCases(t *testing.T) {
	// bảng số chuẩn handoff Phase 3 mục 2:
	// p=0.5, gain=90_000, capture=5_000, bytes=1_000 -> net=40_000, score=40 ns/byte
	res := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob:           0.5,
		GainNs:         90_000,
		CaptureNs:      5_000,
		AccountedBytes: 1_000,
		Threshold:      0,
	})
	if !res.Accept || res.NetNs != 40_000 || res.Score != 40 {
		t.Fatalf("unexpected score result: %+v (want accept=true net=40000 score=40)", res)
	}

	// gain=-2_000 -> net=-6_000, score=-6 ns/byte -> từ chối non_positive_net
	neg := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob:           0.5,
		GainNs:         -2_000,
		CaptureNs:      5_000,
		AccountedBytes: 1_000,
		Threshold:      0,
	})
	if neg.Accept || neg.NetNs != -6_000 || neg.Score != -6 || neg.Reason != "non_positive_net" {
		t.Fatalf("unexpected negative gain result: %+v (want accept=false net=-6000 score=-6 reason=non_positive_net)", neg)
	}

	// ngưỡng đúng bằng score -> chấp nhận; lớn hơn 1 chút -> từ chối below_threshold
	eq := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000, Threshold: 40,
	})
	if !eq.Accept || eq.Score != 40 {
		t.Fatalf("expected accept when score == threshold, got %+v", eq)
	}
	above := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000, Threshold: 40.0001,
	})
	if above.Accept || above.Reason != "below_threshold" {
		t.Fatalf("expected below_threshold when threshold > score, got %+v", above)
	}

	// bytes = 0 -> từ chối zero_bytes
	zero := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 0, Threshold: 0,
	})
	if zero.Accept || zero.Reason != "zero_bytes" {
		t.Fatalf("expected zero_bytes, got %+v", zero)
	}

	// NaN / +Inf / -Inf / prob ngoài [0,1] -> non_finite
	nonFiniteCases := []AdaptiveScoreInput{
		{Prob: math.NaN(), GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: math.Inf(1), GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: -0.1, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: 1.1, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: 0.5, GainNs: math.NaN(), CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: 0.5, GainNs: math.Inf(-1), CaptureNs: 5_000, AccountedBytes: 1_000},
		{Prob: 0.5, GainNs: 90_000, CaptureNs: math.Inf(1), AccountedBytes: 1_000},
		{Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000, Threshold: math.NaN()},
		{Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: 1_000, Threshold: math.Inf(1)},
	}
	for i, tc := range nonFiniteCases {
		r := ScoreAdaptiveCandidate(tc)
		if r.Accept || r.Reason != "non_finite" {
			t.Fatalf("non-finite case %d: got %+v, want non_finite reject", i, r)
		}
	}

	// thiếu mẫu hiệu chuẩn & thiếu mẫu lịch sử
	emptyModel := &AdaptiveModel{FormatVersion: AdaptiveModelFormatVersion}
	if err := emptyModel.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	if _, ok := emptyModel.Lookup(AdaptiveCalibrationKey{PC: 10, SizeBucket: 10}); ok {
		t.Fatal("expected missing calibration sample")
	}
	emptyHist := NewAdaptiveHistoryTracker().SnapshotForBlock()
	if _, _, _, src, ok := emptyHist.LookupProb(AdaptiveLocationKey{PC: 10}); ok || src != "missing" {
		t.Fatalf("expected missing history sample, got ok=%v src=%s", ok, src)
	}

	// thiếu 1 byte trong kho -> từ chối
	const cpSize uint64 = 1000
	shortStore := NewCheckpointStore(cpSize - 1)
	if id, ok := shortStore.Reserve(cpSize); ok || id != 0 {
		t.Fatalf("expected Reserve to fail when short by 1 byte, got id=%d ok=%v", id, ok)
	}
	used, peak, _, skipped := shortStore.Stats()
	if used != 0 || peak != 0 || skipped != 1 {
		t.Fatalf("unexpected short store stats: used=%d peak=%d skipped=%d", used, peak, skipped)
	}

	// FCFS khi 2 điểm đều qua ngưỡng nhưng kho chỉ chứa vừa 1 điểm (và đảo thứ tự)
	testFCFS := func(firstTx, secondTx int) {
		st := NewCheckpointStore(cpSize + cpSize/2)
		s1 := ScoreAdaptiveCandidate(AdaptiveScoreInput{Prob: 0.5, GainNs: 90_000, CaptureNs: 5_000, AccountedBytes: cpSize, Threshold: 10})
		s2 := ScoreAdaptiveCandidate(AdaptiveScoreInput{Prob: 0.9, GainNs: 120_000, CaptureNs: 5_000, AccountedBytes: cpSize, Threshold: 10})
		if !s1.Accept || !s2.Accept {
			t.Fatalf("both candidates must pass score threshold")
		}
		id1, ok1 := st.Reserve(cpSize)
		if !ok1 {
			t.Fatalf("first candidate (tx=%d) must succeed", firstTx)
		}
		if err := st.CommitReservation(id1, firstTx, &PurePrefixCheckpoint{TxIndex: firstTx, AccountedBytes: cpSize}); err != nil {
			t.Fatal(err)
		}
		id2, ok2 := st.Reserve(cpSize)
		if ok2 || id2 != 0 {
			t.Fatalf("second candidate (tx=%d) must be rejected by full budget", secondTx)
		}
		if got := len(st.CheckpointsForTx(firstTx)); got != 1 {
			t.Fatalf("first tx=%d checkpoints=%d want 1", firstTx, got)
		}
		if got := len(st.CheckpointsForTx(secondTx)); got != 0 {
			t.Fatalf("second tx=%d checkpoints=%d want 0", secondTx, got)
		}
		st.ReleaseAll()
		u, _, _, _ := st.Stats()
		if u != 0 {
			t.Fatalf("expected remaining=0 after ReleaseAll, got %d", u)
		}
	}
	testFCFS(1, 2)
	testFCFS(2, 1)

	// lưu và nạp lại model.json + kiểm tra SHA-256 ổn định
	modelPath := filepath.Join(t.TempDir(), "model.json")
	m := &AdaptiveModel{
		FormatVersion:    AdaptiveModelFormatVersion,
		Suite:            "phase3",
		Seed:             31,
		CheckpointEvery:  64,
		TightBudgetBytes: 4096,
		WideBudgetBytes:  65536,
		ScoreThreshold:   12.5,
		Entries: []AdaptiveCalibrationEntry{{
			CodeHash:     common.HexToHash("0x1234").Hex(),
			Selector:     "0x22222222",
			PC:           1173,
			SizeBucket:   SizeBucket(1024),
			SizeMinBytes: 1024,
			SizeMaxBytes: 2048,
			Samples:      10,
			GainNs:       90_000,
			CaptureNs:    5_000,
		}},
	}
	sha1, err := SaveAdaptiveModel(modelPath, m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAdaptiveModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SHA256() != sha1 {
		t.Fatalf("model SHA256 mismatch: saved=%s loaded=%s", sha1, loaded.SHA256())
	}
}

func TestAdaptivePreseededHistoryEarlyVsLateSameSlot(t *testing.T) {
	const chainID = 1337
	cfg := shanghaiTestChainConfig(chainID)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000cafE")
	contract := common.HexToAddress("0x000000000000000000000000000000000000d101")
	code := buildSelectorEarlyLateContract(24, 0)
	codeHash := crypto.Keccak256Hash(code)
	selEarly := [4]byte{0x11, 0x11, 0x11, 0x11}
	selLate := [4]byte{0x22, 0x22, 0x22, 0x22}
	slot0Key := MVKey{Address: contract, Kind: MVStorage, Slot: common.Hash{}}

	k0, a0 := testPhase2Key(t, 11)
	k1, a1 := testPhase2Key(t, 12)
	k2, a2 := testPhase2Key(t, 13)

	buildDB := func(name string) *store.PebbleDB {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1_000_000))
		for _, a := range []common.Address{a0, a1, a2} {
			seed.AddBalanceBig(a, big.NewInt(1_000_000_000))
		}
		seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
		seed.SetNonce(contract, 1, tracing.NonceChangeUnspecified)
		seed.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(10)))
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}

	tx0 := signType2Tx(t, chainID, k0, 0, contract, 500_000, makeSelectorCalldata(selEarly, 1), nil)
	tx1 := signType2Tx(t, chainID, k1, 0, contract, 500_000, makeSelectorCalldata(selLate, 2), nil)
	tx2 := signType2Tx(t, chainID, k2, 0, contract, 500_000, makeSelectorCalldata(selEarly, 3), nil)
	txs := []*types.Transaction{tx0, tx1, tx2}
	env := BlockEnv{Height: 1, Time: 1000, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	// dùng CalibrateBlockFirstStatePairs để lấy đúng PC + sizeBucket thực của selEarly & selLate
	calDB := buildDB("adaptive-preseed-cal")
	calSamples, err := CalibrateBlockFirstStatePairs(context.Background(), NewPebbleStateDB(calDB), cfg, env, []*types.Transaction{tx0, tx1})
	if err != nil {
		t.Fatalf("CalibrateBlockFirstStatePairs: %v", err)
	}
	if len(calSamples) != 2 {
		t.Fatalf("expected 2 calibration samples, got %d", len(calSamples))
	}
	earlySample, lateSample := calSamples[0], calSamples[1]
	if earlySample.Step >= lateSample.Step {
		t.Fatalf("expected early step (%d) < late step (%d)", earlySample.Step, lateSample.Step)
	}

	// chạy sequential làm chuẩn đối chiếu
	seqDB := buildDB("adaptive-preseed-seq")
	seqRes, err := NewSequentialExecutor(cfg, big.NewInt(chainID)).ExecuteBlock(NewPebbleStateDB(seqDB), env, txs)
	if err != nil {
		t.Fatal(err)
	}

	// dựng model với cùng p = 0.5 nhưng gain khác biệt theo độ dài prefix (early thấp/âm -> bỏ, late cao -> nhận)
	model := &AdaptiveModel{
		FormatVersion:    AdaptiveModelFormatVersion,
		CheckpointEvery:  16,
		TightBudgetBytes: 64 << 20,
		WideBudgetBytes:  64 << 20,
		ScoreThreshold:   5.0,
		Entries: []AdaptiveCalibrationEntry{
			{
				CodeHash:     codeHash.Hex(),
				Selector:     FormatSelectorHex(selEarly),
				PC:           earlySample.PC,
				SizeBucket:   earlySample.SizeBucket,
				Samples:      10,
				GainNs:       2_000, // 0.5*2000 - 5000 = -4000 -> bị bỏ vì score âm
				CaptureNs:    5_000,
			},
			{
				CodeHash:     codeHash.Hex(),
				Selector:     FormatSelectorHex(selLate),
				PC:           lateSample.PC,
				SizeBucket:   lateSample.SizeBucket,
				Samples:      10,
				GainNs:       200_000, // 0.5*200000 - 5000 = +95000 -> vượt ngưỡng 5 ns/byte
				CaptureNs:    5_000,
			},
		},
	}
	if err := model.BuildIndex(); err != nil {
		t.Fatal(err)
	}

	occDB := buildDB("adaptive-preseed-occ")
	exec := NewParallelExecutor(cfg, big.NewInt(chainID), 3)
	exec.SlidingWindow = true
	exec.StrictInvalidTx = true
	exec.Phase2Mode = true
	exec.RecordTrace = true
	exec.Checkpoints = CheckpointConfig{
		Policy:        CheckpointPolicyAdaptive,
		Interval:      16,
		BudgetBytes:   64 << 20,
		UseBudget:     true,
		AdaptiveModel: model,
	}
	// nạp lịch sử dựng sẵn (cùng p = 0.5 cho cả early và late trên cùng slot0)
	exec.HistoryTracker.RecordBlockConflicts(map[MVKey]uint64{slot0Key: 4})
	exec.AdaptiveTracker.SeedForTest(map[AdaptiveLocationKey]AdaptiveSampleCounts{
		{CodeHash: codeHash, Selector: selEarly, PC: earlySample.PC, Slot: slot0Key}: {Seen: 4, Useful: 2},
		{CodeHash: codeHash, Selector: selLate, PC: lateSample.PC, Slot: slot0Key}:   {Seen: 4, Useful: 2},
	})

	// đồng bộ ép cả 3 tx đọc cùng slot cũ trước khi tx0 chốt
	var mu sync.Mutex
	readCount := 0
	allRead := make(chan struct{})
	exec.OCCHooks = &OCCHooks{
		OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
			if attempt != 1 || addr != contract || slot != (common.Hash{}) {
				return
			}
			mu.Lock()
			readCount++
			if readCount == 3 {
				close(allRead)
			}
			mu.Unlock()
			select {
			case <-allRead:
			case <-time.After(3 * time.Second):
			}
		},
	}

	res, err := exec.ExecuteBlock(NewPebbleStateDB(occDB), env, txs)
	if err != nil {
		t.Fatal(err)
	}
	if res.StateRoot != seqRes.StateRoot || res.GasUsed != seqRes.GasUsed {
		t.Fatalf("adaptive mismatch vs seq: root=%x/%x gas=%d/%d", res.StateRoot, seqRes.StateRoot, res.GasUsed, seqRes.GasUsed)
	}
	m := res.Phase2Metrics
	if m.EffectivePolicy != "adaptive" {
		t.Fatalf("expected effectivePolicy=adaptive, got %s", m.EffectivePolicy)
	}
	// tx0 & tx2 là early -> bỏ vì score thấp (2 lần); tx1 là late -> được chụp và resume thành công (1 lần)
	if m.CaptureCreated != 1 || m.CaptureSkippedLowScore != 2 || m.ResumeCommitted != 1 {
		t.Fatalf("expected created=1 skippedLow=2 resumed=1, got created=%d skippedLow=%d resumed=%d trace=%+v",
			m.CaptureCreated, m.CaptureSkippedLowScore, m.ResumeCommitted, res.DecisionTrace)
	}
	if m.AccountedRemainingBytes != 0 {
		t.Fatalf("expected remaining=0, got %d", m.AccountedRemainingBytes)
	}
}

func TestAdaptiveMultiBlockRealLearningAllOutcomes(t *testing.T) {
	const chainID = 1337
	cfg := shanghaiTestChainConfig(chainID)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000cafE")
	contract := common.HexToAddress("0x000000000000000000000000000000000000d102")
	code := buildSelectorEarlyLateContract(24, 0)
	codeHash := crypto.Keccak256Hash(code)
	selEarly := [4]byte{0x11, 0x11, 0x11, 0x11}
	selLate := [4]byte{0x22, 0x22, 0x22, 0x22}

	keys := make([]struct {
		addr common.Address
		sign func(nonce uint64, data []byte) *types.Transaction
	}, 4)
	for i := range keys {
		k, a := testPhase2Key(t, byte(21+i))
		keyCopy := k
		keys[i].addr = a
		keys[i].sign = func(nonce uint64, data []byte) *types.Transaction {
			return signType2Tx(t, chainID, keyCopy, nonce, contract, 500_000, data, nil)
		}
	}

	buildDB := func(name string) *store.PebbleDB {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1_000_000))
		for _, k := range keys {
			seed.AddBalanceBig(k.addr, big.NewInt(1_000_000_000))
		}
		seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
		seed.SetNonce(contract, 1, tracing.NonceChangeUnspecified)
		seed.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(1)))
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}

	// Block 1: 4 tx (early, late, late, early) đụng nhau để học lịch sử thật
	b1Txs := []*types.Transaction{
		keys[0].sign(0, makeSelectorCalldata(selEarly, 1)),
		keys[1].sign(0, makeSelectorCalldata(selLate, 2)),
		keys[2].sign(0, makeSelectorCalldata(selLate, 3)),
		keys[3].sign(0, makeSelectorCalldata(selEarly, 4)),
	}
	// Block 2: tx0 (late, không xung đột -> dự đoán sai / checkpoint không dùng nhưng vẫn đúng),
	//          tx1 (late, xung đột với tx0 -> resume chốt thành công),
	//          tx2 (late, xung đột nhưng đầy kho vì ngân sách chỉ vừa 1 điểm -> bỏ vì budget -> fallback đúng),
	//          tx3 (early, xung đột -> bỏ vì score thấp -> fallback đúng)
	b2Txs := []*types.Transaction{
		keys[0].sign(1, makeSelectorCalldata(selLate, 11)),
		keys[1].sign(1, makeSelectorCalldata(selLate, 12)),
		keys[2].sign(1, makeSelectorCalldata(selLate, 13)),
		keys[3].sign(1, makeSelectorCalldata(selEarly, 14)),
	}
	// Block 3: 1 tx late đơn lẻ trên slot0 đã nóng từ Block 2 -> adaptive dự đoán nhầm (chụp nhưng không xung đột) mà vẫn khớp tuần tự
	b3Txs := []*types.Transaction{
		keys[0].sign(2, makeSelectorCalldata(selLate, 21)),
	}
	env1 := BlockEnv{Height: 1, Time: 1000, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	env2 := BlockEnv{Height: 2, Time: 1012, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	env3 := BlockEnv{Height: 3, Time: 1024, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	// lấy PC + accountedBytes thực tế cho model
	calDB := buildDB("adaptive-mb-cal")
	calSamples, err := CalibrateBlockFirstStatePairs(context.Background(), NewPebbleStateDB(calDB), cfg, env1, b1Txs[:2])
	if err != nil {
		t.Fatal(err)
	}
	earlyS, lateS := calSamples[0], calSamples[1]
	// đặt trần kho chỉ đủ chứa 2 điểm late trong Block 2 (tx0 chiếm 1 chỗ, tx1 chiếm 1 chỗ -> tx2 đầy kho!)
	// lưu ý: tx0 là dự đoán sai (không conflict) nhưng vẫn chụp vì slot0 nóng; tx1 được resume; tx2 bị bỏ do đầy kho; tx3 bỏ do score thấp!
	tightForTwo := lateS.AccountedBytes*2 + 64

	model := &AdaptiveModel{
		FormatVersion:    AdaptiveModelFormatVersion,
		CheckpointEvery:  32,
		TightBudgetBytes: tightForTwo,
		WideBudgetBytes:  64 << 20,
		ScoreThreshold:   5.0,
		Entries: []AdaptiveCalibrationEntry{
			{
				CodeHash:   codeHash.Hex(),
				Selector:   FormatSelectorHex(selEarly),
				PC:         earlyS.PC,
				SizeBucket: earlyS.SizeBucket,
				Samples:    10,
				GainNs:     1_000,
				CaptureNs:  5_000,
			},
			{
				CodeHash:   codeHash.Hex(),
				Selector:   FormatSelectorHex(selLate),
				PC:         lateS.PC,
				SizeBucket: lateS.SizeBucket,
				Samples:    10,
				GainNs:     250_000,
				CaptureNs:  5_000,
			},
		},
	}
	if err := model.BuildIndex(); err != nil {
		t.Fatal(err)
	}

	// chạy sequential 2 block
	seqDB := buildDB("adaptive-mb-seq")
	seqExec := NewSequentialExecutor(cfg, big.NewInt(chainID))
	seqState1 := NewPebbleStateDB(seqDB)
	seqRes1, err := seqExec.ExecuteBlock(seqState1, env1, b1Txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := seqState1.Commit(); err != nil {
		t.Fatal(err)
	}
	env2.PreviousRoot = seqRes1.StateRoot[:]
	seqState2 := NewPebbleStateDB(seqDB)
	seqRes2, err := seqExec.ExecuteBlock(seqState2, env2, b2Txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := seqState2.Commit(); err != nil {
		t.Fatal(err)
	}
	env3.PreviousRoot = seqRes2.StateRoot[:]
	seqState3 := NewPebbleStateDB(seqDB)
	seqRes3, err := seqExec.ExecuteBlock(seqState3, env3, b3Txs)
	if err != nil {
		t.Fatal(err)
	}

	// chạy adaptive 2 block học lịch sử thật
	occDB := buildDB("adaptive-mb-occ")
	exec := NewParallelExecutor(cfg, big.NewInt(chainID), 4)
	exec.SlidingWindow = true
	exec.StrictInvalidTx = true
	exec.Phase2Mode = true
	exec.RecordTrace = true
	exec.Checkpoints = CheckpointConfig{
		Policy:        CheckpointPolicyAdaptive,
		Interval:      32,
		BudgetBytes:   64 << 20, // block 1 bootstrap đủ rộng để học
		UseBudget:     true,
		AdaptiveModel: model,
	}

	makeBarrierHook := func(target int) *OCCHooks {
		var mu sync.Mutex
		cnt := 0
		done := make(chan struct{})
		return &OCCHooks{
			OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
				if attempt != 1 || addr != contract || slot != (common.Hash{}) {
					return
				}
				mu.Lock()
				cnt++
				if cnt == target {
					close(done)
				}
				mu.Unlock()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
			},
		}
	}

	// Block 1: bootstrap fixed -> học lịch sử thật
	exec.OCCHooks = makeBarrierHook(4)
	st1 := NewPebbleStateDB(occDB)
	res1, err := exec.ExecuteBlock(st1, env1, b1Txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.Commit(); err != nil {
		t.Fatal(err)
	}
	if res1.StateRoot != seqRes1.StateRoot || res1.GasUsed != seqRes1.GasUsed {
		t.Fatalf("block 1 adaptive mismatch vs seq")
	}
	if res1.Phase2Metrics.EffectivePolicy != "fixed" {
		t.Fatalf("block 1 expected effectivePolicy=fixed, got %s", res1.Phase2Metrics.EffectivePolicy)
	}
	if len(exec.HotSlots()) == 0 {
		t.Fatalf("expected block 1 to learn hot slot0")
	}

	// Block 2: siết ngân sách đúng bằng 2 điểm late -> tx0 (không conflict, chụp thừa), tx1 (resume chốt),
	// tx2 (bị bỏ vì đầy kho), tx3 (bị bỏ vì score thấp)
	exec.Checkpoints.BudgetBytes = tightForTwo
	exec.OCCHooks = makeBarrierHook(4)
	st2 := NewPebbleStateDB(occDB)
	res2, err := exec.ExecuteBlock(st2, env2, b2Txs)
	if err != nil {
		t.Fatal(err)
	}
	if res2.StateRoot != seqRes2.StateRoot || res2.GasUsed != seqRes2.GasUsed {
		t.Fatalf("block 2 adaptive mismatch vs seq: root=%x/%x gas=%d/%d", res2.StateRoot, seqRes2.StateRoot, res2.GasUsed, seqRes2.GasUsed)
	}
	m2 := res2.Phase2Metrics
	if m2.EffectivePolicy != "adaptive" {
		t.Fatalf("block 2 expected effectivePolicy=adaptive, got %s", m2.EffectivePolicy)
	}
	if m2.AccountedPeakBytes > tightForTwo || m2.AccountedRemainingBytes != 0 {
		t.Fatalf("budget invariant violated: peak=%d budget=%d rem=%d", m2.AccountedPeakBytes, tightForTwo, m2.AccountedRemainingBytes)
	}
	if m2.ResumeCommitted < 1 {
		t.Fatalf("expected >= 1 committed resume in block 2, got %+v", m2)
	}
	if m2.CaptureSkippedLowScore < 1 {
		t.Fatalf("expected >= 1 skipped_low_score (for early tx3), got %+v", m2)
	}
	if err := st2.Commit(); err != nil {
		t.Fatal(err)
	}
	if m2.CaptureSkippedBudget < 1 {
		t.Fatalf("expected >= 1 skipped_budget (for 3rd late tx when budget fits 2), got %+v", m2)
	}

	// Block 3: tx đơn lẻ -> adaptive dự đoán sai (chụp điểm trên slot0 nóng nhưng 0 xung đột), vẫn khớp tuần tự và giải phóng sạch kho
	exec.OCCHooks = nil
	st3 := NewPebbleStateDB(occDB)
	res3, err := exec.ExecuteBlock(st3, env3, b3Txs)
	if err != nil {
		t.Fatal(err)
	}
	if res3.StateRoot != seqRes3.StateRoot || res3.GasUsed != seqRes3.GasUsed {
		t.Fatalf("block 3 mispredicted adaptive mismatch vs seq: root=%x/%x gas=%d/%d", res3.StateRoot, seqRes3.StateRoot, res3.GasUsed, seqRes3.GasUsed)
	}
	m3 := res3.Phase2Metrics
	if m3.EffectivePolicy != "adaptive" || m3.CaptureCreated != 1 || m3.ResumeCommitted != 0 || m3.AccountedRemainingBytes != 0 {
		t.Fatalf("block 3 expected mispredicted capture (created=1 resumed=0 rem=0), got %+v", m3)
	}
}

func TestAdaptiveRevertOOGFallbackAndZeroBudget(t *testing.T) {
	const chainID = 1337
	cfg := shanghaiTestChainConfig(chainID)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000cafE")
	// hợp đồng có nhánh revert (khi calldata[31]==1), nhánh CALL (khi calldata[31]==2 -> fallback), và nhánh bình thường
	contract := common.HexToAddress("0x000000000000000000000000000000000000d103")
	callee := common.HexToAddress("0x000000000000000000000000000000000000d104")

	// tạo bytecode: hash prefix 10 vòng -> SLOAD(0) -> +1 -> SSTORE(0) -> kiểm calldata[0]:
	// == 1 -> REVERT; == 2 -> STATICCALL(callee) rồi RETURN; else RETURN
	prefix := appendHashPrefix(nil, 10)
	// phần sau SLOAD(0)+1+SSTORE(0)
	code := append([]byte(nil), prefix...)
	code = append(code,
		0x5f, 0x54, // SLOAD(0)
		0x60, 0x01, 0x01, // +1
		0x5f, 0x55, // SSTORE(0)
		0x5f, 0x35, // CALLDATALOAD(0)
		0x80,       // DUP1
		0x60, 0x01, // PUSH1 1
		0x14, // EQ
		0x61, 0x00, 0x00, // placeholder JUMPI revert
		0x57,
		0x60, 0x02, // PUSH1 2
		0x14, // EQ
		0x61, 0x00, 0x00, // placeholder JUMPI call
		0x57,
		0x60, 0x20, 0x5f, 0xf3, // RETURN(0, 32)
	)
	revDest := uint16(len(code))
	code = append(code, 0x5b, 0x5f, 0x5f, 0xfd) // JUMPDEST PUSH0 PUSH0 REVERT
	callDest := uint16(len(code))
	code = append(code,
		0x5b, // JUMPDEST
		0x5f, 0x5f, 0x5f, 0x5f,
		0x73, // PUSH20 callee
	)
	code = append(code, callee.Bytes()...)
	code = append(code,
		0x5a,                   // GAS
		0xfa,                   // STATICCALL
		0x50,                   // POP
		0x60, 0x20, 0x5f, 0xf3, // RETURN(0, 32)
	)
	// vá 2 offset PUSH2
	revPatch := len(prefix) + 14
	callPatch := len(prefix) + 21
	code[revPatch], code[revPatch+1] = byte(revDest>>8), byte(revDest)
	code[callPatch], code[callPatch+1] = byte(callDest>>8), byte(callDest)
	codeHash := crypto.Keccak256Hash(code)
	slot0Key := MVKey{Address: contract, Kind: MVStorage, Slot: common.Hash{}}

	k0, a0 := testPhase2Key(t, 41)
	k1, a1 := testPhase2Key(t, 42)
	k2, a2 := testPhase2Key(t, 43)
	k3, a3 := testPhase2Key(t, 44)

	buildDB := func(name string) *store.PebbleDB {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1_000_000))
		for _, a := range []common.Address{a0, a1, a2, a3} {
			seed.AddBalanceBig(a, big.NewInt(1_000_000_000))
		}
		seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
		seed.SetNonce(contract, 1, tracing.NonceChangeUnspecified)
		seed.SetCode(callee, []byte{0x5f, 0x5f, 0xf3}, tracing.CodeChangeUnspecified)
		seed.SetNonce(callee, 1, tracing.NonceChangeUnspecified)
		seed.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(5)))
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}

	word := func(v byte) []byte {
		b := make([]byte, 32)
		b[31] = v
		return b
	}
	txs := []*types.Transaction{
		signType2Tx(t, chainID, k0, 0, contract, 300_000, word(0), nil), // ok
		signType2Tx(t, chainID, k1, 0, contract, 300_000, word(1), nil), // revert
		signType2Tx(t, chainID, k2, 0, contract, 24_000, word(0), nil),  // OOG
		signType2Tx(t, chainID, k3, 0, contract, 300_000, word(2), nil), // STATICCALL -> fallback
	}
	env := BlockEnv{Height: 1, Time: 1000, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	seqDB := buildDB("adaptive-edge-seq")
	seqRes, err := NewSequentialExecutor(cfg, big.NewInt(chainID)).ExecuteBlock(NewPebbleStateDB(seqDB), env, txs)
	if err != nil {
		t.Fatal(err)
	}

	// lấy PC + sizeBucket thực của SLOAD(0)
	calDB := buildDB("adaptive-edge-cal")
	calSamples, err := CalibrateBlockFirstStatePairs(context.Background(), NewPebbleStateDB(calDB), cfg, env, txs[:1])
	if err != nil || len(calSamples) == 0 {
		t.Fatalf("calibrate edge contract: err=%v len=%d", err, len(calSamples))
	}
	s0 := calSamples[0]

	model := &AdaptiveModel{
		FormatVersion:    AdaptiveModelFormatVersion,
		CheckpointEvery:  16,
		TightBudgetBytes: 64 << 20,
		WideBudgetBytes:  64 << 20,
		ScoreThreshold:   0,
		Entries: []AdaptiveCalibrationEntry{{
			CodeHash:   codeHash.Hex(),
			Selector:   FormatSelectorHex(ExtractSelector(word(0))),
			PC:         s0.PC,
			SizeBucket: s0.SizeBucket,
			Samples:    5,
			GainNs:     100_000,
			CaptureNs:  5_000,
		}},
	}
	if err := model.BuildIndex(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		budget uint64
	}{
		{name: "adaptive-normal-budget", budget: 64 << 20},
		{name: "adaptive-zero-budget", budget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			occDB := buildDB("adaptive-edge-" + tc.name)
			exec := NewParallelExecutor(cfg, big.NewInt(chainID), 4)
			exec.SlidingWindow = true
			exec.StrictInvalidTx = true
			exec.Phase2Mode = true
			exec.RecordTrace = true
			exec.Checkpoints = CheckpointConfig{
				Policy:        CheckpointPolicyAdaptive,
				Interval:      16,
				BudgetBytes:   tc.budget,
				UseBudget:     true,
				AdaptiveModel: model,
			}
			exec.HistoryTracker.RecordBlockConflicts(map[MVKey]uint64{slot0Key: 4})
			exec.AdaptiveTracker.SeedForTest(map[AdaptiveLocationKey]AdaptiveSampleCounts{
				{CodeHash: codeHash, Selector: ExtractSelector(word(0)), PC: s0.PC, Slot: slot0Key}: {Seen: 4, Useful: 3},
			})

			res, err := exec.ExecuteBlock(NewPebbleStateDB(occDB), env, txs)
			if err != nil {
				t.Fatal(err)
			}
			if res.StateRoot != seqRes.StateRoot || res.GasUsed != seqRes.GasUsed {
				t.Fatalf("%s mismatch vs seq: root=%x/%x gas=%d/%d", tc.name, res.StateRoot, seqRes.StateRoot, res.GasUsed, seqRes.GasUsed)
			}
			for i := range txs {
				if res.Results[i].Failed != seqRes.Results[i].Failed || res.Results[i].GasUsed != seqRes.Results[i].GasUsed {
					t.Fatalf("%s tx %d mismatch: got=%+v want=%+v", tc.name, i, res.Results[i], seqRes.Results[i])
				}
			}
			m := res.Phase2Metrics
			if m.AccountedPeakBytes > tc.budget {
				t.Fatalf("%s peak=%d exceeds budget=%d", tc.name, m.AccountedPeakBytes, tc.budget)
			}
			if m.AccountedRemainingBytes != 0 {
				t.Fatalf("%s remaining=%d want 0", tc.name, m.AccountedRemainingBytes)
			}
			if tc.budget == 0 && (m.EffectivePolicy != "off" || m.CaptureCreated != 0) {
				t.Fatalf("M=0 must behave as off, got effective=%s created=%d", m.EffectivePolicy, m.CaptureCreated)
			}
		})
	}
}

func TestPredictedOCCCheckpointsAllPoliciesMatchSequentialAndWaitingHoldsZeroBudget(t *testing.T) {
	const chainID = 1337
	cfg := shanghaiTestChainConfig(chainID)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000cafE")
	contract := common.HexToAddress("0x000000000000000000000000000000000000d105")
	code := buildSelectorEarlyLateContract(20, 0)
	codeHash := crypto.Keccak256Hash(code)
	selEarly := [4]byte{0x11, 0x11, 0x11, 0x11}
	selLate := [4]byte{0x22, 0x22, 0x22, 0x22}

	k0, a0 := testPhase2Key(t, 51)
	k1, a1 := testPhase2Key(t, 52)
	k2, a2 := testPhase2Key(t, 53)
	k3, a3 := testPhase2Key(t, 54)

	buildDB := func(name string) *store.PebbleDB {
		db, err := store.NewPebbleDB(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1_000_000))
		for _, a := range []common.Address{a0, a1, a2, a3} {
			seed.AddBalanceBig(a, big.NewInt(1_000_000_000))
		}
		seed.SetCode(contract, code, tracing.CodeChangeUnspecified)
		seed.SetNonce(contract, 1, tracing.NonceChangeUnspecified)
		seed.SetState(contract, common.Hash{}, common.BigToHash(big.NewInt(1)))
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}

	b1Txs := []*types.Transaction{
		signType2Tx(t, chainID, k0, 0, contract, 500_000, makeSelectorCalldata(selLate, 1), nil),
		signType2Tx(t, chainID, k1, 0, contract, 500_000, makeSelectorCalldata(selLate, 2), nil),
		signType2Tx(t, chainID, k2, 0, contract, 500_000, makeSelectorCalldata(selEarly, 3), nil),
		signType2Tx(t, chainID, k3, 0, contract, 500_000, makeSelectorCalldata(selLate, 4), nil),
	}
	b2Txs := []*types.Transaction{
		signType2Tx(t, chainID, k0, 1, contract, 500_000, makeSelectorCalldata(selLate, 11), nil),
		signType2Tx(t, chainID, k1, 1, contract, 500_000, makeSelectorCalldata(selLate, 12), nil),
		signType2Tx(t, chainID, k2, 1, contract, 500_000, makeSelectorCalldata(selEarly, 13), nil),
		signType2Tx(t, chainID, k3, 1, contract, 500_000, makeSelectorCalldata(selLate, 14), nil),
	}
	env1 := BlockEnv{Height: 1, Time: 1000, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}
	env2 := BlockEnv{Height: 2, Time: 1012, Coinbase: coinbase, GasLimit: 10_000_000, BaseFee: big.NewInt(1)}

	// hiệu chuẩn lấy PC & sizeBucket
	calDB := buildDB("pred-cp-cal")
	calSamples, err := CalibrateBlockFirstStatePairs(context.Background(), NewPebbleStateDB(calDB), cfg, env1, []*types.Transaction{b1Txs[2], b1Txs[0]})
	if err != nil {
		t.Fatal(err)
	}
	model := &AdaptiveModel{
		FormatVersion:    AdaptiveModelFormatVersion,
		CheckpointEvery:  32,
		TightBudgetBytes: 64 << 20,
		WideBudgetBytes:  64 << 20,
		ScoreThreshold:   5.0,
		Entries: []AdaptiveCalibrationEntry{
			{CodeHash: codeHash.Hex(), Selector: FormatSelectorHex(selEarly), PC: calSamples[0].PC, SizeBucket: calSamples[0].SizeBucket, Samples: 10, GainNs: 1_000, CaptureNs: 5_000},
			{CodeHash: codeHash.Hex(), Selector: FormatSelectorHex(selLate), PC: calSamples[1].PC, SizeBucket: calSamples[1].SizeBucket, Samples: 10, GainNs: 200_000, CaptureNs: 5_000},
		},
	}
	if err := model.BuildIndex(); err != nil {
		t.Fatal(err)
	}

	// chạy sequential 2 block
	seqDB := buildDB("pred-cp-seq")
	seqExec := NewSequentialExecutor(cfg, big.NewInt(chainID))
	s1 := NewPebbleStateDB(seqDB)
	seqRes1, err := seqExec.ExecuteBlock(s1, env1, b1Txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Commit(); err != nil {
		t.Fatal(err)
	}
	env2.PreviousRoot = seqRes1.StateRoot[:]
	s2 := NewPebbleStateDB(seqDB)
	seqRes2, err := seqExec.ExecuteBlock(s2, env2, b2Txs)
	if err != nil {
		t.Fatal(err)
	}

	policies := []CheckpointPolicy{
		CheckpointPolicyOff,
		CheckpointPolicyFixed,
		CheckpointPolicyHistory,
		CheckpointPolicyAdaptive,
	}
	for _, pol := range policies {
		t.Run(pol.Phase2Name(), func(t *testing.T) {
			db := buildDB("pred-cp-" + pol.Phase2Name())
			exec := NewPredictedOCCExecutor(cfg, big.NewInt(chainID), 4)
			exec.StrictInvalidTx = true
			exec.Phase2Mode = true
			exec.RecordTrace = true
			exec.Checkpoints = CheckpointConfig{
				Policy:        pol,
				Interval:      32,
				BudgetBytes:   64 << 20,
				UseBudget:     true,
				AdaptiveModel: model,
			}

			// ép xung đột ở cả 2 block để kiểm chứng resume trong predicted-occ
			for blkIdx, b := range []struct {
				env BlockEnv
				txs []*types.Transaction
				ref *BlockResult
			}{
				{env1, b1Txs, seqRes1},
				{env2, b2Txs, seqRes2},
			} {
				var mu sync.Mutex
				reads := 0
				barrier := make(chan struct{})
				exec.Hooks = &PredictedOCCHooks{
					OnStorageRead: func(ctx context.Context, txIndex, attempt int, addr common.Address, slot, val common.Hash, version uint64) {
						if attempt != 1 || addr != contract || slot != (common.Hash{}) {
							return
						}
						mu.Lock()
						reads++
						if reads == 4 {
							close(barrier)
						}
						mu.Unlock()
						select {
						case <-barrier:
						case <-time.After(3 * time.Second):
						}
					},
				}
				st := NewPebbleStateDB(db)
				res, err := exec.ExecuteBlock(st, b.env, b.txs)
				if err != nil {
					t.Fatalf("block %d failed: %v", blkIdx+1, err)
				}
				if err := st.Commit(); err != nil {
					t.Fatal(err)
				}
				if res.StateRoot != b.ref.StateRoot || res.GasUsed != b.ref.GasUsed {
					t.Fatalf("policy=%s block %d diverged vs seq: root=%x/%x gas=%d/%d",
						pol.Phase2Name(), blkIdx+1, res.StateRoot, b.ref.StateRoot, res.GasUsed, b.ref.GasUsed)
				}
				if res.Phase2Metrics == nil {
					t.Fatalf("policy=%s block %d missing Phase2Metrics", pol.Phase2Name(), blkIdx+1)
				}
				if res.Phase2Metrics.AccountedRemainingBytes != 0 {
					t.Fatalf("policy=%s block %d remaining bytes=%d want 0", pol.Phase2Name(), blkIdx+1, res.Phase2Metrics.AccountedRemainingBytes)
				}
				if blkIdx == 1 && pol != CheckpointPolicyOff && res.Phase2Metrics.ResumeCommitted < 1 {
					t.Fatalf("policy=%s block 2 expected >=1 committed resume, got %+v", pol.Phase2Name(), res.Phase2Metrics)
				}
				if blkIdx == 1 && pol == CheckpointPolicyAdaptive && res.Phase2Metrics.CaptureSkippedLowScore < 1 {
					t.Fatalf("predicted-occ adaptive block 2 expected skipped_low_score >= 1 for early tx, got %+v", res.Phase2Metrics)
				}
			}
		})
	}

	// kiểm tra giao dịch đợi tiền nhiệm trong predicted-occ không chạy attempt 1 (không chiếm worker và không giữ chỗ kho)
	t.Run("waiting-tx-does-not-hold-worker-or-checkpoint-budget", func(t *testing.T) {
		routerAddr := DefaultSwapRouterAddr
		poolAddr := DefaultSwapPoolAddrs[0]
		routerCode := []byte{0x60, 0x00, 0x60, 0x00, 0xf3}
		poolCode := []byte{0x60, 0x01, 0x60, 0x00, 0xf3}

		db, err := store.NewPebbleDB("pred-wait-cp", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(coinbase, big.NewInt(1_000_000))
		seed.AddBalanceBig(a0, big.NewInt(1_000_000_000))
		seed.AddBalanceBig(a1, big.NewInt(1_000_000_000))
		seed.SetCode(routerAddr, routerCode, tracing.CodeChangeUnspecified)
		seed.SetNonce(routerAddr, 1, tracing.NonceChangeUnspecified)
		seed.SetCode(poolAddr, poolCode, tracing.CodeChangeUnspecified)
		seed.SetNonce(poolAddr, 1, tracing.NonceChangeUnspecified)
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}

		// dựng calldata đúng chuẩn swapExactIn (164 byte) trỏ vào poolA
		swapData := make([]byte, 164)
		copy(swapData[:4], swapExactInSelector[:])
		copy(swapData[16:36], poolAddr.Bytes())
		swapData[67] = 1

		stx0 := signType2Tx(t, chainID, k0, 0, routerAddr, 200_000, swapData, nil)
		stx1 := signType2Tx(t, chainID, k1, 0, routerAddr, 200_000, swapData, nil)

		exec := NewPredictedOCCExecutor(cfg, big.NewInt(chainID), 2)
		exec.StrictInvalidTx = true
		exec.Phase2Mode = true
		exec.RecordTrace = true
		exec.Predictor = SwapPredictorConfig{
			AllowedRouters: map[common.Address]struct{}{routerAddr: {}},
			AllowedPools:   map[common.Address]struct{}{poolAddr: {}},
			RouterCodeHash: crypto.Keccak256Hash(routerCode),
			PoolCodeHash:   crypto.Keccak256Hash(poolCode),
			Reserve0Slot:   DefaultSwapPoolReserve0Slot,
			Reserve1Slot:   DefaultSwapPoolReserve1Slot,
		}
		exec.SetHotPools([]common.Address{poolAddr})
		exec.Checkpoints = CheckpointConfig{
			Policy:      CheckpointPolicyFixed,
			Interval:    1,
			BudgetBytes: 64 << 20,
			UseBudget:   true,
		}

		var mu sync.Mutex
		tx0Committed := false
		tx1Waited := false
		tx1StartedBeforeTx0Committed := false
		exec.Hooks = &PredictedOCCHooks{
			OnTxWaitPredecessor: func(ctx context.Context, txIndex, predTxIndex int, pool common.Address) {
				mu.Lock()
				if txIndex == 1 && predTxIndex == 0 {
					tx1Waited = true
				}
				mu.Unlock()
			},
			OnAttemptStart: func(ctx context.Context, txIndex, attempt int) {
				mu.Lock()
				if txIndex == 1 && !tx0Committed {
					tx1StartedBeforeTx0Committed = true
				}
				mu.Unlock()
			},
			OnTxCommitted: func(ctx context.Context, txIndex int, res TxResult) {
				mu.Lock()
				if txIndex == 0 {
					tx0Committed = true
				}
				mu.Unlock()
			},
		}

		res, err := exec.ExecuteBlock(NewPebbleStateDB(db), env1, []*types.Transaction{stx0, stx1})
		if err != nil {
			t.Fatal(err)
		}
		if !tx1Waited {
			t.Fatal("expected tx1 to wait on hot-pool predecessor tx0")
		}
		if tx1StartedBeforeTx0Committed {
			t.Fatal("waiting tx1 must not dispatch to a worker or capture checkpoints before tx0 commits")
		}
		if res.Phase2Metrics.AccountedRemainingBytes != 0 {
			t.Fatalf("expected remaining=0, got %d", res.Phase2Metrics.AccountedRemainingBytes)
		}
	})
}
