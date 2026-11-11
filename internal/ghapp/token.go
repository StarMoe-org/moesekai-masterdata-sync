// Package ghapp 用 GitHub App 的私钥换 installation token，并按仓库收窄（只申请要写的那一个仓库）。
// 与 pjsk-meta-builder 的 GitHubAppToken 一致：私钥可以是原样 PEM、把换行写成 \n 的 PEM，或整个 PEM 的 base64。
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// App 是一个 GitHub App。
type App struct {
	ID      string
	Key     *rsa.PrivateKey
	APIBase string // 默认 https://api.github.com
	HTTP    *http.Client
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cached // owner/repo → token
}

type cached struct {
	token   string
	expires time.Time
}

// New 解析私钥。
func New(id, privateKey string) (*App, error) {
	if id == "" || privateKey == "" {
		return nil, errors.New("ghapp: 缺少 App ID 或私钥")
	}
	key, err := ParseKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &App{ID: id, Key: key, APIBase: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second},
		Now: time.Now, cache: map[string]cached{}}, nil
}

// ParseKey 接受原样 PEM、\n 转义的 PEM、base64 编码的 PEM。
func ParseKey(s string) (*rsa.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "-----BEGIN") {
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, errors.New("ghapp: 私钥既不是 PEM 也不是 base64")
		}
		s = string(raw)
	}
	s = strings.ReplaceAll(s, `\n`, "\n")
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("ghapp: 私钥 PEM 无法解析")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ghapp: 私钥：%w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("ghapp: 私钥不是 RSA")
	}
	return rk, nil
}

func (a *App) jwt() (string, error) {
	now := a.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ID})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Token 返回只对 owner/repo 有效的 installation token；剩余有效期少于 10 分钟时换新的。
func (a *App) Token(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && a.Now().Add(10*time.Minute).Before(c.expires) {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/installation", owner, repo), jwt, nil, &inst); err != nil {
		return "", fmt.Errorf("ghapp: App 没有安装到 %s？%w", key, err)
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{repo}}
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), jwt, body, &tok); err != nil {
		return "", fmt.Errorf("ghapp: 换 %s 的 token：%w", key, err)
	}
	a.mu.Lock()
	a.cache[key] = cached{tok.Token, tok.ExpiresAt}
	a.mu.Unlock()
	return tok.Token, nil
}

func (a *App) call(ctx context.Context, method, path, jwt string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.APIBase, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, e.Message)
	}
	return json.Unmarshal(data, out)
}
