// Package publish 是数据仓库的写入端：本地目录（调试）或 git 仓库（不检出工作目录，设计文档 §7.1）。
package publish

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/master"
)

// Target 是一次发布的目标。
type Target interface {
	master.Sink
	// Current 读当前的 versions/current_version.json（没有时返回 nil）。
	Current(ctx context.Context) (*master.Version, error)
	// Files 是目标里现有的全部文件路径。
	Files() []string
	Delete(path string)
	// Finish 提交这一批修改；没有变化或不需要提交时返回空串。
	Finish(ctx context.Context, message string) (string, error)
}

// Dir 写本地目录。
type Dir struct{ Root string }

func (d *Dir) Put(path string, data []byte) (bool, error) {
	return master.DirSink(d.Root).Put(path, data)
}

func (d *Dir) Current(context.Context) (*master.Version, error) {
	data, err := os.ReadFile(filepath.Join(d.Root, "versions", "current_version.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return master.ParseVersion(data)
}

func (d *Dir) Files() []string {
	var out []string
	_ = filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && !strings.Contains(e.Name(), ".tmp-") {
			rel, _ := filepath.Rel(d.Root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func (d *Dir) Delete(path string) { _ = os.Remove(filepath.Join(d.Root, filepath.FromSlash(path))) }

func (d *Dir) Finish(context.Context, string) (string, error) { return "", nil }

// Git 写 git 仓库。
type Git struct {
	Repo *gitrepo.Repo
	cs   *gitrepo.Changeset
}

// OpenGit 拉取最新的目录树，准备一批修改。
func OpenGit(ctx context.Context, r *gitrepo.Repo) (*Git, error) {
	if err := r.Fetch(ctx); err != nil {
		return nil, err
	}
	return &Git{Repo: r, cs: r.NewChangeset()}, nil
}

func (g *Git) Put(path string, data []byte) (bool, error) { return g.cs.Put(path, data) }

func (g *Git) Current(ctx context.Context) (*master.Version, error) {
	data, err := g.Repo.ReadFile(ctx, "versions/current_version.json")
	if err != nil {
		return nil, err
	}
	return master.ParseVersion(data)
}

func (g *Git) Files() []string {
	out := make([]string, 0, len(g.Repo.Files()))
	for p := range g.Repo.Files() {
		out = append(out, p)
	}
	return out
}

func (g *Git) Delete(path string) { g.cs.Delete(path) }

// Finish 提交并推送；远端在这期间前进过就重新拉取，在新的 HEAD 上重算（最多 3 次）。
func (g *Git) Finish(ctx context.Context, message string) (string, error) {
	for attempt := 0; ; attempt++ {
		commit, err := g.cs.Commit(ctx, message)
		if !errors.Is(err, gitrepo.ErrStale) || attempt >= 2 {
			return commit, err
		}
		if err := g.Repo.Fetch(ctx); err != nil {
			return "", err
		}
	}
}
