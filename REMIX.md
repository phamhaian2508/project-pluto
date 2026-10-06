# Connect Remix to Pluto

Pluto exposes an Ethereum-compatible JSON-RPC endpoint for local development.
The HTTP endpoint accepts JSON-RPC at `http://127.0.0.1:8545`; signed raw
transactions are forwarded to the local CometBFT RPC at `127.0.0.1:26657`.

## Start Pluto

From the repository root, run:

```powershell
go run ./cmd/plutod --home .\.pluto-home
```

Keep this terminal open while using Remix.

## Add the local network to MetaMask

Add a network with these values:

| Setting | Value |
| --- | --- |
| Network name | Pluto Local |
| RPC URL | `http://127.0.0.1:8545` |
| Chain ID | `31337` |
| Currency symbol | `ETH` |

Use a funded local development account. Do not import a wallet containing real
funds into this development chain.

## Deploy and call Counter

1. Open [Remix](https://remix.ethereum.org) and open `contracts/Counter.sol` from
   this repository, or paste its source into a new Remix file.
2. Compile the contract.
3. In **Deploy & Run Transactions**, select **Injected Provider - MetaMask** and
   confirm MetaMask is connected to **Pluto Local**.
4. Select `Counter` and click **Deploy**. Confirm the transaction in MetaMask.
5. In **Deployed Contracts**, expand the new contract, call `increment()`, then
   read `count`. The value should increase by one.

The RPC endpoint serves the latest committed state and transaction/block
metadata. Historical state queries are not available yet.
