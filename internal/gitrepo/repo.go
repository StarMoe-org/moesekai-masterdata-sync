// Package gitrepo 不检出工作目录地读写数据仓库（设计文档 §7.1）：
//
//   - 用部分克隆（blob:none）、深度 1 只拿提交和目录树；
//   - 调用方给出每个文件的新内容，这里算 git 对象名，和树里的比较，只有变化的内容才写成对象；
//   - 自己写出新的树对象，`git commit-tree` 生成提交，`git push`；被拒时重新拉取，在新的 HEAD 上重算。
//
// 全程设置 GIT_NO_LAZY_FETCH=1：任何需要旧文件内容的操作都会直接报错，而不是悄悄把 230MB 下载下来。
package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Identity 是提交的作者和提交者。
type Identity struct {
	Name, Email string
}

// Repo 是一个只有 git 对象、没有工作目录的本地副本。
type Repo struct {
	Dir    string // 裸仓库目录
	URL    string // 远端地址（https://… 或本地路径 / file://）
	Branch string // 默认 main
	// Token 返回 HTTPS 推拉用的 token（GitHub App installation token）；为空表示不需要认证。
	Token  func(ctx context.Context) (string, error)
	Author Identity

	head  string           // 远端分支当前的提交；空表示远端还没有这个分支
	files map[string]Entry // head 的全部文件
}

// Head 返回最近一次 Fetch 得到的提交（空仓库为空串）。
func (r *Repo) Head() string { return r.head }

// Files 返回最近一次 Fetch 得到的文件表（只读）。
func (r *Repo) Files() map[string]Entry { return r.files }

func (r *Repo) branch() string {
	if r.Branch == "" {
		return "main"
	}
	return r.Branch
}

// Fetch 初始化（第一次）并拉取远端分支的最新提交和目录树。
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.Dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(r.Dir, 0o700); err != nil {
			return err
		}
		if _, err := r.git(ctx, nil, "init", "--bare", "-q", "-b", r.branch(), "."); err != nil {
			return err
		}
		for _, kv := range [][2]string{
			{"core.repositoryformatversion", "1"},
			{"extensions.partialClone", "origin"},
			{"remote.origin.url", r.URL},
			{"remote.origin.promisor", "true"},
			{"remote.origin.partialCloneFilter", "blob:none"},
			{"gc.auto", "0"},
		} {
			if _, err := r.git(ctx, nil, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	ref := "refs/remotes/origin/" + r.branch()
	_, err := r.gitAuth(ctx, nil, "fetch", "-q", "--no-tags", "--filter=blob:none", "--depth=1", "origin",
		"+refs/heads/"+r.branch()+":"+ref)
	if err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			r.head, r.files = "", map[string]Entry{}
			return nil
		}
		return err
	}
	head, err := r.git(ctx, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	r.head = strings.TrimSpace(string(head))
	return r.loadTree(ctx)
}

