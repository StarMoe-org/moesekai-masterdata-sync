package appmeta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bits0 = "00000001" // 每字节

func writeRepo(t *testing.T, moeInfo string, recordedSum string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cn", "6.4.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(moeInfo))
	if recordedSum == "" {
		recordedSum = hex.EncodeToString(sum[:])
	}
	latest := `{"schema":1,"region":"cn","appVersion":"6.4.1","appHash":"h","package":{"versionName":"6.4.1","versionCode":4647},"files":{"moe-info.json":"` + recordedSum + `"}}`
	os.WriteFile(filepath.Join(dir, "cn", "latest.json"), []byte(latest), 0o644)
	os.WriteFile(filepath.Join(dir, "cn", "6.4.1", "moe-info.json"), []byte(moeInfo), 0o644)
	return dir
}

func TestLatestFromLocalRepo(t *testing.T) {
	k := strings.Repeat(bits0, 16)
	dir := writeRepo(t, `[{"Moe-Key":"`+k+`","Moe-Secret":"`+k+`"}]`, "")
	c := &Client{Base: "file://" + dir}
	b, err := c.Latest(context.Background(), "cn")
	if err != nil {
		t.Fatal(err)
	}
	if b.Meta.NuverseAppVersion() != "6.4.1.4647" || b.Keys.API().Key[0] != 1 || b.Schema != nil {
		t.Errorf("%+v", b)
	}
	if _, err := c.Latest(context.Background(), "jp"); err == nil {
		t.Error("不存在的区服应报错")
	}
}

func TestLatestRejectsWrongSha(t *testing.T) {
	k := strings.Repeat(bits0, 16)
	dir := writeRepo(t, `[{"Moe-Key":"`+k+`","Moe-Secret":"`+k+`"}]`, strings.Repeat("0", 64))
	if _, err := (&Client{Base: "file://" + dir}).Latest(context.Background(), "cn"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("sha256 不一致应报错：%v", err)
	}
}
