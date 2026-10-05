package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/store"
)

func testStateDB(t *testing.T) *PebbleStateDB {
	t.Helper()
	db, err := store.NewPebbleDB("state", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewPebbleStateDB(db)
}

func TestCommittedStateIsCapturedAtFirstWritePerTransaction(t *testing.T) {
	s := testStateDB(t)
	addr, slot := common.HexToAddress("0x1234"), common.BigToHash(common.Big1)
	original := common.BigToHash(common.Big2)
	s.SetState(addr, slot, original)
	s.Finalise(true)
	if got := s.GetCommittedState(addr, slot); got != original {
		t.Fatalf("initial committed value = %s, want %s", got, original)
	}

	first, second := common.BigToHash(common.Big3), common.BigToHash(big.NewInt(4))
	s.SetState(addr, slot, first)
	if got := s.GetCommittedState(addr, slot); got != original {
		t.Fatalf("committed value after first write = %s, want %s", got, original)
	}
	s.SetState(addr, slot, second)
	if got := s.GetCommittedState(addr, slot); got != original {
		t.Fatalf("committed value after repeated write = %s, want %s", got, original)
	}
	s.Finalise(true)
	if got := s.GetCommittedState(addr, slot); got != second {
		t.Fatalf("next transaction committed value = %s, want %s", got, second)
	}
}

func TestSnapshotsUseStableIDsAndRestoreRefundAndState(t *testing.T) {
	s := testStateDB(t)
	addr, slot := common.HexToAddress("0x1234"), common.BigToHash(common.Big1)
	base := s.Snapshot()
	s.SetState(addr, slot, common.BigToHash(common.Big2))
	s.AddRefund(100)
	nested := s.Snapshot()
	s.SetState(addr, slot, common.BigToHash(common.Big3))
	s.AddRefund(40)
	s.RevertToSnapshot(nested)
	if got := s.GetState(addr, slot); got != common.BigToHash(common.Big2) {
		t.Fatalf("nested revert state = %s", got)
	}
	if got := s.GetRefund(); got != 100 {
		t.Fatalf("nested revert refund = %d, want 100", got)
	}
	newID := s.Snapshot()
	if newID == nested {
		t.Fatalf("snapshot id reused after revert: %d", newID)
	}
	s.SetState(addr, slot, common.BigToHash(big.NewInt(4)))
	s.RevertToSnapshot(newID)
	if got := s.GetState(addr, slot); got != common.BigToHash(common.Big2) {
		t.Fatalf("new snapshot revert state = %s", got)
	}
	s.RevertToSnapshot(base)
	if got := s.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("outer revert state = %s, want zero", got)
	}
	if got := s.GetRefund(); got != 0 {
		t.Fatalf("outer revert refund = %d, want zero", got)
	}
}

func TestFinaliseClearsTransactionJournalRefundAndTransientStorage(t *testing.T) {
	s := testStateDB(t)
	addr, slot := common.HexToAddress("0x1234"), common.BigToHash(common.Big1)
	s.SetTransientState(addr, slot, common.BigToHash(common.Big2))
	s.AddRefund(17)
	s.Snapshot()
	s.Finalise(true)
	if got := s.GetRefund(); got != 0 {
		t.Fatalf("refund after finalise = %d", got)
	}
	if got := s.GetTransientState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("transient storage after finalise = %s", got)
	}
	if got := s.Snapshot(); got <= 0 {
		t.Fatalf("snapshot id did not advance across finalise: %d", got)
	}
}
