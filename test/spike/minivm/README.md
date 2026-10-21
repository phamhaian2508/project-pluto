# M5.1 spike 1C: minimal interpreter

This spike is isolated from Pluto production and reuses the same contract as
spikes 1A/1B. It implements exactly **7 opcodes**: `PUSH0`, `PUSH1`, `ADD`,
`SLOAD`, `SSTORE`, `MSTORE`, and `RETURN`.

The checkpoint owns the PC, stack, memory, remaining gas, storage writes, and
warm-storage set. A test pauses immediately after `SSTORE`, restores a new
machine, and verifies output, storage, and exact remaining gas against both an
uninterrupted mini-interpreter run and geth v1.16.8.

## Complexity and maintenance risk

- The implementation is 299 lines (273 non-blank), plus 76 test lines. Its
  execution core is one loop and one switch with exactly seven opcode cases;
  the checkpoint carries six state groups (PC, stack, memory, gas, writes, and
  warm slots).
- Exact gas for this sample requires EIP-2929 warm/cold storage accounting,
  EIP-2200 `SSTORE` costs, and memory expansion cost. State correctness requires
  a separate write overlay and a warm-slot set in every checkpoint.
- It does not implement refunds, committed-state history, hard-fork selection,
  jumps, calls, logs, revert, create, precompiles, access-list input, or any
  opcode outside the seven listed above.
- Extending it would mean independently maintaining consensus-critical EVM
  semantics as geth and hard-fork rules evolve. This is the highest-risk option
  despite the small proof-of-concept.

No production executor integration or M5.2 work is included.
