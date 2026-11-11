// Package hmacauth 是和 pjsk-meta-builder 双向使用的共享密钥签名（builder 设计文档 §7.3）：
//
//	X-Moe-Timestamp: unix 秒
//	X-Moe-Signature: sha256=hex(HMAC_SHA256(secret, timestamp + "." + body))
//
// 时间戳与本机相差超过 5 分钟拒绝；签名用常量时间比较。
package hmacauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	TimestampHeader = "X-Moe-Timestamp"
	SignatureHeader = "X-Moe-Signature"
	Window          = 5 * time.Minute
)

// Sign 返回 "sha256=<hex>"。
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// SetHeaders 给请求签名。
func SetHeaders(h http.Header, secret []byte, now time.Time, body []byte) {
	ts := now.Unix()
	h.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	h.Set(SignatureHeader, Sign(secret, ts, body))
}

// Verify 校验签名；失败的原因只用于日志，不要回给调用方。
func Verify(secret []byte, h http.Header, body []byte, now time.Time) error {
	tsText, sig := h.Get(TimestampHeader), h.Get(SignatureHeader)
	if tsText == "" || sig == "" {
		return errors.New("缺少签名头")
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil || ts < 0 {
		return errors.New("时间戳格式不对")
	}
	if d := now.Unix() - ts; d > int64(Window/time.Second) || d < -int64(Window/time.Second) {
		return errors.New("时间戳超出 5 分钟窗口")
	}
	if !hmac.Equal([]byte(Sign(secret, ts, body)), []byte(sig)) {
		return errors.New("签名不匹配")
	}
	return nil
}
