package harness_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

type deterministicAccount struct {
	keyHex  string
	address common.Address
}

// newAccountPool derives repeatable test accounts from a public seed. No random
// key or external wallet state is used by the harness.
func newAccountPool(t testing.TB, seed uint64, count int) []deterministicAccount {
	t.Helper()
	if count < 2 {
		t.Fatal("account pool must contain at least two accounts")
	}
	pool := make([]deterministicAccount, count)
	for i := range pool {
		material := []byte(fmt.Sprintf("pluto-m3-account-v1:%d:%d", seed, i))
		privateKeyBytes := sha256.Sum256(material)
		key, err := crypto.ToECDSA(privateKeyBytes[:])
		if err != nil {
			t.Fatalf("derive account %d from seed %d: %v", i, seed, err)
		}
		pool[i] = deterministicAccount{
			keyHex:  hex.EncodeToString(crypto.FromECDSA(key)),
			address: crypto.PubkeyToAddress(key.PublicKey),
		}
	}
	return pool
}

func readCreationCode(t testing.TB, name string) []byte {
	t.Helper()
	artifact, err := os.ReadFile(filepath.Join("..", "..", "contracts", name+".bin"))
	if err != nil {
		t.Fatalf("read %s creation bytecode: %v", name, err)
	}
	code, err := hex.DecodeString(strings.TrimSpace(string(artifact)))
	if err != nil {
		t.Fatalf("decode %s creation bytecode: %v", name, err)
	}
	if len(code) == 0 {
		t.Fatalf("%s creation bytecode is empty", name)
	}
	return code
}

func newERC20DifferentialWorkload(t testing.TB) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 20261012, 2)
	sender, recipient := pool[0], pool[1]
	token := crypto.CreateAddress(sender.address, 0)
	creationCode := readCreationCode(t, "Token")
	amount := new(big.Int).Mul(big.NewInt(7), big.NewInt(1e18))
	calldata := append(common.FromHex("0xa9059cbb"), common.LeftPadBytes(recipient.address.Bytes(), 32)...)
	calldata = append(calldata, common.LeftPadBytes(amount.Bytes(), 32)...)
	transactions := []*types.Transaction{
		signedTx(t, sender.keyHex, 0, 3_000_000, nil, new(big.Int), creationCode),
		signedTx(t, sender.keyHex, 1, 150_000, &token, new(big.Int), calldata),
	}
	senderSlot := crypto.Keccak256Hash(common.LeftPadBytes(sender.address.Bytes(), 32), common.Hash{}.Bytes())
	recipientSlot := crypto.Keccak256Hash(common.LeftPadBytes(recipient.address.Bytes(), 32), common.Hash{}.Bytes())
	return differentialWorkload{
		name:            "erc20-deploy-transfer",
		transactions:    transactions,
		sender:          sender.address,
		recipient:       recipient.address,
		contract:        token,
		accounts:        []common.Address{sender.address, recipient.address, token},
		initialBalances: map[common.Address]*big.Int{sender.address: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		storageSlots: map[common.Address][]common.Hash{
			token: {common.BigToHash(big.NewInt(1)), senderSlot, recipientSlot},
		},
		env: evm.BlockEnv{Height: 1, Time: 1, GasLimit: blockGasLimit, Random: common.HexToHash("0x20261012"), BaseFee: new(big.Int)},
	}
}

