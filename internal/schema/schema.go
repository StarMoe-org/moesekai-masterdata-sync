// Package schema 用 builder 生成的 schema.json（builder 设计文档 §4.4）把 Nuverse 服的 master 还原成
// 和 Haruki 一致的表：
//
//   - 行是 map（[Key("名字")] 的类）：键换成 schema 的字段名（如 receiveRewardEndAt → rewardReceiveEndAt），
//     值按字段类型递归还原，再按键排序；schema 里没有的键原样保留；
//   - 行是数组（[Key(n)] 的类）：按下标还原成对象，字段名取 schema 的 name，值为 null 的字段省略；
//     嵌套的类、类数组、map 递归还原；
//   - compact 表（按列存放）：原样输出，另外展开成一张普通表（compactBondsRewards → bondsRewards）：
//     每个下标一行，列按原顺序，__ENUM__ 里有的列把下标换成取值，null 保留（与 Haruki 一致）。
//
// 还原时检查类型，对不上就报错（结构文件和数据版本不一致时，错位比报错更糟）。行比类多出的尾列
// 丢弃并记告警，和 Haruki 的处理一致（cn 6.4.1 的 cardCostume3ds 就多一列，客户端也不读它）。
package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Schema 是 schema.json。
type Schema struct {
	Version int              `json:"schema"`
	Tables  map[string]Table `json:"tables"`
	Classes map[string]Class `json:"classes"`

	table   string // 正在还原的表（记录多余列用）
	dropped map[string]bool
}

// Table 描述 master 的一个顶层键。
type Table struct {
	Class     string `json:"class"`
	Container string `json:"container"` // array / map / object
	Compact   bool   `json:"compact"`
}

// Class 是一个类的字段表。
type Class struct {
	Fields []Field `json:"fields"`

	intKeyed bool
	byKey    map[int]*Field
	byName   map[string]*Field // 字符串 key → 字段
	maxKey   int
}

// Field 是一个字段。Key 是整数（[Key(n)]）或字符串（[Key("x")]）。
type Field struct {
	Key      any             `json:"key"`
	Name     string          `json:"name"`
	Nullable bool            `json:"nullable"`
	TypeRaw  json.RawMessage `json:"type"`

	typ *Type
}

// Type 是字段类型。
type Type struct {
	Prim  string // int long float double bool string bytes
	Array *Type
	Map   [2]*Type
	Pair  [2]*Type
	Class string
	Enum  string
	Other string // unknown
}

// Parse 解析并检查 schema.json。
func Parse(data []byte) (*Schema, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var s Schema
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("schema.json: %w", err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("schema.json: 不支持的格式版本 %d", s.Version)
	}
	for name, c := range s.Classes {
		c.byKey = map[int]*Field{}
		c.byName = map[string]*Field{}
		ints := 0
		for i := range c.Fields {
			f := &c.Fields[i]
			if n, ok := f.Key.(json.Number); ok {
				k, err := n.Int64()
				if err != nil {
					return nil, fmt.Errorf("schema.json %s.%s: key %v", name, f.Name, f.Key)
				}
				ints++
				c.byKey[int(k)] = f
				c.maxKey = max(c.maxKey, int(k))
			} else if k, ok := f.Key.(string); ok {
				c.byName[k] = f
			}
			t, err := parseType(f.TypeRaw)
			if err != nil {
				return nil, fmt.Errorf("schema.json %s.%s: %w", name, f.Name, err)
			}
			f.typ = t
		}
		if ints > 0 && ints != len(c.Fields) {
			return nil, fmt.Errorf("schema.json %s: 整数和字符串 key 混用", name)
		}
		c.intKeyed = ints > 0
		s.Classes[name] = c
	}
	for name, t := range s.Tables {
		if _, ok := s.Classes[t.Class]; !ok {
			return nil, fmt.Errorf("schema.json: 表 %s 的类 %s 不存在", name, t.Class)
		}
	}
	return &s, nil
}

func parseType(raw json.RawMessage) (*Type, error) {
	var prim string
	if json.Unmarshal(raw, &prim) == nil {
		return &Type{Prim: prim}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("类型 %s 无法解析", raw)
	}
	t := &Type{}
	pair := func(v json.RawMessage) ([2]*Type, error) {
		var two []json.RawMessage
		if err := json.Unmarshal(v, &two); err != nil || len(two) != 2 {
			return [2]*Type{}, fmt.Errorf("类型 %s 应为两个元素", v)
		}
		a, err := parseType(two[0])
		if err != nil {
			return [2]*Type{}, err
		}
		b, err := parseType(two[1])
		return [2]*Type{a, b}, err
	}
	var err error
	switch {
	case obj["array"] != nil:
		t.Array, err = parseType(obj["array"])
	case obj["map"] != nil:
		t.Map, err = pair(obj["map"])
	case obj["pair"] != nil:
		t.Pair, err = pair(obj["pair"])
	case obj["class"] != nil:
		err = json.Unmarshal(obj["class"], &t.Class)
	case obj["enum"] != nil:
		err = json.Unmarshal(obj["enum"], &t.Enum)
	case obj["unknown"] != nil:
		err = json.Unmarshal(obj["unknown"], &t.Other)
	default:
		err = fmt.Errorf("类型 %s 不认识", raw)
	}
	return t, err
}

