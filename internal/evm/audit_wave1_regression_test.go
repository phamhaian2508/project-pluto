package evm

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

func newTestShanghaiConfig(chainID *big.Int) *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	return &params.ChainConfig{
		ChainID:                 new(big.Int).Set(chainID),
		HomesteadBlock:          zero,
		EIP150Block:             zero,
		EIP155Block:             zero,
		EIP158Block:             zero,
		ByzantiumBlock:          zero,
		ConstantinopleBlock:     zero,
		PetersburgBlock:         zero,
		IstanbulBlock:           zero,
		MuirGlacierBlock:        zero,
		BerlinBlock:             zero,
		LondonBlock:             zero,
		ArrowGlacierBlock:       zero,
		GrayGlacierBlock:        zero,
		MergeNetsplitBlock:      zero,
		TerminalTotalDifficulty: zero,
		ShanghaiTime:            &shanghai,
	}
}

// TestResumeMemorySizeOverflowNoPanic kiểm chứng (requested+31)/32 không tràn uint64 gây panic ở biên [2^64-31, 2^64-1].
func TestResumeMemorySizeOverflowNoPanic(t *testing.T) {
	s := testStateDB(t)
	cfg := newTestShanghaiConfig(big.NewInt(1337))
	blockCtx := vm.BlockContext{
		CanTransfer: func(vm.StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(vm.StateDB, common.Address, common.Address, *uint256.Int) {},
		BlockNumber: big.NewInt(1),
		Time:        1,
		Difficulty:  new(big.Int),
		Random:      &common.Hash{},
		BaseFee:     new(big.Int),
		BlobBaseFee: new(big.Int),
		GasLimit:    30_000_000,
	}
	evmEnv := vm.NewEVM(blockCtx, s, cfg, vm.Config{})

	// MLOAD (0x51) đọc 32 byte tại offset = MaxUint64 - 40 => requested = MaxUint64 - 8 (trong khoảng [2^64-31, 2^64-1])
	code := []byte{byte(vm.MLOAD), byte(vm.STOP)}
	offset := uint256.NewInt(math.MaxUint64 - 40)
	frame := CallFrameState{
		PC:       0,
		Depth:    1,
		Caller:   common.HexToAddress("0x1111"),
		Address:  common.HexToAddress("0x2222"),
		CodeHash: crypto.Keccak256Hash(code),
		Code:     code,
		Gas:      1_000_000,
	}
	machine := InterpreterState{
		PC:         0,
		Stack:      []uint256.Int{*offset},
		Gas:        1_000_000,
		CallFrames: []CallFrameState{frame},
	}

	_, _, err := resumeGethFrameStrict(evmEnv, frame, machine, nil)
	if !errors.Is(err, vm.ErrGasUintOverflow) {
		t.Fatalf("resumeGethFrameStrict near uint64 boundary err = %v, want vm.ErrGasUintOverflow", err)
	}
}

// TestResumeAdapterErrorsWrapUnsupported đảm bảo lỗi nội bộ adapter/fork Cancun luôn bọc errResumeUnsupported để fallback FullReplay.
func TestResumeAdapterErrorsWrapUnsupported(t *testing.T) {
	frame := CallFrameState{Depth: 1, Gas: 100_000, Code: []byte{byte(vm.STOP)}}
	machine := InterpreterState{Gas: 100_000, CallFrames: []CallFrameState{frame}}

	// evmEnv nil -> lỗi gethJumpTable phải bọc errResumeUnsupported
	if _, _, err := resumeGethFrameStrict(nil, frame, machine, nil); !errors.Is(err, errResumeUnsupported) {
		t.Fatalf("nil evmEnv err = %v, want wrapping errResumeUnsupported", err)
	}

	// kiểm tra fail-closed: trường private không được hỗ trợ phải trả lỗi (không fallback reflect.NewAt)
	if err := setGethPrivateField(&vm.EVM{}, "nonexistentField", 123); err == nil {
		t.Fatal("setGethPrivateField unexpectedly accepted unknown field")
	}
	if _, err := loadGethFieldOffsets(); err != nil {
		t.Fatalf("loadGethFieldOffsets failed on pinned geth v1.16.8: %v", err)
	}

	// Cancun fork bị chặn trên resumePurePrefixAttempt -> rơi về FullReplay an toàn
	cancunTime := uint64(0)
	cancunCfg := &params.ChainConfig{
		ChainID:                 big.NewInt(1337),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		TerminalTotalDifficulty: big.NewInt(0),
		ShanghaiTime:            &cancunTime,
		CancunTime:              &cancunTime,
	}
	key, _ := crypto.HexToECDSA("00000000000000000000000000000000000000000000000000000000000000aa")
	to := common.HexToAddress("0x3333")
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 100_000, To: &to,
	}), types.LatestSignerForChainID(big.NewInt(1337)), key)
	if err != nil {
		t.Fatal(err)
	}
	s := testStateDB(t)
	cp := &PurePrefixCheckpoint{FormatVersion: PureCheckpointFormatVersion}
	_, reason, _, err := resumePurePrefixAttempt(0, 2, tx, &prefixSnapshot{state: s, remainingGas: 30_000_000}, BlockEnv{Height: 1, Time: 1, GasLimit: 30_000_000, BaseFee: big.NewInt(0)}, cancunCfg, types.LatestSignerForChainID(big.NewInt(1337)), cp)
	if !errors.Is(err, errResumeUnsupported) || reason != "unsupported_fork" {
		t.Fatalf("Cancun resume = (%q, %v), want (unsupported_fork, errResumeUnsupported)", reason, err)
	}
}

