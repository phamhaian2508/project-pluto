package harness_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	gethstate "github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
	plutoconfig "github.com/phamhaian2508/project-pluto/internal/config"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

const blockGasLimit uint64 = 30_000_000

var chainID = big.NewInt(plutoconfig.EVMChainID)

func newChainConfig() *params.ChainConfig {
	zero := big.NewInt(0)
	shanghai := uint64(0)
	return &params.ChainConfig{
		ChainID:                 chainID,
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

func signedTx(t testing.TB, keyHex string, nonce, gas uint64, to *common.Address, value *big.Int, data []byte) *types.Transaction {
	t.Helper()
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int), Gas: gas, To: to, Value: value, Data: data})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// This test harness deliberately compares observable transaction results and
// account state, not state roots: Pluto currently uses a custom app hash while
// geth uses an Ethereum Merkle Patricia trie root.
const differentialTestKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

type differentialWorkload struct {
	name            string
	transactions    []*types.Transaction
	sender          common.Address
	recipient       common.Address
	contract        common.Address
	accounts        []common.Address
	initialBalances map[common.Address]*big.Int
	storageSlots    map[common.Address][]common.Hash
	initialBalance  *big.Int
	env             evm.BlockEnv
}

type differentialAccount struct {
	balance *big.Int
	nonce   uint64
	code    []byte
	storage map[common.Hash]common.Hash
}

type differentialObservation struct {
	results        []evm.TxResult
	receipts       []*types.Receipt
	gasUsed        uint64
	accounts       map[common.Address]differentialAccount
	stateRoot      common.Hash
	hasStateRoot   bool
	comparisonRoot common.Hash
	metrics        map[string]float64
	checkpoint     evm.CheckpointMetrics
}

func newCounterDifferentialWorkload(t testing.TB) differentialWorkload {
	t.Helper()
	key, err := crypto.HexToECDSA(differentialTestKey)
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x0000000000000000000000000000000000001234")
	contract := crypto.CreateAddress(sender, 1) // nonce 0 transfers; nonce 1 creates Counter.

	bytecode, err := os.ReadFile(filepath.Join("..", "..", "contracts", "Counter.bin"))
	if err != nil {
		t.Fatalf("read Counter creation bytecode: %v", err)
	}
	creationCode, err := hex.DecodeString(string(bytes.TrimSpace(bytecode)))
	if err != nil {
		t.Fatalf("decode Counter creation bytecode: %v", err)
	}
	if len(creationCode) == 0 {
		t.Fatal("Counter creation bytecode is empty")
	}

	increment := common.FromHex("0xd09de08a") // increment()
	addSeven := append(common.FromHex("0x1003e2d2"), common.LeftPadBytes(big.NewInt(7).Bytes(), 32)...)

	transactions := []*types.Transaction{
		signedTx(t, differentialTestKey, 0, 21_000, &recipient, big.NewInt(42), nil),
		signedTx(t, differentialTestKey, 1, 3_000_000, nil, new(big.Int), creationCode),
		signedTx(t, differentialTestKey, 2, 100_000, &contract, new(big.Int), increment),
		signedTx(t, differentialTestKey, 3, 100_000, &contract, new(big.Int), increment),
		signedTx(t, differentialTestKey, 4, 100_000, &contract, new(big.Int), addSeven),
		signedTx(t, differentialTestKey, 5, 100_000, &contract, new(big.Int), common.FromHex("0xa9cc4718")), // fail()
	}

	getHash := func(height uint64) common.Hash {
		return crypto.Keccak256Hash([]byte("pluto-differential-block"), common.LeftPadBytes(new(big.Int).SetUint64(height).Bytes(), 8))
	}
	return differentialWorkload{
		name:           "transfer-deploy-counter-increment-add-revert",
		transactions:   transactions,
		sender:         sender,
		recipient:      recipient,
		contract:       contract,
		accounts:       []common.Address{sender, recipient, contract},
		storageSlots:   map[common.Address][]common.Hash{contract: {common.Hash{}}},
		initialBalance: new(big.Int).Mul(big.NewInt(1_000), big.NewInt(1e18)),
		env: evm.BlockEnv{
			Height:   1,
			Time:     1,
			GasLimit: blockGasLimit,
			Random:   common.HexToHash("0x1234"),
			GetHash:  getHash,
			BaseFee:  new(big.Int),
		},
	}
}

