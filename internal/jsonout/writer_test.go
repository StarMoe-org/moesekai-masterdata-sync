package jsonout

import (
	"math"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// 期望值来自 Python 3：json.dumps(v)。
func TestFloatMatchesPython(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0.0, "0.0"}, {1.0, "1.0"}, {-1.0, "-1.0"}, {0.1, "0.1"}, {1e-05, "1e-05"}, {0.0001, "0.0001"},
		{1.5e-07, "1.5e-07"}, {123456789012345.0, "123456789012345.0"}, {1234567890123456.0, "1234567890123456.0"},
		{1e+16, "1e+16"}, {1.7976931348623157e+308, "1.7976931348623157e+308"}, {5e-324, "5e-324"},
		{60.0, "60.0"}, {2.5, "2.5"}, {math.Copysign(0, -1), "-0.0"},
		{float64(float32(8.989)), "8.98900032043457"}, {float64(float32(1.091)), "1.090999960899353"},
		{float64(float32(0.3)), "0.30000001192092896"}, {float64(float32(1e-8)), "9.99999993922529e-09"},
		{float64(float32(3.4e38)), "3.3999999521443642e+38"},
	}
	for _, c := range cases {
		if got := Float(c.v); got != c.want {
			t.Errorf("Float(%v) = %s，期望 %s", c.v, got, c.want)
		}
	}
}

// 期望值来自 Python 3：json.dumps(v, ensure_ascii=False, indent=2)。
func TestMarshalMatchesPython(t *testing.T) {
	inner := &msgpack.Map{Keys: []any{"b"}, Values: []any{"中\"\\\n\x01\x7f/<>& "}}
	v := &msgpack.Map{
		Keys:   []any{"a", "c"},
		Values: []any{[]any{int64(1), inner, []any{}}, &msgpack.Map{}},
	}
	want := "{\n  \"a\": [\n    1,\n    {\n      \"b\": \"中\\\"\\\\\\n\\u0001\x7f/<>& \"\n    },\n    []\n  ],\n  \"c\": {}\n}"
	got, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("得到\n%s\n期望\n%s", got, want)
	}
}

func TestNonStringKeysAndBinary(t *testing.T) {
	v := &msgpack.Map{Keys: []any{int64(3), true}, Values: []any{[]byte{1, 2}, nil}}
	got, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"3\": \"AQI=\",\n  \"true\": null\n}"
	if string(got) != want {
		t.Errorf("得到 %q", got)
	}
}
