// mcp-memory-server
// A standalone Rust/wasm32-wasip1 example plugin for the Nexus plugin loader.
//
// Routes:
//   /mcp/sse      - SSE endpoint advertising the message POST URL.
//   /mcp/messages - JSON-RPC endpoint for MCP initialize/tools/list/tools/call.
//   /memory/search - Vector search demo (requires vector:search capability).
//
// Build:
//   cargo build --target wasm32-wasip1 --release
//   cp target/wasm32-wasip1/release/mcp-memory-server.wasm ./plugin.wasm
//
// This crate lives outside the main Nexus Go module so it can be developed
// and versioned as an independent project.

use std::str;

// ---------------------------------------------------------------------------
// Host imports (nexus:host module)
// ---------------------------------------------------------------------------

#[link(wasm_import_module = "nexus:host")]
unsafe extern "C" {
    #[link_name = "request.read"]
    fn host_request_read(
        req_handle: u64,
        out_ptr: u32,
        out_cap: u32,
        out_len: *mut u32,
        out_status: *mut u32,
    );

    #[link_name = "request.body"]
    fn host_request_body(
        req_handle: u64,
        out_ptr: u32,
        out_cap: u32,
        out_len: *mut u32,
        out_status: *mut u32,
    );

    #[link_name = "route.response-write"]
    fn host_route_response_write(
        token: u64,
        req_handle: u64,
        status_code: u32,
        headers_ptr: u32,
        headers_len: u32,
        body_ptr: u32,
        body_len: u32,
        out_status: *mut u32,
    );

    #[link_name = "vector.search"]
    fn host_vector_search(
        token: u64,
        bucket_ptr: u32,
        bucket_len: u32,
        query_ptr: u32,
        query_len_floats: u32,
        top_k: u32,
        out_status: *mut u32,
        out_result_ptr: u32,
        out_result_cap: u32,
        out_result_len: *mut u32,
    );

    #[link_name = "vector.index"]
    fn host_vector_index(
        token: u64,
        bucket_ptr: u32,
        bucket_len: u32,
        key_ptr: u32,
        key_len: u32,
        vec_ptr: u32,
        vec_len_floats: u32,
        out_status: *mut u32,
    );

    #[link_name = "state.get"]
    fn host_state_get(
        token: u64,
        key_ptr: u32,
        key_len: u32,
        val_ptr: u32,
        val_cap: u32,
        out_status: *mut u32,
        out_len: *mut u32,
        out_version: *mut u64,
    );

    #[link_name = "state.put"]
    fn host_state_put(
        token: u64,
        key_ptr: u32,
        key_len: u32,
        val_ptr: u32,
        val_len: u32,
        expected_version: u64,
        out_status: *mut u32,
        out_version: *mut u64,
    );
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

static mut GLOBAL_TOKEN: u64 = 0;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn set_token(token: u64) {
    unsafe { GLOBAL_TOKEN = token }
}

fn token() -> u64 {
    unsafe { GLOBAL_TOKEN }
}

fn string_to_ptr(s: &str) -> (u32, u32) {
    (s.as_ptr() as u32, s.len() as u32)
}

fn vec_to_ptr(v: &Vec<u8>) -> (u32, u32) {
    (v.as_ptr() as u32, v.len() as u32)
}

fn escape_json_string(s: &str) -> String {
    let mut out = String::new();
    for ch in s.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c.is_control() => out.push(' '),
            c => out.push(c),
        }
    }
    out
}

fn embedding_for_text(text: &str) -> [f32; 4] {
    let mut hash: u32 = 2166136261;
    for b in text.as_bytes() {
        hash ^= *b as u32;
        hash = hash.wrapping_mul(16777619);
    }
    [
        ((hash & 0xff) as f32) / 255.0,
        (((hash >> 8) & 0xff) as f32) / 255.0,
        (((hash >> 16) & 0xff) as f32) / 255.0,
        (((hash >> 24) & 0xff) as f32) / 255.0,
    ]
}

/// Read the request metadata JSON from the host.
fn read_request_info(req_handle: u64) -> Vec<u8> {
    let mut buf = vec![0u8; 4096];
    let mut len: u32 = 0;
    let mut status: u32 = 0;
    unsafe {
        host_request_read(
            req_handle,
            buf.as_mut_ptr() as u32,
            buf.len() as u32,
            &mut len,
            &mut status,
        );
    }
    if status != 0 || len == 0 || len as usize > buf.len() {
        return Vec::new();
    }
    buf.truncate(len as usize);
    buf
}

