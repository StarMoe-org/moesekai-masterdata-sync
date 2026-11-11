// Package jsonout 把 msgpack 解码出的值写成 JSON，格式与 Haruki 的数据仓库一致：
// 等同于 Python 的 json.dumps(v, indent=2, ensure_ascii=False)，文件末尾不加换行。
//
//   - 两个空格缩进，键值之间是 ": "，元素之间是 ","（换行）；空数组和空对象写成 [] 和 {}；
//   - 非 ASCII 字符不转义，只转义 " \ 和控制字符；
//   - 浮点数用最短表示（Python repr 的规则：整数值写成 1.0，指数小于 -4 或不小于 16 时用 1e-05 这种形式）；
//     float32 先转成 float64 再输出（和上游 Python 解码 msgpack 的结果一致）；
//   - 二进制和扩展类型写成 base64 字符串；非字符串的 map 键转成字符串。
package jsonout

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Marshal 返回 v 的 JSON 文本。
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func indent(b *bytes.Buffer, depth int) {
	b.WriteByte('\n')
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
}

func write(b *bytes.Buffer, v any, depth int) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float32:
		b.WriteString(Float(float64(x)))
	case float64:
		b.WriteString(Float(x))
	case string:
		writeString(b, x)
	case []byte:
		writeString(b, base64.StdEncoding.EncodeToString(x))
	case msgpack.Ext:
		writeString(b, base64.StdEncoding.EncodeToString(x.Data))
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			indent(b, depth+1)
			if err := write(b, e, depth+1); err != nil {
				return err
			}
		}
		indent(b, depth)
		b.WriteByte(']')
	case *msgpack.Map:
		if x.Len() == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i := range x.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			indent(b, depth+1)
			k, err := keyString(x.Keys[i])
			if err != nil {
				return err
			}
			writeString(b, k)
			b.WriteString(": ")
			if err := write(b, x.Values[i], depth+1); err != nil {
				return err
			}
		}
		indent(b, depth)
		b.WriteByte('}')
	default:
		return fmt.Errorf("jsonout: 不支持的类型 %T", v)
	}
	return nil
}

// keyString 按 Python json 的规则把非字符串键转成字符串。
func keyString(k any) (string, error) {
	switch x := k.(type) {
	case string:
		return x, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "null", nil
	case float32:
		return Float(float64(x)), nil
	case float64:
		return Float(x), nil
	}
	return "", fmt.Errorf("jsonout: 不能作为键的类型 %T", k)
}

const hexDigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				if c < 0x20 || c == 0x7f {
					if c == 0x7f { // Python ensure_ascii=False 不转义 DEL
						b.WriteByte(c)
					} else {
						b.WriteString(`\u00`)
						b.WriteByte(hexDigits[c>>4])
						b.WriteByte(hexDigits[c&0xf])
					}
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// 无效 UTF-8：按 Python 解码时的 surrogateescape 以外的默认行为替换成 U+FFFD
			b.WriteString("�")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// Float 按 Python repr(float) 的规则输出。
func Float(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	// 最短的有效数字和十进制指数：d.ddd e±x
	e := strconv.FormatFloat(f, 'e', -1, 64)
	neg := strings.HasPrefix(e, "-")
	if neg {
		e = e[1:]
	}
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)

	var out string
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		out = fmt.Sprintf("%se%s%02d", m, sign, exp)
	} else if exp < 0 {
		out = "0." + strings.Repeat("0", -exp-1) + digits
	} else if len(digits) <= exp+1 {
		out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	} else {
		out = digits[:exp+1] + "." + digits[exp+1:]
	}
	if neg {
		out = "-" + out
	}
	return out
}
