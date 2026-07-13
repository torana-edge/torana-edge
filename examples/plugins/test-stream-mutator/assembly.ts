// AssemblyScript plugin for Torana Edge
// Build: npx asc assembly.ts -o plugin.wasm --target release

@external("env", "log")
declare function env_log(level: i32, ptr: usize, len: i32): void;

export function alloc(size: u32): usize {
  return __alloc(size);
}

export function dealloc(ptr: usize, size: u32): void {
  __free(ptr);
}

export function on_stream_event(ptr: u32, size: u32): u64 {
  let input = String.UTF8.decodeUnsafe(ptr, size);
  
  // If the input has a TextDelta, mutate it.
  // This is a naive regex-like replace since AS doesn't have full regex or JSON parser out of box.
  // We'll just check if it contains `"TextDelta":"secret"`
  if (input.includes('"TextDelta":"secret"')) {
    let output = input.replace('"TextDelta":"secret"', '"TextDelta":"[REDACTED]"');
    let outBuf = String.UTF8.encode(output);
    let outPtr = changetype<usize>(outBuf);
    return (u64(outPtr) << 32) | u64(outBuf.byteLength);
  }

  // Pass-through
  return 0;
}
