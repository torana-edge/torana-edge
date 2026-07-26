# ts-logger (AssemblyScript example plugin)

Demonstrates the Torana plugin ABI from a non-Go language.

## Build

Dependencies are **not** committed. Restore them first:

```bash
npm install
npm run build   # asc assembly.ts -o plugin.wasm --target release
```

`node_modules/` and `plugin.wasm` are both gitignored — they are build inputs and
outputs, not source. `package.json` and `package-lock.json` pin the toolchain
(`assemblyscript ^0.27.0`), so the build is reproducible.
