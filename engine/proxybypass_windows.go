package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 系统代理绕过列表（ProxyOverride）自动同步。
//
// 背景：开系统代理（Clash 等）时，浏览器把映射域名交给代理服务器解析，
// 代理解析不了本地域名直接断连（ERR_CONNECTION_CLOSED）。Windows 的绕过列表
// 命中即直连，LocalMap 按「映射什么写什么」维护它：
//   - 增删映射时同步（API 调用点触发）
//   - 引擎每次启动时自愈同步（代理软件开关系统代理会整体重写列表，把条目抹掉）
//   - bypassJanitor 每分钟周期校对：开机自启（SYSTEM 计划任务）先于用户登录，
//     启动那次同步因没有桌面会话注定失败，靠周期重试补回
//
// 权属：只删除自己写入的条目（bypass.json 里记录 owned），用户手动加的同名条目不动。
// 并发：所有 ProxyOverride 读-改-写与 bypass.json 落盘都经 Engine.bypassMu 串行化
// （多个 HTTP handler 与 janitor 可能同时触发）。

const (
	psInternetSettingsHKCU = `Registry::HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	regInternetSettingsHKU = `HKEY_USERS\` // + <SID>\Software\...
)

// bypassSettings 持久化到 dataDir/bypass.json
type bypassSettings struct {
	SyncOn bool     `json:"syncOn"` // 自愈同步开关，默认开
	Owned  []string `json:"owned"`  // LocalMap 写入 ProxyOverride 的条目（权属记录）
}

func bypassSettingsPath(dataDir string) string {
	return filepath.Join(dataDir, "bypass.json")
}

// loadBypassSettings 读取设置；文件不存在时返回默认（开关打开）
func loadBypassSettings(dataDir string) *bypassSettings {
	st := &bypassSettings{SyncOn: true}
	raw, err := os.ReadFile(bypassSettingsPath(dataDir))
	if err == nil {
		_ = json.Unmarshal(raw, st) // 字段缺失时保留默认值
	}
	return st
}

func saveBypassSettings(dataDir string, st *bypassSettings) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("[bypass] 序列化设置失败: %v", err)
		return
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Printf("[bypass] 创建数据目录失败: %v", err)
		return
	}
	if err := os.WriteFile(bypassSettingsPath(dataDir), data, 0644); err != nil {
		log.Printf("[bypass] 写入 bypass.json 失败: %v", err)
	}
}

// desiredBypassEntries 每个映射域名对应两条：精确域名 + 通配子域
func desiredBypassEntries(domains []string) []string {
	out := make([]string, 0, len(domains)*2)
	for _, d := range domains {
		out = append(out, d, "*."+d)
	}
	return out
}

// interactiveUserSID 解析当前登录（交互式）用户的 SID。
// 引擎以 SYSTEM 身份运行时 HKCU 指向 SYSTEM 自己的 hive，必须改写登录用户的
// HKEY_USERS\<SID>。取 explorer.exe 属主最可靠（有桌面会话才有 explorer）。
// 注意：多会话（RDP + 本地）时取第一个 explorer 属主，可能不是目标用户。
func interactiveUserSID() (string, error) {
	out, err := runHidden("powershell", "-NoProfile", "-Command",
		"$u=(Get-Process -Name explorer -IncludeUserName -ErrorAction SilentlyContinue | Select-Object -First 1).UserName;"+
			" if ($u) { (New-Object System.Security.Principal.NTAccount($u)).Translate([System.Security.Principal.SecurityIdentifier]).Value }")
	sid := strings.TrimSpace(out)
	if err != nil || sid == "" {
		return "", fmt.Errorf("找不到登录用户（无桌面会话？）: %v %s", err, strings.TrimSpace(out))
	}
	return sid, nil
}

// proxyOverridePaths 返回 Internet Settings 键的两种路径形式（PowerShell / reg.exe）
func proxyOverridePaths() (psPath, regPath string, err error) {
	if runningAsSystem() {
		sid, err2 := interactiveUserSID()
		if err2 != nil {
			return "", "", err2
		}
		sub := sid + `\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
		return `Registry::HKEY_USERS\` + sub, regInternetSettingsHKU + sub, nil
	}
	return psInternetSettingsHKCU,
		`HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, nil
}

// splitBypassEntries 拆分分号分隔的绕过列表，去空去重（保序，大小写不敏感）
func splitBypassEntries(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range strings.Split(s, ";") {
		e = strings.TrimSpace(e)
		if e == "" || seen[strings.ToLower(e)] {
			continue
		}
		seen[strings.ToLower(e)] = true
		out = append(out, e)
	}
	return out
}

func readProxyOverride(psPath string) ([]string, error) {
	out, err := runHidden("powershell", "-NoProfile", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8;"+
			" (Get-ItemProperty -Path '"+psPath+"' -Name ProxyOverride -ErrorAction SilentlyContinue).ProxyOverride")
	if err != nil {
		return nil, &CmdError{Cmd: "Get-ItemProperty ProxyOverride", Out: out, Err: err}
	}
	return splitBypassEntries(out), nil
}

