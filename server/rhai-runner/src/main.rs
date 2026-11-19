//! rhai-runner：在隔离环境里执行 Rhai 脚本的独立进程。
//!
//! 为什么单独一个进程：Rhai 是 Rust 库，Go 没有原生绑定；而且把脚本执行放在
//! 独立进程里，超时可以直接 kill，崩溃也不会拖垮 chat-server。
//!
//! 隔离手段（缺一不可）：
//!   1. 标准 Rhai 引擎**不注册任何 IO**：没有文件、网络、进程、环境变量 API；
//!   2. 限制操作数（max_operations）与内存类上限（字符串/数组/映射长度），
//!      死循环会被引擎在有限步内打断；
//!   3. 超时由调用方（Go）负责 kill 本进程，脚本里没有任何逃逸手段；
//!   4. 输出统一走 JSON，脚本自己的 print 被重定向到缓冲区，不会污染协议。
//!
//! 输入（stdin，JSON）：{ "script": string, "max_operations": number,
//!                       "max_string_size": number, "max_array_size": number,
//!                       "max_depth": number }
//! 输出（stdout，JSON）：{ "ok": bool, "output": string, "prints": string,
//!                       "error": string, "outcome": string }

use std::io::Read;
use std::time::Instant;

use rhai::{Dynamic, Engine, Scope};
use serde_json::json;

fn main() {
    let mut input = String::new();
    if std::io::stdin().read_to_string(&mut input).is_err() {
        emit(&json!({"ok": false, "error": "读取输入失败"}));
        return;
    }
    let req: serde_json::Value = match serde_json::from_str(&input) {
        Ok(v) => v,
        Err(e) => {
            emit(&json!({"ok": false, "error": format!("请求不是合法 JSON: {e}")}));
            return;
        }
    };

    let script = req.get("script").and_then(|v| v.as_str()).unwrap_or("");
    if script.trim().is_empty() {
        emit(&json!({"ok": false, "error": "脚本为空"}));
        return;
    }

    let max_operations = req
        .get("max_operations")
        .and_then(|v| v.as_u64())
        .unwrap_or(200_000) as u64;
    let max_string_size = req
        .get("max_string_size")
        .and_then(|v| v.as_u64())
        .unwrap_or(64_000) as usize;
    let max_array_size = req
        .get("max_array_size")
        .and_then(|v| v.as_u64())
        .unwrap_or(10_000) as usize;
    let max_depth = req.get("max_depth").and_then(|v| v.as_u64()).unwrap_or(24) as u64;

    let mut engine = Engine::new();
    engine.set_max_operations(max_operations);
    engine.set_max_string_size(max_string_size);
    engine.set_max_array_size(max_array_size);
    engine.set_max_map_size(max_array_size);
    // 表达式/函数嵌套深度：挡住递归爆栈的一类脚本
    engine.set_max_expr_depths(max_depth as usize, max_depth as usize);

    // print / debug 重定向到缓冲区：脚本的输出不能直接写 stdout，否则会破坏 JSON 协议
    let printed = std::sync::Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
    let p1 = printed.clone();
    engine.on_print(move |s| {
        if let Ok(mut v) = p1.lock() {
            if v.len() < 200 {
                v.push(s.to_string());
            }
        }
    });
    let p2 = printed.clone();
    engine.on_debug(move |s, src, pos| {
        if let Ok(mut v) = p2.lock() {
            if v.len() < 200 {
                let where_ = src.as_ref().map(|x| x.to_string()).unwrap_or_default();
                v.push(format!("[debug {where_}:{pos}] {s}"));
            }
        }
    });

    let started = Instant::now();
    let mut scope = Scope::new();

    // 先编译再执行：语法错误单独报，便于模型改脚本
    let ast = match engine.compile(script) {
        Ok(ast) => ast,
        Err(e) => {
            emit(&json!({
                "ok": false,
                "error": format!("语法错误: {e}"),
                "elapsed_ms": started.elapsed().as_millis(),
            }));
            return;
        }
    };

    let result: Result<Dynamic, _> = engine.eval_ast_with_scope(&mut scope, &ast);
    let prints = printed
        .lock()
        .map(|v| v.join("\n"))
        .unwrap_or_default();
    let elapsed = started.elapsed().as_millis();

    match result {
        Ok(value) => {
            let output = render_value(&value);
            emit(&json!({
                "ok": true,
                "output": output,
                "prints": prints,
                "elapsed_ms": elapsed,
            }));
        }
        Err(e) => emit(&json!({
            "ok": false,
            "error": format!("{e}"),
            "prints": prints,
            "elapsed_ms": elapsed,
        })),
    }
}

/// 把脚本返回值转成可读文本。() 视为无返回值。
fn render_value(value: &Dynamic) -> String {
    if value.is_unit() {
        return String::new();
    }
    if value.is::<String>() {
        return value.clone().cast::<String>();
    }
    format!("{value:?}")
}

fn emit(value: &serde_json::Value) {
    // 输出也带上限：超大返回值截断，避免把协议撑爆
    let mut text = value.to_string();
    if text.len() > 200_000 {
        text.truncate(200_000);
        text.push_str("...(截断)");
    }
    println!("{text}");
}
