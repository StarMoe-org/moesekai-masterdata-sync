package msgpack

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestDecodeTypes(t *testing.T) {
	cases := []struct {
		in   []byte
		want any
	}{
		{[]byte{0x05}, int64(5)},
		{[]byte{0xff}, int64(-1)},
		{[]byte{0xcc, 0xc8}, int64(200)},
		{[]byte{0xcd, 0x01, 0x00}, int64(256)},
		{[]byte{0xd0, 0x80}, int64(-128)},
		{[]byte{0xd3, 0x80, 0, 0, 0, 0, 0, 0, 0}, int64(math.MinInt64)},
		{[]byte{0xcf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint64(math.MaxUint64)},
		{[]byte{0xc0}, nil},
		{[]byte{0xc3}, true},
		{[]byte{0xca, 0x3f, 0x80, 0, 0}, float32(1)},
		{[]byte{0xcb, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0}, float64(1)},
		{[]byte{0xa3, 'a', 'b', 'c'}, "abc"},
		{[]byte{0xd9, 0x02, 0xe4, 0xb8}, "\xe4\xb8"},
		{[]byte{0xc4, 0x02, 1, 2}, []byte{1, 2}},
		{[]byte{0x92, 0x01, 0xc0}, []any{int64(1), nil}},
		{[]byte{0xd4, 0x05, 0x09}, Ext{Type: 5, Data: []byte{9}}},
	}
	for _, c := range cases {
		got, err := Decode(c.in)
		if err != nil {
			t.Errorf("% x：%v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("% x：得到 %#v，期望 %#v", c.in, got, c.want)
		}
	}
}

func TestMapKeepsOrder(t *testing.T) {
	// {"b":1,"a":2,3:"x"}
	got, err := Decode([]byte{0x83, 0xa1, 'b', 0x01, 0xa1, 'a', 0x02, 0x03, 0xa1, 'x'})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(*Map)
	if !reflect.DeepEqual(m.Keys, []any{"b", "a", int64(3)}) {
		t.Errorf("键顺序 %v", m.Keys)
	}
	if v, ok := m.Get("a"); !ok || v != int64(2) {
		t.Errorf("Get(a) = %v", v)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, in := range [][]byte{
		{},                             // 空
		{0x92, 0x01},                   // 数组缺元素
		{0xdd, 0xff, 0xff, 0xff, 0xff}, // 长度远超数据
		{0xa5, 'a'},                    // 字符串不完整
		{0x01, 0x02},                   // 多余字节
		{0xc1},                         // 保留类型
	} {
		if _, err := Decode(in); err == nil {
			t.Errorf("% x：应报错", in)
		}
	}
	if _, err := Decode([]byte{0x92, 0x01}); !errors.Is(err, ErrTruncated) {
		t.Errorf("不完整的数据应返回 ErrTruncated，得到 %v", err)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	v := M("s", "中文", "n", int64(-5), "big", int64(1<<40), "f", float64(1.5), "a", []any{nil, true, []byte{1}},
		"long", string(make([]byte, 300)), "m", M(int64(1), "x"))
	b, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, v) {
		t.Errorf("往返不一致：%#v", got)
	}
}
