package sega

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/crypto"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

func testBundle() *appmeta.Bundle {
	var k crypto.Key
	for i := range k.Key {
		k.Key[i], k.IV[i] = byte(i), byte(i+7)
	}
	b := &appmeta.Bundle{Keys: appmeta.Keys{k}}
	b.Meta.Region, b.Meta.AppVersion, b.Meta.AppHash, b.Meta.UnityVersion = "jp", "7.1.0", "hash", "2022.3.62f2"
	return b
}

// fakeServer 模拟 game-version、issue 和游戏 API（同一个 TLS 服务器）。
type fakeServer struct {
	t           *testing.T
	srv         *httptest.Server
	key         crypto.Key
	forbidFirst int  // 登录先返回几次 403
	status      int  // 非 0 时登录直接返回这个状态码
	cookieOK    bool // 已发过 cookie
	logins      int
	splitHits   []string
	sessionSeen []string
}

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, key: testBundle().Keys.API()}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) reply(w http.ResponseWriter, v any) {
	b, err := msgpack.Encode(v)
	if err != nil {
		f.t.Fatal(err)
	}
	w.Write(f.key.Encrypt(b))
}

func (f *fakeServer) body(r *http.Request) *msgpack.Map {
	raw, _ := io.ReadAll(r.Body)
	plain, err := f.key.Decrypt(raw)
	if err != nil {
		f.t.Errorf("请求体解密失败：%v", err)
		return nil
	}
	v, _ := msgpack.Decode(plain)
	m, _ := v.(*msgpack.Map)
	return m
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	h := r.Header
	if h.Get("X-App-Version") != "7.1.0" || h.Get("X-App-Hash") != "hash" || h.Get("X-Unity-Version") != "2022.3.62f2" ||
		h.Get("X-Platform") != "Android" || h.Get("X-Request-Id") == "" {
		f.t.Errorf("%s 请求头不全：%v", r.URL.Path, h)
	}
	host := strings.TrimPrefix(f.srv.URL, "https://")
	switch {
	case r.URL.Path == "/7.1.0/hash":
		f.reply(w, msgpack.M("profile", "production", "assetbundleHostHash", "cf", "domain", host))
	case r.URL.Path == "/api/signature":
		http.SetCookie(w, &http.Cookie{Name: "CloudFront-Policy", Value: "p"})
		http.SetCookie(w, &http.Cookie{Name: "CloudFront-Signature", Value: "s"})
		http.SetCookie(w, &http.Cookie{Name: "CloudFront-Key-Pair-Id", Value: "k"})
		w.Write([]byte("{}\n"))
	case h.Get("Cookie") != "CloudFront-Policy=p; CloudFront-Signature=s; CloudFront-Key-Pair-Id=k":
		w.WriteHeader(http.StatusForbidden)
	case r.Method == http.MethodPost && r.URL.Path == "/api/user":
		b := f.body(r)
		if p, _ := b.Get("platform"); p != "Android" {
			f.t.Errorf("注册 platform = %v", p)
		}
		if h.Get("X-Install-Id") == "" {
			f.t.Error("注册时应带 X-Install-Id")
		}
		f.reply(w, msgpack.M("userRegistration", msgpack.M("userId", int64(1234), "registeredAt", int64(99)), "credential", "cred"))
	case r.Method == http.MethodPut && r.URL.Path == "/api/user/1234/auth":
		f.logins++
		if r.URL.RawQuery != "refreshUpdatedResources=False" {
			f.t.Errorf("auth query = %q", r.URL.RawQuery)
		}
		if c, _ := f.body(r).Get("credential"); c != "cred" {
			f.t.Errorf("credential = %v", c)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if f.forbidFirst > 0 {
			f.forbidFirst--
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.reply(w, msgpack.M("sessionToken", "tok1", "appVersion", "7.1.0", "appVersionStatus", "available",
			"dataVersion", "7.1.0.60", "assetVersion", "7.1.0.60", "assetHash", "ah", "multiPlayVersion", "miku",
			"removeAssetVersion", "1.13.0.30", "suiteMasterSplitPath", []any{"suitemasterfile/7.1.0.60/a", "suitemasterfile/7.1.0.60/b"}))
	case strings.HasPrefix(r.URL.Path, "/api/suitemasterfile/"):
		f.splitHits = append(f.splitHits, r.URL.Path)
		f.sessionSeen = append(f.sessionSeen, h.Get("X-Session-Token"))
		if h.Get("X-Data-Version") != "7.1.0.60" {
			f.t.Errorf("分片请求的 X-Data-Version = %q", h.Get("X-Data-Version"))
		}
		w.Header().Set("X-Session-Token", "tok2") // 服务器换发 token
		if strings.HasSuffix(r.URL.Path, "/a") {
			f.reply(w, msgpack.M("cards", []any{msgpack.M("id", int64(1))}))
		} else {
			f.reply(w, msgpack.M("events", []any{}, "gachas", []any{}))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeServer) client() *Client {
	cfg := JP
	cfg.GameVersionBase, cfg.IssueBase = f.srv.URL, f.srv.URL
	c := New(cfg, testBundle())
	c.HTTP = f.srv.Client()
	return c
}

func TestRegisterLoginMaster(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := f.client()
	if err := c.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Profile() != "production" {
		t.Errorf("profile = %q", c.Profile())
	}
	store := FileStore(filepath.Join(t.TempDir(), "sub", "account.json"))
	if _, _, err := EnsureAccount(ctx, c, store, false); err == nil {
		t.Fatal("不允许注册时应报错")
	}
	a, created, err := EnsureAccount(ctx, c, store, true)
	if err != nil || !created || a.UserID != 1234 || a.Credential != "cred" || a.InstallID == "" || a.RegisteredAt != 99 {
		t.Fatalf("注册：%+v %v %v", a, created, err)
	}
	if st, err := os.Stat(string(store)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("账号文件权限：%v %v", st, err)
	}
	// 第二次：直接读已保存的
	again, created, err := EnsureAccount(ctx, c, store, true)
	if err != nil || created || *again != *a {
		t.Fatalf("读已保存的账号：%+v %v %v", again, created, err)
	}

	f.forbidFirst = 1 // 第一次 403：重新拿 cookie 后重试
	s, err := c.Login(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 || s.DataVersion != "7.1.0.60" || s.RemoveAssetVersion != "1.13.0.30" || len(s.SplitPaths) != 2 {
		t.Fatalf("登录：%d %+v", f.logins, s)
	}
	var tables []string
	err = c.Master(ctx, func(split *msgpack.Map) error {
		for _, k := range split.Keys {
			tables = append(tables, k.(string))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tables, ",") != "cards,events,gachas" {
		t.Errorf("表 = %v", tables)
	}
	if strings.Join(f.sessionSeen, ",") != "tok1,tok2" {
		t.Errorf("应使用服务器换发的 token：%v", f.sessionSeen)
	}
}

func TestLoginErrors(t *testing.T) {
	ctx := context.Background()
	a := &Account{UserID: 1234, Credential: "cred", InstallID: "i"}
	for _, c := range []struct {
		status int
		want   error
	}{
		{http.StatusUpgradeRequired, ErrOutdated},
		{http.StatusServiceUnavailable, ErrMaintenance},
		{http.StatusForbidden, ErrAccount},
	} {
		f := newFake(t)
		f.status = c.status
		cl := f.client()
		if err := cl.Prepare(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := cl.Login(ctx, a)
		if !errors.Is(err, c.want) {
			t.Errorf("HTTP %d：%v", c.status, err)
		}
		if err != nil && strings.Contains(err.Error(), "1234") {
			t.Errorf("错误信息里不应有 userId：%v", err)
		}
	}
	// 500：HTTPError，路径里不带 userId
	f := newFake(t)
	f.status = 500
	cl := f.client()
	cl.Prepare(ctx)
	_, err := cl.Login(ctx, a)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 500 || strings.Contains(err.Error(), "1234") {
		t.Errorf("500：%v", err)
	}
}

func TestMasterRejectsBadPath(t *testing.T) {
	c := New(JP, testBundle())
	c.session = &Session{Token: "t", SplitPaths: []string{"../x"}}
	if err := c.Master(context.Background(), func(*msgpack.Map) error { return nil }); err == nil {
		t.Error("带 .. 的分片路径应报错")
	}
}
