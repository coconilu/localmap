package main

import (
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed admin.html
var adminHTML string

type Engine struct {
	store     *Store
	dataDir   string
	proxyAddr string
	apiAddr   string
	token     string
	exePath   string
	certs     *CertStore
	httpsOn   atomic.Bool
	tlsSrv    *http.Server
	// bypassMu 串行化 ProxyOverride 读-改-写与 bypass.json 落盘
	//（HTTP handler 与 bypassJanitor 可能并发触发）
	bypassMu sync.Mutex
}

type mappingState struct {
	Domain   string `json:"domain"`
	Target   string `json:"target"`
	TargetUp bool   `json:"targetUp"`
}

type stateResponse struct {
	Version      string         `json:"version"`
	ProxyAddr    string         `json:"proxyAddr"`
	APIAddr      string         `json:"apiAddr"`
	DataDir      string         `json:"dataDir"`
	HostsOK      bool           `json:"hostsOk"`
	HostsErr     string         `json:"hostsErr"`
	TaskOn       bool           `json:"taskInstalled"`
	TaskState    string         `json:"taskStatus"`
	TaskExe      string         `json:"taskExe"`
	TaskMatch    bool           `json:"taskMatch"`
	IsSystem     bool           `json:"isSystem"`
	HttpsEnabled bool           `json:"httpsEnabled"`
	CATrusted    bool           `json:"caTrusted"`
	BypassSync   bool           `json:"proxyBypassSync"`
	Mappings     []mappingState `json:"mappings"`
}

// checkItem 自检单项结果（GET /api/selfcheck）
type checkItem struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func loadOrCreateToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "token")
	if raw, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(raw)); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(t), 0644); err != nil {
		return "", err
	}
	return t, nil
}

func (e *Engine) domains() []string {
	var out []string
	for _, m := range e.store.List() {
		out = append(out, m.Domain)
	}
	return out
}

func (e *Engine) syncHosts() {
	if err := SyncHosts(e.domains()); err != nil {
		log.Printf("[hosts] 同步失败（权限不足？）: %v", err)
	}
}

// auth 校验变更类请求的 token
func (e *Engine) auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-LocalMap-Token") != e.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// allowedAPIHosts 控制 API 只响应这些 Host（防 DNS-rebinding：
// 否则恶意网站把域名解析到 127.0.0.1 即可读到内嵌 token 的管理页并调用变更 API）
var allowedAPIHosts = map[string]bool{
	"127.0.0.1":     true,
	"localhost":     true,
	"localmap.test": true,
}

// checkHost 校验请求 Host；返回 false 表示已拒绝
func checkHost(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !allowedAPIHosts[normalizeDomain(host)] {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return false
	}
	return true
}