func newCounterContentionWorkload(t testing.TB, increments int) differentialWorkload {
	t.Helper()
	if increments <= 0 {
		t.Fatal("increment count must be positive")
	}
	pool := newAccountPool(t, 33, 2)
	sender := pool[0]
	contract := crypto.CreateAddress(sender.address, 0)
	txs := make([]*types.Transaction, 0, increments+1)
	txs = append(txs, signedTx(t, sender.keyHex, 0, 3_000_000, nil, new(big.Int), readCreationCode(t, "Counter")))
	for i := 0; i < increments; i++ {
		txs = append(txs, signedTx(t, sender.keyHex, uint64(i+1), 100_000, &contract, new(big.Int), common.FromHex("0xd09de08a")))
	}
	return differentialWorkload{
		name: "counter-hot-storage-contention", transactions: txs,
		sender: sender.address, contract: contract,
		accounts:        []common.Address{sender.address, pool[1].address, contract},
		initialBalances: map[common.Address]*big.Int{sender.address: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		storageSlots:    map[common.Address][]common.Hash{contract: {common.Hash{}}},
		env:             evm.BlockEnv{Height: 2, Time: 2, GasLimit: blockGasLimit, Random: common.HexToHash("0x33"), BaseFee: new(big.Int)},
	}
}

// forwarderCreationCode makes a tiny Shanghai contract which forwards its
// calldata to a fixed target with CALL. This gives the workload a real nested
// contract call without depending on a Solidity compiler at test time.
func forwarderCreationCode(target common.Address) []byte {
	runtime := []byte{
		0x36, 0x5f, 0x5f, 0x37, // copy the complete calldata to memory[0]
		0x5f, 0x5f, 0x36, 0x5f, 0x5f, 0x73, // CALL output/input/value arguments
	}
	runtime = append(runtime, target.Bytes()...)
	runtime = append(runtime, 0x5a, 0xf1, 0x50, 0x5f, 0x5f, 0xf3) // GAS CALL POP RETURN(0, 0)
	length := len(runtime)
	// PUSH2 length, PUSH2 13, PUSH0, CODECOPY, PUSH2 length, PUSH0, RETURN.
	init := []byte{0x61, byte(length >> 8), byte(length), 0x61, 0, 13, 0x5f, 0x39, 0x61, byte(length >> 8), byte(length), 0x5f, 0xf3}
	return append(init, runtime...)
}

// simpleAMMCreationCode returns a compact test fixture implementing a
// constant-product swap. It stores reserve0/reserve1 at slots 0/1 and exposes
// swap(uint256 amountIn), returning floor(amountIn*reserve1/(reserve0+amountIn)).
// Keeping this tiny fixture in Go avoids making test execution depend on solc.
func simpleAMMCreationCode() []byte {
	selector := crypto.Keccak256([]byte("swap(uint256)"))[:4]
	runtime := []byte{
		0x5f, 0x35, 0x60, 0xe0, 0x1c, // selector = calldata[0:4] >> 224
		0x63, selector[0], selector[1], selector[2], selector[3], 0x14,
		0x60, 0x11, 0x57, // jump to function if selector matches
		0x5f, 0x5f, 0xfd, // unknown selector: revert(0, 0)
		0x5b,
		0x60, 0x04, 0x35, 0x5f, 0x52, // amountIn -> memory[0]
		0x5f, 0x54, 0x60, 0x01, 0x54, // reserve0, reserve1
		0x81, 0x5f, 0x51, 0x01, 0x60, 0x40, 0x52, // denominator -> memory[0x40]
		0x5f, 0x51, 0x81, 0x02, 0x60, 0x60, 0x52, // numerator -> memory[0x60]
		0x60, 0x40, 0x51, 0x60, 0x60, 0x51, 0x04, 0x60, 0x80, 0x52, // amountOut -> memory[0x80]
		0x81, 0x5f, 0x51, 0x01, 0x5f, 0x55, // reserve0 += amountIn
		0x60, 0x80, 0x51, 0x90, 0x03, 0x60, 0x01, 0x55, // reserve1 -= amountOut
		0x60, 0x80, 0x51, 0x5f, 0x52, 0x60, 0x20, 0x5f, 0xf3, // return amountOut
	}
	length := len(runtime)
	const returnCodeOffset = 28
	init := []byte{
		0x63, 0x00, 0x0f, 0x42, 0x40, 0x5f, 0x55, // reserve0 = 1,000,000
		0x63, 0x00, 0x0f, 0x42, 0x40, 0x60, 0x01, 0x55, // reserve1 = 1,000,000
		0x61, byte(length >> 8), byte(length), 0x61, 0, returnCodeOffset, 0x5f, 0x39,
		0x61, byte(length >> 8), byte(length), 0x5f, 0xf3,
	}
	return append(init, runtime...)
}

func newAMMDifferentialWorkload(t testing.TB, swaps int) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 66, 2)
	sender := pool[0]
	amm := crypto.CreateAddress(sender.address, 0)
	txs := make([]*types.Transaction, 0, swaps+1)
	txs = append(txs, signedTx(t, sender.keyHex, 0, 3_000_000, nil, new(big.Int), simpleAMMCreationCode()))
	selector := crypto.Keccak256([]byte("swap(uint256)"))[:4]
	for i := 0; i < swaps; i++ {
		amountIn := big.NewInt(int64(1_000 + i))
		data := append([]byte(nil), selector...)
		data = append(data, common.LeftPadBytes(amountIn.Bytes(), 32)...)
		txs = append(txs, signedTx(t, sender.keyHex, uint64(i+1), 200_000, &amm, new(big.Int), data))
	}
	return differentialWorkload{
		name: "constant-product-amm-swaps", transactions: txs,
		sender: sender.address, contract: amm,
		accounts:        []common.Address{sender.address, pool[1].address, amm},
		initialBalances: map[common.Address]*big.Int{sender.address: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		storageSlots:    map[common.Address][]common.Hash{amm: {common.Hash{}, common.BigToHash(big.NewInt(1))}},
		env:             evm.BlockEnv{Height: 6, Time: 6, GasLimit: blockGasLimit, Random: common.HexToHash("0x66"), BaseFee: new(big.Int)},
	}
}

