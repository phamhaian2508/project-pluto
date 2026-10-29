package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

func TestStateForkIsIsolatedUntilAcceptedWritesAreApplied(t *testing.T) {
	db, err := store.NewPebbleDB("tx-state-fork", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	addr := common.HexToAddress("0x1234")
	seed := NewPebbleStateDB(db)
	seed.AddBalanceBig(addr, big.NewInt(10))
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	parent := NewPebbleStateDB(db)
	fork := parent.Fork()
	fork.AddBalance(addr, uint256.NewInt(3), 0)
	fork.SetNonce(addr, 1, 0)

	if got := parent.GetBalance(addr).ToBig(); got.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("parent balance changed before resolve: got %s, want 10", got)
	}
	if got := parent.GetNonce(addr); got != 0 {
		t.Fatalf("parent nonce changed before resolve: got %d, want 0", got)
	}

	writes, err := fork.PendingWrites()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.ApplyPendingWrites(writes); err != nil {
		t.Fatal(err)
	}
	if got := parent.GetBalance(addr).ToBig(); got.Cmp(big.NewInt(13)) != 0 {
		t.Fatalf("resolved balance = %s, want 13", got)
	}
	if got := parent.GetNonce(addr); got != 1 {
		t.Fatalf("resolved nonce = %d, want 1", got)
	}
}

func TestMVMemoryTracksVersionsAndDetectsStaleReads(t *testing.T) {
	memory := NewMVMemory()
	key := MVKey{Address: common.HexToAddress("0xabcd"), Kind: MVBalance}
	observed := memory.Snapshot([]MVKey{key})
	if !memory.Validate(observed) {
		t.Fatal("fresh read version should validate")
	}
	memory.Commit([]MVKey{key})
	if memory.Validate(observed) {
		t.Fatal("read version should fail after an accepted write")
	}
	if got := memory.Version(key); got != 1 {
		t.Fatalf("key version = %d, want 1", got)
	}
}

func TestMVMemoryKeepsAcceptedValuesByTransactionIndex(t *testing.T) {
	memory := NewMVMemory()
	key := MVKey{Address: common.HexToAddress("0xabcd"), Kind: MVStorage, Slot: common.HexToHash("0x01")}
	memory.CommitTransaction(4, 2, map[MVKey][]byte{key: {0x04}})
	memory.CommitTransaction(8, 1, map[MVKey][]byte{key: {0x08}})

	if _, ok := memory.ReadBefore(key, 4); ok {
		t.Fatal("transaction should not observe a write from itself or a later transaction")
	}
	got, ok := memory.ReadBefore(key, 9)
	if !ok || got.TxIndex != 8 || got.Incarnation != 1 || got.Version != 2 || len(got.Value) != 1 || got.Value[0] != 0x08 {
		t.Fatalf("latest predecessor version = %+v, found=%v", got, ok)
	}
	got.Value[0] = 0xff
	again, _ := memory.ReadBefore(key, 9)
	if again.Value[0] != 0x08 {
		t.Fatal("ReadBefore returned a mutable reference into MVMemory")
	}
}

func TestMVMemoryDistinguishesZeroValueFromDeletion(t *testing.T) {
	memory := NewMVMemory()
	balanceKey := MVKey{Address: common.HexToAddress("0xabcd"), Kind: MVBalance}
	storageKey := MVKey{Address: common.HexToAddress("0xabcd"), Kind: MVStorage, Slot: common.HexToHash("0x02")}
	zeroBalance := make([]byte, 32)
	memory.CommitTransaction(3, 1, map[MVKey][]byte{balanceKey: zeroBalance, storageKey: nil})

	balance, ok := memory.ReadBefore(balanceKey, 4)
	if !ok || balance.Deleted || len(balance.Value) != 32 {
		t.Fatalf("zero balance version = %+v, found=%v; want present 32-byte zero", balance, ok)
	}
	storage, ok := memory.ReadBefore(storageKey, 4)
	if !ok || !storage.Deleted {
		t.Fatalf("deleted storage version = %+v, found=%v; want tombstone", storage, ok)
	}
}