/// Read the request body from the host.
fn read_request_body(req_handle: u64) -> Vec<u8> {
    let mut buf = vec![0u8; 8192];
    let mut len: u32 = 0;
    let mut status: u32 = 0;
    unsafe {
        host_request_body(
            req_handle,
            buf.as_mut_ptr() as u32,
            buf.len() as u32,
            &mut len,
            &mut status,
        );
    }
    if status != 0 || len as usize > buf.len() {
        return Vec::new();
    }
    buf.truncate(len as usize);
    buf
}

/// Extract a JSON string field value by naive search: `"field":"value"`.
/// Returns the raw value without quotes.
fn extract_json_string<'a>(json: &'a str, field: &str) -> Option<&'a str> {
    let needle = format!("\"{}\":\"", field);
    let start = json.find(&needle)? + needle.len();
    let end = json[start..].find('"')?;
    Some(&json[start..start + end])
}

/// Extract a JSON number field value.
fn extract_json_number(json: &str, field: &str) -> Option<u32> {
    let needle = format!("\"{}\":", field);
    let start = json.find(&needle)? + needle.len();
    let rest = &json[start..];
    let end = rest
        .find(|c: char| !c.is_ascii_digit() && c != '-' && c != '.')
        .unwrap_or(rest.len());
    rest[..end].parse().ok()
}

/// Extract the `path` value from the request metadata JSON.
fn extract_path(info: &str) -> &str {
    extract_json_string(info, "path").unwrap_or("")
}