func newNestedCallDifferentialWorkload(t testing.TB) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 44, 2)
	sender := pool[0]
	counter := crypto.CreateAddress(sender.address, 0)
	forwarder := crypto.CreateAddress(sender.address, 1)
	txs := []*types.Transaction{
		signedTx(t, sender.keyHex, 0, 3_000_000, nil, new(big.Int), readCreationCode(t, "Counter")),
		signedTx(t, sender.keyHex, 1, 300_000, nil, new(big.Int), forwarderCreationCode(counter)),
		signedTx(t, sender.keyHex, 2, 200_000, &forwarder, new(big.Int), common.FromHex("0xd09de08a")),
	}
	return differentialWorkload{
		name: "counter-through-forwarder-contract-call", transactions: txs,
		sender: sender.address, contract: counter,
		accounts:        []common.Address{sender.address, pool[1].address, counter, forwarder},
		initialBalances: map[common.Address]*big.Int{sender.address: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		storageSlots:    map[common.Address][]common.Hash{counter: {common.Hash{}}},
		env:             evm.BlockEnv{Height: 3, Time: 3, GasLimit: blockGasLimit, Random: common.HexToHash("0x44"), BaseFee: new(big.Int)},
	}
}

func newFailureModesDifferentialWorkload(t testing.TB) differentialWorkload {
	t.Helper()
	pool := newAccountPool(t, 55, 2)
	sender := pool[0]
	contract := crypto.CreateAddress(sender.address, 0)
	transactions := []*types.Transaction{
		signedTx(t, sender.keyHex, 0, 3_000_000, nil, new(big.Int), readCreationCode(t, "Counter")),
		// The intrinsic gas is valid, but leaves no gas for increment() execution.
		signedTx(t, sender.keyHex, 1, 22_000, &contract, new(big.Int), common.FromHex("0xd09de08a")),
		// An explicit revert after the out-of-gas transaction.
		signedTx(t, sender.keyHex, 2, 100_000, &contract, new(big.Int), common.FromHex("0xa9cc4718")),
		// Nonce gap: invalid transaction, which must not consume the sender nonce.
		signedTx(t, sender.keyHex, 7, 21_000, &pool[1].address, big.NewInt(1), nil),
	}
	return differentialWorkload{
		name: "counter-out-of-gas-revert-invalid-nonce", transactions: transactions,
		sender: sender.address, recipient: pool[1].address, contract: contract,
		accounts:        []common.Address{sender.address, pool[1].address, contract},
		initialBalances: map[common.Address]*big.Int{sender.address: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		storageSlots:    map[common.Address][]common.Hash{contract: {common.Hash{}}},
		env:             evm.BlockEnv{Height: 5, Time: 5, GasLimit: blockGasLimit, Random: common.HexToHash("0x55"), BaseFee: new(big.Int)},
	}
}

// newTransferWorkload produces signed transfers from a seeded account pool.
// hotWalletRatio controls how often account 0 is selected as the sender.
func newTransferWorkload(t testing.TB, seed uint64, txCount, accountCount int, hotWalletRatio float64) differentialWorkload {
	t.Helper()
	if txCount <= 0 || accountCount < 2 || hotWalletRatio < 0 || hotWalletRatio > 1 {
		t.Fatalf("invalid transfer workload parameters: tx=%d accounts=%d hot ratio=%f", txCount, accountCount, hotWalletRatio)
	}
	pool := newAccountPool(t, seed, accountCount)
	rng := rand.New(rand.NewSource(int64(seed)))
	nonces := make([]uint64, accountCount)
	txs := make([]*types.Transaction, 0, txCount)
	for i := 0; i < txCount; i++ {
		senderIndex := rng.Intn(accountCount)
		if rng.Float64() < hotWalletRatio {
			senderIndex = 0
		}
		recipientIndex := rng.Intn(accountCount - 1)
		if recipientIndex >= senderIndex {
			recipientIndex++
		}
		value := big.NewInt(int64(rng.Intn(1_000_000) + 1))
		txs = append(txs, signedTx(t, pool[senderIndex].keyHex, nonces[senderIndex], 21_000, &pool[recipientIndex].address, value, nil))
		nonces[senderIndex]++
	}
	addresses := make([]common.Address, len(pool))
	balances := make(map[common.Address]*big.Int, len(pool))
	for i, account := range pool {
		addresses[i] = account.address
		balances[account.address] = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	}
	gasLimit := uint64(txCount)*21_000 + 1
	if gasLimit < blockGasLimit {
		gasLimit = blockGasLimit
	}
	return differentialWorkload{
		name: fmt.Sprintf("seeded-hot-wallet-transfers-seed-%d", seed), transactions: txs,
		sender: pool[0].address, recipient: pool[1].address, accounts: addresses,
		initialBalances: balances,
		env:             evm.BlockEnv{Height: 4, Time: 4, GasLimit: gasLimit, Random: common.BigToHash(new(big.Int).SetUint64(seed)), BaseFee: new(big.Int)},
	}
}

// newMixedWorkload generates a replayable block with ordinary transfers,
// contract storage writes, ERC-20 transfers, reverts, out-of-gas calls, and a
// configurable hot sender. The seed is included in the workload name so a
// failing CI log contains the exact input needed to reproduce the case.
func newMixedWorkload(t testing.TB, seed uint64, txCount, accountCount int, hotWalletRatio float64) differentialWorkload {
	t.Helper()
	if txCount < 2 || accountCount < 2 || hotWalletRatio < 0 || hotWalletRatio > 1 {
		t.Fatalf("invalid mixed workload parameters: tx=%d accounts=%d hot ratio=%f", txCount, accountCount, hotWalletRatio)
	}
	pool := newAccountPool(t, seed, accountCount)
	rng := rand.New(rand.NewSource(int64(seed)))
	counter := crypto.CreateAddress(pool[0].address, 0)
	token := crypto.CreateAddress(pool[0].address, 1)
	transactions := make([]*types.Transaction, 0, txCount)
	transactions = append(transactions,
		signedTx(t, pool[0].keyHex, 0, 3_000_000, nil, new(big.Int), readCreationCode(t, "Counter")),
		signedTx(t, pool[0].keyHex, 1, 3_000_000, nil, new(big.Int), readCreationCode(t, "Token")),
	)
	nonces := make([]uint64, accountCount)
	nonces[0] = 2
	transferSelector := common.FromHex("0xa9059cbb")
	for len(transactions) < txCount {
		senderIndex := rng.Intn(accountCount)
		if rng.Float64() < hotWalletRatio {
			senderIndex = 0
		}
		sender := pool[senderIndex]
		recipientIndex := rng.Intn(accountCount - 1)
		if recipientIndex >= senderIndex {
			recipientIndex++
		}
		var tx *types.Transaction
		switch action := rng.Intn(100); {
		case action < 40:
			tx = signedTx(t, sender.keyHex, nonces[senderIndex], 21_000, &pool[recipientIndex].address, big.NewInt(int64(rng.Intn(1_000_000)+1)), nil)
		case action < 60:
			tx = signedTx(t, sender.keyHex, nonces[senderIndex], 100_000, &counter, new(big.Int), common.FromHex("0xd09de08a"))
		case action < 70:
			amount := big.NewInt(int64(rng.Intn(100) + 1))
			data := append(common.FromHex("0x1003e2d2"), common.LeftPadBytes(amount.Bytes(), 32)...)
			tx = signedTx(t, sender.keyHex, nonces[senderIndex], 100_000, &counter, new(big.Int), data)
		case action < 80:
			data := append([]byte(nil), transferSelector...)
			data = append(data, common.LeftPadBytes(pool[recipientIndex].address.Bytes(), 32)...)
			data = append(data, common.LeftPadBytes(big.NewInt(int64(rng.Intn(1000)+1)).Bytes(), 32)...)
			tx = signedTx(t, pool[0].keyHex, nonces[0], 150_000, &token, new(big.Int), data)
			senderIndex = 0
		case action < 90:
			tx = signedTx(t, sender.keyHex, nonces[senderIndex], 100_000, &counter, new(big.Int), common.FromHex("0xa9cc4718"))
		default:
			// 10% calls pass intrinsic-gas validation but run out of execution gas.
			tx = signedTx(t, sender.keyHex, nonces[senderIndex], 22_000, &counter, new(big.Int), common.FromHex("0xd09de08a"))
		}
		transactions = append(transactions, tx)
		nonces[senderIndex]++
	}
	addresses := make([]common.Address, len(pool)+2)
	balances := make(map[common.Address]*big.Int, len(pool))
	for i, account := range pool {
		addresses[i] = account.address
		balances[account.address] = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	}
	addresses[len(pool)] = counter
	addresses[len(pool)+1] = token
	storageSlots := map[common.Address][]common.Hash{
		counter: {common.Hash{}},
		token:   {common.BigToHash(big.NewInt(1))},
	}
	for _, account := range pool {
		slot := crypto.Keccak256Hash(common.LeftPadBytes(account.address.Bytes(), 32), common.Hash{}.Bytes())
		storageSlots[token] = append(storageSlots[token], slot)
	}
	gasLimit := uint64(txCount)*150_000 + 6_000_000
	if gasLimit < blockGasLimit {
		gasLimit = blockGasLimit
	}
	return differentialWorkload{
		name: fmt.Sprintf("seeded-mixed-evm-workload-seed-%d", seed), transactions: transactions,
		sender: pool[0].address, recipient: pool[1].address, contract: counter,
		accounts: addresses, initialBalances: balances, storageSlots: storageSlots,
		env: evm.BlockEnv{Height: 7, Time: 7, GasLimit: gasLimit, Random: common.BigToHash(new(big.Int).SetUint64(seed)), BaseFee: new(big.Int)},
	}
}

func TestDifferentialHarnessERC20MatchesGeth(t *testing.T) {
	workload := newERC20DifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]
	if got := pluto.accounts[workload.contract].storage[workload.storageSlots[workload.contract][2]]; got != common.BigToHash(new(big.Int).Mul(big.NewInt(7), big.NewInt(1e18))) {
		t.Fatalf("ERC20 recipient balance = %s, want 7 tokens", got)
	}
}

