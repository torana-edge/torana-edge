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

// run_before_request receives a serialized torana.v1.ChatRequest protobuf
// (see sdk/pb/torana.proto). Return 0 to pass the request through unchanged.
export function run_before_request(reqID: u64, ptr: u32, size: u32): u64 {
  let msg = String.UTF8.encode("Hello from AssemblyScript plugin!");
  env_log(1, changetype<usize>(msg), msg.byteLength);
  return 0;
}
