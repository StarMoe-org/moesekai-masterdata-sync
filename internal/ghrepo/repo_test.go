package ghrepo

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
)

// fakeGitHub 是 Git Data API 的最小模拟：树按「完整路径 → 条目」扁平存放。
type fakeGitHub struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	trees   map[string]map[string]Entry
	commits map[string][2]string // sha → {tree, parent}
	ref     string
	calls   map[string]int // "METHOD 路径前缀" → 次数
	corrupt bool           // 上传后返回错误的对象名
	n       int
}

func newFake() *fakeGitHub {
	return &fakeGitHub{blobs: map[string][]byte{}, trees: map[string]map[string]Entry{},
		commits: map[string][2]string{}, calls: map[string]int{}}
}

func (f *fakeGitHub) addBlob(data []byte) string {
	sha := gitrepo.BlobHash(data)
	f.blobs[sha] = append([]byte(nil), data...)
	if f.corrupt {
		return strings.Repeat("0", 40)
	}
	return sha
}

func (f *fakeGitHub) addTree(files map[string]Entry) string {
	keys := make([]string, 0, len(files))
	for p := range files {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	h := sha1.New()
	for _, p := range keys {
		fmt.Fprintf(h, "%s %s %s\n", files[p].Mode, files[p].Hash, p)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	f.trees[sha] = files
	return sha
}

func (f *fakeGitHub) addCommit(tree, parent string) string {
	f.n++
	sha := fmt.Sprintf("%040x", f.n)
	f.commits[sha] = [2]string{tree, parent}
	return sha
}

// seed 直接在远端写一个提交（模拟别人推送）。
func (f *fakeGitHub) seed(files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parentFiles map[string]Entry
	if f.ref != "" {
		parentFiles = f.trees[f.commits[f.ref][0]]
	}
	tree := map[string]Entry{}
	for p, e := range parentFiles {
		tree[p] = e
	}
	for p, content := range files {
		tree[p] = Entry{"100644", f.addBlob([]byte(content))}
	}
	f.ref = f.addCommit(f.addTree(tree), f.ref)
	return f.ref
}

// content 返回分支当前的全部文件内容。
func (f *fakeGitHub) content() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for p, e := range f.trees[f.commits[f.ref][0]] {
		out[p] = string(f.blobs[e.Hash])
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/")
	if !ok || r.Header.Get("Authorization") != "Bearer tok" {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	parts := strings.SplitN(path, "/", 3)
	f.calls[r.Method+" "+parts[0]+"/"+parts[1]]++
	fail := func(status int, msg string) { writeJSON(w, status, map[string]string{"message": msg}) }
	switch {
	case r.Method == "GET" && path == "git/ref/heads/main":
		if f.ref == "" {
			fail(409, "Git Repository is empty.")
			return
		}
		writeJSON(w, 200, map[string]any{"object": map[string]string{"sha": f.ref, "type": "commit"}})
	case r.Method == "GET" && parts[1] == "commits":
		c, ok := f.commits[parts[2]]
		if !ok {
			fail(404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"sha": parts[2], "tree": map[string]string{"sha": c[0]}})
	case r.Method == "GET" && parts[1] == "trees":
		t, ok := f.trees[parts[2]]
		if !ok || r.URL.Query().Get("recursive") != "1" {
			fail(404, "Not Found")
			return
		}
		var list []map[string]string
		dirs := map[string]bool{}
		for p, e := range t {
			list = append(list, map[string]string{"path": p, "mode": e.Mode, "type": "blob", "sha": e.Hash})
			if d, _, ok := strings.Cut(p, "/"); ok && !dirs[d] {
				dirs[d] = true
				list = append(list, map[string]string{"path": d, "mode": "040000", "type": "tree", "sha": "dir"})
			}
		}
		writeJSON(w, 200, map[string]any{"sha": parts[2], "tree": list, "truncated": false})
	case r.Method == "GET" && parts[1] == "blobs":
		data, ok := f.blobs[parts[2]]
		if !ok {
			fail(404, "Not Found")
			return
		}
		enc := base64.StdEncoding.EncodeToString(data)
		var lines []string
		for len(enc) > 60 {
			lines, enc = append(lines, enc[:60]), enc[60:]
		}
		writeJSON(w, 200, map[string]string{"content": strings.Join(append(lines, enc), "\n") + "\n", "encoding": "base64"})
	case r.Method == "POST" && path == "git/blobs":
		var in struct{ Content, Encoding string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		data, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil || in.Encoding != "base64" {
			fail(422, "bad blob")
			return
		}
		writeJSON(w, 201, map[string]string{"sha": f.addBlob(data)})
	case r.Method == "POST" && path == "git/trees":
		var in struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path, Mode, Type string
				SHA              *string `json:"sha"`
				Content          *string `json:"content"`
			} `json:"tree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		files := map[string]Entry{}
		if in.BaseTree != "" {
			base, ok := f.trees[in.BaseTree]
			if !ok {
				fail(422, "base_tree not found")
				return
			}
			for p, e := range base {
				files[p] = e
			}
		}
		var out []map[string]string
		for _, it := range in.Tree {
			switch {
			case it.Content != nil:
				files[it.Path] = Entry{it.Mode, f.addBlob([]byte(*it.Content))}
			case it.SHA == nil:
				if _, ok := files[it.Path]; !ok {
					fail(422, "path not in base tree")
					return
				}
				delete(files, it.Path)
				continue
			default:
				if _, ok := f.blobs[*it.SHA]; !ok {
					fail(422, "sha not found")
					return
				}
				files[it.Path] = Entry{it.Mode, *it.SHA}
			}
			out = append(out, map[string]string{"path": it.Path, "sha": files[it.Path].Hash})
		}
		writeJSON(w, 201, map[string]any{"sha": f.addTree(files), "tree": out})
	case r.Method == "POST" && path == "git/commits":
		var in struct {
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
			Message string   `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := f.trees[in.Tree]; !ok || len(in.Parents) != 1 || in.Message == "" {
			fail(422, "bad commit")
			return
		}
		writeJSON(w, 201, map[string]string{"sha": f.addCommit(in.Tree, in.Parents[0])})
	case r.Method == "PATCH" && path == "git/refs/heads/main":
		var in struct {
			SHA   string `json:"sha"`
			Force bool   `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c, ok := f.commits[in.SHA]
		if !ok {
			fail(422, "Object does not exist")
			return
		}
		if !in.Force && c[1] != f.ref {
			fail(422, "Update is not a fast forward")
			return
		}
		f.ref = in.SHA
		writeJSON(w, 200, map[string]any{"object": map[string]string{"sha": f.ref}})
	default:
		fail(404, "Not Found")
	}
}

func setup(t *testing.T) (*fakeGitHub, *Repo) {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	r := &Repo{Owner: "o", Name: "r", APIBase: srv.URL, HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return "tok", nil }}
	return f, r
}

func mustPut(t *testing.T, cs *Changeset, path, data string) bool {
	t.Helper()
	changed, err := cs.Put(path, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestPublishFlow(t *testing.T) {
	ctx := context.Background()
	f, r := setup(t)
	f.seed(map[string]string{"master/a.json": "[1]\n", "master/b.json": "[2]\n", "versions/current_version.json": `{"v":1}`})

	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.Files()) != 3 {
		t.Fatalf("文件数 %d，目录条目应被跳过", len(r.Files()))
	}
	v, err := r.ReadFile(ctx, "versions/current_version.json")
	if err != nil || string(v) != `{"v":1}` {
		t.Fatalf("ReadFile = %q, %v", v, err)
	}
	if v, err := r.ReadFile(ctx, "nope"); v != nil || err != nil {
		t.Fatalf("不存在的文件 = %q, %v", v, err)
	}

	cs := r.NewChangeset()
	if mustPut(t, cs, "master/a.json", "[1]\n") {
		t.Fatal("没变的文件报告为有变化")
	}
	if !mustPut(t, cs, "master/b.json", "[2,3]\n") || !mustPut(t, cs, "master/c.json", "[4]\n") {
		t.Fatal("变化的文件报告为没变")
	}
	if !mustPut(t, cs, "master/d.json", "[1]\n") { // 内容和远端已有的 a 一样：不用上传
		t.Fatal("新文件报告为没变")
	}
	mustPut(t, cs, "versions/current_version.json", `{"v":2}`)
	cs.Delete("master/zzz.json") // 不存在：忽略
	commit, err := cs.Commit(ctx, "second")
	if err != nil || commit == "" {
		t.Fatalf("Commit = %q, %v", commit, err)
	}
	want := map[string]string{"master/a.json": "[1]\n", "master/b.json": "[2,3]\n", "master/c.json": "[4]\n",
		"master/d.json": "[1]\n", "versions/current_version.json": `{"v":2}`}
	if got := f.content(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("远端内容\n got %v\nwant %v", got, want)
	}
	if f.calls["POST git/blobs"] != 0 || f.calls["POST git/trees"] != 2 {
		t.Fatalf("请求次数 %v：应一次内联上传 + 一次建树", f.calls)
	}

	// 删除；提交没变时 Fetch 不重新读树
	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls["GET git/trees"] != 1 {
		t.Fatalf("head 没变却重新读了树：%v", f.calls)
	}
	cs = r.NewChangeset()
	cs.Delete("master/d.json")
	if _, err := cs.Commit(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.content()["master/d.json"]; ok {
		t.Fatal("没有删掉 master/d.json")
	}

	// 什么都没变：不提交
	cs = r.NewChangeset()
	mustPut(t, cs, "master/a.json", "[1]\n")
	if c, err := cs.Commit(ctx, "noop"); c != "" || err != nil {
		t.Fatalf("noop Commit = %q, %v", c, err)
	}
}

func TestBatches(t *testing.T) {
	ctx := context.Background()
	f, r := setup(t)
	f.seed(map[string]string{"x": "0"})
	r.BatchBytes = 10
	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	cs := r.NewChangeset()
	want := map[string]string{"x": "0"}
	for i := range 7 {
		p, data := fmt.Sprintf("master/%d.json", i), fmt.Sprintf("[%d,%d]", i, i)
		mustPut(t, cs, p, data)
		want[p] = data
	}
	mustPut(t, cs, "big.json", "0123456789abcdef") // 超过一批的上限：单独上传
	mustPut(t, cs, "bin", "\xff\xfe")              // 不是 UTF-8：单独上传
	want["big.json"], want["bin"] = "0123456789abcdef", "\xff\xfe"
	if _, err := cs.Commit(ctx, "batches"); err != nil {
		t.Fatal(err)
	}
	if got := f.content(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("远端内容\n got %v\nwant %v", got, want)
	}
	// 7 个 5 字节的文件、每批 10 字节 → 4 批，加最后建树
	if f.calls["POST git/blobs"] != 2 || f.calls["POST git/trees"] != 5 {
		t.Fatalf("请求次数 %v", f.calls)
	}
}

func TestStale(t *testing.T) {
	ctx := context.Background()
	f, r := setup(t)
	f.seed(map[string]string{"a": "1", "b": "1"})
	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	cs := r.NewChangeset()
	mustPut(t, cs, "a", "2")
	mustPut(t, cs, "b", "1") // 当时没变、没上传
	f.seed(map[string]string{"b": "9", "c": "3"})
	if _, err := cs.Commit(ctx, "x"); !errors.Is(err, ErrStale) {
		t.Fatalf("远端前进后 Commit = %v，应为 ErrStale", err)
	}
	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Commit(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "2", "b": "1", "c": "3"}
	if got := f.content(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("远端内容\n got %v\nwant %v", got, want)
	}
}

func TestEmptyAndCorrupt(t *testing.T) {
	ctx := context.Background()
	f, r := setup(t)
	if err := r.Fetch(ctx); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空仓库 Fetch = %v", err)
	}
	f.seed(map[string]string{"a": "1"})
	if err := r.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	f.corrupt = true
	cs := r.NewChangeset()
	mustPut(t, cs, "a", "2")
	if _, err := cs.Commit(ctx, "x"); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("对象名不一致时 Commit = %v", err)
	}
	if got := f.content()["a"]; got != "1" {
		t.Fatalf("出错后远端被改成了 %q", got)
	}
}