func TestDifferentialHarnessCounterContentionMatchesGeth(t *testing.T) {
	const increments = 32
	workload := newCounterContentionWorkload(t, increments)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]
	if got := pluto.accounts[workload.contract].storage[common.Hash{}]; got != common.BigToHash(big.NewInt(increments)) {
		t.Fatalf("Counter.count = %s, want %d", got, increments)
	}
}

func TestDifferentialHarnessNestedCallMatchesGeth(t *testing.T) {
	workload := newNestedCallDifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]
	if got := pluto.accounts[workload.contract].storage[common.Hash{}]; got != common.BigToHash(big.NewInt(1)) {
		t.Fatalf("Counter.count after forwarded call = %s, want 1", got)
	}
}

func TestDifferentialHarnessConstantProductAMMMatchesGeth(t *testing.T) {
	const swaps = 8
	workload := newAMMDifferentialWorkload(t, swaps)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]
	if pluto.results[0].Failed {
		t.Fatal("constant-product AMM runtime failed on creation")
	}
	for i := 1; i < len(pluto.results); i++ {
		if pluto.results[i].Failed || pluto.results[i].Invalid {
			t.Fatalf("AMM swap tx[%d] failed: %+v", i, pluto.results[i])
		}
	}
	reserve0 := big.NewInt(1_000_000)
	reserve1 := big.NewInt(1_000_000)
	for i := 0; i < swaps; i++ {
		amountIn := big.NewInt(int64(1_000 + i))
		amountOut := new(big.Int).Mul(amountIn, reserve1)
		amountOut.Div(amountOut, new(big.Int).Add(new(big.Int).Set(reserve0), amountIn))
		reserve0.Add(reserve0, amountIn)
		reserve1.Sub(reserve1, amountOut)
	}
	if got := pluto.accounts[workload.contract].storage[common.Hash{}]; got != common.BigToHash(reserve0) {
		t.Fatalf("AMM reserve0 = %s, want %s", got, reserve0)
	}
	if got := pluto.accounts[workload.contract].storage[common.BigToHash(big.NewInt(1))]; got != common.BigToHash(reserve1) {
		t.Fatalf("AMM reserve1 = %s, want %s; first swap return=%x", got, reserve1, pluto.results[1].ReturnData)
	}
}

