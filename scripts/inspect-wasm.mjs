#!/usr/bin/env node

import { readFileSync } from "node:fs";

const [mode, path] = process.argv.slice(2);
if (!path || !["imports", "exports"].includes(mode)) {
  console.error("usage: inspect-wasm.mjs <imports|exports> <artifact.wasm>");
  process.exit(2);
}

const module = new WebAssembly.Module(readFileSync(path));
const entries =
  mode === "imports"
    ? WebAssembly.Module.imports(module).map(({ module: owner, name, kind }) =>
        `${owner}.${name}:${kind}`,
      )
    : WebAssembly.Module.exports(module).map(({ name, kind }) => `${name}:${kind}`);

for (const entry of entries.sort()) {
  console.log(entry);
}
