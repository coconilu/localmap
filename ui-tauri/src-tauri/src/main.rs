// LocalMap 桌面壳：加载引擎的管理界面；引擎未运行时尝试拉起同目录的引擎。
// 自动更新走 tauri-plugin-updater（GitHub Releases latest.json），
// 通过自定义 command 暴露给远端管理页面（admin.html 由引擎内嵌服务，非打包前端）。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::process::Command;
use std::time::Duration;
use tauri_plugin_updater::UpdaterExt;

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

/// 检查更新：有新版本返回版本号，无则 None。失败由前端静默处理。
#[tauri::command]
async fn check_app_update(app: tauri::AppHandle) -> Result<Option<String>, String> {
    let updater = app.updater().map_err(|e| e.to_string())?;
    match updater.check().await {
        Ok(Some(u)) => Ok(Some(u.version)),
        Ok(None) => Ok(None),
        Err(e) => Err(e.to_string()),
    }
}

/// 下载并安装最新版本：NSIS 安装器接管安装并自动重启，无需手动 relaunch。
#[tauri::command]
async fn install_app_update(app: tauri::AppHandle) -> Result<(), String> {
    let updater = app.updater().map_err(|e| e.to_string())?;
    let update = updater.check().await.map_err(|e| e.to_string())?;
    if let Some(u) = update {
        u.download_and_install(|_, _| {}, || {})
            .await
            .map_err(|e| e.to_string())?;
    }
    Ok(())
}

fn main() {
    if !engine_running() {
        try_spawn_engine();
    }
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .invoke_handler(tauri::generate_handler![check_app_update, install_app_update])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