// Accepting evm.Executor lets the same workload run against a future parallel
// executor without changing the workload or comparison code.
func runPlutoDifferentialWorkload(t testing.TB, executor evm.Executor, workload differentialWorkload) differentialObservation {
	t.Helper()
	db, err := store.NewPebbleDB("differential", t.TempDir())
	if err != nil {
		t.Fatalf("create Pluto harness state: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		if err := db.Close(); err != nil {
			t.Errorf("close Pluto harness state: %v", err)
		}
	})
	seed := evm.NewPebbleStateDB(db)
	for addr, balance := range workloadBalances(workload) {
		seed.AddBalanceBig(addr, balance)
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("seed Pluto harness state: %v", err)
	}
	stateDB := evm.NewPebbleStateDB(db)
	result, err := executor.ExecuteBlock(stateDB, workload.env, workload.transactions)
	if err != nil {
		t.Fatalf("run %s on Pluto: %v", workload.name, err)
	}
	observation := capturePlutoObservation(stateDB, result, workload)
	observation.stateRoot = common.BytesToHash(result.StateRoot[:])
	observation.hasStateRoot = true
	observation.metrics = result.Metrics
	observation.checkpoint = result.CheckpointMetrics
	if err := db.Close(); err != nil {
		t.Fatalf("close Pluto harness state: %v", err)
	}
	closed = true
	return observation
}

func workloadBalances(workload differentialWorkload) map[common.Address]*big.Int {
	if workload.initialBalances != nil {
		return workload.initialBalances
	}
	return map[common.Address]*big.Int{workload.sender: new(big.Int).Set(workload.initialBalance)}
}

func workloadAddresses(workload differentialWorkload) []common.Address {
	seen := make(map[common.Address]bool)
	addresses := make([]common.Address, 0, len(workload.accounts)+3)
	add := func(addr common.Address) {
		if !seen[addr] {
			seen[addr] = true
			addresses = append(addresses, addr)
		}
	}
	for _, addr := range workload.accounts {
		add(addr)
	}
	add(workload.sender)
	add(workload.recipient)
	add(workload.contract)
	for addr := range workload.storageSlots {
		add(addr)
	}
	return addresses
}

func capturePlutoObservation(stateDB *evm.PebbleStateDB, result *evm.BlockResult, workload differentialWorkload) differentialObservation {
	addresses := workloadAddresses(workload)
	accounts := make(map[common.Address]differentialAccount, len(addresses))
	for _, addr := range addresses {
		account := differentialAccount{
			balance: stateDB.GetBalance(addr).ToBig(),
			nonce:   stateDB.GetNonce(addr),
			code:    append([]byte(nil), stateDB.GetCode(addr)...),
			storage: make(map[common.Hash]common.Hash),
		}
		for _, slot := range workload.storageSlots[addr] {
			account.storage[slot] = stateDB.GetState(addr, slot)
		}
		accounts[addr] = account
	}
	return differentialObservation{
		results: result.Results, receipts: buildReceipts(workload, result.Results),
		gasUsed: result.GasUsed, accounts: accounts, comparisonRoot: observedStateRoot(accounts),
	}
}

