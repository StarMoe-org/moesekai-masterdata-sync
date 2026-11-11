package nuverse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/crypto"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

func keys() appmeta.Keys {
	var a, b crypto.Key
	for i := range a.Key {
		a.Key[i], a.IV[i] = byte(i), byte(i+1)
		b.Key[i], b.IV[i] = byte(0x80+i), byte(0x90+i)
	}
	return appmeta.Keys{a, b}
}

func bundle() *appmeta.Bundle {
	b := &appmeta.Bundle{Keys: keys()}
	b.Meta.Region, b.Meta.AppVersion, b.Meta.AppHash = "cn", "6.4.1", "hash"
	b.Meta.Package.VersionName, b.Meta.Package.VersionCode = "6.4.1", 4647
	return b
}

func encrypted(t *testing.T, k crypto.Key, v any) []byte {
	t.Helper()
	b, err := msgpack.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return k.Encrypt(b)
}

func TestLogin(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header
		body, _ := io.ReadAll(r.Body)
		if plain, err := keys()[0].Decrypt(body); err != nil || len(plain) != 0 {
			t.Errorf("请求体应为 [0] 密钥加密的空明文：%v", err)
		}
		switch r.Header.Get("X-App-Version") {
		case "6.4.1.4647":
			w.Write(encrypted(t, keys()[0], msgpack.M("appVersion", "6.4.1", "appVersionStatus", "available",
				"dataVersion", "6.4.1.6", "assetVersion", "6.4.1.0", "cdnVersion", int64(186), "multiPlayVersion", "miku")))
		default: // 旧版本：426，响应用 [1] 加密
			w.WriteHeader(http.StatusUpgradeRequired)
			w.Write(encrypted(t, keys()[1], msgpack.M("appVersion", "6.0.0", "appVersionStatus", "not_available",
				"dataVersion", "6.0.0.61", "cdnVersion", int64(169))))
		}
	}))
	defer srv.Close()
	c := New(Config{APIBase: srv.URL + "/api/", UserID: 1234567890123456789})

	st, err := c.Login(context.Background(), bundle())
	if err != nil {
		t.Fatal(err)
	}
	if st.CDNVersion != 186 || st.DataVersion != "6.4.1.6" || st.Outdated() {
		t.Errorf("%+v", st)
	}
	if gotPath != "/api/user/1234567890123456789/login" || gotHeader.Get("X-App-Hash") != "hash" ||
		gotHeader.Get("X-Platform") != "Android" || gotHeader.Get("X-Session-Token") != "" {
		t.Errorf("请求不对：%s %v", gotPath, gotHeader)
	}

	old := bundle()
	old.Meta.Package.VersionName = "6.0.0"
	st, err = c.Login(context.Background(), old)
	if err != nil || !st.Outdated() || st.HTTPStatus != 426 {
		t.Errorf("旧版本应判为过期：%+v %v", st, err)
	}
}

func TestLoginErrors(t *testing.T) {
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	c := New(Config{APIBase: srv.URL + "/api/", UserID: 1})
	if _, err := c.Login(context.Background(), bundle()); !errors.Is(err, ErrMaintenance) {
		t.Errorf("503 应为维护：%v", err)
	}
	status = http.StatusInternalServerError
	if _, err := c.Login(context.Background(), bundle()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 应报错：%v", err)
	}
	if _, err := New(Config{APIBase: srv.URL + "/api/"}).Login(context.Background(), bundle()); !errors.Is(err, ErrNoUser) {
		t.Errorf("没有 uid 应报错：%v", err)
	}
}

func TestDownloadMasterFallsBack(t *testing.T) {
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if strings.HasPrefix(r.URL.Path, "/good/") && strings.HasSuffix(r.URL.Path, "/60001/master-data-186.info") {
			w.Write([]byte("data"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(Config{ServerID: 60001, CDNBases: []string{srv.URL + "/bad/AssetBundle/", srv.URL + "/good/AssetBundle/"}})
	for i := 0; i < 4; i++ { // 起点随机，多试几次都应拿到
		data, url, err := c.DownloadMaster(context.Background(), 186)
		if err != nil || string(data) != "data" || !strings.Contains(url, "/good/MasterData/60001/") {
			t.Fatalf("%q %s %v", data, url, err)
		}
	}
	if _, _, err := c.DownloadMaster(context.Background(), 187); !errors.Is(err, ErrNotFound) {
		t.Errorf("全部 404 应为 ErrNotFound：%v", err)
	}
}
