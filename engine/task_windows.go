package main

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
)

const taskName = "LocalMapEngine"

// runHidden 执行外部命令且不弹出控制台窗口（引擎以 SYSTEM 运行时尤为重要）
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// TaskInstall 注册开机自启计划任务（需要管理员/SYSTEM 权限）
func TaskInstall(exePath, dataDir string) error {
	tr := `"` + exePath + `" -data "` + dataDir + `"`
	out, err := runHidden("schtasks", "/create", "/tn", taskName, "/sc", "onstart",
		"/ru", "SYSTEM", "/rl", "HIGHEST", "/f", "/tr", tr)
	if err != nil {
		return &CmdError{Cmd: "schtasks /create", Out: out, Err: err}
	}
	return nil
}

// TaskUninstall 删除计划任务
func TaskUninstall() error {
	out, err := runHidden("schtasks", "/delete", "/tn", taskName, "/f")
	if err != nil {
		return &CmdError{Cmd: "schtasks /delete", Out: out, Err: err}
	}
	return nil
}

// TaskRunNow 立即启动计划任务
func TaskRunNow() error {
	out, err := runHidden("schtasks", "/run", "/tn", taskName)
	if err != nil {
		return &CmdError{Cmd: "schtasks /run", Out: out, Err: err}
	}
	return nil
}

// TaskStatus 返回 (已安装, 状态描述)。用 PowerShell 取 TaskState 枚举，
// 避免 schtasks 输出随系统语言变化（GBK 中文状态）导致乱码。
func TaskStatus() (bool, string) {
	out, err := runHidden("powershell", "-NoProfile", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; (Get-ScheduledTask -TaskName '"+taskName+"' -ErrorAction SilentlyContinue).State")
	if err != nil {
		return false, ""
	}
	status := strings.TrimSpace(out)
	if status == "" {
		return false, ""
	}
	return true, status
}

func lastField(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

type CmdError struct {
	Cmd string
	Out string
	Err error
}

func (e *CmdError) Error() string {
	return e.Cmd + " 失败: " + e.Err.Error() + " | " + strings.TrimSpace(e.Out)
}
