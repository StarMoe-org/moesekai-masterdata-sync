// Package httpapi 是本服务的 HTTP 接口（只在集群内网）：健康检查、指标、builder 回调。
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/builder"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/hmacauth"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/worker"
)

// Server 把请求分给各区服的 worker。
type Server struct {
	Version string
	Secret  []byte
	Workers map[string]*worker.Worker
	Now     func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("POST /v1/app-meta/callback", s.callback)
	return mux
}

func (s *Server) regions() []string {
	out := make([]string, 0, len(s.Workers))
	for r := range s.Workers {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// healthz：进程活着就返回 200，附带各区服状态（不含账号信息）。
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	type region struct {
		AppVersion          string `json:"appVersion"`
		DataVersion         string `json:"dataVersion"`
		WaitingBuilder      bool   `json:"waitingBuilder"`
		ConsecutiveFailures int    `json:"consecutiveFailures"`
		LastSuccess         string `json:"lastSuccess,omitempty"`
		LastUpdate          string `json:"lastUpdate,omitempty"`
		LastError           string `json:"lastError,omitempty"`
	}
	out := struct {
		Version string            `json:"version"`
		Regions map[string]region `json:"regions"`
	}{Version: s.Version, Regions: map[string]region{}}
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	for _, r := range s.regions() {
		st := s.Workers[r].Status()
		out.Regions[r] = region{st.AppVersion, st.DataVersion, st.WaitingBuilder, st.ConsecutiveFailures, ts(st.LastSuccess), ts(st.LastUpdate), st.LastError}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// metrics：Prometheus 文本格式。
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	gauge := func(name, help string, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	sts := map[string]worker.Status{}
	for _, r := range s.regions() {
		sts[r] = s.Workers[r].Status()
	}
	emit := func(name, help, typ string, val func(worker.Status) float64) {
		gauge(name, help, typ)
		for _, r := range s.regions() {
			fmt.Fprintf(&b, "%s{region=%q} %g\n", name, r, val(sts[r]))
		}
	}
	unix := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.Unix())
	}
	bool01 := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	emit("moesekai_sync_rounds_total", "同步轮数", "counter", func(s worker.Status) float64 { return float64(s.Rounds) })
	emit("moesekai_sync_failures_total", "失败轮数", "counter", func(s worker.Status) float64 { return float64(s.Failures) })
	emit("moesekai_sync_updates_total", "写出新 master 的次数", "counter", func(s worker.Status) float64 { return float64(s.Updates) })
	emit("moesekai_sync_consecutive_failures", "连续失败轮数", "gauge", func(s worker.Status) float64 { return float64(s.ConsecutiveFailures) })
	emit("moesekai_sync_waiting_builder", "是否在等新的 app 元数据", "gauge", func(s worker.Status) float64 { return bool01(s.WaitingBuilder) })
	emit("moesekai_sync_last_success_timestamp_seconds", "上次成功的时间", "gauge", func(s worker.Status) float64 { return unix(s.LastSuccess) })
	emit("moesekai_sync_last_update_timestamp_seconds", "上次写出新 master 的时间", "gauge", func(s worker.Status) float64 { return unix(s.LastUpdate) })
	gauge("moesekai_sync_info", "当前版本", "gauge")
	for _, r := range s.regions() {
		fmt.Fprintf(&b, "moesekai_sync_info{region=%q,app_version=%q,data_version=%q} 1\n", r, sts[r].AppVersion, sts[r].DataVersion)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	io.WriteString(w, b.String())
}

// callback：builder 处理完的通知（HMAC 签名）。只是通知，以 app-meta 仓库为准。
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		http.Error(w, "请求体过大", http.StatusRequestEntityTooLarge)
		return
	}
	if err := hmacauth.Verify(s.Secret, r.Header, body, s.now()); err != nil {
		slog.Warn("回调签名无效", "reason", err, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var cb builder.Callback
	if err := json.Unmarshal(body, &cb); err != nil || cb.Region == "" || cb.Status == "" {
		http.Error(w, "回调格式不对", http.StatusBadRequest)
		return
	}
	wk, ok := s.Workers[cb.Region]
	if !ok {
		// 本服务没在同步这个区服（比如 builder 自己发现了 en 的新版本）：收下即可
		slog.Info("忽略未同步区服的回调", "region", cb.Region, "requestId", cb.RequestID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	wk.Notify(&cb)
	w.WriteHeader(http.StatusNoContent)
}
