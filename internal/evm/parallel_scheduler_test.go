package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestAdaptiveContractSchedulerUsesConservativeConflictHints(t *testing.T) {
	state := testStateDB(t)
	contract := common.HexToAddress("0x1234")
	state.SetCode(contract, []byte{0x00}, 0)

	to := contract
	legacyCalls := []*types.Transaction{
		types.NewTx(&types.LegacyTx{To: &to}),
		types.NewTx(&types.LegacyTx{To: &to}),
	}
	chainID := big.NewInt(1)
	if !shouldSeriallyScheduleContractBatch(state, chainID, legacyCalls) {
		t.Fatal("calls without slot hints should use the conservative contract fallback")
	}

	sharedSlotCalls := []*types.Transaction{
		newAccessListCall(contract, common.HexToHash("0x01")),
		newAccessListCall(contract, common.HexToHash("0x01")),
	}
	if !shouldSeriallyScheduleContractBatch(state, chainID, sharedSlotCalls) {
		t.Fatal("calls with an overlapping declared storage slot should use the contract fallback")
	}

	distinctSlotCalls := []*types.Transaction{
		newAccessListCall(contract, common.HexToHash("0x01")),
		newAccessListCall(contract, common.HexToHash("0x02")),
	}
	if shouldSeriallyScheduleContractBatch(state, chainID, distinctSlotCalls) {
		t.Fatal("calls with distinct complete slot hints should remain eligible for parallel execution")
	}
}

func newAccessListCall(contract common.Address, slot common.Hash) *types.Transaction {
	to := contract
	return types.NewTx(&types.AccessListTx{
		AccessList: types.AccessList{{Address: contract, StorageKeys: []common.Hash{slot}}},
		To:         &to,
	})
}
