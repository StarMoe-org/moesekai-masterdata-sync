// Package publish 是数据仓库的写入端：本地目录（调试）、GitHub 仓库（Git Data API）或其他 git 仓库（不检出工作目录，设计文档 §7.1）。
package publish

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/ghrepo"
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

// GitHub 用 GitHub 的 Git Data API 写数据仓库（不需要本地 git）。
type GitHub struct {
	Repo *ghrepo.Repo
	cs   *ghrepo.Changeset
}

// OpenGitHub 读最新的目录树，准备一批修改。仓库还没有提交时返回 ghrepo.ErrEmpty。
// 先删掉上一轮没清理的大文件临时分支（失败只记日志）。
func OpenGitHub(ctx context.Context, r *ghrepo.Repo) (*GitHub, error) {
	if err := r.Fetch(ctx); err != nil {
		return nil, err
	}
	if r.PushBlob != nil {
		if err := r.CleanupTemp(ctx); err != nil {
			slog.Warn("清理遗留的临时分支失败", "repo", r.Owner+"/"+r.Name, "err", err)
		}
	}
	return &GitHub{Repo: r, cs: r.NewChangeset()}, nil
}

func (g *GitHub) Put(path string, data []byte) (bool, error) { return g.cs.Put(path, data) }

func (g *GitHub) Current(ctx context.Context) (*master.Version, error) {
	data, err := g.Repo.ReadFile(ctx, "versions/current_version.json")
	if err != nil {
		return nil, err
	}
	return master.ParseVersion(data)
}

func (g *GitHub) Files() []string {
	out := make([]string, 0, len(g.Repo.Files()))
	for p := range g.Repo.Files() {
		out = append(out, p)
	}
	return out
}

func (g *GitHub) Delete(path string) { g.cs.Delete(path) }

// Finish 建提交并更新分支；远端在这期间前进过就重新读取，在新的 HEAD 上重算（最多 3 次）。
func (g *GitHub) Finish(ctx context.Context, message string) (string, error) {
	for attempt := 0; ; attempt++ {
		commit, err := g.cs.Commit(ctx, message)
		if !errors.Is(err, ghrepo.ErrStale) || attempt >= 2 {
			if errors.Is(err, ghrepo.ErrStale) {
				_ = g.cs.Cleanup(ctx) // 放弃重试：临时分支不再需要（删不掉的下轮 OpenGitHub 再删）
			}
			return commit, err
		}
		if err := g.Repo.Fetch(ctx); err != nil {
			return "", err
		}
	}
}
