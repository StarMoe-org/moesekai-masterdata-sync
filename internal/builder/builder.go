// Package builder 调用 pjsk-meta-builder（builder 设计文档 §7）：发起处理、解析回调。
package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/hmacauth"
)

// Client 调 builder 的 POST /v1/builds。
type Client struct {
	Base     string // 例：http://pjsk-meta-builder.apps.svc.cluster.local:8080
	Callback string // 本服务的回调地址
	Secret   []byte
	HTTP     *http.Client
	Now      func() time.Time
}

// Request 发起处理的结果。
type Request struct {
	RequestID string
	Status    string // queued / running
	Existing  bool   // 409：同一区服已有任务，等那个任务的回调
}

// RequestBuild 请求 builder 处理 region 的最新安装包（不指定 versionCode，由 builder 按默认来源取）。
func (c *Client) RequestBuild(ctx context.Context, region string) (*Request, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	id := fmt.Sprintf("sync-%s-%d", region, now().Unix())
	body, _ := json.Marshal(map[string]string{"requestId": id, "region": region, "callback": c.Callback})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Base, "/")+"/v1/builds", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hmacauth.SetHeaders(req.Header, c.Secret, now(), body)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("builder：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusConflict:
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("builder 响应：%w", err)
		}
		return &Request{RequestID: out.RequestID, Status: out.Status, Existing: resp.StatusCode == http.StatusConflict}, nil
	default:
		return nil, fmt.Errorf("builder：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
}

// Callback 是 builder 处理完后发来的通知（builder 设计文档 §7.2）。
type Callback struct {
	RequestID   string `json:"requestId"`
	Region      string `json:"region"`
	Status      string `json:"status"` // succeeded / failed
	Result      string `json:"result"` // advanced / snapshot_only / noop / rejected
	AppVersion  string `json:"appVersion"`
	VersionCode int64  `json:"versionCode"`
	Commit      string `json:"commit"`
	Error       *struct {
		Kind    string `json:"kind"` // network / not_ready / data / config / internal
		Message string `json:"message"`
	} `json:"error"`
}

// Retryable 表示失败的回调可以过一段时间再发起（network / not_ready）。
func (c *Callback) Retryable() bool {
	return c.Status == "failed" && c.Error != nil && (c.Error.Kind == "network" || c.Error.Kind == "not_ready")
}