func writeProxyOverride(regPath string, entries []string) error {
	v := strings.Join(entries, ";")
	out, err := runHidden("reg", "add", regPath, "/v", "ProxyOverride", "/t", "REG_SZ", "/d", v, "/f")
	if err != nil {
		return &CmdError{Cmd: "reg add ProxyOverride", Out: out, Err: err}
	}
	// 通知系统代理设置已变更（让 WinINET/浏览器立刻重读），失败不致命。
	// 限制：引擎以 SYSTEM（session 0）运行时，该广播只刷新 SYSTEM 自己的
	// WinINET 状态，用户会话里的浏览器可能要切换网络/重启浏览器后才重读。
	_, _ = runHidden("rundll32", "user32.dll,UpdatePerUserSystemParameters")
	return nil
}

// syncProxyBypass 自愈同步（加锁入口）：把缺失的映射域名补进 ProxyOverride，
// 移除不再映射的 owned 条目。开关关闭时直接返回 nil。
func (e *Engine) syncProxyBypass() error {
	e.bypassMu.Lock()
	defer e.bypassMu.Unlock()
	return e.syncProxyBypassLocked()
}

// logSyncProxyBypass 失败只记日志：映射增删改的主流程不受 bypass 失败影响
func (e *Engine) logSyncProxyBypass() {
	if err := e.syncProxyBypass(); err != nil {
		log.Printf("[bypass] 同步失败: %v", err)
	}
}

// bypassJanitor 周期自愈校对。开机自启（SYSTEM 计划任务）时可能还没有登录会话，
// 启动那次同步注定失败；代理软件之后重写 ProxyOverride 也需要有人补回。
func (e *Engine) bypassJanitor(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		e.logSyncProxyBypass()
	}
}

// syncProxyBypassLocked 是 syncProxyBypass 的锁内实现，调用方必须已持有 bypassMu
func (e *Engine) syncProxyBypassLocked() error {
	st := loadBypassSettings(e.dataDir)
	if !st.SyncOn {
		return nil
	}
	psPath, regPath, err := proxyOverridePaths()
	if err != nil {
		return fmt.Errorf("定位用户注册表失败: %w", err)
	}
	entries, err := readProxyOverride(psPath)
	if err != nil {
		return fmt.Errorf("读取 ProxyOverride 失败: %w", err)
	}

	desired := map[string]bool{}
	for _, x := range desiredBypassEntries(e.domains()) {
		desired[strings.ToLower(x)] = true
	}
	have := map[string]bool{}
	for _, x := range entries {
		have[strings.ToLower(x)] = true
	}
	owned := map[string]bool{}
	for _, x := range st.Owned {
		owned[x] = true
	}

	changed := false
	// 补齐缺失条目（已存在但非 LocalMap 写入的：不动、不记权属）
	for _, x := range desiredBypassEntries(e.domains()) {
		lx := strings.ToLower(x)
		if !have[lx] {
			entries = append(entries, x)
			have[lx] = true
			owned[x] = true
			changed = true
		}
	}
	// 移除 owned 中已不再是映射的条目
	kept := entries[:0]
	for _, x := range entries {
		lx := strings.ToLower(x)
		if owned[x] && !desired[lx] {
			delete(owned, x)
			changed = true
			continue
		}
		kept = append(kept, x)
	}
	entries = kept

	if changed {
		if err := writeProxyOverride(regPath, entries); err != nil {
			return fmt.Errorf("写入 ProxyOverride 失败: %w", err)
		}
		log.Printf("[bypass] ProxyOverride 已同步（受管 %d 条）", len(owned))
	}
	st.Owned = sortedKeys(owned)
	saveBypassSettings(e.dataDir, st)
	return nil
}

// cleanupProxyBypass 移除所有 LocalMap 写入的条目（关闭开关且用户选择清理时调用）
func (e *Engine) cleanupProxyBypass() error {
	e.bypassMu.Lock()
	defer e.bypassMu.Unlock()
	return e.cleanupProxyBypassLocked()
}

// cleanupProxyBypassLocked 是 cleanupProxyBypass 的锁内实现，调用方必须已持有 bypassMu
func (e *Engine) cleanupProxyBypassLocked() error {
	st := loadBypassSettings(e.dataDir)
	if len(st.Owned) == 0 {
		return nil
	}
	psPath, regPath, err := proxyOverridePaths()
	if err != nil {
		return err
	}
	entries, err := readProxyOverride(psPath)
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for _, x := range st.Owned {
		owned[x] = true
		owned[strings.ToLower(x)] = true
	}
	kept := entries[:0]
	removed := 0
	for _, x := range entries {
		if owned[x] || owned[strings.ToLower(x)] {
			removed++
			continue
		}
		kept = append(kept, x)
	}
	if removed == 0 {
		st.Owned = nil
		saveBypassSettings(e.dataDir, st)
		return nil
	}
	if err := writeProxyOverride(regPath, kept); err != nil {
		return err
	}
	log.Printf("[bypass] 已清理 %d 条 ProxyOverride 条目", removed)
	st.Owned = nil
	saveBypassSettings(e.dataDir, st)
	return nil
}

// setProxyBypassSync 开关切换：开=立即同步；关=cleanup 时清理已写入条目。
// 同步/清理失败会返回错误，由 API 层透传给 GUI 如实提示。
func (e *Engine) setProxyBypassSync(enabled, cleanup bool) error {
	e.bypassMu.Lock()
	defer e.bypassMu.Unlock()
	st := loadBypassSettings(e.dataDir)
	st.SyncOn = enabled
	saveBypassSettings(e.dataDir, st)
	if enabled {
		return e.syncProxyBypassLocked()
	}
	if cleanup {
		return e.cleanupProxyBypassLocked()
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
