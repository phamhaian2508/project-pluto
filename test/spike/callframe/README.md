# M5.1 spike 1B: checkpoint at an EVM call-frame boundary

This spike uses geth unchanged and is isolated from Pluto's production executor.
It reuses `gethfork.SampleContract` from spike 1A.

## Result

- **Works at an external, complete frame boundary:** take a `StateDB` snapshot
  before a top-level call, revert to it, and replay the whole parent frame. The
  test proves identical output, gas, nested trace, and storage.
- **Observable:** public tracing hooks report nested `CALL` entry/exit,
  `returnData`, gas used, revert, and out-of-gas. Emitted logs remain available
  in `StateDB` and are rolled back by snapshots.
- **Not resumable at a nested `CALL` boundary:** `OnExit` does not expose the
  suspended parent's PC, stack, memory, remaining gas, or return-data buffer.
  Public geth APIs also provide no way to restore those fields. Therefore a
  checkpoint captured when a child returns cannot continue the parent at its
  next opcode; the whole parent frame must be replayed.

This approach changes no geth interpreter code and has a small maintenance
surface, but its checkpoint granularity is coarser than spike 1A. It does not
implement production integration or M5.2.
