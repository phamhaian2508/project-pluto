import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const nodeModulesDir = process.env.SOLC_NODE_MODULES
  ? path.resolve(process.env.SOLC_NODE_MODULES)
  : path.resolve(here, "node_modules");

const req = createRequire(path.join(nodeModulesDir, "_anchor.js"));
const solc = req("solc");

const source = fs.readFileSync(path.join(here, "ApproveToken.sol"), "utf8");

const input = {
  language: "Solidity",
  sources: {
    "ApproveToken.sol": { content: source },
  },
  settings: {
    evmVersion: "shanghai",
    optimizer: { enabled: false },
    metadata: { bytecodeHash: "none" },
    outputSelection: {
      "*": {
        "*": [
          "abi",
          "evm.bytecode.object",
          "evm.deployedBytecode.object",
          "storageLayout",
        ],
      },
    },
  },
};

function findImports(importPath) {
  const candidates = [
    path.join(here, importPath),
    path.join(nodeModulesDir, importPath),
  ];
  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      return { contents: fs.readFileSync(candidate, "utf8") };
    }
  }
  return { error: `File not found: ${importPath}` };
}

const output = JSON.parse(
  solc.compile(JSON.stringify(input), { import: findImports })
);

if (output.errors) {
  const fatal = output.errors.filter((e) => e.severity === "error");
  for (const err of output.errors) {
    console.error(err.formattedMessage);
  }
  if (fatal.length > 0) {
    process.exit(1);
  }
}

const contract = output.contracts["ApproveToken.sol"]["ApproveToken"];
if (!contract) {
  console.error("ApproveToken contract output missing");
  process.exit(1);
}

const runtimeHex = "0x" + contract.evm.deployedBytecode.object;
fs.writeFileSync(path.join(here, "runtime-bytecode.hex"), runtimeHex + "\n");
fs.writeFileSync(
  path.join(here, "storage-layout.json"),
  JSON.stringify(contract.storageLayout, null, 2) + "\n"
);
fs.writeFileSync(
  path.join(here, "abi.json"),
  JSON.stringify(contract.abi, null, 2) + "\n"
);

console.log(
  JSON.stringify(
    {
      solcVersion: solc.version(),
      runtimeBytecodeBytes: contract.evm.deployedBytecode.object.length / 2,
      storageEntries: contract.storageLayout.storage.map((s) => ({
        label: s.label,
        slot: s.slot,
        offset: s.offset,
        type: s.type,
      })),
    },
    null,
    2
  )
);
