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

// run_before_request receives a serialized torana.v1.ChatRequest protobuf.
// Return 0 to pass the request through unchanged, or pack a pointer/length
// to a re-serialized ChatRequest as ((ptr as u64) << 32) | len.
//
// A real plugin would decode the payload with `prost` (see sdk/pb/torana.proto
// for the schema). This example validates the polyglot ABI surface only:
// alloc/dealloc, the 3-argument hook signature, and the u64 return packing.
#[no_mangle]
pub extern "C" fn run_before_request(_req_id: u64, ptr: u32, size: u32) -> u64 {
    let _input = unsafe { std::slice::from_raw_parts(ptr as *const u8, size as usize) };
    // Pass-through.
    0
}
