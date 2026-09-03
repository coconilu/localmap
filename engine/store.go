package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Mapping 一条本地域名映射：domain -> target(host:port)
type Mapping struct {
	Domain string `json:"domain"`
	Target string `json:"target"`
}

var domainRe = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// Store 映射表持久化（JSON 文件）
type Store struct {
	path string
	mu   sync.Mutex
	list []Mapping
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.list); err != nil {
		return nil, err
	}
	return s, nil
}

func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(d, ".")))
}

func ValidateMapping(m Mapping) error {
	m.Domain = normalizeDomain(m.Domain)
	if !domainRe.MatchString(m.Domain) {
		return errors.New("域名格式不合法")
	}
	if m.Domain == "localhost" || strings.HasSuffix(m.Domain, ".localhost") {
		return errors.New("localhost 域名无需映射")
	}
	t := strings.TrimSpace(m.Target)
	host, port, ok := strings.Cut(t, ":")
	if !ok || host == "" || port == "" {
		return errors.New("目标必须是 host:port 形式，如 127.0.0.1:58628")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return errors.New("目标端口必须是数字")
		}
	}
	return nil
}

func (s *Store) List() []Mapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Mapping, len(s.list))
	copy(out, s.list)
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

func (s *Store) Find(domain string) (Mapping, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.list {
		if m.Domain == domain {
			return m, true
		}
	}
	return Mapping{}, false
}

// Upsert 新增或更新映射，返回是否为新条目
func (s *Store) Upsert(m Mapping) (bool, error) {
	m.Domain = normalizeDomain(m.Domain)
	m.Target = strings.TrimSpace(m.Target)
	if err := ValidateMapping(m); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.list {
		if x.Domain == m.Domain {
			s.list[i] = m
			return false, s.saveLocked()
		}
	}
	s.list = append(s.list, m)
	return true, s.saveLocked()
}

func (s *Store) Remove(domain string) bool {
	domain = normalizeDomain(domain)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.list {
		if m.Domain == domain {
			s.list = append(s.list[:i], s.list[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// Replace 用新内容替换旧域名对应的条目（域名本身也可改）
func (s *Store) Replace(oldDomain string, m Mapping) error {
	oldDomain = normalizeDomain(oldDomain)
	m.Domain = normalizeDomain(m.Domain)
	m.Target = strings.TrimSpace(m.Target)
	if err := ValidateMapping(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, x := range s.list {
		if x.Domain == oldDomain {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("映射不存在")
	}
	if m.Domain != oldDomain {
		for i, x := range s.list {
			if x.Domain == m.Domain && i != idx {
				return errors.New("域名已被其它映射占用")
			}
		}
	}
	s.list[idx] = m
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}
