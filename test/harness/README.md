# Pluto differential EVM harness

The harness lives outside production packages so running it does not alter node behavior. It creates test-only Pebble state, signs transactions from deterministic accounts, executes the same block through Pluto executors and go-ethereum, and reports the first block/transaction mismatch.

## Workloads

- deterministic account pools and signed transfer workloads with a configurable hot sender ratio;
- Counter deploy, increments, storage contention, and forwarded contract calls;
- ERC-20 deployment and `transfer`;
- a small constant-product AMM fixture with repeatable `swap(uint256)` calls;
- revert, out-of-gas, and invalid-nonce transactions;
- a seeded mixed block with 10,000 transactions.

The fixture in `differential_workloads_test.go` implements the behavior of `contracts/SimpleAMM.sol` directly as EVM bytecode, so CI does not need a Solidity compiler. The fixture's reserve values are asserted independently of the geth comparison.

## Run

Run the full harness, including the 10,000-transaction seeded block:

```powershell
go test ./test/harness -count=1 -v
```

Run the large reproducible comparison by itself:

```powershell
go test ./test/harness -run '^TestDifferentialHarnessTenThousandSeededMixedTransactions$' -count=1 -v
```

Run the Go fuzz target for a chosen duration:

```powershell
go test ./test/harness -run '^$' -fuzz '^FuzzDifferentialHarnessSeededMixedBlocks$' -fuzztime=5m
```

Run workload benchmarks for transfer, hot-wallet, Counter, ERC-20, and AMM cases:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkDifferentialWorkloads$' -benchmem -count=1
```

Each generated workload name includes its seed. A failure prints that seed and the first differing block/transaction; Go also saves minimized fuzz failures under `test/harness/testdata/fuzz/` for replay.

## Comparison rules

The harness compares transaction validity/status, gas, return data, logs, balances, nonces, runtime code, and declared storage slots. Pluto executor implementations are also compared against each other including Pluto's deterministic state root. geth computes an Ethereum Merkle Patricia Trie root, while Pluto currently uses a separate application hash, so those two roots are not treated as interchangeable; geth is compared using observable account/contract state instead.

`compareAllExecutors` accepts any implementation of `evm.Executor`. Add the parallel implementation to its executor list when it is available; the harness then compares it against geth and the sequential Pluto baseline using the same signed workload.

`mutation_test.go` contains deliberately faulty test-only executors. The tests verify that the comparator catches an accepted invalid transaction and a wrong committed storage value. They do not modify the production executor.
