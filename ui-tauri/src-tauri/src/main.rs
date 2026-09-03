// LocalMap 桌面壳：加载引擎的管理界面；引擎未运行时尝试拉起同目录的引擎。
// 自动更新走 tauri-plugin-updater（GitHub Releases latest.json），
// 前端经 withGlobalTauri 暴露的 window.__TAURI__.updater 调用插件官方 JS API
//（管理页是远端 URL，自定义 command 会被 ACL 拦截，故不在 Rust 侧包命令）。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::process::Command;
use std::time::Duration;

#[cfg(windows)]
use std::os::windows::process::CommandExt;

const ENGINE_API: &str = "127.0.0.1:20190";
const CREATE_NO_WINDOW: u32 = 0x08000000;

fn engine_running() -> bool {
    TcpStream::connect_timeout(&ENGINE_API.parse().unwrap(), Duration::from_millis(300)).is_ok()
}

/// 引擎未运行时，尝试启动与 app 同目录的 localmap-engine.exe（无窗口）
fn try_spawn_engine() {
    let Ok(exe) = std::env::current_exe() else { return };
    let engine = exe.with_file_name("localmap-engine.exe");
    if !engine.exists() {
        return;
    }
    let data = exe
        .parent()
        .map(|p| p.join("data"))
        .unwrap_or_else(|| "data".into());
    let mut cmd = Command::new(engine);
    cmd.arg("-data").arg(data);
    #[cfg(windows)]
    cmd.creation_flags(CREATE_NO_WINDOW);
    if cmd.spawn().is_ok() {
        // 等引擎就绪（最多 ~3s）
        for _ in 0..10 {
            if engine_running() {
                break;
            }
            std::thread::sleep(Duration::from_millis(300));
        }
    }
}

fn main() {
    if !engine_running() {
        try_spawn_engine();
    }
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