func (e *Engine) ServeMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page := strings.Replace(adminHTML, "__LOCALMAP_TOKEN__", e.token, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		hostsOK, hostsErr := HostsInSync(e.domains())
		taskOn, taskState := TaskStatus()
		taskExe, _ := TaskAction()
		taskMatch := !taskOn || taskExe == "" || sameExePath(taskExe, e.exePath)
		var ms []mappingState
		for _, m := range e.store.List() {
			ms = append(ms, mappingState{
				Domain:   m.Domain,
				Target:   m.Target,
				TargetUp: probeTarget(m.Target),
			})
		}
		writeJSON(w, stateResponse{
			Version:      version,
			ProxyAddr:    e.proxyAddr,
			APIAddr:      e.apiAddr,
			DataDir:      e.dataDir,
			HostsOK:      hostsOK,
			HostsErr:     hostsErr,
			TaskOn:       taskOn,
			TaskState:    taskState,
			TaskExe:      taskExe,
			TaskMatch:    taskMatch,
			IsSystem:     runningAsSystem(),
			HttpsEnabled: e.httpsOn.Load(),
			CATrusted:    e.certs != nil && CATrusted(),
			BypassSync:   loadBypassSettings(e.dataDir).SyncOn,
			Mappings:     ms,
		})
	})

	mux.HandleFunc("POST /api/mappings", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		var m Mapping
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
			return
		}
		isNew, err := e.store.Upsert(m)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.syncHosts()
		e.logSyncProxyBypass()
		writeJSON(w, map[string]any{"ok": true, "created": isNew})
	})

	mux.HandleFunc("DELETE /api/mappings/{domain}", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		if !e.store.Remove(r.PathValue("domain")) {
			http.Error(w, "映射不存在", http.StatusNotFound)
			return
		}
		e.syncHosts()
		e.logSyncProxyBypass()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("PUT /api/mappings/{domain}", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		var m Mapping
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
			return
		}
		if err := e.store.Replace(r.PathValue("domain"), m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.syncHosts()
		e.logSyncProxyBypass()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/task/install", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		if err := TaskInstall(e.exePath, e.dataDir); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/task/uninstall", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		if err := TaskUninstall(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// 启用本地 HTTPS：创建/加载 CA → 装入系统信任存储 → 起 443 TLS 监听
	mux.HandleFunc("POST /api/https/enable", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		if e.httpsOn.Load() {
			// 数据目录迁移 / 重装后 CA 可能已更换但未装入信任存储
			//（浏览器报 ERR_CERT_AUTHORITY_INVALID），此处幂等补装
			if e.certs != nil && !CATrusted() {
				if err := e.certs.InstallCA(); err != nil {
					http.Error(w, "安装 CA 到信任存储失败（需要管理员权限）: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			writeJSON(w, map[string]any{"ok": true, "already": true, "caTrusted": CATrusted()})
			return
		}
		cs, err := LoadOrCreateCA(e.dataDir)
		if err != nil {
			http.Error(w, "创建本地 CA 失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !CATrusted() {
			if err := cs.InstallCA(); err != nil {
				http.Error(w, "安装 CA 到信任存储失败（需要管理员权限）: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		e.certs = cs
		if err := e.startHTTPS(); err != nil {
			http.Error(w, "启动 443 监听失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "caTrusted": CATrusted()})
	})

	// 关闭本地 HTTPS：停 443 监听 → 从信任存储删 CA → 删除证书文件
	mux.HandleFunc("POST /api/https/disable", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		if !e.httpsOn.Load() {
			writeJSON(w, map[string]any{"ok": true, "already": true})
			return
		}
		if e.tlsSrv != nil {
			_ = e.tlsSrv.Close()
			e.tlsSrv = nil
		}
		if e.certs != nil {
			if err := e.certs.UninstallCA(); err != nil {
				http.Error(w, "从信任存储删除 CA 失败: "+err.Error(), http.StatusInternalServerError)
				return
			}
			e.certs = nil
		}
		e.httpsOn.Store(false)
		writeJSON(w, map[string]any{"ok": true})
	})

	// 系统代理绕过自动同步开关：开=立即同步一次；关=cleanup 时清理已写入条目
	mux.HandleFunc("POST /api/proxy-bypass", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
			Cleanup bool `json:"cleanup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
			return
		}
		if err := e.setProxyBypassSync(req.Enabled, req.Cleanup); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// 端到端自检：模拟浏览器访问链路逐项检查，定位"目标在线但打不开"类故障
	mux.HandleFunc("GET /api/selfcheck", func(w http.ResponseWriter, r *http.Request) {
		if !e.auth(w, r) {
			return
		}
		writeJSON(w, e.selfCheck())
	})

	return hostGuard(mux)
}

// selfCheck 逐项检查：端口归属 → HTTPS 监听与证书签发者 → CA 信任 →
// hosts 同步 → 代理绕过覆盖 → 开机自启指向。返回顺序即浏览器链路顺序。
func (e *Engine) selfCheck() []checkItem {
	var items []checkItem

	// 1. 代理端口由本实例监听（而非被其它进程/另一实例抢占）：
	// 未映射域名的 404 响应带 "LocalMap:" 前缀，是本引擎的指纹
	ok, detail := probeProxyOwnership(e.proxyAddr)
	item := checkItem{Name: "代理端口归属", OK: ok, Detail: detail}
	if !ok {
		item.Fix = "另一个程序占用了 " + e.proxyAddr + "；结束占用进程或退出多余的 LocalMap 实例"
	}
	items = append(items, item)

	// 2/3. HTTPS 监听与 CA 信任（仅启用 HTTPS 时检查）
	if e.httpsOn.Load() {
		ok, detail = probeTLSIdentity(e.domains())
		item = checkItem{Name: "HTTPS 监听与证书", OK: ok, Detail: detail}
		if !ok {
			item.Fix = "443 端口可能被其它程序占用；或在「本地 HTTPS」面板关闭后重新启用"
		}
		items = append(items, item)

		trusted := CATrusted()
		item = checkItem{Name: "CA 已被系统信任", OK: trusted,
			Detail: map[bool]string{true: "信任存储中存在 LocalMap Local CA", false: "浏览器会报 ERR_CERT_AUTHORITY_INVALID"}[trusted]}
		if !trusted {
			item.Fix = "在「本地 HTTPS」面板点击「修复信任」"
		}
		items = append(items, item)
	}

	// 4. hosts 同步
	hostsOK, hostsErr := HostsInSync(e.domains())
	item = checkItem{Name: "hosts 托管区块", OK: hostsOK,
		Detail: map[bool]string{true: "与当前映射一致", false: hostsErr}[hostsOK]}
	if !hostsOK {
		item.Fix = "增删任意一条映射会触发重写；仍失败则检查引擎是否有管理员权限"
	}
	items = append(items, item)

	// 5. 系统代理绕过列表覆盖全部映射（系统代理未开启时无需绕过）
	if proxyEnabled, _ := readProxyEnabled(); !proxyEnabled {
		items = append(items, checkItem{Name: "系统代理绕过", OK: true, Detail: "系统代理未开启，无需绕过"})
	} else {
		missing := e.missingBypassEntries()
		ok = len(missing) == 0
		item = checkItem{Name: "系统代理绕过", OK: ok,
			Detail: map[bool]string{true: "绕过列表已覆盖全部映射域名", false: "缺少: " + strings.Join(missing, ", ")}[ok]}
		if !ok {
			item.Fix = "已触发自愈同步，约 1 分钟内补回；浏览器需重启或切换网络后才会重读"
			e.logSyncProxyBypass()
		}
		items = append(items, item)
	}

	// 6. 开机自启指向当前安装
	taskOn, _ := TaskStatus()
	taskExe, _ := TaskAction()
	switch {
	case !taskOn:
		items = append(items, checkItem{Name: "开机自启", OK: true, Detail: "未注册（不影响当前使用）"})
	case taskExe == "" || sameExePath(taskExe, e.exePath):
		items = append(items, checkItem{Name: "开机自启", OK: true, Detail: "指向当前引擎 " + e.exePath})
	default:
		items = append(items, checkItem{Name: "开机自启", OK: false,
			Detail: "计划任务指向旧安装 " + taskExe + "，开机后两个实例会抢占端口",
			Fix:    "在「开机自启」面板点击「注册开机自启」覆盖为当前引擎"})
	}
	return items
}

// probeProxyOwnership 以未映射域名请求代理端口，响应含 "LocalMap:" 前缀即本引擎
func probeProxyOwnership(proxyAddr string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("GET", "http://"+proxyAddr+"/", nil)
	if err != nil {
		return false, err.Error()
	}
	req.Host = "selfcheck.invalid"
	resp, err := client.Do(req)
	if err != nil {
		return false, "无法连接 " + proxyAddr + "（" + err.Error() + "）"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if strings.Contains(string(body), "LocalMap:") {
		return true, proxyAddr + " 由本引擎监听"
	}
	return false, proxyAddr + " 的响应不含 LocalMap 标识，可能被其它程序占用"
}

// probeTLSIdentity 与 443 握手，确认对端证书由本引擎的 CA 签发
func probeTLSIdentity(domains []string) (bool, string) {
	serverName := "selfcheck.invalid"
	if len(domains) > 0 {
		serverName = domains[0]
	}
	conn, err := tls.Dial("tcp", "127.0.0.1:443", &tls.Config{
		InsecureSkipVerify: true, // 只验签发者身份，CA 信任由单独一项检查
		ServerName:         serverName,
	})
	if err != nil {
		return false, "443 握手失败（" + err.Error() + "）"
	}
	defer conn.Close()
	peers := conn.ConnectionState().PeerCertificates
	if len(peers) > 0 && peers[0].Issuer.CommonName == caCommonName {
		return true, "证书由 LocalMap Local CA 签发"
	}
	return false, "443 上的证书不是 LocalMap 签发的，端口可能被其它程序占用"
}

// hostGuard 统一校验 Host 头，拦截 DNS-rebinding 请求
func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkHost(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// runningAsSystem 粗略判断当前进程是否以 SYSTEM 身份运行
func runningAsSystem() bool {
	out, err := runHidden("whoami")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToUpper(out), "SYSTEM")
}
