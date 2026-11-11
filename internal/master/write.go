// Package master 把还原好的表写成数据仓库的目录结构（master/<表名>.json），格式见设计文档 §7。
package master

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/jsonout"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Sink 接收一个个文件（路径相对仓库根目录，用 /）。返回内容是否有变化。
type Sink interface {
	Put(path string, data []byte) (bool, error)
}

// WriteResult 统计一次写出。
type WriteResult struct {
	Written   []string // 新建或内容变化的表
	Unchanged int
	Produced  map[string]bool // 这次生成的全部表
}

// WriteTables 把每张表写成 master/<名字>.json。写完一张就把它从 tables 里清掉，降低内存峰值。
func WriteTables(sink Sink, tables *msgpack.Map) (*WriteResult, error) {
	res := &WriteResult{Produced: map[string]bool{}}
	for i, k := range tables.Keys {
		name := k.(string)
		if !validName(name) {
			return nil, fmt.Errorf("表名 %q 不能用作文件名", name)
		}
		res.Produced[name] = true
		data, err := jsonout.Marshal(tables.Values[i])
		if err != nil {
			return nil, fmt.Errorf("表 %s：%w", name, err)
		}
		tables.Values[i] = nil
		changed, err := sink.Put("master/"+name+".json", data)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Written = append(res.Written, name)
		} else {
			res.Unchanged++
		}
	}
	return res, nil
}

// TablesIn 从文件路径列表里挑出 master/<名字>.json 的表名。
func TablesIn(paths []string) []string {
	var out []string
	for _, p := range paths {
		rest, ok := strings.CutPrefix(p, "master/")
		if !ok || strings.Contains(rest, "/") {
			continue
		}
		if name, ok := strings.CutSuffix(rest, ".json"); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`+"\x00")
}

// DirSink 写到本地目录（离线调试用）。
type DirSink string

// Put 内容不同才写：先写临时文件再改名。
func (d DirSink) Put(path string, data []byte) (bool, error) {
	full := filepath.Join(string(d), filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	return WriteFileIfChanged(full, data)
}

// WriteFileIfChanged 内容不同才写：先写临时文件再改名。返回是否写了。
func WriteFileIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}