func buildReceipts(workload differentialWorkload, results []evm.TxResult) []*types.Receipt {
	receipts := make([]*types.Receipt, len(results))
	var cumulativeGas uint64
	for i, result := range results {
		if result.Invalid {
			continue
		}
		cumulativeGas += result.GasUsed
		status := types.ReceiptStatusSuccessful
		if result.Failed {
			status = types.ReceiptStatusFailed
		}
		tx := workload.transactions[i]
		receipt := &types.Receipt{
			Type: tx.Type(), Status: status, CumulativeGasUsed: cumulativeGas,
			Logs: result.Logs, TxHash: tx.Hash(), GasUsed: result.GasUsed,
			EffectiveGasPrice: new(big.Int), BlockNumber: big.NewInt(workload.env.Height), TransactionIndex: uint(i),
		}
		if tx.To() == nil {
			if sender, err := types.Sender(types.LatestSignerForChainID(chainID), tx); err == nil {
				receipt.ContractAddress = crypto.CreateAddress(sender, tx.Nonce())
			}
		}
		receipt.Bloom = types.CreateBloom(receipt)
		receipts[i] = receipt
	}
	return receipts
}

func runGethDifferentialWorkload(t testing.TB, workload differentialWorkload) differentialObservation {
	t.Helper()
	chainConfig := newChainConfig()
	stateDB, err := gethstate.New(common.Hash{}, gethstate.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("create geth reference state: %v", err)
	}
	for addr, balance := range workloadBalances(workload) {
		initialBalance, overflow := uint256.FromBig(balance)
		if overflow {
			t.Fatalf("initial balance for %s does not fit uint256", addr)
		}
		stateDB.SetBalance(addr, initialBalance, tracing.BalanceChangeUnspecified)
	}

	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     workload.env.GetHash,
		Coinbase:    workload.env.Coinbase,
		BlockNumber: big.NewInt(workload.env.Height),
		Time:        workload.env.Time,
		Difficulty:  new(big.Int),
		Random:      &workload.env.Random,
		BaseFee:     new(big.Int).Set(workload.env.BaseFee),
		BlobBaseFee: new(big.Int),
		GasLimit:    workload.env.GasLimit,
	}
	evmEnv := vm.NewEVM(blockContext, stateDB, chainConfig, vm.Config{})
	signer := types.LatestSignerForChainID(chainID)
	gasPool := new(core.GasPool).AddGas(workload.env.GasLimit)
	results := make([]evm.TxResult, len(workload.transactions))
	var blockGasUsed uint64

	for i, tx := range workload.transactions {
		result := evm.TxResult{Index: i}
		msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(workload.env.BaseFee))
		if err != nil {
			result.Invalid = true
			result.ErrMsg = err.Error()
			results[i] = result
			continue
		}
		stateDB.SetTxContext(tx.Hash(), i)
		evmEnv.SetTxContext(core.NewEVMTxContext(msg))
		snapshot := stateDB.Snapshot()
		transition, err := core.ApplyMessage(evmEnv, msg, gasPool)
		if err != nil {
			stateDB.RevertToSnapshot(snapshot)
			stateDB.Finalise(true)
			result.Invalid = true
			result.ErrMsg = err.Error()
			results[i] = result
			continue
		}
		stateDB.Finalise(true)
		result.GasUsed = transition.UsedGas
		blockGasUsed += transition.UsedGas
		result.Failed = transition.Err != nil
		if transition.Err != nil {
			result.ErrMsg = transition.Err.Error()
		}
		result.ReturnData = append([]byte(nil), transition.ReturnData...)
		for _, logEntry := range stateDB.GetLogs(tx.Hash(), uint64(workload.env.Height), common.Hash{}, workload.env.Time) {
			logCopy := *logEntry
			result.Logs = append(result.Logs, &logCopy)
		}
		results[i] = result
	}

	addresses := workloadAddresses(workload)
	accounts := make(map[common.Address]differentialAccount, len(addresses))
	for _, addr := range addresses {
		account := differentialAccount{
			balance: stateDB.GetBalance(addr).ToBig(),
			nonce:   stateDB.GetNonce(addr),
			code:    append([]byte(nil), stateDB.GetCode(addr)...),
			storage: make(map[common.Hash]common.Hash),
		}
		for _, slot := range workload.storageSlots[addr] {
			account.storage[slot] = stateDB.GetState(addr, slot)
		}
		accounts[addr] = account
	}
	return differentialObservation{
		results: results, receipts: buildReceipts(workload, results),
		gasUsed: blockGasUsed, accounts: accounts, comparisonRoot: observedStateRoot(accounts),
	}
}