// TestSelfDestructRecreateSameBlockNoZombieState kiểm chứng SELFDESTRUCT rồi tái tạo cùng block không hồi sinh code/storage cũ.
func TestSelfDestructRecreateSameBlockNoZombieState(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := newTestShanghaiConfig(chainID)
	keyA, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000011")
	keyB, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000022")
	senderA := crypto.PubkeyToAddress(keyA.PublicKey)
	senderB := crypto.PubkeyToAddress(keyB.PublicKey)
	target := common.HexToAddress("0x000000000000000000000000000000000000c0de")
	slot := common.BigToHash(big.NewInt(7))
	oldVal := common.BigToHash(big.NewInt(0xdead))
	// CALLER SELFDESTRUCT (0x33 0xff)
	selfDestructCode := []byte{byte(vm.CALLER), byte(vm.SELFDESTRUCT)}

	signer := types.LatestSignerForChainID(chainID)
	tx1, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 200_000, To: &target,
	}), signer, keyA)
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 100_000, To: &target, Value: big.NewInt(1),
	}), signer, keyB)
	if err != nil {
		t.Fatal(err)
	}

	executors := []struct {
		name string
		make func() Executor
	}{
		{"sequential", func() Executor { return NewSequentialExecutor(cfg, chainID) }},
		{"occ", func() Executor {
			e := NewParallelExecutor(cfg, chainID, 4)
			e.SlidingWindow = true
			return e
		}},
		{"predicted-occ", func() Executor { return NewPredictedOCCExecutor(cfg, chainID, 4) }},
	}

	var refRoot [32]byte
	for idx, tc := range executors {
		db, err := store.NewPebbleDB("state", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })

		// Block 0: gieo contract có code + storage != 0 trong PebbleDB gốc
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(senderA, big.NewInt(1_000_000))
		seed.AddBalanceBig(senderB, big.NewInt(1_000_000))
		seed.CreateAccount(target)
		seed.SetNonce(target, 1, 0)
		seed.SetCode(target, selfDestructCode, 0)
		seed.SetState(target, slot, oldVal)
		seed.Finalise(true)
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}

		// Block 1: Tx1 SELFDESTRUCT(target), Tx2 gửi 1 wei tái tạo target cùng block
		s1 := NewPebbleStateDB(db)
		env := BlockEnv{Height: 1, Time: 1, GasLimit: 30_000_000, BaseFee: big.NewInt(0)}
		res, err := tc.make().ExecuteBlock(s1, env, []*types.Transaction{tx1, tx2})
		if err != nil {
			t.Fatalf("%s ExecuteBlock failed: %v", tc.name, err)
		}
		if res.Results[0].Failed || res.Results[0].Invalid || res.Results[1].Failed || res.Results[1].Invalid {
			t.Fatalf("%s unexpected tx failure: %+v", tc.name, res.Results)
		}
		if idx == 0 {
			refRoot = res.StateRoot
		} else if res.StateRoot != refRoot {
			t.Fatalf("%s StateRoot = %x, want %x", tc.name, res.StateRoot, refRoot)
		}

		// Kiểm tra ExportLogicalState trước commit không còn zombie code/storage
		exported, err := s1.ExportLogicalState()
		if err != nil {
			t.Fatalf("%s ExportLogicalState failed: %v", tc.name, err)
		}
		if len(exported.OrphanKeys) != 0 {
			t.Fatalf("%s ExportLogicalState has %d orphan keys", tc.name, len(exported.OrphanKeys))
		}
		la := exported.Accounts[target]
		if la == nil || la.Balance.Uint64() != 1 || len(la.Code) != 0 || len(la.Storage) != 0 {
			t.Fatalf("%s exported recreated account = %+v, want balance=1 empty code/storage", tc.name, la)
		}

		// Commit PendingWrites xuống Pebble và mở StateDB mới ở Block 2 để xác nhận không hồi sinh từ đĩa
		batch := db.NewBatch()
		for k, v := range res.Writes {
			if v == nil {
				if err := batch.Delete([]byte(k)); err != nil {
					t.Fatal(err)
				}
			} else if err := batch.Set([]byte(k), v); err != nil {
				t.Fatal(err)
			}
		}
		if err := batch.WriteSync(); err != nil {
			t.Fatal(err)
		}
		_ = batch.Close()

		s2 := NewPebbleStateDB(db)
		if got := s2.GetCode(target); len(got) != 0 {
			t.Fatalf("%s Block 2 resurrected zombie code %x", tc.name, got)
		}
		if got := s2.GetState(target, slot); got != (common.Hash{}) {
			t.Fatalf("%s Block 2 resurrected zombie storage %s", tc.name, got.Hex())
		}
		if got := s2.GetBalance(target); got.Uint64() != 1 {
			t.Fatalf("%s Block 2 recreated balance = %s, want 1", tc.name, got)
		}
	}
}

