; 安装/卸载前清场（issue #4）：
; 1. 结束所有 localmap-engine.exe —— sidecar 引擎可能从旧安装路径运行并锁住文件、
;    抢占 80/443/20190 端口；Tauri NSIS 模板默认只杀主程序进程，不管 sidecar。
; 2. 开机自启计划任务按指向路径处理，同路径升级不丢失自启：
;    - 安装：仅当任务指向其它（旧）安装路径时删除（删后管理页会提示重新注册）
;    - 卸载：仅当任务指向本安装路径时删除
;
; 挂载：tauri.conf.json  "bundle": { "windows": { "nsis": { "installerHooks": "./installer-hooks.nsh" } } }
;
; 已知限制：默认 currentUser 安装模式不提权，杀不掉/删不掉以 SYSTEM 运行的
; 引擎和任务（Access denied 被静默吞掉）。兜底是引擎自身的端口绑定失败即退出
; + 管理页"开机自启"面板的指向旧安装红色告警与一键修复。
;
; 注意 NSIS 转义：外层双引号字符串里 $$ = 字面 $、$\" = 字面双引号；
; PowerShell 侧用单引号字符串，$INSTDIR 里的反斜杠是字面量，比较用 -ine（不区分大小写）。

!macro KillEngineProcesses
  nsExec::ExecToLog "taskkill /F /IM localmap-engine.exe"
  Pop $0
  Sleep 1000
!macroend

!macro NSIS_HOOK_PREINSTALL
  !insertmacro KillEngineProcesses
  nsExec::ExecToLog "powershell -NoProfile -ExecutionPolicy Bypass -Command $\"$$a=(Get-ScheduledTask -TaskName 'LocalMapEngine' -ErrorAction SilentlyContinue).Actions | Select-Object -First 1; if ($$a -and $$a.Execute.Trim('$\"') -ine '$INSTDIR\localmap-engine.exe') { schtasks /delete /tn 'LocalMapEngine' /f }$\""
  Pop $0
!macroend

!macro NSIS_HOOK_PREUNINSTALL
  !insertmacro KillEngineProcesses
  nsExec::ExecToLog "powershell -NoProfile -ExecutionPolicy Bypass -Command $\"$$a=(Get-ScheduledTask -TaskName 'LocalMapEngine' -ErrorAction SilentlyContinue).Actions | Select-Object -First 1; if ($$a -and $$a.Execute.Trim('$\"') -ieq '$INSTDIR\localmap-engine.exe') { schtasks /delete /tn 'LocalMapEngine' /f }$\""
  Pop $0
!macroend