type observedRootSlot struct {
	Key   common.Hash
	Value common.Hash
}

type observedRootAccount struct {
	Address common.Address
	Balance *big.Int
	Nonce   uint64
	Code    []byte
	Storage []observedRootSlot
}

// observedStateRoot gives Pluto and geth a shared root domain over every
// account and storage slot declared by the workload. Pluto's native app root is
// still compared separately among Pluto executors because it is not an MPT.
func observedStateRoot(accounts map[common.Address]differentialAccount) common.Hash {
	addresses := make([]common.Address, 0, len(accounts))
	for address := range accounts {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
	canonical := make([]observedRootAccount, 0, len(addresses))
	for _, address := range addresses {
		account := accounts[address]
		slots := make([]common.Hash, 0, len(account.storage))
		for slot := range account.storage {
			slots = append(slots, slot)
		}
		sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i][:], slots[j][:]) < 0 })
		entry := observedRootAccount{
			Address: address, Balance: new(big.Int).Set(account.balance), Nonce: account.nonce,
			Code: append([]byte(nil), account.code...), Storage: make([]observedRootSlot, 0, len(slots)),
		}
		for _, slot := range slots {
			entry.Storage = append(entry.Storage, observedRootSlot{Key: slot, Value: account.storage[slot]})
		}
		canonical = append(canonical, entry)
	}
	encoded, err := rlp.EncodeToBytes(canonical)
	if err != nil {
		panic(err)
	}
	return crypto.Keccak256Hash(encoded)
}