func TestDifferentialHarnessFailureModesMatchGeth(t *testing.T) {
	workload := newFailureModesDifferentialWorkload(t)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
	)
	pluto := observations["Pluto sequential"]
	if !pluto.results[1].Failed || !pluto.results[2].Failed || !pluto.results[3].Invalid {
		t.Fatalf("failure status mismatch: OOG failed=%t revert failed=%t nonce-gap invalid=%t", pluto.results[1].Failed, pluto.results[2].Failed, pluto.results[3].Invalid)
	}
	if got := pluto.accounts[workload.contract].storage[common.Hash{}]; got != (common.Hash{}) {
		t.Fatalf("failed Counter calls changed count: %s", got)
	}
}

func TestCheckpointPoliciesMatchSequentialAndGethAcrossWorkloads(t *testing.T) {
	const seed = uint64(20261020)
	workloads := []differentialWorkload{
		newCounterDifferentialWorkload(t),
		newCounterContentionWorkload(t, 32),
		newERC20DifferentialWorkload(t),
		newAMMDifferentialWorkload(t, 8),
		newNestedCallDifferentialWorkload(t),
		newFailureModesDifferentialWorkload(t),
		newMixedWorkload(t, seed, 64, 8, 0.9),
	}
	for _, workload := range workloads {
		workload := workload
		t.Run(workload.name, func(t *testing.T) {
			compareAllExecutors(t, workload, checkpointPolicyExecutors(t, 4)...)
		})
	}
}

