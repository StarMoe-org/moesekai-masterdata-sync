// Package kube 用集群内 ServiceAccount 读写一个已存在的 Secret（设计文档 S6）：只需要对这一个对象的 get / patch 权限。
// 不引入 client-go：只用到两个请求。
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/sega"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// SecretClient 读写一个 Secret 的 data 字段。
type SecretClient struct {
	APIBase   string // https://kubernetes.default.svc
	Namespace string
	Name      string
	Token     func() (string, error) // 每次请求时读（投射的 token 会轮换）
	HTTP      *http.Client
}

// InCluster 用 Pod 里挂载的 ServiceAccount token、CA 和命名空间。
func InCluster(name string) (*SecretClient, error) {
	ns, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("kube：不在集群里？%w", err)
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("kube：ca.crt 无效")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kube：缺少 KUBERNETES_SERVICE_HOST / PORT")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return &SecretClient{
		APIBase:   "https://" + host + ":" + port,
		Namespace: strings.TrimSpace(string(ns)),
		Name:      name,
		Token: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
		HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (c *SecretClient) url() string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", strings.TrimRight(c.APIBase, "/"), c.Namespace, c.Name)
}

func (c *SecretClient) do(ctx context.Context, method, contentType string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(), rd)
	if err != nil {
		return nil, err
	}
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &st)
		return nil, fmt.Errorf("kube：%s secret/%s：HTTP %d %s", method, c.Name, resp.StatusCode, st.Message)
	}
	return data, nil
}

// Get 读 data 里的一个键；不存在时返回 nil、nil。
func (c *SecretClient) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := c.do(ctx, http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	v, ok := s.Data[key]
	if !ok {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(v)
}

// Put 用 merge patch 写 data 里的一个键（其他键不动）。
func (c *SecretClient) Put(ctx context.Context, key string, value []byte) error {
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{key: base64.StdEncoding.EncodeToString(value)}})
	_, err := c.do(ctx, http.MethodPatch, "application/merge-patch+json", patch)
	return err
}

// AccountStore 把一个区服的账号存在 Secret 的 "<region>.json" 键里。
type AccountStore struct {
	Secret *SecretClient
	Region string
}

var _ sega.AccountStore = (*AccountStore)(nil)

func (s *AccountStore) key() string { return s.Region + ".json" }

func (s *AccountStore) Load(ctx context.Context) (*sega.Account, error) {
	data, err := s.Secret.Get(ctx, s.key())
	if err != nil || data == nil {
		return nil, err
	}
	var a sega.Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("账号 Secret 的 %s：%w", s.key(), err)
	}
	if a.UserID == 0 || a.Credential == "" {
		return nil, fmt.Errorf("账号 Secret 的 %s 缺少 userId 或 credential", s.key())
	}
	return &a, nil
}

func (s *AccountStore) Save(ctx context.Context, a *sega.Account) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := s.Secret.Put(ctx, s.key(), data); err != nil {
		return err
	}
	// 读回来确认真的存上了，再允许使用（设计文档 §5.1）
	got, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if got == nil || got.UserID != a.UserID || got.Credential != a.Credential {
		return errors.New("账号写入 Secret 后读回不一致")
	}
	return nil
}