// TestCreateThenSelfDestructSameBlockAppHashParity kiểm chứng deploy rồi SELFDESTRUCT cùng block cho AppHash giống hệt giữa sequential và OCC/predicted-occ.
func TestCreateThenSelfDestructSameBlockAppHashParity(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := newTestShanghaiConfig(chainID)
	key, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000033")
	sender := crypto.PubkeyToAddress(key.PublicKey)
	deployedAddr := crypto.CreateAddress(sender, 0)

	// Initcode: SSTORE(slot=1, val=0x42), sau đó trả runtime bytecode `CALLER SELFDESTRUCT` (0x33 0xff)
	// PUSH1 0x42, PUSH1 0x01, SSTORE, PUSH2 0x33ff, PUSH1 0x00, MSTORE, PUSH1 0x02, PUSH1 0x1e, RETURN
	initCode := []byte{
		0x60, 0x42, 0x60, 0x01, 0x55,
		0x61, 0x33, 0xff, 0x60, 0x00, 0x52,
		0x60, 0x02, 0x60, 0x1e, 0xf3,
	}

	signer := types.LatestSignerForChainID(chainID)
	txDeploy, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 300_000, Data: initCode,
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}
	txDestroy, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 1, GasPrice: big.NewInt(0), Gas: 200_000, To: &deployedAddr,
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}

	executors := []struct {
		name string
		make func() Executor
	}{
		{"sequential", func() Executor { return NewSequentialExecutor(cfg, chainID) }},
		{"occ", func() Executor {
			e := NewParallelExecutor(cfg, chainID, 4)
			e.SlidingWindow = true
			return e
		}},
		{"predicted-occ", func() Executor { return NewPredictedOCCExecutor(cfg, chainID, 4) }},
	}

	var refRoot [32]byte
	for idx, tc := range executors {
		db, err := store.NewPebbleDB("state", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })

		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(sender, big.NewInt(1_000_000))
		seed.Finalise(true)
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}

		s1 := NewPebbleStateDB(db)
		env := BlockEnv{Height: 1, Time: 1, GasLimit: 30_000_000, BaseFee: big.NewInt(0)}
		res, err := tc.make().ExecuteBlock(s1, env, []*types.Transaction{txDeploy, txDestroy})
		if err != nil {
			t.Fatalf("%s ExecuteBlock failed: %v", tc.name, err)
		}
		if res.Results[0].Failed || res.Results[1].Failed {
			t.Fatalf("%s tx failed: %+v", tc.name, res.Results)
		}
		if idx == 0 {
			refRoot = res.StateRoot
		} else if !bytes.Equal(res.StateRoot[:], refRoot[:]) {
			t.Fatalf("%s AppHash mismatch (consensus fork!): got %x, want %x", tc.name, res.StateRoot, refRoot)
		}
		if _, exists := res.Writes["acc-"+string(deployedAddr.Bytes())]; exists {
			t.Fatalf("%s emitted phantom tombstone for never-persisted account %s", tc.name, deployedAddr.Hex())
		}
	}
}

