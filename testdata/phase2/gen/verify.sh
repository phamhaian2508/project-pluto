#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PHASE2_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${PHASE2_ROOT}/../.." && pwd)"
EVM_BIN="${EVM_BIN:-/tmp/plutof-evm-v1168/evm}"
PLUTO_LAB_BIN="${PLUTO_LAB_BIN:-/tmp/pluto-lab-p2-verify}"

cd "${REPO_ROOT}"
GOWORK=off go build -o "${PLUTO_LAB_BIN}" ./cmd/pluto-lab

CASES=(
  checkpoint-independent
  checkpoint-early
  checkpoint-late
  checkpoint-sstore-gas
  checkpoint-errors
)

for c in "${CASES[@]}"; do
  echo "=== verify ${c} (sequential) ==="
  "${PLUTO_LAB_BIN}" verify --testdata "${PHASE2_ROOT}" --case "${c}" --mode sequential --evm-bin "${EVM_BIN}"
  echo "=== verify ${c} (occ 4w) ==="
  "${PLUTO_LAB_BIN}" verify --testdata "${PHASE2_ROOT}" --case "${c}" --mode occ --workers 4 --evm-bin "${EVM_BIN}"
done

echo "ALL 5 PHASE 2 FIXTURE CASES VERIFIED WITH PLUTO-LAB (SEQUENTIAL + OCC) & EVM T8N!"
