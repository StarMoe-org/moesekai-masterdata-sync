package redact

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestURLError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/api/user/7445088458181384997/login", nil)
	_, err := http.DefaultClient.Do(req)
	if err == nil || !strings.Contains(err.Error(), "7445088458181384997") {
		t.Fatalf("前提不成立：%v", err)
	}
	got := URLError(err)
	if strings.Contains(got.Error(), "7445088458181384997") || !strings.Contains(got.Error(), "/api/user/…/login") {
		t.Fatalf("没有脱敏：%v", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("脱敏后丢了原来的错误：%v", got)
	}
	if e := errors.New("x"); URLError(e) != e || URLError(nil) != nil {
		t.Fatal("其他错误应原样返回")
	}
	if s := UserPath("https://h/api/user/123?x=1"); s != "https://h/api/user/…?x=1" {
		t.Fatal(s)
	}
}