// TestLegacyParallelExecutorFixes kiểm chứng 3 bản vá trên đường legacy SlidingWindow=false:
// (1) revert trên resume vẫn trừ gas & tăng nonce, (2) tôn trọng remainingGas, (3) lan truyền lỗi DB.
func TestLegacyParallelExecutorFixes(t *testing.T) {
	chainID := big.NewInt(1337)
	cfg := newTestShanghaiConfig(chainID)
	signer := types.LatestSignerForChainID(chainID)
	key1, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000044")
	key2, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000055")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	contract := common.HexToAddress("0x000000000000000000000000000000000000beef")

	// Contract: nếu calldata[0]==1 thì SSTORE(0,1); còn nếu calldata[0]==0 thì SLOAD(0), nếu !=0 thì REVERT!
	// Mục tiêu: Tx0 ghi slot0=1; Tx1 chạy song song đọc slot0=0 -> thành công; khi retry/resume trên nền mới đọc slot0=1 -> REVERT!
	// Runtime:
	// PUSH1 0x00, CALLDATALOAD, PUSH1 0x00, BYTE, ISZERO, PUSH1 0x10, JUMPI
	// PUSH1 0x01, PUSH1 0x00, SSTORE, STOP
	// JUMPDEST(0x10): PUSH1 0x00, SLOAD, ISZERO, PUSH1 0x1c, JUMPI
	// PUSH1 0x00, PUSH1 0x00, REVERT
	// JUMPDEST(0x1c): STOP
	code := []byte{
		0x60, 0x00, 0x35, 0x60, 0x00, 0x1a, 0x15, 0x60, 0x10, 0x57,
		0x60, 0x01, 0x60, 0x00, 0x55, 0x00,
		0x5b, 0x60, 0x00, 0x54, 0x15, 0x60, 0x1c, 0x57,
		0x60, 0x00, 0x60, 0x00, 0xfd,
		0x5b, 0x00,
	}

	gasPrice := big.NewInt(1_000_000_000)
	tx0, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: gasPrice, Gas: 100_000, To: &contract, Data: []byte{1},
	}), signer, key1)
	tx1, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: 0, GasPrice: gasPrice, Gas: 100_000, To: &contract, Data: []byte{0},
	}), signer, key2)

	runWith := func(exec Executor, gasLimit uint64) (*BlockResult, *PebbleStateDB) {
		db, err := store.NewPebbleDB("state", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		seed := NewPebbleStateDB(db)
		seed.AddBalanceBig(sender1, big.NewInt(1e18))
		seed.AddBalanceBig(sender2, big.NewInt(1e18))
		seed.CreateAccount(contract)
		seed.SetNonce(contract, 1, 0)
		seed.SetCode(contract, code, 0)
		seed.Finalise(true)
		if err := seed.Commit(); err != nil {
			t.Fatal(err)
		}
		s := NewPebbleStateDB(db)
		env := BlockEnv{Height: 1, Time: 1, GasLimit: gasLimit, BaseFee: big.NewInt(0)}
		res, err := exec.ExecuteBlock(s, env, []*types.Transaction{tx0, tx1})
		if err != nil {
			t.Fatalf("ExecuteBlock failed: %v", err)
		}
		return res, s
	}

	// (1) Resume trên REVERT phải khớp sequential về GasUsed, Nonce, Balance và StateRoot
	seqRes, seqState := runWith(NewSequentialExecutor(cfg, chainID), 30_000_000)
	par := NewParallelExecutor(cfg, chainID, 2)
	par.SlidingWindow = false
	par.Checkpoints = CheckpointConfig{Policy: CheckpointPolicyP1}
	parRes, parState := runWith(par, 30_000_000)

	if !parRes.Results[1].Failed {
		t.Fatalf("legacy parallel tx1 expected Failed=true on revert, got %+v", parRes.Results[1])
	}
	if parRes.Results[1].GasUsed != seqRes.Results[1].GasUsed {
		t.Fatalf("legacy parallel tx1 GasUsed = %d, want %d", parRes.Results[1].GasUsed, seqRes.Results[1].GasUsed)
	}
	if parState.GetNonce(sender2) != 1 || seqState.GetNonce(sender2) != 1 {
		t.Fatalf("legacy parallel sender2 nonce = %d, want 1 (must not revert pre-ApplyMessage snapshot)", parState.GetNonce(sender2))
	}
	if parState.GetBalance(sender2).Cmp(seqState.GetBalance(sender2)) != 0 {
		t.Fatalf("legacy parallel sender2 balance = %s, want %s", parState.GetBalance(sender2), seqState.GetBalance(sender2))
	}
	if parRes.StateRoot != seqRes.StateRoot {
		t.Fatalf("legacy parallel StateRoot = %x, want %x", parRes.StateRoot, seqRes.StateRoot)
	}

	// (2) BlockGasLimit chặt (120_000): tx0 tốn ~43k gas -> remainingGas ~77k < tx1.Gas() (100k) => tx1 phải Invalid như sequential
	seqTight, _ := runWith(NewSequentialExecutor(cfg, chainID), 120_000)
	parTight := NewParallelExecutor(cfg, chainID, 2)
	parTight.SlidingWindow = false
	parTight.Checkpoints = CheckpointConfig{Policy: CheckpointPolicyP1}
	parTightRes, _ := runWith(parTight, 120_000)
	if !seqTight.Results[1].Invalid || !parTightRes.Results[1].Invalid {
		t.Fatalf("tight block gas limit: seq.Invalid=%v par.Invalid=%v, want both true", seqTight.Results[1].Invalid, parTightRes.Results[1].Invalid)
	}

	// (3) Lỗi đọc DB trên legacy SlidingWindow=false phải trả lỗi (không bị nuốt thành Invalid: true)
	errState := NewPebbleStateDB(nil)
	errState.kv = failingStateKV{err: errors.New("injected io error")}
	parErr := NewParallelExecutor(cfg, chainID, 2)
	parErr.SlidingWindow = false
	if _, err := parErr.ExecuteBlock(errState, BlockEnv{Height: 1, Time: 1, GasLimit: 30_000_000, BaseFee: big.NewInt(0)}, []*types.Transaction{tx0}); err == nil {
		t.Fatal("legacy ParallelExecutor swallowed DB read error instead of returning error")
	}
}
