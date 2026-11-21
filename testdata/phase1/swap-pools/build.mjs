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

const source = fs.readFileSync(path.join(here, "SwapAMM.sol"), "utf8");

const input = {
  language: "Solidity",
  sources: {
    "SwapAMM.sol": { content: source },
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

const contracts = output.contracts["SwapAMM.sol"];
const targets = [
  { contract: "SwapToken", prefix: "token" },
  { contract: "SwapPool", prefix: "pool" },
  { contract: "SwapRouter", prefix: "router" },
];

const summary = { solcVersion: solc.version(), contracts: {} };
for (const t of targets) {
  const c = contracts[t.contract];
  if (!c) {
    console.error(`Missing contract output: ${t.contract}`);
    process.exit(1);
  }
  const rtHex = "0x" + c.evm.deployedBytecode.object;
  fs.writeFileSync(path.join(here, `${t.prefix}-runtime-bytecode.hex`), rtHex + "\n");
  fs.writeFileSync(
    path.join(here, `${t.prefix}-abi.json`),
    JSON.stringify(c.abi, null, 2) + "\n"
  );
  fs.writeFileSync(
    path.join(here, `${t.prefix}-storage-layout.json`),
    JSON.stringify(c.storageLayout, null, 2) + "\n"
  );
  summary.contracts[t.contract] = {
    runtimeBytecodeBytes: c.evm.deployedBytecode.object.length / 2,
    storage: c.storageLayout.storage.map((s) => ({
      label: s.label,
      slot: s.slot,
      offset: s.offset,
      type: s.type,
    })),
  };
}

console.log(JSON.stringify(summary, null, 2));
