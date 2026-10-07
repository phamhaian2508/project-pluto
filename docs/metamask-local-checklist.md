# MetaMask local checklist

## Prepare the local chain

- The sender must be funded in `genesis.json` before the chain is initialized. The Foundry development account and allocation example are documented in `foundry-e2e.md`.
- Start the node with the same home directory every time:

```powershell
go run ./cmd/plutod --home .\.pluto-home
```

- Confirm that `http://127.0.0.1:8545` is listening.

## Add the network to MetaMask

Use these values in **Settings → Networks → Add network manually**:

| Field | Value |
|---|---|
| Network name | Pluto Local |
| RPC URL | `http://127.0.0.1:8545` |
| Chain ID | `31337` |
| Currency symbol | `PLUTO` |
| Block explorer | Leave empty |

Import only a development account whose address is funded in genesis. Never use the included public test key on a real network.

## Send and verify a transaction

- Select **Pluto Local** and verify that the funded balance appears.
- Send a small amount to a second local address.
- Confirm the transaction in MetaMask and record its transaction hash.
- Wait until MetaMask changes the transaction from pending to confirmed.
- Verify the receipt directly if Foundry is installed:

```powershell
cast receipt 0xTRANSACTION_HASH --rpc-url http://127.0.0.1:8545
```

Expected receipt checks:

- `status` is `0x1`.
- `transactionHash` matches the submitted hash.
- `blockHash` and `blockNumber` are present.
- `gasUsed` is non-zero; a plain value transfer uses `0x5208` gas.
- Sender nonce increases and recipient balance changes.

## Restart persistence check

- Record the current block number, sender/recipient balances, transaction hash, and any deployed contract address/state.
- Stop Pluto cleanly with `Ctrl+C` and wait for shutdown to finish.
- Start it again with the exact same `--home .\.pluto-home` argument.
- Re-select or refresh **Pluto Local** in MetaMask.
- Confirm that the block number does not move backwards and balances remain unchanged.
- Query the recorded transaction hash again and confirm that its receipt is still available.
- If `Counter` was deployed, call `count()` again and confirm the stored value survived restart:

```powershell
cast call 0xCOUNTER_ADDRESS "count()(uint256)" --rpc-url http://127.0.0.1:8545
```

Do not delete `.pluto-home/data` or switch the home directory during the restart check; either action starts from different state.
