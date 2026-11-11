package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/hmacauth"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/syncer"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/worker"
)

func TestServer(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	wk := &worker.Worker{
		Region: "jp",
		Timing: worker.DefaultTiming(5 * time.Minute),
		Runner: worker.RunnerFunc(func(context.Context, *appmeta.Bundle) (*syncer.Result, error) {
			return &syncer.Result{DataVersion: "7.1.0.60", Updated: true}, nil
		}),
		AppMeta: func(context.Context, string) (*appmeta.Bundle, error) {
			b := &appmeta.Bundle{}
			b.Meta.AppVersion = "7.1.0"
			return b, nil
		},
	}
	wk.Step(context.Background())
	s := &Server{Version: "test", Secret: secret, Workers: map[string]*worker.Worker{"jp": wk}, Now: func() time.Time { return now }}
	h := s.Handler()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/healthz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"dataVersion":"7.1.0.60"`) {
		t.Errorf("healthz：%d %s", rec.Code, rec.Body)
	}
	if rec := get("/metrics"); !strings.Contains(rec.Body.String(), `moesekai_sync_updates_total{region="jp"} 1`) ||
		!strings.Contains(rec.Body.String(), `moesekai_sync_info{region="jp",app_version="7.1.0",data_version="7.1.0.60"} 1`) {
		t.Errorf("metrics：%s", rec.Body)
	}

	post := func(body string, sign bool) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/app-meta/callback", strings.NewReader(body))
		if sign {
			hmacauth.SetHeaders(req.Header, secret, now, []byte(body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	ok := `{"requestId":"r","region":"jp","status":"succeeded","result":"advanced","appVersion":"7.2.0"}`
	if c := post(ok, false); c != 401 {
		t.Errorf("没签名应 401：%d", c)
	}
	if c := post(ok, true); c != 204 {
		t.Errorf("正常回调：%d", c)
	}
	if c := post(`{"requestId":"r","region":"en","status":"succeeded"}`, true); c != 204 {
		t.Errorf("未同步的区服应收下：%d", c)
	}
	if c := post(`{"region":""}`, true); c != 400 {
		t.Errorf("格式不对应 400：%d", c)
	}
}
