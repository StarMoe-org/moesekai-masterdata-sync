package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRemote 建一个允许部分克隆的本地裸仓库，返回 file:// 地址和目录。
func newRemote(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "init", "--bare", "-q", "-b", "main", dir)
	run(t, dir, "config", "uploadpack.allowFilter", "true")
	run(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	return "file://" + dir, dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return string(out)
}

func newRepo(t *testing.T, url string) *Repo {
	t.Helper()
	r := &Repo{Dir: filepath.Join(t.TempDir(), "work"), URL: url, Author: Identity{"bot", "bot@example.com"}}
	if err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func put(t *testing.T, cs *Changeset, path, data string) bool {
	t.Helper()
	changed, err := cs.Put(path, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// 本地有没有这个对象（不触发按需下载）。
func hasObject(r *Repo, sha string) bool {
	_, err := r.git(context.Background(), nil, "cat-file", "-e", sha)
	return err == nil
}

func TestPublishFlow(t *testing.T) {
	ctx := context.Background()
	url, remote := newRemote(t)

	// 1. 空仓库：第一次提交
	a := newRepo(t, url)
	if a.Head() != "" || len(a.Files()) != 0 {
		t.Fatalf("空仓库应没有 HEAD：%q %v", a.Head(), a.Files())
	}
	cs := a.NewChangeset()
	// 这几个名字覆盖 git 树的排序规则：目录按「名字/」比较（'-' < '.' < '/' < '0'）
	for _, p := range []string{"a-b", "a.txt", "a/b", "a0", "master/x.json", "master/y.json", "versions/current_version.json"} {
		put(t, cs, p, "content of "+p)
	}
	c1, err := cs.Commit(ctx, "first")
	if err != nil || c1 == "" {
		t.Fatalf("第一次提交：%q %v", c1, err)
	}
	run(t, remote, "fsck", "--strict", "--no-dangling")
	if got := run(t, remote, "show", "main:master/x.json"); got != "content of master/x.json" {
		t.Errorf("远端内容 = %q", got)
	}
	if msg := strings.TrimSpace(run(t, remote, "log", "-1", "--format=%an <%ae>|%cn|%s", "main")); msg != "bot <bot@example.com>|bot|first" {
		t.Errorf("提交信息 = %q", msg)
	}

	// 2. 新的副本：只有树，没有文件内容
	b := newRepo(t, url)
	if b.Head() != c1 || len(b.Files()) != 7 {
		t.Fatalf("拉取：%q %d", b.Head(), len(b.Files()))
	}
	if hasObject(b, b.Files()["master/x.json"].Hash) {
		t.Error("部分克隆不应拿到文件内容")
	}
	// 读一个小文件：只按需下载这一个
	data, err := b.ReadFile(ctx, "versions/current_version.json")
	if err != nil || string(data) != "content of versions/current_version.json" {
		t.Fatalf("ReadFile = %q %v", data, err)
	}
	if hasObject(b, b.Files()["master/y.json"].Hash) {
		t.Error("ReadFile 不应下载别的文件")
	}
	if data, err := b.ReadFile(ctx, "nope"); data != nil || err != nil {
		t.Errorf("不存在的文件：%q %v", data, err)
	}

	// 3. 内容不变：不写对象、不提交
	cs = b.NewChangeset()
	if put(t, cs, "master/x.json", "content of master/x.json") {
		t.Error("内容相同应返回 false")
	}
	if c, err := cs.Commit(ctx, "noop"); c != "" || err != nil {
		t.Fatalf("没有变化不应提交：%q %v", c, err)
	}

	// 4. 改一个、删一个、加一个：只写新内容
	cs = b.NewChangeset()
	put(t, cs, "master/x.json", "content of master/x.json")
	if !put(t, cs, "master/y.json", "y v2") || !put(t, cs, "master/z.json", "z") {
		t.Error("内容变化应返回 true")
	}
	cs.Delete("a/b")
	if cs.Changed() != 2 {
		t.Errorf("Changed = %d", cs.Changed())
	}
	c2, err := cs.Commit(ctx, "second")
	if err != nil || c2 == "" {
		t.Fatalf("第二次提交：%q %v", c2, err)
	}
	if hasObject(b, b.Files()["master/x.json"].Hash) {
		t.Error("没变的文件不应被下载")
	}
	run(t, remote, "fsck", "--strict", "--no-dangling")
	files := run(t, remote, "ls-tree", "-r", "--name-only", "main")
	if files != "a-b\na.txt\na0\nmaster/x.json\nmaster/y.json\nmaster/z.json\nversions/current_version.json\n" {
		t.Errorf("远端文件 = %q", files)
	}
	if got := run(t, remote, "show", "main:master/y.json"); got != "y v2" {
		t.Errorf("y = %q", got)
	}
	if parent := strings.TrimSpace(run(t, remote, "rev-parse", "main^")); parent != c1 {
		t.Errorf("父提交 = %s，应为 %s", parent, c1)
	}

	// 5. a 的副本已经过期：推送被拒 → 重新拉取 → 在新 HEAD 上重算
	cs = a.NewChangeset()
	put(t, cs, "master/w.json", "w")
	if _, err := cs.Commit(ctx, "third"); !errors.Is(err, ErrStale) {
		t.Fatalf("过期的副本推送应返回 ErrStale：%v", err)
	}
	if err := a.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	c3, err := cs.Commit(ctx, "third")
	if err != nil || c3 == "" {
		t.Fatalf("重试：%q %v", c3, err)
	}
	if got := run(t, remote, "show", "main:master/y.json"); got != "y v2" {
		t.Errorf("重试不应覆盖别人的修改：y = %q", got)
	}
	run(t, remote, "show", "main:master/w.json")
	run(t, remote, "fsck", "--strict", "--no-dangling")
}

// 推送被拒后，当初因为「没变」没写对象的文件在远端又被改了：必须报错，不能写进一个不存在的对象。
func TestStaleUnwrittenBlob(t *testing.T) {
	ctx := context.Background()
	url, _ := newRemote(t)
	a := newRepo(t, url)
	cs := a.NewChangeset()
	put(t, cs, "f", "v1")
	if _, err := cs.Commit(ctx, "init"); err != nil {
		t.Fatal(err)
	}

	b := newRepo(t, url)
	stale := b.NewChangeset()
	put(t, stale, "f", "v1") // 对 b 来说没变，不写对象
	put(t, stale, "g", "new")

	cs = a.NewChangeset()
	put(t, cs, "f", "v2")
	if _, err := cs.Commit(ctx, "change f"); err != nil {
		t.Fatal(err)
	}

	if _, err := stale.Commit(ctx, "b"); !errors.Is(err, ErrStale) {
		t.Fatalf("应返回 ErrStale：%v", err)
	}
	if err := b.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Commit(ctx, "b"); err == nil || !strings.Contains(err.Error(), "重新生成") {
		t.Fatalf("应要求重新生成：%v", err)
	}
}

func TestBuildTreeRejects(t *testing.T) {
	dir := t.TempDir()
	h := BlobHash(nil)
	for _, files := range []map[string]Entry{
		{"a": {"100644", h}, "a/b": {"100644", h}},
		{"/a": {"100644", h}},
		{"a/../b": {"100644", h}},
		{"a": {"100644", "zz"}},
	} {
		if _, err := buildTree(dir, files); err == nil {
			t.Errorf("%v 应报错", files)
		}
	}
	if BlobHash([]byte("hello\n")) != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Error("BlobHash 和 git hash-object 不一致")
	}
}

func TestResetIfLarger(t *testing.T) {
	ctx := context.Background()
	url, _ := newRemote(t)
	a := newRepo(t, url)
	cs := a.NewChangeset()
	put(t, cs, "f", strings.Repeat("x", 10000))
	if _, err := cs.Commit(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	if reset, err := a.ResetIfLarger(1 << 30); reset || err != nil {
		t.Fatalf("没超过上限不应删除：%v %v", reset, err)
	}
	if reset, err := a.ResetIfLarger(10); !reset || err != nil {
		t.Fatalf("超过上限应删除：%v %v", reset, err)
	}
	if err := a.Fetch(ctx); err != nil || len(a.Files()) != 1 {
		t.Fatalf("删除后重新拉取：%v %v", err, a.Files())
	}
	// ReadFile 的缓存：第二次不再访问远端
	data, err := a.ReadFile(ctx, "f")
	if err != nil || len(data) != 10000 {
		t.Fatalf("ReadFile：%d %v", len(data), err)
	}
	a.URL = "file:///nonexistent"
	if _, err := a.ReadFile(ctx, "f"); err != nil {
		t.Errorf("第二次应读缓存：%v", err)
	}
}
