// Package ghrepo 用 GitHub 的 Git Data API 读写数据仓库，不需要本地 git（设计文档 §7.1）。
//
//   - 读：分支 → 提交 → 递归目录树，得到每个文件的 blob 对象名；只按需读小文件（版本文件）。
//   - 写：内容有变化的文件攒成一批，用 POST git/trees 的内联内容一次上传（每批一个请求，
//     不按文件逐个请求，避开 GitHub 对创建内容的频率限制）；非 UTF-8 或单个过大的文件走 POST git/blobs。
//     超过 LargeBytes 的文件 API 收不下（HTTP 422），交给 PushBlob 用 git 推到临时分支，提交后删掉临时分支。
//     每个上传的对象名都和本地算出的比对。最后在当前树上建新树、建提交、以快进方式更新分支。
//
// 早先用 `git push` 推送部分克隆（blob:none）的本地副本：HTTPS 推送时 git 忽略 --no-thin，
// 打包要读改动文件的旧内容做差量基准，本地没有就失败（2026-11-12 cn 187）。改用 API 后没有这个问题。
package ghrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
)

var (
	// ErrEmpty：仓库还没有任何提交（或没有这个分支）。Git Data API 在空仓库上不可用，首个提交由调用方另行处理。
	ErrEmpty = errors.New("ghrepo: 数据仓库还没有提交")
	// ErrStale：更新分支时远端已经前进（不是快进）；调用方应 Fetch 后用同一个 Changeset 重试 Commit。
	ErrStale = errors.New("ghrepo: 远端分支已更新，提交被拒")
)

const (
	defaultBatchBytes = 8 << 20 // 一批内联上传的内容上限
	maxBatchFiles     = 200     // 一批最多几个文件
	smallFileLimit    = 64 << 10
	// defaultLargeBytes：超过这个大小的文件不走 POST git/blobs。2026-11-19 实测 51,116,009 字节
	// （base64 后约 68MB）返回 422 "input was too large"；35MB 的 cards.json 之前能传。
	defaultLargeBytes = 32 << 20
	// TempBranchPrefix：大文件临时分支的前缀（refs/heads/ 之后），发布完成后删除。
	TempBranchPrefix = "moesekai-sync/upload-"
)

// Entry 是树里的一个条目（含子模块等非 blob 条目，原样保留）。
type Entry struct {
	Mode string
	Hash string
}

// Repo 是 GitHub 上的一个数据仓库。
type Repo struct {
	Owner, Name string
	Branch      string // 默认 main
	APIBase     string // 默认 https://api.github.com
	HTTP        *http.Client
	// Token 返回 installation token；为空表示不认证（只用于测试）。
	Token func(ctx context.Context) (string, error)
	// BatchBytes 是一批内联上传的内容上限（默认 8MB）。
	BatchBytes int
	// PushBlob 把 data 推到远端的 ref（refs/heads/…）上，让 GitHub 有这个 blob；用于超过 LargeBytes 的文件。
	// 为 nil 时大文件仍走 POST git/blobs。
	PushBlob func(ctx context.Context, ref string, data []byte) error
	// LargeBytes 是走 PushBlob 的文件大小下限（默认 32MB）。
	LargeBytes int

	head   string           // 分支当前的提交；Fetch 之后非空
	tree   string           // head 的根树
	files  map[string]Entry // head 的全部文件
	hashes map[string]bool  // head 里出现过的 blob（远端一定已有，不用再上传）
	small  map[string][]byte
}

// Head 返回最近一次 Fetch 得到的提交。
func (r *Repo) Head() string { return r.head }

// Files 返回最近一次 Fetch 得到的文件表（只读）。
func (r *Repo) Files() map[string]Entry { return r.files }

func (r *Repo) branch() string {
	if r.Branch == "" {
		return "main"
	}
	return r.Branch
}