func TestCheckpointPoliciesConcurrentSeededWorkload(t *testing.T) {
	const seed = uint64(0x5a17cafe)
	workload := newMixedWorkload(t, seed, 48, 8, 0.85)
	sequential := runPlutoDifferentialWorkload(t, evm.NewSequentialExecutor(newChainConfig(), chainID), workload)
	geth := runGethDifferentialWorkload(t, workload)
	assertDifferentialObservationsEqual(t, workload, "Pluto sequential", sequential, "geth reference", geth)

	for _, named := range checkpointPolicyExecutors(t, 4)[1:] {
		named := named
		t.Run(named.name, func(t *testing.T) {
			t.Parallel()
			got := runPlutoDifferentialWorkload(t, named.ev, workload)
			assertDifferentialObservationsEqual(t, workload, named.name, got, "Pluto sequential", sequential)
		})
	}
}

func TestDeterministicAccountPoolAndWorkload(t *testing.T) {
	first := newAccountPool(t, 99, 4)
	second := newAccountPool(t, 99, 4)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("account pool changed for same seed at index %d", i)
		}
	}
	workloadA := newTransferWorkload(t, 101, 50, 8, 0.75)
	workloadB := newTransferWorkload(t, 101, 50, 8, 0.75)
	for i := range workloadA.transactions {
		if workloadA.transactions[i].Hash() != workloadB.transactions[i].Hash() {
			t.Fatalf("transaction %d changed for same workload seed", i)
		}
	}
}