func differentialMismatches(workload differentialWorkload, leftName string, left differentialObservation, rightName string, right differentialObservation) []string {
	var mismatches []string
	blockName := fmt.Sprintf("block[%d] workload[%s]", workload.env.Height, workload.name)
	if len(left.results) != len(right.results) {
		return []string{fmt.Sprintf("%s: transaction result count: %s=%d %s=%d", blockName, leftName, len(left.results), rightName, len(right.results))}
	}
	if left.gasUsed != right.gasUsed {
		mismatches = append(mismatches, fmt.Sprintf("%s: block gas used: %s=%d %s=%d", blockName, leftName, left.gasUsed, rightName, right.gasUsed))
	}
	if left.hasStateRoot && right.hasStateRoot && left.stateRoot != right.stateRoot {
		mismatches = append(mismatches, fmt.Sprintf("%s: Pluto state root: %s=%s %s=%s", blockName, leftName, left.stateRoot, rightName, right.stateRoot))
	}
	if left.comparisonRoot != right.comparisonRoot {
		mismatches = append(mismatches, fmt.Sprintf("%s: observed final-state root: %s=%s %s=%s", blockName, leftName, left.comparisonRoot, rightName, right.comparisonRoot))
	}
	for i := range left.results {
		got, want := left.results[i], right.results[i]
		if got.Index != want.Index || got.Invalid != want.Invalid || got.Failed != want.Failed || got.GasUsed != want.GasUsed || !bytes.Equal(got.ReturnData, want.ReturnData) {
			mismatches = append(mismatches, fmt.Sprintf("%s tx[%d] (%s): %s invalid=%t failed=%t gas=%d return=%x; %s invalid=%t failed=%t gas=%d return=%x", blockName, i, workload.transactions[i].Hash(), leftName, got.Invalid, got.Failed, got.GasUsed, got.ReturnData, rightName, want.Invalid, want.Failed, want.GasUsed, want.ReturnData))
		}
		if len(got.Logs) != len(want.Logs) {
			mismatches = append(mismatches, fmt.Sprintf("%s tx[%d] (%s): log count %s=%d %s=%d", blockName, i, workload.transactions[i].Hash(), leftName, len(got.Logs), rightName, len(want.Logs)))
			continue
		}
		for j := range got.Logs {
			if !sameLog(got.Logs[j], want.Logs[j]) {
				mismatches = append(mismatches, fmt.Sprintf("%s tx[%d] log[%d] differs: %s=%+v %s=%+v", blockName, i, j, leftName, got.Logs[j], rightName, want.Logs[j]))
			}
		}
	}
	if len(left.receipts) != len(right.receipts) {
		mismatches = append(mismatches, fmt.Sprintf("%s: receipt count: %s=%d %s=%d", blockName, leftName, len(left.receipts), rightName, len(right.receipts)))
	} else {
		for i := range left.receipts {
			if !sameReceipt(left.receipts[i], right.receipts[i]) {
				mismatches = append(mismatches, fmt.Sprintf("%s receipt[%d] differs: %s=%+v %s=%+v", blockName, i, leftName, left.receipts[i], rightName, right.receipts[i]))
			}
		}
	}

	for addr, got := range left.accounts {
		want, ok := right.accounts[addr]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s: %s observation missing account %s", blockName, rightName, addr))
			continue
		}
		if got.balance.Cmp(want.balance) != 0 || got.nonce != want.nonce || !bytes.Equal(got.code, want.code) {
			mismatches = append(mismatches, fmt.Sprintf("%s account %s differs: %s balance=%s nonce=%d code=%x; %s balance=%s nonce=%d code=%x", blockName, addr, leftName, got.balance, got.nonce, got.code, rightName, want.balance, want.nonce, want.code))
		}
		for slot, gotValue := range got.storage {
			wantValue, exists := want.storage[slot]
			if !exists || gotValue != wantValue {
				mismatches = append(mismatches, fmt.Sprintf("%s account %s storage[%s] differs: %s=%s %s=%s (present=%t)", blockName, addr, slot, leftName, gotValue, rightName, wantValue, exists))
			}
		}
		for slot, wantValue := range want.storage {
			if _, exists := got.storage[slot]; !exists {
				mismatches = append(mismatches, fmt.Sprintf("%s account %s storage[%s] missing from %s (value=%s)", blockName, addr, slot, leftName, wantValue))
			}
		}
	}
	for addr := range right.accounts {
		if _, ok := left.accounts[addr]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s: %s observation missing account %s", blockName, leftName, addr))
		}
	}
	return mismatches
}

func assertDifferentialObservationsEqual(t testing.TB, workload differentialWorkload, leftName string, left differentialObservation, rightName string, right differentialObservation) {
	t.Helper()
	mismatches := differentialMismatches(workload, leftName, left, rightName, right)
	if len(mismatches) == 0 {
		return
	}
	t.Errorf("%s: %d differential mismatch(es); first: %s", workload.name, len(mismatches), mismatches[0])
}

type namedExecutor struct {
	name string
	ev   evm.Executor
}

// compareAllExecutors keeps the workload independent of concrete engines. A
// future parallel executor is added by passing another namedExecutor here;
// every Pluto result is checked against geth and against the first Pluto
// executor, including Pluto's custom state root.
func compareAllExecutors(t testing.TB, workload differentialWorkload, executors ...namedExecutor) map[string]differentialObservation {
	t.Helper()
	if len(executors) == 0 {
		t.Fatal("differential harness requires at least one Pluto executor")
	}
	geth := runGethDifferentialWorkload(t, workload)
	observations := make(map[string]differentialObservation, len(executors))
	var baselineName string
	var baseline differentialObservation
	for i, executor := range executors {
		got := runPlutoDifferentialWorkload(t, executor.ev, workload)
		observations[executor.name] = got
		assertDifferentialObservationsEqual(t, workload, executor.name, got, "geth reference", geth)
		if i == 0 {
			baselineName, baseline = executor.name, got
			continue
		}
		assertDifferentialObservationsEqual(t, workload, executor.name, got, baselineName, baseline)
	}
	return observations
}