func TestTxStateRecordsPersistentStateReadsAndWrites(t *testing.T) {
	db, err := store.NewPebbleDB("tx-state-accesses", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := NewPebbleStateDB(db)
	addr := common.HexToAddress("0x1234")
	slot := common.HexToHash("0x99")
	txState := newTxState(base, 7, 1, nil, nil)

	_ = txState.GetBalance(addr)
	_ = txState.GetNonce(addr)
	_ = txState.GetState(addr, slot)
	txState.SetNonce(addr, 1, 0)
	txState.SetState(addr, slot, common.HexToHash("0x01"))

	reads, writes := txState.AccessSets()
	assertHasKey := func(name string, keys []MVKey, want MVKey) {
		t.Helper()
		for _, key := range keys {
			if key == want {
				return
			}
		}
		t.Errorf("%s does not contain %+v: %+v", name, want, keys)
	}

	assertHasKey("read set", reads, MVKey{Address: addr, Kind: MVBalance})
	assertHasKey("read set", reads, MVKey{Address: addr, Kind: MVNonce})
	assertHasKey("read set", reads, MVKey{Address: addr, Kind: MVStorage, Slot: slot})
	assertHasKey("read set", reads, MVKey{Address: addr, Kind: MVAccount})
	assertHasKey("write set", writes, MVKey{Address: addr, Kind: MVNonce})
	assertHasKey("write set", writes, MVKey{Address: addr, Kind: MVStorage, Slot: slot})
	assertHasKey("write set", writes, MVKey{Address: addr, Kind: MVAccount})

	if got := base.GetNonce(addr); got != 0 {
		t.Fatalf("recording tx state changed base nonce: got %d, want 0", got)
	}
	if got := base.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("recording tx state changed base storage: got %s, want zero", got)
	}
}

func TestTxStateVersionedWritesKeepsStorageDeletionTombstone(t *testing.T) {
	db, err := store.NewPebbleDB("tx-state-storage-tombstone", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	addr := common.HexToAddress("0x1234")
	slot := common.HexToHash("0x99")
	base := NewPebbleStateDB(db)
	base.SetState(addr, slot, common.HexToHash("0x01"))
	if err := base.Commit(); err != nil {
		t.Fatal(err)
	}
	base = NewPebbleStateDB(db)

	txState := newTxState(base, 1, 1, nil, nil)
	txState.SetState(addr, slot, common.Hash{})
	writes := txState.VersionedWrites()
	key := MVKey{Address: addr, Kind: MVStorage, Slot: slot}
	value, ok := writes[key]
	if !ok {
		t.Fatal("zero storage write was omitted; MVMemory would miss the conflict")
	}
	if value != nil {
		t.Fatalf("zero storage write = %x, want explicit deletion tombstone", value)
	}

	memory := NewMVMemory()
	observed := memory.Snapshot([]MVKey{key})
	memory.CommitTransaction(1, txState.Incarnation, writes)
	if memory.Validate(observed) {
		t.Fatal("read version remained valid after storage was cleared")
	}
}

func TestOptimisticAttemptExecutesOnIsolatedStateAndReturnsDelta(t *testing.T) {
	db := testStateDB(t)
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x1234")
	seed := new(big.Int).Mul(big.NewInt(10), big.NewInt(1_000_000_000_000_000_000))
	db.AddBalanceBig(sender, seed)
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	base := NewPebbleStateDB(db.db)
	chainID := big.NewInt(plutoconfig.EVMChainID)
	config := overlayTestChainConfig(chainID)
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: new(big.Int), Gas: 21_000, To: &recipient, Value: big.NewInt(2_000_000_000_000_000_000)})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	env := BlockEnv{Height: 1, GasLimit: 30_000_000, BaseFee: new(big.Int)}
	attempt, err := executeOptimisticAttempt(base, NewMVMemory(), config, chainID, env, tx, 0, 1, env.GasLimit)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.result.Invalid || attempt.result.Failed {
		t.Fatalf("transfer attempt failed: invalid=%v failed=%v error=%q", attempt.result.Invalid, attempt.result.Failed, attempt.result.ErrMsg)
	}
	if attempt.result.GasUsed != 21_000 {
		t.Fatalf("gas used = %d, want 21000", attempt.result.GasUsed)
	}
	if got := base.GetBalance(recipient); got.Sign() != 0 {
		t.Fatalf("speculative execution leaked into base state: recipient balance=%s", got)
	}
	if err := base.ApplyPendingWrites(attempt.delta); err != nil {
		t.Fatal(err)
	}
	if got := base.GetBalance(recipient).ToBig(); got.Cmp(big.NewInt(2_000_000_000_000_000_000)) != 0 {
		t.Fatalf("applied recipient balance = %s, want 2000000000000000000", got)
	}
	if got := base.GetNonce(sender); got != 1 {
		t.Fatalf("applied sender nonce = %d, want 1", got)
	}
	reads, writes := attempt.state.AccessSets()
	if len(reads) == 0 || len(writes) == 0 {
		t.Fatalf("attempt access sets are empty: reads=%d writes=%d", len(reads), len(writes))
	}
}

func overlayTestChainConfig(chainID *big.Int) *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	return &params.ChainConfig{
		ChainID: chainID, HomesteadBlock: zero, EIP150Block: zero, EIP155Block: zero,
		EIP158Block: zero, ByzantiumBlock: zero, ConstantinopleBlock: zero,
		PetersburgBlock: zero, IstanbulBlock: zero, MuirGlacierBlock: zero,
		BerlinBlock: zero, LondonBlock: zero, ArrowGlacierBlock: zero,
		GrayGlacierBlock: zero, MergeNetsplitBlock: zero,
		TerminalTotalDifficulty: zero, ShanghaiTime: &shanghai,
	}
}
