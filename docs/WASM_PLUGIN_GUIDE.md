# Torana WASM Plugin Development Guide (For AI Agents & Humans)

This document is a critical reference for implementing WebAssembly (WASM) plugins in Torana Edge. **AI Coding Agents MUST read this document before generating or modifying Torana WASM plugins.**

## 1. The Core Architecture (Linear Memory)
WASM plugins in Torana run inside a highly restricted sandbox (using `wazero`). 
The Go host (Torana) and the guest (the plugin) do NOT share variables, structs, or garbage collection. They only share a single, flat byte array called **Linear Memory**.

To pass a JSON string from the host to the plugin:
1. The host calls the plugin's `alloc(size)` function.
2. The plugin allocates memory and returns a 32-bit pointer.
3. The host writes the JSON string into the plugin's memory at that pointer.
4. The host calls the plugin's hook (e.g., `on_chat_request(ptr, size)`).

## 2. The Golden Rule of Memory Allocation
**NEVER USE STATIC BUMP ALLOCATORS.**

### ❌ WRONG (Causes OOM Crashes):
```typescript
let bump: u32 = 0;
export function alloc(size: u32): u32 {
  let ptr = bump;
  bump += size;
  return ptr;
}
```
*Why it fails:* The `bump` pointer only goes up. Even if the host calls `dealloc`, the memory is never reused. After a few megabytes of JSON requests, the plugin will crash the server.

### ✅ CORRECT:
Use the standard library allocator for your language.
* **Go (TinyGo)**:
  ```go
  var memory map[uint64][]byte // For tracking allocations
  // Use standard make([]byte) and return unsafe.Pointer
  ```
  *(See `pkg/plugin-sdk` for the robust Go implementation)*
* **Rust**:
  ```rust
  use std::alloc::{alloc, dealloc, Layout};
  
  #[no_mangle]
  pub extern "C" fn alloc(size: u32) -> u32 {
      let layout = Layout::array::<u8>(size as usize).unwrap();
      unsafe { alloc(layout) as u32 }
  }
  
  #[no_mangle]
  pub extern "C" fn dealloc(ptr: u32, size: u32) {
      let layout = Layout::array::<u8>(size as usize).unwrap();
      unsafe { dealloc(ptr as *mut u8, layout) }
  }
  ```
* **AssemblyScript**:
  Export the built-in allocator wrappers.
  ```typescript
  export function alloc(size: u32): usize {
    return __alloc(size);
  }
  export function dealloc(ptr: usize): void {
    __free(ptr);
  }
  ```

## 3. The 64-bit Return ABI
Hooks like `on_chat_request(ptr: u32, size: u32)` must return a **64-bit integer (`u64` or `uint64`)**.
Because WASM32 only supports 32-bit pointers, we pack the pointer and the length of the response into a single 64-bit integer.

* **Format**: `(pointer << 32) | size`
* **Pass-Through**: If you don't want to modify the request, return `0`.

### Packing Example (Rust):
```rust
let out_ptr = alloc(output.len() as u32);
// ... copy data to out_ptr ...
return ((out_ptr as u64) << 32) | (output.len() as u64);
```

## 4. Host Functions (`env.*`)
Torana exports several functions to the plugin via the `env` module.
If you use these, you must request them in your `plugin.json` under the `permissions` array, or the host will reject the plugin.

* `env.log(level: i32, ptr: i32, len: i32)`
* `env.emit_metric(type: i32, ptr: i32, len: i32, value: f64)`

When passing strings TO the host, you don't need to pack them into a 64-bit integer. You just pass the 32-bit `ptr` and `len` as separate arguments.

## 5. Summary Checklist for AI Agents
1. Did I use a real allocator (not a bump allocator)?
2. Did I implement both `alloc` and `dealloc`?
3. Did I pack the return pointer and size into a `u64`?
4. Did I return `0` for passthrough?
5. Did I parse and serialize JSON properly within the memory bounds?
