package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	HostsOK      bool           `json:"hostsOk"`
	HostsErr     string         `json:"hostsErr"`
	TaskOn       bool           `json:"taskInstalled"`
	TaskState    string         `json:"taskStatus"`
	IsSystem     bool           `json:"isSystem"`
	HttpsEnabled bool           `json:"httpsEnabled"`
	CATrusted    bool           `json:"caTrusted"`
	Mappings     []mappingState `json:"mappings"`
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
			HostsOK:      hostsOK,
			HostsErr:     hostsErr,
			TaskOn:       taskOn,
			TaskState:    taskState,
			IsSystem:     runningAsSystem(),
			HttpsEnabled: e.httpsOn.Load(),
			CATrusted:    e.certs != nil && CATrusted(),
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

	return hostGuard(mux)
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
