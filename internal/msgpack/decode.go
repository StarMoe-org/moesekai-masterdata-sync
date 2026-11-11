// Package msgpack 解码 MessagePack，保留 map 的键顺序（输出 JSON 时要和上游一致）。
//
// 解码结果的类型：
//
//	nil、bool、int64、uint64、float32、float64、string、[]byte、[]any、*Map、Ext
//
// 只有大于 math.MaxInt64 的无符号整数才会是 uint64，其余整数统一为 int64。
package msgpack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Map 是按原始顺序保存键值对的 map。
type Map struct {
	Keys   []any
	Values []any
}

// Len 返回键值对数量。
func (m *Map) Len() int { return len(m.Keys) }

// Get 按字符串键查找；找不到时 ok 为 false。
func (m *Map) Get(key string) (any, bool) {
	for i, k := range m.Keys {
		if s, ok := k.(string); ok && s == key {
			return m.Values[i], true
		}
	}
	return nil, false
}

// Ext 是 MessagePack 扩展类型（时间戳以外的都原样保留）。
type Ext struct {
	Type int8
	Data []byte
}

// ErrTruncated 表示数据在一个值的中间结束。
var ErrTruncated = errors.New("msgpack: 数据不完整")

// maxDepth 防止恶意数据造成栈溢出。
const maxDepth = 512

// Decode 解码一个完整的值；末尾有多余字节时报错。
func Decode(b []byte) (any, error) {
	d := decoder{b: b}
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	if d.i != len(b) {
		return nil, fmt.Errorf("msgpack: 第 %d 字节之后还有 %d 字节多余数据", d.i, len(b)-d.i)
	}
	return v, nil
}

type decoder struct {
	b []byte
	i int
}

func (d *decoder) need(n int) error {
	if n < 0 || len(d.b)-d.i < n {
		return ErrTruncated
	}
	return nil
}

func (d *decoder) byte1() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	c := d.b[d.i]
	d.i++
	return c, nil
}

func (d *decoder) take(n int) ([]byte, error) {
	if err := d.need(n); err != nil {
		return nil, err
	}
	s := d.b[d.i : d.i+n]
	d.i += n
	return s, nil
}

func (d *decoder) uint(n int) (uint64, error) {
	s, err := d.take(n)
	if err != nil {
		return 0, err
	}
	switch n {
	case 1:
		return uint64(s[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(s)), nil
	case 4:
		return uint64(binary.BigEndian.Uint32(s)), nil
	default:
		return binary.BigEndian.Uint64(s), nil
	}
}

func (d *decoder) length(n int) (int, error) {
	v, err := d.uint(n)
	if err != nil {
		return 0, err
	}
	if v > uint64(len(d.b)) {
		return 0, ErrTruncated
	}
	return int(v), nil
}

func (d *decoder) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("msgpack: 嵌套过深")
	}
	c, err := d.byte1()
	if err != nil {
		return nil, err
	}
	switch {
	case c <= 0x7f:
		return int64(c), nil
	case c >= 0xe0:
		return int64(int8(c)), nil
	case c&0xf0 == 0x80:
		return d.mapN(int(c&0x0f), depth)
	case c&0xf0 == 0x90:
		return d.arrayN(int(c&0x0f), depth)
	case c&0xe0 == 0xa0:
		return d.str(int(c & 0x1f))
	}
	switch c {
	case 0xc0:
		return nil, nil
	case 0xc2:
		return false, nil
	case 0xc3:
		return true, nil
	case 0xc4, 0xc5, 0xc6:
		n, err := d.length(1 << (c - 0xc4))
		if err != nil {
			return nil, err
		}
		s, err := d.take(n)
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), s...), nil
	case 0xc7, 0xc8, 0xc9:
		n, err := d.length(1 << (c - 0xc7))
		if err != nil {
			return nil, err
		}
		return d.ext(n)
	case 0xca:
		v, err := d.uint(4)
		return math.Float32frombits(uint32(v)), err
	case 0xcb:
		v, err := d.uint(8)
		return math.Float64frombits(v), err
	case 0xcc, 0xcd, 0xce, 0xcf:
		v, err := d.uint(1 << (c - 0xcc))
		if err != nil {
			return nil, err
		}
		if v > math.MaxInt64 {
			return v, nil
		}
		return int64(v), nil
	case 0xd0:
		v, err := d.uint(1)
		return int64(int8(v)), err
	case 0xd1:
		v, err := d.uint(2)
		return int64(int16(v)), err
	case 0xd2:
		v, err := d.uint(4)
		return int64(int32(v)), err
	case 0xd3:
		v, err := d.uint(8)
		return int64(v), err
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8:
		return d.ext(1 << (c - 0xd4))
	case 0xd9, 0xda, 0xdb:
		n, err := d.length(1 << (c - 0xd9))
		if err != nil {
			return nil, err
		}
		return d.str(n)
	case 0xdc, 0xdd:
		n, err := d.length(2 << (c - 0xdc))
		if err != nil {
			return nil, err
		}
		return d.arrayN(n, depth)
	case 0xde, 0xdf:
		n, err := d.length(2 << (c - 0xde))
		if err != nil {
			return nil, err
		}
		return d.mapN(n, depth)
	}
	return nil, fmt.Errorf("msgpack: 第 %d 字节是不认识的类型 0x%02x", d.i-1, c)
}

func (d *decoder) str(n int) (any, error) {
	s, err := d.take(n)
	if err != nil {
		return nil, err
	}
	return string(s), nil
}

func (d *decoder) ext(n int) (any, error) {
	t, err := d.byte1()
	if err != nil {
		return nil, err
	}
	s, err := d.take(n)
	if err != nil {
		return nil, err
	}
	return Ext{Type: int8(t), Data: append([]byte(nil), s...)}, nil
}

func (d *decoder) arrayN(n, depth int) (any, error) {
	if err := d.need(n); err != nil { // 每个元素至少 1 字节
		return nil, err
	}
	a := make([]any, n)
	for k := range a {
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		a[k] = v
	}
	return a, nil
}

func (d *decoder) mapN(n, depth int) (any, error) {
	if err := d.need(2 * n); err != nil {
		return nil, err
	}
	m := &Map{Keys: make([]any, n), Values: make([]any, n)}
	for k := 0; k < n; k++ {
		key, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		val, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		m.Keys[k], m.Values[k] = key, val
	}
	return m, nil
}
