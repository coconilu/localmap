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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.4.0"

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

	// -data 未显式指定（用默认目录）时，把旧版 exe 同级 data\ 的数据迁过来
	dataExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			dataExplicit = true
		}
	})
	if !dataExplicit {
		migrateLegacyData(*dataDir, legacyDataDir())
	}

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

	// 先绑定再服务：端口被占（多半是另一个 LocalMap 实例在跑）直接退出，
	// 避免双实例并存——各持一份数据、谁抢到请求算谁（见 issue #4）
	proxyLn, err := net.Listen("tcp", *proxyAddr)
	if err != nil {
		log.Fatalf("[proxy] 无法监听 %s: %v（另一个 LocalMap 实例可能在运行）", *proxyAddr, err)
	}
	apiLn, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		log.Fatalf("[api] 无法监听 %s: %v（另一个 LocalMap 实例可能在运行）", *apiAddr, err)
	}

	errCh := make(chan error, 2)
	go func() {
		log.Printf("[proxy] 监听 http://%s", *proxyAddr)
		errCh <- proxySrv.Serve(proxyLn)
	}()
	go func() {
		log.Printf("[api] 管理界面 http://%s/", *apiAddr)
		errCh <- apiSrv.Serve(apiLn)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		log.Println("正在停止…")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(ctx)
	_ = apiSrv.Shutdown(ctx)
}

// defaultDataDir 固定到 %ProgramData%\LocalMap：SYSTEM（开机计划任务）与
// 用户态（UI 拉起）引擎共享同一份数据，避免多实例各持一份
// mappings.json / CA 导致映射"消失"（见 issue #4）。
// ProgramData 默认 ACL 允许标准用户创建子目录，无需提权。
func defaultDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "LocalMap")
	}
	return `C:\ProgramData\LocalMap`
}

// legacyDataDir 旧版默认数据目录（exe 同级 data\），仅用于迁移
func legacyDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

// migrateLegacyData 新目录还没有数据、旧目录有 mappings.json 时整体复制过来
//（复制而非移动，旧目录留作备份）。robocopy 退出码 <8 均为成功。
func migrateLegacyData(newDir, legacyDir string) {
	if legacyDir == "" || filepath.Clean(newDir) == filepath.Clean(legacyDir) {
		return
	}
	if _, err := os.Stat(filepath.Join(newDir, "mappings.json")); err == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "mappings.json")); err != nil {
		return
	}
	if err := os.MkdirAll(newDir, 0755); err != nil {
		log.Printf("[migrate] 创建数据目录失败: %v", err)
		return
	}
	out, err := runHidden("robocopy", legacyDir, newDir, "/E", "/COPY:DAT", "/NFL", "/NDL", "/NJH", "/NJS")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() >= 8 {
			log.Printf("[migrate] 迁移旧数据失败: %v %s", err, strings.TrimSpace(out))
			return
		}
	}
	log.Printf("[migrate] 已把旧数据目录 %s 迁移到 %s", legacyDir, newDir)
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
