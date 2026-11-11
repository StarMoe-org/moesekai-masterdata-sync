package master

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

func TestWriteTables(t *testing.T) {
	dir := t.TempDir()
	tables := msgpack.M("a", []any{int64(1)}, "b", &msgpack.Map{})
	res, err := WriteTables(DirSink(dir), tables)
	if err != nil || len(res.Written) != 2 || !res.Produced["b"] {
		t.Fatalf("%v %+v", err, res)
	}
	if tables.Values[0] != nil {
		t.Error("写完的表应从内存里清掉")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "master", "a.json"))
	if string(data) != "[\n  1\n]" {
		t.Errorf("a.json = %q", data)
	}
	res, err = WriteTables(DirSink(dir), msgpack.M("a", []any{int64(1)}))
	if err != nil || len(res.Written) != 0 || res.Unchanged != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := WriteTables(DirSink(dir), msgpack.M("../x", []any{})); err == nil {
		t.Error("表名带路径应报错")
	}
	got := TablesIn([]string{"master/a.json", "master/x/y.json", "versions/1.json", "master/b.json", "README.md"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("TablesIn = %v", got)
	}
}

func TestDeleteGuard(t *testing.T) {
	g := NewDeleteGuard()
	existing := []string{"a", "b", "c", "d"}
	produced := map[string]bool{"a": true, "b": true, "c": true}
	if del := g.Decide(existing, produced); del != nil {
		t.Errorf("第一次缺失不应删除：%v", del)
	}
	if del := g.Decide(existing, produced); !reflect.DeepEqual(del, []string{"d"}) {
		t.Errorf("连续两次缺失应删除 d：%v", del)
	}
	// 缺失后又出现：计数清零
	g.Decide(existing, produced)
	g.Decide(existing, map[string]bool{"a": true, "b": true, "c": true, "d": true})
	if del := g.Decide(existing, produced); del != nil {
		t.Errorf("重新出现过的表要重新计数：%v", del)
	}
	// 这次的表太少：一张都不删、也不计数
	g2 := NewDeleteGuard()
	few := map[string]bool{"a": true, "b": true}
	g2.Decide(existing, few)
	if del := g2.Decide(existing, few); del != nil {
		t.Errorf("少于 75%% 不应删除：%v", del)
	}
	// 每次最多删 MaxPerRound 张
	g3 := &DeleteGuard{MaxPerRound: 1, missing: map[string]int{}}
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h", "x", "y"}
	keep := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true, "f": true, "g": true, "h": true}
	g3.Decide(many, keep)
	if del := g3.Decide(many, keep); !reflect.DeepEqual(del, []string{"x"}) {
		t.Errorf("每次最多 1 张：%v", del)
	}
	if del := g3.Decide(many, keep); !reflect.DeepEqual(del, []string{"y"}) {
		t.Errorf("剩下的下一次再删：%v", del)
	}
}

func TestVersionFile(t *testing.T) {
	dir := t.TempDir()
	v := Version{SystemProfile: "production", AppVersion: "6.4.1", MultiPlayVersion: "miku", DataVersion: "6.4.1.6",
		AssetVersion: "6.4.1.0", AppHash: "h", AppVersionStatus: "available", CDNVersion: 186}
	if err := WriteVersion(DirSink(dir), v); err != nil {
		t.Fatal(err)
	}
	// 和 Haruki sc 仓库 current_version.json 的格式一致
	want := "{\n  \"systemProfile\": \"production\",\n  \"appVersion\": \"6.4.1\",\n  \"multiPlayVersion\": \"miku\",\n" +
		"  \"dataVersion\": \"6.4.1.6\",\n  \"assetVersion\": \"6.4.1.0\",\n  \"appHash\": \"h\",\n  \"assetHash\": \"\",\n" +
		"  \"appVersionStatus\": \"available\",\n  \"cdnVersion\": 186\n}"
	for _, name := range []string{"current_version.json", "6.4.1.6.json"} {
		data, _ := os.ReadFile(filepath.Join(dir, "versions", name))
		if string(data) != want {
			t.Errorf("%s = %q", name, data)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "versions", "current_version.json"))
	got, err := ParseVersion(data)
	if err != nil || got.CDNVersion != 186 || got.DataVersion != "6.4.1.6" {
		t.Errorf("ParseVersion = %+v %v", got, err)
	}
	if v, err := ParseVersion(nil); v != nil || err != nil {
		t.Errorf("nil 应返回 nil")
	}
}
