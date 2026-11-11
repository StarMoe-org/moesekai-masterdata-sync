package hmacauth

import (
	"net/http"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	// 固定向量（Python hmac 独立算出）：HMAC_SHA256(secret, "1700000000.{}")
	if got := Sign(secret, 1700000000, []byte("{}")); got != "sha256=417adf36b901772f658597a76d329694e335e65fc0849d7715c3ca33406309b5" {
		t.Errorf("Sign = %s", got)
	}
	now := time.Unix(1700000000, 0)
	h := http.Header{}
	SetHeaders(h, secret, now, []byte("body"))
	if err := Verify(secret, h, []byte("body"), now.Add(4*time.Minute)); err != nil {
		t.Errorf("应通过：%v", err)
	}
	if Verify(secret, h, []byte("body2"), now) == nil {
		t.Error("body 不同应失败")
	}
	if Verify([]byte("other"), h, []byte("body"), now) == nil {
		t.Error("密钥不同应失败")
	}
	if Verify(secret, h, []byte("body"), now.Add(6*time.Minute)) == nil {
		t.Error("超出窗口应失败")
	}
	if Verify(secret, http.Header{}, nil, now) == nil {
		t.Error("没有签名头应失败")
	}
}