// Fetch 读分支的最新提交和目录树；提交没变时不重新读树。
func (r *Repo) Fetch(ctx context.Context) error {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := r.call(ctx, http.MethodGet, "git/ref/heads/"+r.branch(), nil, &ref)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusConflict) {
		return ErrEmpty // 404：没有这个分支；409：Git Repository is empty
	}
	if err != nil {
		return err
	}
	head := ref.Object.SHA
	if head == "" {
		return fmt.Errorf("ghrepo: 分支 %s 的响应里没有提交", r.branch())
	}
	if head == r.head && r.files != nil {
		return nil
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := r.call(ctx, http.MethodGet, "git/commits/"+head, nil, &commit); err != nil {
		return err
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := r.call(ctx, http.MethodGet, "git/trees/"+commit.Tree.SHA+"?recursive=1", nil, &tree); err != nil {
		return err
	}
	if tree.Truncated {
		return errors.New("ghrepo: 目录树太大，GitHub 返回的列表被截断")
	}
	files := map[string]Entry{}
	hashes := map[string]bool{}
	for _, e := range tree.Tree {
		if e.Type == "tree" {
			continue
		}
		files[e.Path] = Entry{Mode: e.Mode, Hash: e.SHA}
		if e.Type == "blob" {
			hashes[e.SHA] = true
		}
	}
	r.head, r.tree, r.files, r.hashes = head, commit.Tree.SHA, files, hashes
	return nil
}

// ReadFile 读 head 里一个文件的内容（如 versions/current_version.json）；不存在时返回 nil、nil。
func (r *Repo) ReadFile(ctx context.Context, path string) ([]byte, error) {
	e, ok := r.files[path]
	if !ok {
		return nil, nil
	}
	if data, ok := r.small[e.Hash]; ok {
		return data, nil
	}
	var blob struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := r.call(ctx, http.MethodGet, "git/blobs/"+e.Hash, nil, &blob); err != nil {
		return nil, err
	}
	if blob.Encoding != "base64" {
		return nil, fmt.Errorf("ghrepo: %s 的编码是 %q", path, blob.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("ghrepo: 解 %s 的内容：%w", path, err)
	}
	if got := gitrepo.BlobHash(data); got != e.Hash {
		return nil, fmt.Errorf("ghrepo: %s 的内容和对象名对不上（%s ≠ %s）", path, got, e.Hash)
	}
	if len(data) <= smallFileLimit {
		if r.small == nil {
			r.small = map[string][]byte{}
		}
		r.small[e.Hash] = data
	}
	return data, nil
}

// Changeset 收集一次发布要写的文件。
type Changeset struct {
	repo     *Repo
	put      map[string]string // 路径 → 新内容的对象名（含没变的，在新 HEAD 上重算时用）
	deletes  map[string]bool
	uploaded map[string]bool // 已上传到 GitHub 的对象
	pending  []pendingBlob
	pendSize int
	changed  int
	temps    []string // 推大文件用的临时分支（refs/heads/ 之后的部分）
}

type pendingBlob struct {
	sha  string
	data []byte
}

// NewChangeset 基于最近一次 Fetch 的内容开始一批修改。
func (r *Repo) NewChangeset() *Changeset {
	return &Changeset{repo: r, put: map[string]string{}, deletes: map[string]bool{}, uploaded: map[string]bool{}}
}

// Put 设置文件内容；和当前树里一样就只记下对象名。内容有变化时排进待上传的一批（攒够就上传）。
func (c *Changeset) Put(path string, data []byte) (bool, error) {
	ctx := context.Background()
	sha := gitrepo.BlobHash(data)
	c.put[path] = sha
	delete(c.deletes, path)
	if old, ok := c.repo.files[path]; ok && old.Hash == sha && old.Mode == "100644" {
		return false, nil
	}
	c.changed++
	if c.uploaded[sha] || c.repo.hashes[sha] || c.isPending(sha) {
		return true, nil
	}
	if len(data) > c.repo.largeBytes() && c.repo.PushBlob != nil {
		branch := TempBranchPrefix + sha[:12]
		if err := c.repo.PushBlob(ctx, "refs/heads/"+branch, data); err != nil {
			return false, fmt.Errorf("推送 %s（%d 字节）：%w", path, len(data), err)
		}
		c.temps = append(c.temps, branch)
		c.uploaded[sha] = true
		return true, nil
	}
	if !utf8.Valid(data) || len(data) > c.repo.batchBytes() {
		if err := c.repo.uploadBlob(ctx, sha, data); err != nil {
			return false, err
		}
		c.uploaded[sha] = true
		return true, nil
	}
	if c.pendSize+len(data) > c.repo.batchBytes() || len(c.pending) >= maxBatchFiles {
		if err := c.flush(ctx); err != nil {
			return false, err
		}
	}
	c.pending = append(c.pending, pendingBlob{sha, bytes.Clone(data)})
	c.pendSize += len(data)
	return true, nil
}

func (c *Changeset) isPending(sha string) bool {
	for _, p := range c.pending {
		if p.sha == sha {
			return true
		}
	}
	return false
}

// Delete 删除文件（不存在时忽略）。
func (c *Changeset) Delete(path string) {
	delete(c.put, path)
	c.deletes[path] = true
}

// Changed 是 Put 里内容有变化的文件数。
func (c *Changeset) Changed() int { return c.changed }

// flush 用一个 POST git/trees 上传待上传的内容：树里的条目按序号命名，只是借它让 GitHub 写出这些 blob；
// 返回的每个条目的对象名都要和本地算出的一致。
func (c *Changeset) flush(ctx context.Context) error {
	if len(c.pending) == 0 {
		return nil
	}
	type item struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	items := make([]item, len(c.pending))
	want := map[string]string{}
	for i, p := range c.pending {
		name := fmt.Sprintf("%04d", i)
		items[i] = item{name, "100644", "blob", string(p.data)}
		want[name] = p.sha
	}
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.repo.call(ctx, http.MethodPost, "git/trees", map[string]any{"tree": items}, &out); err != nil {
		return fmt.Errorf("上传 %d 个文件：%w", len(items), err)
	}
	got := map[string]string{}
	for _, e := range out.Tree {
		got[e.Path] = e.SHA
	}
	for name, sha := range want {
		if got[name] != sha {
			return fmt.Errorf("ghrepo: GitHub 存下的内容和本地不一致（%s ≠ %s）", got[name], sha)
		}
	}
	for _, p := range c.pending {
		c.uploaded[p.sha] = true
	}
	c.pending, c.pendSize = nil, 0
	return nil
}

func (r *Repo) uploadBlob(ctx context.Context, sha string, data []byte) error {
	var out struct {
		SHA string `json:"sha"`
	}
	body := map[string]string{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64"}
	if err := r.call(ctx, http.MethodPost, "git/blobs", body, &out); err != nil {
		return fmt.Errorf("上传 %d 字节：%w", len(data), err)
	}
	if out.SHA != sha {
		return fmt.Errorf("ghrepo: GitHub 存下的内容和本地不一致（%s ≠ %s）", out.SHA, sha)
	}
	return nil
}

func (r *Repo) largeBytes() int {
	if r.LargeBytes > 0 {
		return r.LargeBytes
	}
	return defaultLargeBytes
}

// Cleanup 删掉这批修改推大文件用的临时分支。Commit 成功或出错（ErrStale 除外，调用方还会重试）后自动调用；
// 不再 Commit 时由调用方调用。删除失败只返回错误，不影响已经完成的提交。
func (c *Changeset) Cleanup(ctx context.Context) error {
	err := c.repo.deleteBranches(ctx, c.temps)
	c.temps = nil
	return err
}

// CleanupTemp 删掉远端所有遗留的临时分支（上一轮推了大文件、但没走到 Commit 就失败了）。
func (r *Repo) CleanupTemp(ctx context.Context) error {
	var refs []struct {
		Ref string `json:"ref"`
	}
	if err := r.call(ctx, http.MethodGet, "git/matching-refs/heads/"+TempBranchPrefix, nil, &refs); err != nil {
		return err
	}
	var branches []string
	for _, x := range refs {
		if b, ok := strings.CutPrefix(x.Ref, "refs/heads/"); ok && strings.HasPrefix(b, TempBranchPrefix) {
			branches = append(branches, b)
		}
	}
	return r.deleteBranches(ctx, branches)
}

func (r *Repo) deleteBranches(ctx context.Context, branches []string) error {
	var errs []error
	for _, b := range branches {
		err := r.call(ctx, http.MethodDelete, "git/refs/heads/"+b, nil, nil)
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusUnprocessableEntity) {
			err = nil // 已经不在了
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("删除临时分支 %s：%w", b, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Repo) batchBytes() int {
	if r.BatchBytes > 0 {
		return r.BatchBytes
	}
	return defaultBatchBytes
}

// Commit 在当前 head 上应用修改、建提交并快进更新分支。没有任何变化时返回空串。
// 除 ErrStale（调用方会 Fetch 后重试）外，返回前删掉推大文件用的临时分支。
func (c *Changeset) Commit(ctx context.Context, message string) (sha string, err error) {
	defer func() {
		if errors.Is(err, ErrStale) {
			return
		}
		if cerr := c.Cleanup(ctx); cerr != nil {
			if err == nil {
				slog.Warn("提交已完成，但临时分支没删掉", "repo", c.repo.Owner+"/"+c.repo.Name, "err", cerr)
			} else {
				err = errors.Join(err, cerr)
			}
		}
	}()
	return c.commit(ctx, message)
}

func (c *Changeset) commit(ctx context.Context, message string) (string, error) {
	r := c.repo
	if r.head == "" {
		return "", ErrEmpty
	}
	if err := c.flush(ctx); err != nil {
		return "", err
	}
	type item struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"` // nil 表示删除
	}
	var items []item
	paths := make([]string, 0, len(c.put))
	for p := range c.put {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sha := c.put[p]
		if old, ok := r.files[p]; ok && old.Hash == sha && old.Mode == "100644" {
			continue
		}
		// 远端在这期间改过、而当初因为「和旧树一样」没上传的内容：它在旧树里，GitHub 上一定有这个对象
		items = append(items, item{p, "100644", "blob", &sha})
	}
	dels := make([]string, 0, len(c.deletes))
	for p := range c.deletes {
		if _, ok := r.files[p]; ok {
			dels = append(dels, p)
		}
	}
	sort.Strings(dels)
	for _, p := range dels {
		items = append(items, item{p, "100644", "blob", nil})
	}
	if len(items) == 0 {
		return "", nil
	}

	var tree struct {
		SHA string `json:"sha"`
	}
	if err := r.call(ctx, http.MethodPost, "git/trees", map[string]any{"base_tree": r.tree, "tree": items}, &tree); err != nil {
		return "", fmt.Errorf("建目录树：%w", err)
	}
	if tree.SHA == r.tree {
		return "", nil
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	// 不填作者：GitHub 用 App 的身份（moesekai-bot[bot]）并给提交签名
	body := map[string]any{"message": message, "tree": tree.SHA, "parents": []string{r.head}}
	if err := r.call(ctx, http.MethodPost, "git/commits", body, &commit); err != nil {
		return "", fmt.Errorf("建提交：%w", err)
	}
	err := r.call(ctx, http.MethodPatch, "git/refs/heads/"+r.branch(), map[string]any{"sha": commit.SHA, "force": false}, nil)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(he.Message), "fast forward") {
		return "", ErrStale
	}
	if err != nil {
		return "", fmt.Errorf("更新分支：%w", err)
	}

	files := make(map[string]Entry, len(r.files)+len(items))
	for p, e := range r.files {
		files[p] = e
	}
	for _, it := range items {
		if it.SHA == nil {
			delete(files, it.Path)
		} else {
			files[it.Path] = Entry{Mode: it.Mode, Hash: *it.SHA}
			r.hashes[*it.SHA] = true
		}
	}
	r.head, r.tree, r.files = commit.SHA, tree.SHA, files
	return commit.SHA, nil
}

// HTTPError 是 GitHub API 返回的非 2xx 响应。
type HTTPError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub %s %s：HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("GitHub %s %s：HTTP %d %s", e.Method, e.Path, e.Status, e.Message)
}

var defaultHTTP = &http.Client{Timeout: 5 * time.Minute}

// call 请求 /repos/{owner}/{name}/{path}。
func (r *Repo) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	base := r.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	u := fmt.Sprintf("%s/repos/%s/%s/%s", strings.TrimRight(base, "/"), url.PathEscape(r.Owner), url.PathEscape(r.Name), path)
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.Token != nil {
		tok, err := r.Token(ctx)
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	hc := r.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		p, _, _ := strings.Cut(path, "?")
		return &HTTPError{Method: method, Path: p, Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
