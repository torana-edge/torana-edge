// AssemblyScript plugin for Torana Edge
// Build: npx asc assembly.ts -o plugin.wasm --target release

// Host functions
@external("env", "log")
declare function env_log(level: i32, ptr: usize, len: i32): void;

export function alloc(size: u32): usize {
  return __alloc(size);
}

export function dealloc(ptr: usize, size: u32): void {
  __free(ptr);
}

export function on_chat_request(ptr: u32, size: u32): u64 {
  // Pass-through: 0 means no modification
  let msg = String.UTF8.encode("Hello from AssemblyScript plugin!");
  env_log(1, changetype<usize>(msg), msg.byteLength);
  return 0;
}
