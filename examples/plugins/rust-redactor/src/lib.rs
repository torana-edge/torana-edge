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

#[no_mangle]
pub extern "C" fn on_chat_request(ptr: u32, size: u32) -> u64 {
    let input = unsafe { std::slice::from_raw_parts(ptr as *const u8, size as usize) };
    let mut value: serde_json::Value = serde_json::from_slice(input).unwrap_or_default();
    value["handled_by"] = serde_json::json!("rust-redactor.wasm");
    let output = serde_json::to_vec(&value).unwrap_or_default();
    let out_ptr = alloc(output.len() as u32);
    unsafe { std::ptr::copy_nonoverlapping(output.as_ptr(), out_ptr as *mut u8, output.len()) }
    ((out_ptr as u64) << 32) | (output.len() as u64)
}