func FuzzDifferentialHarnessSeededMixedBlocks(f *testing.F) {
	f.Add(uint64(1))
	f.Add(uint64(20261012))
	f.Add(uint64(0xdeadbeef))
	f.Fuzz(func(t *testing.T, seed uint64) {
		workload := newMixedWorkload(t, seed, 32, 8, 0.7)
		compareAllExecutors(t, workload, checkpointPolicyExecutors(t, 4)...)
	})
}

func TestDifferentialHarnessTenThousandSeededMixedTransactions(t *testing.T) {
	const seed = 20261012
	workload := newMixedWorkload(t, seed, 10_000, 16, 0.8)
	observations := compareAllExecutors(t, workload,
		namedExecutor{name: "Pluto sequential", ev: evm.NewSequentialExecutor(newChainConfig(), chainID)},
		namedExecutor{name: "Pluto parallel", ev: evm.NewParallelExecutor(newChainConfig(), chainID, 4)},
	)
	pluto := observations["Pluto sequential"]
	parallel := observations["Pluto parallel"]
	if parallel.metrics["tx_class_s"] == 0 || parallel.metrics["tx_class_o"] == 0 || parallel.metrics["tx_parallel"] == 0 {
		t.Fatalf("seed=%d did not exercise both execution paths: S=%v O=%v parallel=%v", seed, parallel.metrics["tx_class_s"], parallel.metrics["tx_class_o"], parallel.metrics["tx_parallel"])
	}
	var nativeTransfers, counterCalls, tokenTransfers, revertCalls, outOfGasCalls int
	for i := 2; i < len(workload.transactions); i++ {
		tx := workload.transactions[i]
		if tx.To() == nil {
			continue
		}
		switch {
		case *tx.To() == workload.contract:
			switch {
			case string(tx.Data()[:4]) == string(common.FromHex("0xa9cc4718")):
				revertCalls++
			case tx.Gas() == 22_000:
				outOfGasCalls++
			default:
				counterCalls++
			}
		case len(tx.Data()) == 68:
			tokenTransfers++
		default:
			nativeTransfers++
		}
	}
	if nativeTransfers == 0 || counterCalls == 0 || tokenTransfers == 0 || revertCalls == 0 || outOfGasCalls == 0 {
		t.Fatalf("seed %d did not cover all transaction classes: native=%d counter=%d token=%d revert=%d out-of-gas=%d", seed, nativeTransfers, counterCalls, tokenTransfers, revertCalls, outOfGasCalls)
	}
	if len(pluto.results) != 10_000 {
		t.Fatalf("processed %d transactions, want 10000", len(pluto.results))
	}
}
