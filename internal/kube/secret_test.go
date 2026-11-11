package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/sega"
)

// fakeAPI 模拟 API server 上的一个 Secret。
func fakeAPI(t *testing.T, data map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/api/v1/namespaces/apps/secrets/accounts" {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Errorf("Content-Type = %s", r.Header.Get("Content-Type"))
			}
			b, _ := io.ReadAll(r.Body)
			var p struct{ Data map[string]string }
			json.Unmarshal(b, &p)
			for k, v := range p.Data {
				data[k] = v
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAccountStore(t *testing.T) {
	ctx := context.Background()
	data := map[string]string{"other": base64.StdEncoding.EncodeToString([]byte("keep"))}
	srv := fakeAPI(t, data)
	sc := &SecretClient{APIBase: srv.URL, Namespace: "apps", Name: "accounts", Token: func() (string, error) { return "tok", nil }, HTTP: srv.Client()}
	store := &AccountStore{Secret: sc, Region: "jp"}

	if a, err := store.Load(ctx); a != nil || err != nil {
		t.Fatalf("空的 Secret：%v %v", a, err)
	}
	want := &sega.Account{UserID: 42, Credential: "c", InstallID: "i"}
	if err := store.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx)
	if err != nil || *got != *want {
		t.Fatalf("Load = %+v %v", got, err)
	}
	if data["other"] == "" {
		t.Error("其他键不应被改动")
	}

	bad := &SecretClient{APIBase: srv.URL, Namespace: "apps", Name: "nope", Token: sc.Token, HTTP: srv.Client()}
	if _, err := (&AccountStore{Secret: bad, Region: "jp"}).Load(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Secret 不存在应报错：%v", err)
	}
}
