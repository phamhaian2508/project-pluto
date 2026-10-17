package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
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
