package gitrepo

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BlobHash 是 git 给这段内容算的对象名：sha1("blob <长度>\0" + 内容)。
func BlobHash(data []byte) string {
	return hashObject("blob", data)
}

func hashObject(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// writeLoose 把对象写成松散对象（已存在就跳过）。只写本服务新产生的对象，数量很少，不需要打包。
func writeLoose(gitDir, kind string, data []byte) (string, error) {
	sha := hashObject(kind, data)
	path := filepath.Join(gitDir, "objects", sha[:2], sha[2:])
	if _, err := os.Stat(path); err == nil {
		return sha, nil
	}
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	fmt.Fprintf(zw, "%s %d\x00", kind, len(data))
	zw.Write(data)
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp_obj_*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", err
	}
	return sha, os.Rename(tmp.Name(), path)
}

// Entry 是树里的一个文件（含子模块等非 blob 条目，原样保留）。
type Entry struct {
	Mode string // 100644 / 100755 / 120000 / 160000
	Hash string
}

// buildTree 按扁平的「路径 → 条目」写出整棵树，返回根树的对象名。
// 只有内容变化的子树会产生新对象；没变的子树算出来的对象名和原来一样，写入时直接跳过。
func buildTree(gitDir string, files map[string]Entry) (string, error) {
	type node struct {
		blobs map[string]Entry
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{blobs: map[string]Entry{}, dirs: map[string]*node{}} }
	root := newNode()
	for path, e := range files {
		if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
			return "", fmt.Errorf("gitrepo: 非法路径 %q", path)
		}
		parts := strings.Split(path, "/")
		n := root
		for _, p := range parts[:len(parts)-1] {
			if p == "" || p == "." || p == ".." {
				return "", fmt.Errorf("gitrepo: 非法路径 %q", path)
			}
			if _, clash := n.blobs[p]; clash {
				return "", fmt.Errorf("gitrepo: %q 既是文件又是目录", path)
			}
			if n.dirs[p] == nil {
				n.dirs[p] = newNode()
			}
			n = n.dirs[p]
		}
		name := parts[len(parts)-1]
		if _, clash := n.dirs[name]; clash {
			return "", fmt.Errorf("gitrepo: %q 既是文件又是目录", path)
		}
		n.blobs[name] = e
	}
	var write func(n *node) (string, error)
	write = func(n *node) (string, error) {
		type item struct {
			name, sortKey, mode, hash string
		}
		var items []item
		for name, e := range n.blobs {
			items = append(items, item{name, name, e.Mode, e.Hash})
		}
		for name, d := range n.dirs {
			h, err := write(d)
			if err != nil {
				return "", err
			}
			items = append(items, item{name, name + "/", "40000", h}) // git 排序时目录名后面算一个 /
		}
		sort.Slice(items, func(i, j int) bool { return items[i].sortKey < items[j].sortKey })
		var buf bytes.Buffer
		for _, it := range items {
			raw, err := hex.DecodeString(it.hash)
			if err != nil || len(raw) != sha1.Size {
				return "", fmt.Errorf("gitrepo: %s 的对象名 %q 无效", it.name, it.hash)
			}
			fmt.Fprintf(&buf, "%s %s\x00", it.mode, it.name)
			buf.Write(raw)
		}
		return writeLoose(gitDir, "tree", buf.Bytes())
	}
	return write(root)
}
