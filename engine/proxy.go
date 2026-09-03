package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// NewProxyHandler 按 Host 头把请求路由到映射目标。
// Host 头会被改写为目标地址，以通过本地服务常见的 DNS-rebinding 校验；
// WebSocket / SSE 由 httputil.ReverseProxy 原生支持。
// HTTPS 启用后，80 端口上的已映射域名一律 308 跳转到 https。
func NewProxyHandler(e *Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		domain := normalizeDomain(host)

		m, ok := e.store.Find(domain)
		if ok && e.httpsOn.Load() && r.TLS == nil {
			http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		if !ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "LocalMap: 没有域名 %q 的映射\n", domain)
			return
		}

		target := &url.URL{Scheme: "http", Host: m.Target}
		rp := httputil.NewSingleHostReverseProxy(target)
		base := rp.Director
		rp.Director = func(req *http.Request) {
			base(req)
			req.Host = m.Target
			// 本地服务常校验 WebSocket/REST 的 Origin 头（防 DNS rebinding）。
			// 仅当 Origin 确实是当前映射域名本身时改写为目标源；
			// 第三方站点跨域请求（Origin 为别的域）原样转发，仍会被服务端拒绝。
			if origin := req.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err == nil {
					oh := u.Host
					if h, _, err2 := net.SplitHostPort(oh); err2 == nil {
						oh = h
					}
					if normalizeDomain(oh) == domain {
						req.Header.Set("Origin", "http://"+m.Target)
					}
				}
			}
		}
		// SSE / HMR 等流式响应立即冲刷
		rp.FlushInterval = -1
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "LocalMap: 无法连接 %s（%s）\n目标服务可能未启动。\n", m.Target, err)
		}
		rp.ServeHTTP(w, r)
	})
}

// probeTarget TCP 探测目标端口是否可达
func probeTarget(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
