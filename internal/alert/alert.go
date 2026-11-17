// Package alert 发告警：连续失败、恢复、builder 需要人工处理。
// 配了 Webhook 就 POST 一个 JSON；没配时只写日志（日志里总会有一条「告警」）。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Kind 是告警的种类。
type Kind string

const (
	Failing   Kind = "failing"   // 连续失败达到阈值（之后每隔一段时间再提醒）
	Recovered Kind = "recovered" // 告警过的区服恢复了
	Builder   Kind = "builder"   // builder 处理失败且不可重试，需要人工处理
)

// Alert 是一条告警。
type Alert struct {
	Region   string    `json:"region"`
	Kind     Kind      `json:"kind"`
	Failures int       `json:"failures,omitempty"` // 连续失败轮数
	Since    time.Time `json:"since,omitzero"`     // 这次连续失败从什么时候开始
	Error    string    `json:"error,omitempty"`
}

// Text 是给人看的一行。
func (a Alert) Text() string {
	const name = "moesekai-masterdata-sync"
	since := a.Since.UTC().Format("2006-01-02 15:04 UTC")
	switch a.Kind {
	case Failing:
		return fmt.Sprintf("[%s] %s 连续失败 %d 轮（自 %s 起）：%s", name, a.Region, a.Failures, since, clip(a.Error, 600))
	case Recovered:
		return fmt.Sprintf("[%s] %s 已恢复（此前连续失败 %d 轮，自 %s 起）", name, a.Region, a.Failures, since)
	default:
		return fmt.Sprintf("[%s] %s builder 处理失败，需要人工处理：%s", name, a.Region, clip(a.Error, 600))
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Sender 发送告警。
type Sender interface {
	Send(ctx context.Context, a Alert) error
}

// Webhook 把告警 POST 到一个地址。请求体同时带 text（Slack / Mattermost 等）和 content（Discord），以及结构化字段。
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func (w *Webhook) Send(ctx context.Context, a Alert) error {
	text := a.Text()
	body, err := json.Marshal(struct {
		Alert
		Text    string `json:"text"`
		Content string `json:"content"`
	}{a, text, text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("告警 Webhook 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// Notify 写日志并（配置了 Sender 时）发送；发送失败只记日志。
func Notify(ctx context.Context, s Sender, a Alert) {
	if a.Kind == Recovered {
		slog.Info("告警", "region", a.Region, "kind", a.Kind, "text", a.Text())
	} else {
		slog.Error("告警", "region", a.Region, "kind", a.Kind, "text", a.Text())
	}
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := s.Send(ctx, a); err != nil {
		slog.Warn("发送告警失败", "region", a.Region, "kind", a.Kind, "err", err)
	}
}
