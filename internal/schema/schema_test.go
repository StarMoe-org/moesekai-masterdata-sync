package schema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/jsonout"
	. "github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

const testSchema = `{
  "schema": 1,
  "source": {"appVersion": "1.0.0"},
  "tables": {
    "cards": {"class": "Sekai.MasterCard", "container": "array"},
    "areas": {"class": "Sekai.MasterArea", "container": "array"},
    "single": {"class": "Sekai.MasterSingle", "container": "object"},
    "compactRewards": {"class": "Sekai.CompactRewards", "container": "object", "compact": true}
  },
  "classes": {
    "Sekai.MasterCard": {"fields": [
      {"key": 0, "name": "id", "type": "int"},
      {"key": 1, "name": "name", "type": "string", "nullable": true},
      {"key": 3, "name": "params", "type": {"class": "Sekai.Params"}, "nullable": true},
      {"key": 4, "name": "rewards", "type": {"array": {"class": "Sekai.Reward"}}, "nullable": true},
      {"key": 5, "name": "rate", "type": "float"}
    ]},
    "Sekai.Params": {"fields": [{"key": "param1", "name": "param1", "type": {"array": "int"}}]},
    "Sekai.Reward": {"fields": [{"key": 0, "name": "resourceBoxId", "type": "int"}]},
    "Sekai.MasterArea": {"fields": [
      {"key": "id", "name": "id", "type": "int"},
      {"key": "receiveRewardEndAt", "name": "rewardReceiveEndAt", "type": "long"},
      {"key": "rewards", "name": "rewards", "type": {"array": {"class": "Sekai.Reward"}}, "nullable": true}
    ]},
    "Sekai.MasterSingle": {"fields": [{"key": "v", "name": "v", "type": "int"}]},
    "Sekai.CompactRewards": {"fields": [{"key": "__ENUM__", "name": "enums", "type": {"unknown": "x"}}]}
  }
}`

func mustParse(t *testing.T) *Schema {
	t.Helper()
	s, err := Parse([]byte(testSchema))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func restoreJSON(t *testing.T, master any) (string, *Result) {
	t.Helper()
	res, err := mustParse(t).Restore(master)
	if err != nil {
		t.Fatal(err)
	}
	b, err := jsonout.Marshal(res.Tables)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(b)), " "), res
}

func TestRestoreIntKeyedRows(t *testing.T) {
	master := M("cards", []any{
		// 下标 2 在类里没有：跳过；name 为 null：省略；嵌套类和类数组递归还原
		[]any{int64(1), nil, "ignored", M("param1", []any{int64(5)}), []any{[]any{int64(8000)}}, float32(0.5)},
	})
	got, _ := restoreJSON(t, master)
	want := `{ "cards": [ { "id": 1, "params": { "param1": [ 5 ] }, "rewards": [ { "resourceBoxId": 8000 } ], "rate": 0.5 } ] }`
	if got != want {
		t.Errorf("得到 %s\n期望 %s", got, want)
	}
}

func TestRestoreNameKeyedRows(t *testing.T) {
	// 键换成字段名、递归还原、按键排序；schema 里没有的键原样保留
	master := M("areas", []any{M("rewards", []any{[]any{int64(7)}}, "receiveRewardEndAt", int64(9), "id", int64(1), "extra", "x")})
	got, _ := restoreJSON(t, master)
	want := `{ "areas": [ { "extra": "x", "id": 1, "rewardReceiveEndAt": 9, "rewards": [ { "resourceBoxId": 7 } ] } ] }`
	if got != want {
		t.Errorf("得到 %s\n期望 %s", got, want)
	}
}

func TestExtraTrailingColumnsDropped(t *testing.T) {
	master := M("cards", []any{[]any{int64(1), "a", nil, nil, nil, float64(1), true}})
	got, res := restoreJSON(t, master)
	if !strings.Contains(got, `"rate": 1.0 }`) || !reflect.DeepEqual(res.Dropped, []string{"cards"}) {
		t.Errorf("得到 %s，dropped %v", got, res.Dropped)
	}
}

func TestTypeMismatchFails(t *testing.T) {
	for _, master := range []any{
		M("cards", []any{[]any{"not-int"}}),          // int 位置出现 string（错位）
		M("cards", []any{M("id", int64(1))}),         // 按下标的类却是 map
		M("areas", []any{[]any{int64(1)}}),           // 按名字的类却是数组
		M("single", []any{int64(1)}),                 // object 却是数组
		M("cards", []any{[]any{int64(1), int64(2)}}), // string 位置是 int
	} {
		if _, err := mustParse(t).Restore(master); err == nil {
			t.Errorf("%v：应报错", master)
		}
	}
}

func TestCompactExpanded(t *testing.T) {
	master := M("compactRewards", M(
		"__ENUM__", M("type", []any{"resource", "voice"}),
		"id", []any{int64(1), int64(2)},
		"type", []any{int64(1), nil},
		"box", []any{nil, int64(5)},
	))
	got, res := restoreJSON(t, master)
	if !strings.Contains(got, `"rewards": [ { "id": 1, "type": "voice", "box": null }, { "id": 2, "type": null, "box": 5 } ]`) {
		t.Errorf("得到 %s", got)
	}
	if !strings.HasPrefix(got, `{ "compactRewards": { "__ENUM__"`) || !reflect.DeepEqual(res.Expanded, []string{"rewards"}) {
		t.Errorf("compact 表应原样保留在前：%s", got)
	}
	bad := M("compactRewards", M("__ENUM__", M("type", []any{"a"}), "type", []any{int64(3)}))
	if _, err := mustParse(t).Restore(bad); err == nil {
		t.Error("枚举下标越界应报错")
	}
	uneven := M("compactRewards", M("a", []any{int64(1)}, "b", []any{}))
	if _, err := mustParse(t).Restore(uneven); err == nil {
		t.Error("列长度不一致应报错")
	}
}

func TestUnknownTablesKeptRaw(t *testing.T) {
	got, res := restoreJSON(t, M("newTable", []any{[]any{int64(1)}}))
	if got != `{ "newTable": [ [ 1 ] ] }` || !reflect.DeepEqual(res.Unknown, []string{"newTable"}) {
		t.Errorf("得到 %s，unknown %v", got, res.Unknown)
	}
}
