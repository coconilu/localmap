package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const version = "0.3.0"

func main() {
	var (
		dataDir   = flag.String("data", defaultDataDir(), "数据目录（mappings.json / token）")
		proxyAddr = flag.String("proxy-addr", "127.0.0.1:80", "反向代理监听地址")
		apiAddr   = flag.String("api-addr", "127.0.0.1:20190", "控制 API / 管理界面监听地址")
		install   = flag.Bool("install", false, "注册开机自启计划任务后退出")
		uninstall = flag.Bool("uninstall", false, "删除开机自启计划任务后退出")
	)
	flag.Parse()

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exePath, _ = filepath.Abs(exePath)
	*dataDir, _ = filepath.Abs(*dataDir)

	if *install {
		if err := TaskInstall(exePath, *dataDir); err != nil {
			log.Fatal(err)
		}
		fmt.Println("计划任务已注册:", taskName)
		return
	}
	if *uninstall {
		if err := TaskUninstall(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("计划任务已删除:", taskName)
		return
	}

	store, err := NewStore(filepath.Join(*dataDir, "mappings.json"))
	if err != nil {
		log.Fatal("加载映射表失败: ", err)
	}
	token, err := loadOrCreateToken(*dataDir)
	if err != nil {
		log.Fatal("初始化 token 失败: ", err)
	}

	engine := &Engine{
		store:     store,
		dataDir:   *dataDir,
		proxyAddr: *proxyAddr,
		apiAddr:   *apiAddr,
		token:     token,
		exePath:   exePath,
	}
	engine.syncHosts()
	engine.logSyncProxyBypass()          // 自愈：补回被代理软件覆盖的绕过条目
	go engine.bypassJanitor(time.Minute) // 周期校对（开机启动时可能尚无登录会话）

	// 已启用过 HTTPS（CA 已存在）则自动恢复 443 监听
	if _, err := os.Stat(filepath.Join(*dataDir, "ca.crt")); err == nil {
		if cs, err := LoadOrCreateCA(*dataDir); err != nil {
			log.Printf("[https] 加载 CA 失败，HTTPS 未启用: %v", err)
		} else {
			engine.certs = cs
			if err := engine.startHTTPS(); err != nil {
				log.Printf("[https] 启动 443 监听失败: %v", err)
			}
		}
	}

	proxySrv := &http.Server{Addr: *proxyAddr, Handler: NewProxyHandler(engine)}
	apiSrv := &http.Server{Addr: *apiAddr, Handler: engine.ServeMux()}

	go func() {
		log.Printf("[proxy] 监听 http://%s", *proxyAddr)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[proxy] 退出: %v", err)
		}
	}()
	go func() {
		log.Printf("[api] 管理界面 http://%s/", *apiAddr)
		if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] 退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("正在停止…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(ctx)
	_ = apiSrv.Shutdown(ctx)
}

func defaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

// startHTTPS 在 127.0.0.1:443 起 TLS 监听，SNI 按域名从 CertStore 取证书，
// 未映射域名的握手直接拒绝。与 80 端口共用同一个代理 Handler。
func (e *Engine) startHTTPS() error {
	ln, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		return err
	}
	tlsCfg := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			d := normalizeDomain(hello.ServerName)
			if _, ok := e.store.Find(d); !ok {
				return nil, fmt.Errorf("没有域名 %q 的映射", d)
			}
			return e.certs.Leaf(d)
		},
	}
	srv := &http.Server{Handler: NewProxyHandler(e)}
	e.tlsSrv = srv
	go func() {
		log.Printf("[https] 监听 https://127.0.0.1:443")
		if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil && err != http.ErrServerClosed {
			log.Printf("[https] 退出: %v", err)
		}
	}()
	e.httpsOn.Store(true)
	return nil
}