// Result 是还原一份 master 的结果。
type Result struct {
	Tables   *msgpack.Map // 表名 → 还原后的值，顺序与 master 相同
	Unknown  []string     // schema 里没有、原样输出的顶层键
	Dropped  []string     // 有多余尾列被丢弃的表（每张表只记一次）
	Expanded []string     // 由 compact 表展开出来的表
}

// Restore 还原整个 master（解密后的顶层 map）。
func (s *Schema) Restore(master any) (*Result, error) {
	top, ok := master.(*msgpack.Map)
	if !ok {
		return nil, fmt.Errorf("master 顶层不是 map（%T）", master)
	}
	res := &Result{Tables: &msgpack.Map{}}
	s.dropped = map[string]bool{}
	for i, k := range top.Keys {
		name, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("master 顶层键不是字符串：%v", k)
		}
		v := top.Values[i]
		t, ok := s.Tables[name]
		if !ok {
			res.Unknown = append(res.Unknown, name)
		} else if !t.Compact {
			r, err := s.restoreTable(name, t, v)
			if err != nil {
				return nil, err
			}
			v = r
		}
		if s.dropped[name] {
			res.Dropped = append(res.Dropped, name)
		}
		res.Tables.Keys = append(res.Tables.Keys, name)
		res.Tables.Values = append(res.Tables.Values, v)
		if ok && t.Compact {
			expName, rows, err := expandCompact(name, v)
			if err != nil {
				return nil, err
			}
			if _, exists := top.Get(expName); exists {
				return nil, fmt.Errorf("compact 表 %s 展开后的 %s 和已有的表重名", name, expName)
			}
			res.Tables.Keys = append(res.Tables.Keys, expName)
			res.Tables.Values = append(res.Tables.Values, rows)
			res.Expanded = append(res.Expanded, expName)
		}
	}
	sort.Strings(res.Unknown)
	return res, nil
}

func (s *Schema) restoreTable(name string, t Table, v any) (any, error) {
	elem := &Type{Class: t.Class}
	var typ *Type
	switch t.Container {
	case "array":
		typ = &Type{Array: elem}
	case "map":
		typ = &Type{Map: [2]*Type{{Prim: "any"}, elem}}
	case "object":
		typ = elem
	default:
		return nil, fmt.Errorf("表 %s：不认识的 container %q", name, t.Container)
	}
	s.table = name
	out, err := s.restore(v, typ, name)
	if err != nil {
		return nil, fmt.Errorf("表 %s：%w", name, err)
	}
	return out, nil
}

