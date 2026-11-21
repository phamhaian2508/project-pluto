#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PHASE3_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
EVM_BIN="${EVM_BIN:-/tmp/plutof-evm-v1168/evm}"
PLUTO_LAB_BIN="${PLUTO_LAB_BIN:-/tmp/project-pluto-lab}"

if [[ ! -x "${PLUTO_LAB_BIN}" ]]; then
  echo "pluto-lab binary not executable: ${PLUTO_LAB_BIN}" >&2
  exit 1
fi

for c in merkle-2tx-early merkle-2tx-late merkle-errors; do
  echo "=== verify ${c} ==="
  "${PLUTO_LAB_BIN}" verify --testdata "${PHASE3_ROOT}" --case "${c}" --mode occ --workers 4 --evm-bin "${EVM_BIN}"
done

for s in seed-31 seed-73; do
  for c in merkle-early merkle-late merkle-mixed merkle-independent-early merkle-independent-late; do
    echo "=== verify ${s}/${c} ==="
    "${PLUTO_LAB_BIN}" verify --testdata "${PHASE3_ROOT}/${s}" --case "${c}" --mode occ --workers 4 --evm-bin "${EVM_BIN}"
  done
done

echo "ALL 13 PHASE 3 FIXTURE CASES VERIFIED WITH PLUTO-LAB + EVM T8N!"