func sameLog(a, b *types.Log) bool {
	if a == nil || b == nil || a.Address != b.Address || a.TxHash != b.TxHash || !bytes.Equal(a.Data, b.Data) || len(a.Topics) != len(b.Topics) {
		return false
	}
	for i := range a.Topics {
		if a.Topics[i] != b.Topics[i] {
			return false
		}
	}
	return true
}

func sameReceipt(a, b *types.Receipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Type != b.Type || a.Status != b.Status || a.CumulativeGasUsed != b.CumulativeGasUsed ||
		a.Bloom != b.Bloom || a.TxHash != b.TxHash || a.ContractAddress != b.ContractAddress ||
		a.GasUsed != b.GasUsed || a.TransactionIndex != b.TransactionIndex || len(a.Logs) != len(b.Logs) {
		return false
	}
	for i := range a.Logs {
		if !sameLog(a.Logs[i], b.Logs[i]) {
			return false
		}
	}
	return true
}

func checkpointPolicyExecutors(t testing.TB, workers int) []namedExecutor {
	t.Helper()
	executors := []namedExecutor{
		{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	}
	configs := []struct {
		name   string
		config evm.CheckpointConfig
	}{
		{name: "Pluto parallel P0", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP0}},
		{name: "Pluto parallel P1", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP1}},
		{name: "Pluto parallel P2-N100", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 100}},
		{name: "Pluto parallel P2-N1000", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP2, Interval: 1000}},
		{name: "Pluto parallel P3", config: evm.CheckpointConfig{Policy: evm.CheckpointPolicyP3}},
	}
	for _, item := range configs {
		executor := evm.NewParallelExecutor(newChainConfig(), chainID, workers)
		if err := executor.ConfigureCheckpoints(item.config); err != nil {
			t.Fatalf("configure %s: %v", item.name, err)
		}
		executors = append(executors, namedExecutor{name: item.name, ev: executor})
	}
	return executors
}

func TestDifferentialHarnessCounterWorkloadMatchesGeth(t *testing.T) {
	workload := newCounterDifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]

	if got := pluto.accounts[workload.contract].storage[common.Hash{}]; got != common.BigToHash(big.NewInt(9)) {
		t.Fatalf("Counter.count after workload = %s, want 9", got)
	}
	if got := pluto.accounts[workload.recipient].balance; got.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("transfer recipient balance = %s, want 42 wei", got)
	}
	if got := pluto.accounts[workload.sender].nonce; got != uint64(len(workload.transactions)) {
		t.Fatalf("sender nonce after workload = %d, want %d", got, len(workload.transactions))
	}
	if len(pluto.accounts[workload.contract].code) == 0 {
		t.Fatal("Counter runtime code is empty after deployment")
	}
	if !pluto.results[5].Failed {
		t.Fatal("Counter.fail() transaction did not revert")
	}
}

func TestDifferentialHarnessComparesPlutoExecutorsAndStateRoot(t *testing.T) {
	workload := newCounterDifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "sequential-a", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
		namedExecutor{name: "sequential-b", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	if observations["sequential-a"].stateRoot != observations["sequential-b"].stateRoot {
		t.Fatalf("same Pluto executor produced different state roots")
	}
}

func TestDifferentialHarnessParallelStaticTransfersMatchSequentialAndGeth(t *testing.T) {
	const transferCount = 32
	pool := newAccountPool(t, 0x504f53, transferCount*2)
	transactions := make([]*types.Transaction, transferCount)
	balances := make(map[common.Address]*big.Int, len(pool))
	addresses := make([]common.Address, len(pool))
	for i, account := range pool {
		balances[account.address] = new(big.Int).SetUint64(1_000_000)
		addresses[i] = account.address
	}
	for i := range transactions {
		sender, recipient := pool[i*2], pool[i*2+1]
		transactions[i] = signedTx(t, sender.keyHex, 0, 21_000, &recipient.address, big.NewInt(int64(i+1)), nil)
	}
	workload := differentialWorkload{
		name:            "independent-static-transfers",
		transactions:    transactions,
		accounts:        addresses,
		initialBalances: balances,
		env:             evm.BlockEnv{Height: 11, Time: 11, GasLimit: blockGasLimit, BaseFee: new(big.Int)},
	}
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
		namedExecutor{name: "Pluto parallel", ev: evm.NewParallelExecutor(newChainConfig(), chainID, 4)},
	)
	parallel := observations["Pluto parallel"]
	if parallel.metrics["tx_class_s"] != transferCount {
		t.Fatalf("parallel S transaction count = %v, want %d", parallel.metrics["tx_class_s"], transferCount)
	}
	if parallel.metrics["worker_count"] != 4 {
		t.Fatalf("parallel worker_count = %v, want 4", parallel.metrics["worker_count"])
	}
}