func (r *Repo) loadTree(ctx context.Context) error {
	out, err := r.git(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", r.head)
	if err != nil {
		return err
	}
	files := map[string]Entry{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 {
			return fmt.Errorf("gitrepo: 看不懂 ls-tree 输出 %q", rec)
		}
		files[string(path)] = Entry{Mode: f[0], Hash: f[2]}
	}
	r.files = files
	return nil
}

// Changeset 收集一次发布要写的文件。
type Changeset struct {
	repo    *Repo
	put     map[string]string // 路径 → 新 blob 的对象名（含没变的，用于在新 HEAD 上重算）
	deletes map[string]bool
	written map[string]bool // 本批写过的对象
	changed int
}

// NewChangeset 基于最近一次 Fetch 的内容开始一批修改。
func (r *Repo) NewChangeset() *Changeset {
	return &Changeset{repo: r, put: map[string]string{}, deletes: map[string]bool{}, written: map[string]bool{}}
}

// Put 设置文件内容；和当前树里一样就只记下对象名，不写对象。返回内容是否有变化。
func (c *Changeset) Put(path string, data []byte) (bool, error) {
	sha := BlobHash(data)
	c.put[path] = sha
	delete(c.deletes, path)
	if old, ok := c.repo.files[path]; ok && old.Hash == sha && old.Mode == "100644" {
		return false, nil
	}
	if _, err := writeLoose(c.repo.Dir, "blob", data); err != nil {
		return false, err
	}
	c.written[sha] = true
	c.changed++
	return true, nil
}

// Delete 删除文件（不存在时忽略）。
func (c *Changeset) Delete(path string) {
	delete(c.put, path)
	c.deletes[path] = true
}

// Changed 是 Put 里内容有变化的文件数。
func (c *Changeset) Changed() int { return c.changed }

// ErrStale 表示推送时远端已经前进（非快进）；调用方应 Fetch 后用同一个 Changeset 重试 Commit。
var ErrStale = errors.New("gitrepo: 远端分支已更新，推送被拒")

// Commit 在当前 head 上应用修改、生成提交并推送。没有任何变化时返回空串。
func (c *Changeset) Commit(ctx context.Context, message string) (string, error) {
	r := c.repo
	files := make(map[string]Entry, len(r.files)+len(c.put))
	for p, e := range r.files {
		files[p] = e
	}
	for p := range c.deletes {
		delete(files, p)
	}
	for p, sha := range c.put {
		// 推送被拒、在新 HEAD 上重算时：当初因为「和旧树一样」没写的内容，如果新树里又不一样了，本地没有这个对象
		if old, ok := r.files[p]; !c.written[sha] && (!ok || old.Hash != sha) {
			return "", fmt.Errorf("gitrepo: %s 的内容需要重新生成（远端在这期间改过它）", p)
		}
		files[p] = Entry{Mode: "100644", Hash: sha}
	}
	tree, err := buildTree(r.Dir, files)
	if err != nil {
		return "", err
	}
	if r.head != "" {
		old, err := r.git(ctx, nil, "rev-parse", r.head+"^{tree}")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(old)) == tree {
			return "", nil
		}
	} else if len(files) == 0 {
		return "", nil
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	if r.head != "" {
		args = append(args, "-p", r.head)
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + r.Author.Name, "GIT_AUTHOR_EMAIL=" + r.Author.Email,
		"GIT_COMMITTER_NAME=" + r.Author.Name, "GIT_COMMITTER_EMAIL=" + r.Author.Email,
	}
	out, err := r.gitIn(ctx, env, strings.NewReader(message), args...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	// --no-thin：thin pack 会拿远端已有的旧内容做差量基准，需要读本地没有的旧 blob（会触发按需下载并失败）
	if _, err := r.gitAuth(ctx, nil, "push", "-q", "--no-thin", "origin", commit+":refs/heads/"+r.branch()); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first") ||
			strings.Contains(msg, "[rejected]") || strings.Contains(msg, "stale info") {
			return "", ErrStale
		}
		return "", err
	}
	r.head = commit
	r.files = files
	return commit, nil
}

// git 运行 git 命令（不带认证）。
func (r *Repo) git(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return r.gitIn(ctx, env, nil, args...)
}

// gitAuth 运行需要访问远端的命令；token 只通过环境变量里的配置传给 git，不出现在命令行参数里。
func (r *Repo) gitAuth(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if r.Token != nil {
		tok, err := r.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
			env = append(env, "GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http.extraHeader",
				"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
		}
	}
	return r.gitIn(ctx, env, nil, args...)
}

func (r *Repo) gitIn(ctx context.Context, env []string, stdin *strings.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_NO_LAZY_FETCH=1",   // 绝不按需下载缺失的 blob
		"GIT_TERMINAL_PROMPT=0", // 不弹出凭据输入
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LC_ALL=C",
	)
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s：%w：%s", args[0], err, firstLines(stderr.String(), 6))
	}
	return stdout.Bytes(), nil
}

func firstLines(s string, n int) string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() && len(out) < n {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, " | ")
}

// ReadFile 读 head 里一个小文件的内容（如 versions/current_version.json）。
// 这是唯一允许按需下载 blob 的地方：只下载这一个对象。文件不存在时返回 nil、nil。
func (r *Repo) ReadFile(ctx context.Context, path string) ([]byte, error) {
	e, ok := r.files[path]
	if !ok {
		return nil, nil
	}
	var env []string
	if r.Token != nil {
		tok, err := r.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
			env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
				"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
		}
	}
	env = append(env, "GIT_NO_LAZY_FETCH=0") // 覆盖 gitIn 里的默认值（后设置的生效）
	return r.gitIn(ctx, env, nil, "cat-file", "blob", e.Hash)
}