/// Write an HTTP response back to the host.
fn write_response(req_handle: u64, status: u32, headers: &str, body: &[u8]) {
    let mut out_status: u32 = 0;
    let (headers_ptr, headers_len) = string_to_ptr(headers);
    let (body_ptr, body_len) = (body.as_ptr() as u32, body.len() as u32);
    unsafe {
        host_route_response_write(
            token(),
            req_handle,
            status,
            headers_ptr,
            headers_len,
            body_ptr,
            body_len,
            &mut out_status,
        );
    }
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

fn handle_mcp_sse(req_handle: u64) {
    let body = b"event: endpoint\ndata: /mcp/messages?session_id=phase1-demo\n\n";
    let headers = r#"{"Content-Type":"text/event-stream","Cache-Control":"no-cache"}"#;
    write_response(req_handle, 200, headers, body);
}

fn handle_mcp_messages(req_handle: u64) {
    let body = read_request_body(req_handle);
    let body_str = match str::from_utf8(&body) {
        Ok(s) => s,
        Err(_) => {
            write_response(
                req_handle,
                400,
                r#"{"Content-Type":"application/json"}"#,
                b"{\"jsonrpc\":\"2.0\",\"error\":{\"code\":-32700,\"message\":\"parse error\"}}",
            );
            return;
        }
    };

    let method = extract_json_string(body_str, "method").unwrap_or("");
    let id = extract_json_number(body_str, "id");

    let response_body: String = match method {
        "initialize" => {
            r#"{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"mcp-memory-server","version":"0.1.0"}},"id":ID}"#.replace("ID", &id.map(|n| n.to_string()).unwrap_or("null".to_string()))
        }
        "tools/list" => {
            r#"{"jsonrpc":"2.0","result":{"tools":[{"name":"memory_search","description":"Search memory via vector index","inputSchema":{"type":"object","properties":{"query":{"type":"string"},"top_k":{"type":"integer","default":3}},"required":["query"]}},{"name":"memory_store","description":"Store a memory in the plugin state KV","inputSchema":{"type":"object","properties":{"key":{"type":"string"},"value":{"type":"string"}},"required":["key","value"]}},{"name":"memory_retrieve","description":"Retrieve a stored memory by key","inputSchema":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}]},"id":ID}"#.replace("ID", &id.map(|n| n.to_string()).unwrap_or("null".to_string()))
        }
        "tools/call" => {
            let name = extract_json_string(body_str, "name").unwrap_or("");
            match name {
                "memory_search" => {
                    let query = extract_json_string(body_str, "query").unwrap_or("");
                    let vbody = handle_memory_search_body(query);
                    let escaped = escape_json_string(&vbody);
                    format!(r#"{{"jsonrpc":"2.0","result":{{"content":[{{"type":"text","text":"{escaped}"}}]}},"id":{}}}"#, id.map(|n| n.to_string()).unwrap_or("null".to_string()))
                }
                "memory_store" => {
                    let key = extract_json_string(body_str, "key").unwrap_or("");
                    let value = extract_json_string(body_str, "value").unwrap_or("");
                    let state_status = state_put_str(key, value.as_bytes());
                    let vector_status = if state_status == 0 {
                        vector_index_str("memory", key, &embedding_for_text(value))
                    } else {
                        state_status
                    };
                    let msg = if state_status == 0 && vector_status == 0 {
                        format!(r#"{{"status":"stored","indexed":true,"key":"{key}"}}"#)
                    } else if state_status == 0 {
                        format!(r#"{{"status":"stored","indexed":false,"key":"{key}","vector_status":{vector_status}}}"#)
                    } else {
                        format!(r#"{{"status":"error","host_status":{state_status}}}"#)
                    };
                    let escaped = escape_json_string(&msg);
                    format!(r#"{{"jsonrpc":"2.0","result":{{"content":[{{"type":"text","text":"{escaped}"}}]}},"id":{}}}"#, id.map(|n| n.to_string()).unwrap_or("null".to_string()))
                }
                "memory_retrieve" => {
                    let key = extract_json_string(body_str, "key").unwrap_or("");
                    let (val, status) = state_get_str(key);
                    let msg = if status == 0 {
                        format!(r#"{{"status":"found","key":"{key}","value":"{}"}}"#, escape_json_string(&val))
                    } else {
                        format!(r#"{{"status":"not_found","key":"{key}","host_status":{status}}}"#)
                    };
                    let escaped = escape_json_string(&msg);
                    format!(r#"{{"jsonrpc":"2.0","result":{{"content":[{{"type":"text","text":"{escaped}"}}]}},"id":{}}}"#, id.map(|n| n.to_string()).unwrap_or("null".to_string()))
                }
                _ => {
                    r#"{"jsonrpc":"2.0","error":{"code":-32601,"message":"tool not found"},"id":ID}"#.replace("ID", &id.map(|n| n.to_string()).unwrap_or("null".to_string()))
                }
            }
        }
        _ => {
            r#"{"jsonrpc":"2.0","error":{"code":-32601,"message":"method not found"},"id":ID}"#.replace("ID", &id.map(|n| n.to_string()).unwrap_or("null".to_string()))
        }
    };

    let headers = r#"{"Content-Type":"application/json"}"#;
    write_response(req_handle, 200, headers, response_body.as_bytes());
}

/// state_put_str stores a key-value pair in the plugin state KV.
fn state_put_str(key: &str, value: &[u8]) -> u32 {
    let mut status: u32 = 0;
    let mut version: u64 = 0;
    let (key_ptr, key_len) = string_to_ptr(key);
    let (val_ptr, val_len) = vec_to_ptr(&value.to_vec());
    unsafe {
        host_state_put(
            token(),
            key_ptr,
            key_len,
            val_ptr,
            val_len,
            0, // no CAS
            &mut status,
            &mut version,
        );
    }
    status
}

/// state_get_str retrieves a value by key from the plugin state KV.
/// Returns (value_string, status_code).
fn state_get_str(key: &str) -> (String, u32) {
    let mut buf = vec![0u8; 4096];
    let mut len: u32 = 0;
    let mut status: u32 = 0;
    let mut version: u64 = 0;
    let (key_ptr, key_len) = string_to_ptr(key);
    unsafe {
        host_state_get(
            token(),
            key_ptr,
            key_len,
            buf.as_mut_ptr() as u32,
            buf.len() as u32,
            &mut status,
            &mut len,
            &mut version,
        );
    }
    if status != 0 || len as usize > buf.len() {
        return (String::new(), status);
    }
    buf.truncate(len as usize);
    (String::from_utf8_lossy(&buf).into_owned(), status)
}

fn vector_index_str(bucket: &str, key: &str, vec: &[f32; 4]) -> u32 {
    let mut status: u32 = 0;
    let (bucket_ptr, bucket_len) = string_to_ptr(bucket);
    let (key_ptr, key_len) = string_to_ptr(key);
    unsafe {
        host_vector_index(
            token(),
            bucket_ptr,
            bucket_len,
            key_ptr,
            key_len,
            vec.as_ptr() as u32,
            vec.len() as u32,
            &mut status,
        );
    }
    status
}

/// handle_memory_search_body returns the raw vector search result.
fn handle_memory_search_body(query_text: &str) -> String {
    let bucket = "memory";
    let query = embedding_for_text(query_text);
    let mut result_buf = vec![0u8; 4096];
    let mut len: u32 = 0;
    let mut status: u32 = 0;

    let (bucket_ptr, bucket_len) = string_to_ptr(bucket);
    let (query_ptr, _) = (query.as_ptr() as u32, query.len() as u32);

    unsafe {
        host_vector_search(
            token(),
            bucket_ptr,
            bucket_len,
            query_ptr,
            query.len() as u32,
            3,
            &mut status,
            result_buf.as_mut_ptr() as u32,
            result_buf.len() as u32,
            &mut len,
        );
    }

    if status == 0 {
        result_buf.truncate(len as usize);
        str::from_utf8(&result_buf).unwrap_or("[]").to_string()
    } else {
        format!(r#"{{"status":"demo","host_status":{status}}}"#)
    }
}

fn handle_memory_search(req_handle: u64) {
    let body = read_request_body(req_handle);
    let body_str = str::from_utf8(&body).unwrap_or("");
    let query_text = extract_json_string(body_str, "query").unwrap_or("");
    let bucket = "memory";
    let query = embedding_for_text(query_text);
    let mut result_buf = vec![0u8; 4096];
    let mut len: u32 = 0;
    let mut status: u32 = 0;

    let (bucket_ptr, bucket_len) = string_to_ptr(bucket);
    let (query_ptr, _) = (query.as_ptr() as u32, query.len() as u32);

    unsafe {
        host_vector_search(
            token(),
            bucket_ptr,
            bucket_len,
            query_ptr,
            query.len() as u32,
            3,
            &mut status,
            result_buf.as_mut_ptr() as u32,
            result_buf.len() as u32,
            &mut len,
        );
    }

    let response = if status == 0 {
        result_buf.truncate(len as usize);
        format!(
            "{{\"status\":\"ok\",\"results\":{}}}",
            str::from_utf8(&result_buf).unwrap_or("[]")
        )
    } else {
        format!(
            "{{\"status\":\"demo\",\"host_status\":{status},\"note\":\"vector.search not yet backed by a real index in Phase 1\"}}"
        )
    };

    let headers = r#"{"Content-Type":"application/json"}"#;
    write_response(req_handle, 200, headers, response.as_bytes());
}

fn handle_request(req_handle: u64) {
    let info = read_request_info(req_handle);
    let info_str = str::from_utf8(&info).unwrap_or("");
    let path = extract_path(info_str);

    if path.starts_with("/mcp/sse") {
        handle_mcp_sse(req_handle);
    } else if path.starts_with("/mcp/messages") {
        handle_mcp_messages(req_handle);
    } else if path.starts_with("/memory/search") {
        handle_memory_search(req_handle);
    } else {
        let body = format!(
            "{{\"error\":\"not found\",\"path\":\"{}\"}}",
            path.replace('"', "\\\"")
        );
        let headers = r#"{"Content-Type":"application/json"}"#;
        write_response(req_handle, 404, headers, body.as_bytes());
    }
}

// ---------------------------------------------------------------------------
// Plugin entry points (exported to the loader)
// ---------------------------------------------------------------------------

#[unsafe(no_mangle)]
pub extern "C" fn wasm_init(token: u64) -> u32 {
    set_token(token);
    0
}

#[unsafe(no_mangle)]
pub extern "C" fn on_hook(token: u64, hook_id: u64, ctx_handle: u64) -> u32 {
    set_token(token);
    // Phase 1: no hooks implemented; acknowledge the call.
    let _ = ctx_handle;
    let _ = hook_id;
    0
}

#[unsafe(no_mangle)]
pub extern "C" fn on_request(token: u64, route_id: u64, req_handle: u64) -> u32 {
    set_token(token);
    let _ = route_id;
    handle_request(req_handle);
    0
}

fn main() {
    // This crate is a wasm plugin; the loader calls exported entry points
    // directly. The Rust std runtime provides _start, which we leave empty.
}
