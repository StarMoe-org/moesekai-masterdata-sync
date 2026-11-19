package gitrepo

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// PushBlobs 把一批内容放进一个没有父提交的提交，强制推到远端的 ref 上，让远端有这些 blob。
//
// 用途：GitHub 的 Git Data API 收不下大文件（2026-11-19 jp：51MB 的 gachas.json 在 POST git/blobs
// 返回 422 "input was too large"），这些文件改用 git 推送。提交里只有这几个 blob，本地仓库是全新的、
// 不是部分克隆，打包不需要任何旧内容，不会遇到 cn 187 那样的问题。
//
// r.Dir 是专用的临时目录：每次重新初始化，推送完就删掉。只用到 r.URL、r.Token、r.Author。
func (r *Repo) PushBlobs(ctx context.Context, ref string, blobs ...[]byte) (string, error) {
	if !strings.HasPrefix(ref, "refs/") {
		return "", fmt.Errorf("gitrepo: ref %q 不完整", ref)
	}
	if err := os.RemoveAll(r.Dir); err != nil {
		return "", err
	}
	defer os.RemoveAll(r.Dir)
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, nil, "init", "--bare", "-q", "."); err != nil {
		return "", err
	}
	files := make(map[string]Entry, len(blobs))
	for i, data := range blobs {
		sha, err := writeLoose(r.Dir, "blob", data)
		if err != nil {
			return "", err
		}
		files[fmt.Sprintf("%04d", i)] = Entry{Mode: "100644", Hash: sha}
	}
	tree, err := buildTree(r.Dir, files)
	if err != nil {
		return "", err
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + r.Author.Name, "GIT_AUTHOR_EMAIL=" + r.Author.Email,
		"GIT_COMMITTER_NAME=" + r.Author.Name, "GIT_COMMITTER_EMAIL=" + r.Author.Email,
	}
	out, err := r.gitIn(ctx, env, strings.NewReader("临时：上传大文件，发布后删除\n"), "commit-tree", tree, "-F", "-")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if _, err := r.gitAuth(ctx, nil, "push", "-q", "--no-thin", "--force", r.URL, commit+":"+ref); err != nil {
		return "", err
	}
	return commit, nil
}
