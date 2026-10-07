# Foundry end-to-end test

## Prerequisites

- Install Foundry so `forge` and `cast` are available in `PATH`.
- Fund the test account in `genesis.json` **before the chain is initialized**:

```json
"app_state": {
  "alloc": {
    "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266": {
      "balance": "1000000000000000000000"
    }
  }
}
```

The included private key is Foundry/Anvil's public development key. Never use it on a real network. If the current `.pluto-home/data` was already initialized without this allocation, use a backed-up and freshly initialized development home; changing genesis does not retroactively change existing state.

## Run

Start Pluto in terminal 1:

```powershell
go run ./cmd/plutod --home .\.pluto-home
```

Run the complete test in terminal 2:

```powershell
.\scripts\foundry-e2e.ps1
```

The script verifies chain ID and balance, sends 1 wei, checks its receipt, compiles and deploys `Counter`, calls `count()`, submits `increment()`, and verifies that `count()` becomes 1.

To use another funded key or endpoint:

```powershell
.\scripts\foundry-e2e.ps1 -RpcUrl http://127.0.0.1:8545 -PrivateKey 0xYOUR_TEST_KEY
```