func TestDifferentialHarnessParallelFullyReplaysAfterValidationFailure(t *testing.T) {
	workload := newCounterDifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
		namedExecutor{name: "Pluto parallel", ev: evm.NewParallelExecutor(newChainConfig(), chainID, 4)},
	)
	parallel := observations["Pluto parallel"]
	if parallel.metrics["tx_class_o"] != float64(len(workload.transactions)-1) {
		t.Fatalf("parallel O transaction count = %v, want %d", parallel.metrics["tx_class_o"], len(workload.transactions)-1)
	}
	if parallel.metrics["tx_class_s"] != 1 {
		t.Fatalf("transfer in contract workload classified as S count = %v, want 1", parallel.metrics["tx_class_s"])
	}

	// Every transaction after the first speculatively reads the sender nonce
	// before its predecessor is accepted. Ordered validation therefore aborts
	// those stale incarnation-1 attempts and the current implementation starts
	// each transaction again from its entry point as incarnation 2.
	wantRetries := float64(len(workload.transactions) - 1)
	if got := parallel.metrics["retry_total"]; got != wantRetries {
		t.Fatalf("parallel retry_total = %v, want %v full transaction replays", got, wantRetries)
	}
	if got := parallel.metrics["abort_total"]; got != wantRetries {
		t.Fatalf("parallel abort_total = %v, want %v stale attempts", got, wantRetries)
	}
	wantIncarnations := float64(len(workload.transactions)) + wantRetries
	if got := parallel.metrics["incarnation_total"]; got != wantIncarnations {
		t.Fatalf("parallel incarnation_total = %v, want %v initial attempts plus full replays", got, wantIncarnations)
	}
}

func TestDifferentialHarnessParallelStaticTransferFailuresMatch(t *testing.T) {
	pool := newAccountPool(t, 0x534641494c, 4)
	transactions := []*types.Transaction{
		signedTx(t, pool[0].keyHex, 0, 21_000, &pool[1].address, big.NewInt(2), nil), // insufficient balance
		signedTx(t, pool[2].keyHex, 1, 21_000, &pool[3].address, big.NewInt(1), nil), // nonce gap
	}
	balances := map[common.Address]*big.Int{
		pool[0].address: big.NewInt(1),
		pool[2].address: big.NewInt(10),
	}
	workload := differentialWorkload{
		name:            "static-transfer-insufficient-balance-and-nonce-gap",
		transactions:    transactions,
		accounts:        []common.Address{pool[0].address, pool[1].address, pool[2].address, pool[3].address},
		initialBalances: balances,
		env:             evm.BlockEnv{Height: 12, Time: 12, GasLimit: blockGasLimit, BaseFee: new(big.Int)},
	}
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
		namedExecutor{name: "Pluto parallel", ev: evm.NewParallelExecutor(newChainConfig(), chainID, 4)},
	)
	parallel := observations["Pluto parallel"]
	for i, result := range parallel.results {
		if !result.Invalid || result.GasUsed != 0 {
			t.Fatalf("parallel invalid transfer tx[%d] = %+v, want invalid with zero gas", i, result)
		}
	}
}
