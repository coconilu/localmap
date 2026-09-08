; 安装/卸载前清场（issue #4）：
; 1. 结束所有 localmap-engine.exe —— sidecar 引擎可能从旧安装路径运行并锁住文件、
;    抢占 80/443/20190 端口；Tauri NSIS 模板默认只杀主程序进程，不管 sidecar。
; 2. 删除指向旧路径的开机自启计划任务 —— 旧任务会在开机时拉起旧引擎形成双实例；
;    安装完成后用户在管理页一键「注册开机自启」即可指向新引擎。
; 挂载：tauri.conf.json  "bundle": { "nsis": { "installerHooks": "./installer-hooks.nsh" } }

!macro KillEngineProcesses
  nsExec::ExecToLog "taskkill /F /IM localmap-engine.exe"
  Pop $0
  Sleep 1000
!macroend

!macro RemoveOldEngineTask
  nsExec::ExecToLog "schtasks /delete /tn LocalMapEngine /f"
  Pop $0
!macroend

!macro NSIS_HOOK_PREINSTALL
  !insertmacro KillEngineProcesses
  !insertmacro RemoveOldEngineTask
!macroend

!macro NSIS_HOOK_PREUNINSTALL
  !insertmacro KillEngineProcesses
  !insertmacro RemoveOldEngineTask
!macroend