// restore 按类型还原一个值；path 用于错误信息。
func (s *Schema) restore(v any, t *Type, path string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch {
	case t.Array != nil:
		a, ok := v.([]any)
		if !ok {
			return nil, mismatch(path, "数组", v)
		}
		out := make([]any, len(a))
		for i, e := range a {
			r, err := s.restore(e, t.Array, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case t.Map[1] != nil:
		m, ok := v.(*msgpack.Map)
		if !ok {
			return nil, mismatch(path, "map", v)
		}
		out := &msgpack.Map{Keys: m.Keys, Values: make([]any, len(m.Values))}
		for i, e := range m.Values {
			r, err := s.restore(e, t.Map[1], fmt.Sprintf("%s[%v]", path, m.Keys[i]))
			if err != nil {
				return nil, err
			}
			out.Values[i] = r
		}
		return out, nil
	case t.Class != "":
		return s.restoreObject(v, t.Class, path)
	case t.Prim != "":
		if err := checkPrim(v, t.Prim); err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		return v, nil
	}
	// pair / enum / unknown：原样输出
	return v, nil
}

func (s *Schema) restoreObject(v any, className, path string) (any, error) {
	c, ok := s.Classes[className]
	if !ok {
		return nil, fmt.Errorf("%s：类 %s 不存在", path, className)
	}
	switch x := v.(type) {
	case *msgpack.Map:
		if c.intKeyed {
			return nil, fmt.Errorf("%s：类 %s 按下标序列化，数据却是 map", path, className)
		}
		out := &msgpack.Map{Keys: make([]any, x.Len()), Values: make([]any, x.Len())}
		for i, k := range x.Keys {
			out.Keys[i], out.Values[i] = k, x.Values[i]
			name, ok := k.(string)
			if !ok {
				continue
			}
			f, ok := c.byName[name]
			if !ok {
				continue // schema 里没有：原样保留
			}
			r, err := s.restore(x.Values[i], f.typ, path+"."+f.Name)
			if err != nil {
				return nil, err
			}
			out.Keys[i], out.Values[i] = f.Name, r
		}
		return sortedByKey(out), nil
	case []any:
		if !c.intKeyed {
			return nil, fmt.Errorf("%s：类 %s 按名字序列化，数据却是数组", path, className)
		}
		if len(x) > c.maxKey+1 {
			s.dropped[s.table] = true
			x = x[:c.maxKey+1]
		}
		out := &msgpack.Map{}
		for i, e := range x {
			f, ok := c.byKey[i]
			if !ok || e == nil {
				continue // 空位（客户端类里没有这个下标）或 null：省略
			}
			r, err := s.restore(e, f.typ, path+"."+f.Name)
			if err != nil {
				return nil, err
			}
			out.Keys = append(out.Keys, f.Name)
			out.Values = append(out.Values, r)
		}
		return out, nil
	}
	return nil, mismatch(path, "对象", v)
}

// sortedByKey 返回按字符串键（code point 顺序）排序的副本；有非字符串键时原样返回。
func sortedByKey(m *msgpack.Map) *msgpack.Map {
	idx := make([]int, m.Len())
	for i, k := range m.Keys {
		if _, ok := k.(string); !ok {
			return m
		}
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return m.Keys[idx[a]].(string) < m.Keys[idx[b]].(string) })
	out := &msgpack.Map{Keys: make([]any, len(idx)), Values: make([]any, len(idx))}
	for i, j := range idx {
		out.Keys[i], out.Values[i] = m.Keys[j], m.Values[j]
	}
	return out
}

func checkPrim(v any, prim string) error {
	ok := true
	switch prim {
	case "int", "long":
		switch v.(type) {
		case int64, uint64:
		default:
			ok = false
		}
	case "float", "double":
		switch v.(type) {
		case float32, float64, int64, uint64:
		default:
			ok = false
		}
	case "bool":
		_, ok = v.(bool)
	case "string":
		_, ok = v.(string)
	case "bytes":
		_, ok = v.([]byte)
	}
	if !ok {
		return fmt.Errorf("应为 %s，实际是 %T", prim, v)
	}
	return nil
}

func mismatch(path, want string, v any) error {
	return fmt.Errorf("%s：应为%s，实际是 %T", path, want, v)
}

// expandCompact 把按列存放的 compact 表展开成行。
func expandCompact(name string, v any) (string, []any, error) {
	rest, ok := strings.CutPrefix(name, "compact")
	if !ok || rest == "" {
		return "", nil, fmt.Errorf("compact 表名 %s 不以 compact 开头", name)
	}
	expName := strings.ToLower(rest[:1]) + rest[1:]
	m, ok := v.(*msgpack.Map)
	if !ok {
		return "", nil, fmt.Errorf("compact 表 %s 不是 map（%T）", name, v)
	}
	enums := map[string][]any{}
	var cols []string
	var data [][]any
	n := -1
	for i, k := range m.Keys {
		col, ok := k.(string)
		if !ok {
			return "", nil, fmt.Errorf("compact 表 %s 有非字符串列名 %v", name, k)
		}
		if col == "__ENUM__" {
			em, ok := m.Values[i].(*msgpack.Map)
			if !ok && m.Values[i] != nil {
				return "", nil, fmt.Errorf("compact 表 %s 的 __ENUM__ 不是 map", name)
			}
			if em != nil {
				for j, ek := range em.Keys {
					arr, ok := em.Values[j].([]any)
					es, ok2 := ek.(string)
					if !ok || !ok2 {
						return "", nil, fmt.Errorf("compact 表 %s 的 __ENUM__.%v 不是数组", name, ek)
					}
					enums[es] = arr
				}
			}
			continue
		}
		arr, ok := m.Values[i].([]any)
		if !ok {
			if m.Values[i] == nil {
				arr = nil
			} else {
				return "", nil, fmt.Errorf("compact 表 %s 的列 %s 不是数组", name, col)
			}
		}
		if n >= 0 && len(arr) != n {
			return "", nil, fmt.Errorf("compact 表 %s 的列 %s 有 %d 行，其他列 %d 行", name, col, len(arr), n)
		}
		n = len(arr)
		cols = append(cols, col)
		data = append(data, arr)
	}
	rows := make([]any, max(n, 0))
	for r := range rows {
		row := &msgpack.Map{Keys: make([]any, len(cols)), Values: make([]any, len(cols))}
		for c, col := range cols {
			val := data[c][r]
			if values, ok := enums[col]; ok && val != nil {
				idx, ok := val.(int64)
				if !ok || idx < 0 || idx >= int64(len(values)) {
					return "", nil, fmt.Errorf("compact 表 %s 第 %d 行 %s 的枚举下标 %v 超出范围", name, r, col, val)
				}
				val = values[idx]
			}
			row.Keys[c], row.Values[c] = col, val
		}
		rows[r] = row
	}
	return expName, rows, nil
}
