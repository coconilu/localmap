package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const caCommonName = "LocalMap Local CA"

// CertStore 管理本地 CA 和按域名签发的叶子证书（缓存于 data/certs/）
type CertStore struct {
	dir    string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	mu     sync.Mutex
	leafs  map[string]*tls.Certificate
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	return rand.Int(rand.Reader, limit)
}

// LoadOrCreateCA 加载已有 CA，不存在则新建（ECDSA P-256，10 年）
func LoadOrCreateCA(dir string) (*CertStore, error) {
	crtPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if crtPEM, err1 := os.ReadFile(crtPath); err1 == nil {
		if keyPEM, err2 := os.ReadFile(keyPath); err2 == nil {
			block, _ := pem.Decode(crtPEM)
			if block == nil {
				return nil, errors.New("ca.crt 不是合法 PEM")
			}
			caCert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			kb, _ := pem.Decode(keyPEM)
			if kb == nil {
				return nil, errors.New("ca.key 不是合法 PEM")
			}
			caKey, err := x509.ParseECPrivateKey(kb.Bytes)
			if err != nil {
				return nil, err
			}
			return &CertStore{dir: dir, caCert: caCert, caKey: caKey, leafs: map[string]*tls.Certificate{}}, nil
		}
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{"LocalMap"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(crtPath, crtPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	return &CertStore{dir: dir, caCert: caCert, caKey: caKey, leafs: map[string]*tls.Certificate{}}, nil
}

// Leaf 取域名的叶子证书：内存缓存 → 磁盘缓存 → 现场签发（2 年，SAN=域名）
func (c *CertStore) Leaf(domain string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.leafs[domain]; ok {
		return cert, nil
	}

	certsDir := filepath.Join(c.dir, "certs")
	crtPath := filepath.Join(certsDir, domain+".crt")
	keyPath := filepath.Join(certsDir, domain+".key")
	if _, err := os.Stat(crtPath); err == nil {
		if cert, err := tls.LoadX509KeyPair(crtPath, keyPath); err == nil {
			c.leafs[domain] = &cert
			return &cert, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: domain, Organization: []string{"LocalMap"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.caCert, &key.PublicKey, c.caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(certsDir, 0755); err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(crtPath, crtPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	c.leafs[domain] = &cert
	return &cert, nil
}

// InstallCA 把 CA 装进 Windows 受信任根证书存储（需要管理员/SYSTEM）
func (c *CertStore) InstallCA() error {
	out, err := runHidden("certutil", "-addstore", "-f", "root", filepath.Join(c.dir, "ca.crt"))
	if err != nil {
		return &CmdError{Cmd: "certutil -addstore", Out: out, Err: err}
	}
	return nil
}

// UninstallCA 从系统受信任根存储删除 CA，并删除本地 CA/叶子证书文件
func (c *CertStore) UninstallCA() error {
	out, err := runHidden("certutil", "-delstore", "root", caCommonName)
	if err != nil {
		return &CmdError{Cmd: "certutil -delstore", Out: out, Err: err}
	}
	_ = os.Remove(filepath.Join(c.dir, "ca.crt"))
	_ = os.Remove(filepath.Join(c.dir, "ca.key"))
	_ = os.RemoveAll(filepath.Join(c.dir, "certs"))
	return nil
}

// CATrusted 检查 CA 是否已在受信任根存储中
func CATrusted() bool {
	out, err := runHidden("certutil", "-store", "root", caCommonName)
	return err == nil && strings.Contains(out, caCommonName)
}
