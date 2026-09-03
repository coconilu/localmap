package main

import (
	"os"
	"strings"
)

const hostsPath = `C:\Windows\System32\drivers\etc\hosts`

const (
	hostsBegin = "# >>> LOCALMAP (managed, do not edit) >>>"
	hostsEnd   = "# <<< LOCALMAP <<<"
)

// desiredHostsBlock 生成受管区块内容（不含 markers 时为 nil）
func desiredHostsBlock(domains []string) []string {
	if len(domains) == 0 {
		return nil
	}
	lines := []string{hostsBegin}
	for _, d := range domains {
		lines = append(lines, "127.0.0.1 "+d)
	}
	lines = append(lines, hostsEnd)
	return lines
}

// splitHosts 把 hosts 内容拆成「受管区块之外的行」和「受管区块内的域名」
func splitHosts(content string) (outside []string, managed []string) {
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimRight(line, "\r")
		switch strings.TrimSpace(trim) {
		case hostsBegin:
			inBlock = true
			continue
		case hostsEnd:
			inBlock = false
			continue
		}
		if inBlock {
			fields := strings.Fields(trim)
			if len(fields) == 2 && fields[0] == "127.0.0.1" {
				managed = append(managed, fields[1])
			}
			continue
		}
		outside = append(outside, trim)
	}
	// 去掉尾部空行，便于稳定拼接
	for len(outside) > 0 && strings.TrimSpace(outside[len(outside)-1]) == "" {
		outside = outside[:len(outside)-1]
	}
	return outside, managed
}

// SyncHosts 把 hosts 受管区块同步为给定域名集合。返回错误不致命（可能无权限）。
func SyncHosts(domains []string) error {
	raw, err := os.ReadFile(hostsPath)
	if err != nil {
		return err
	}
	outside, _ := splitHosts(string(raw))

	var b strings.Builder
	for _, l := range outside {
		b.WriteString(l + "\r\n")
	}
	if block := desiredHostsBlock(domains); block != nil {
		b.WriteString("\r\n")
		for _, l := range block {
			b.WriteString(l + "\r\n")
		}
	}
	// hosts 文件可能带只读属性（Windows 上 Chmod 的 0200 位控制只读属性）
	_ = os.Chmod(hostsPath, 0666)
	return os.WriteFile(hostsPath, []byte(b.String()), 0644)
}

// HostsInSync 检查 hosts 受管区块与当前域名集合是否一致
func HostsInSync(domains []string) (bool, string) {
	raw, err := os.ReadFile(hostsPath)
	if err != nil {
		return false, err.Error()
	}
	_, managed := splitHosts(string(raw))
	want := map[string]bool{}
	for _, d := range domains {
		want[d] = true
	}
	got := map[string]bool{}
	for _, d := range managed {
		got[d] = true
	}
	if len(want) != len(got) {
		return false, "受管条目数量不一致"
	}
	for d := range want {
		if !got[d] {
			return false, "hosts 缺少 " + d
		}
	}
	return true, ""
}
