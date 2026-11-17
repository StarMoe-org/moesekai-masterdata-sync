package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhook(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	a := Alert{Region: "cn", Kind: Failing, Failures: 5, Since: time.Date(2026, 11, 12, 12, 26, 0, 0, time.UTC), Error: "git push：exit status 1"}
	if err := (&Webhook{URL: srv.URL}).Send(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	text, _ := got["text"].(string)
	if !strings.Contains(text, "cn 连续失败 5 轮（自 2026-11-12 12:26 UTC 起）：git push") || got["content"] != text {
		t.Fatalf("text = %q, content = %v", text, got["content"])
	}
	if got["region"] != "cn" || got["kind"] != "failing" || got["failures"] != float64(5) {
		t.Fatalf("结构化字段 %v", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if err := (&Webhook{URL: bad.URL}).Send(context.Background(), a); err == nil {
		t.Fatal("500 应返回错误")
	}
}
