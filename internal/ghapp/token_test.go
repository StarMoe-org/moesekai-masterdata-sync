package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return k, pkcs1, pkcs8
}

func TestParseKey(t *testing.T) {
	k, pkcs1, pkcs8 := testKey(t)
	for name, s := range map[string]string{
		"pkcs1":   pkcs1,
		"pkcs8":   pkcs8,
		"escaped": strings.ReplaceAll(pkcs1, "\n", `\n`),
		"base64":  base64.StdEncoding.EncodeToString([]byte(pkcs8)),
	} {
		got, err := ParseKey(s)
		if err != nil || !got.Equal(k) {
			t.Errorf("%s：%v", name, err)
		}
	}
	if _, err := ParseKey("not a key"); err == nil {
		t.Error("乱码应报错")
	}
}

func TestToken(t *testing.T) {
	k, pkcs1, _ := testKey(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		// 校验 JWT：RS256 签名、iss、iat/exp
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if !ok || len(parts) != 3 {
			http.Error(w, `{"message":"bad auth"}`, 401)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			http.Error(w, `{"message":"bad signature"}`, 401)
			return
		}
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Iss      string `json:"iss"`
			Iat, Exp int64
		}
		json.Unmarshal(raw, &claims)
		if claims.Iss != "42" || claims.Iat != now.Unix()-60 || claims.Exp != now.Add(9*time.Minute).Unix() {
			http.Error(w, `{"message":"bad claims"}`, 401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/o/data/installation":
			fmt.Fprint(w, `{"id":7}`)
		case "POST /app/installations/7/access_tokens":
			var body struct{ Repositories []string }
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Repositories) != 1 || body.Repositories[0] != "data" {
				http.Error(w, `{"message":"bad repos"}`, 422)
				return
			}
			fmt.Fprintf(w, `{"token":"tok-%d","expires_at":%q}`, len(calls), now.Add(time.Hour).Format(time.RFC3339))
		default:
			http.Error(w, `{"message":"Not Found"}`, 404)
		}
	}))
	defer srv.Close()

	a, err := New("42", pkcs1)
	if err != nil {
		t.Fatal(err)
	}
	a.APIBase, a.Now = srv.URL, func() time.Time { return now }
	ctx := context.Background()
	if tok, err := a.Token(ctx, "o", "data"); err != nil || tok != "tok-2" {
		t.Fatalf("Token = %q %v", tok, err)
	}
	// 缓存：还有 1 小时有效期，不再请求
	if tok, _ := a.Token(ctx, "o", "data"); tok != "tok-2" || len(calls) != 2 {
		t.Errorf("应使用缓存：%q %v", tok, calls)
	}
	// 剩余不到 10 分钟：换新的
	now = now.Add(55 * time.Minute)
	if tok, err := a.Token(ctx, "o", "data"); err != nil || tok != "tok-4" {
		t.Errorf("应换新 token：%q %v", tok, err)
	}
	// 没安装到的仓库
	if _, err := a.Token(ctx, "o", "other"); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("未安装应报错：%v", err)
	}
}
