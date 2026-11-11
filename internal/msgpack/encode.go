package msgpack

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Encode 编码 Decode 能产生的那些类型（另外接受 int）。主要给测试构造数据用。
func Encode(v any) ([]byte, error) {
	var b []byte
	return appendValue(b, v)
}

func appendValue(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, 0xc0), nil
	case bool:
		if x {
			return append(b, 0xc3), nil
		}
		return append(b, 0xc2), nil
	case int:
		return appendInt(b, int64(x)), nil
	case int64:
		return appendInt(b, x), nil
	case uint64:
		return binary.BigEndian.AppendUint64(append(b, 0xcf), x), nil
	case float32:
		return binary.BigEndian.AppendUint32(append(b, 0xca), math.Float32bits(x)), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(b, 0xcb), math.Float64bits(x)), nil
	case string:
		b = appendLen(b, len(x), 0xa0, 31, 0xd9)
		return append(b, x...), nil
	case []byte:
		b = binary.BigEndian.AppendUint32(append(b, 0xc6), uint32(len(x)))
		return append(b, x...), nil
	case []any:
		b = appendLen(b, len(x), 0x90, 15, 0xdc)
		var err error
		for _, e := range x {
			if b, err = appendValue(b, e); err != nil {
				return nil, err
			}
		}
		return b, nil
	case *Map:
		b = appendLen(b, x.Len(), 0x80, 15, 0xde)
		var err error
		for i := range x.Keys {
			if b, err = appendValue(b, x.Keys[i]); err != nil {
				return nil, err
			}
			if b, err = appendValue(b, x.Values[i]); err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	return nil, fmt.Errorf("msgpack: 不能编码 %T", v)
}

func appendInt(b []byte, x int64) []byte {
	switch {
	case x >= 0 && x <= 0x7f:
		return append(b, byte(x))
	case x < 0 && x >= -32:
		return append(b, byte(x))
	default:
		return binary.BigEndian.AppendUint64(append(b, 0xd3), uint64(x))
	}
}

// appendLen 写长度头：fix 形式放得下就用 fix，否则用 16/32 位（str 用 8/16/32 位）。
func appendLen(b []byte, n int, fix byte, fixMax int, base byte) []byte {
	if n <= fixMax {
		return append(b, fix|byte(n))
	}
	if base == 0xd9 { // str8/16/32
		switch {
		case n <= 0xff:
			return append(b, 0xd9, byte(n))
		case n <= 0xffff:
			return binary.BigEndian.AppendUint16(append(b, 0xda), uint16(n))
		}
		return binary.BigEndian.AppendUint32(append(b, 0xdb), uint32(n))
	}
	if n <= 0xffff {
		return binary.BigEndian.AppendUint16(append(b, base), uint16(n))
	}
	return binary.BigEndian.AppendUint32(append(b, base+1), uint32(n))
}

// M 方便测试里按顺序构造 map：M("a", 1, "b", 2)。
func M(kv ...any) *Map {
	m := &Map{}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Keys = append(m.Keys, kv[i])
		m.Values = append(m.Values, kv[i+1])
	}
	return m
}
