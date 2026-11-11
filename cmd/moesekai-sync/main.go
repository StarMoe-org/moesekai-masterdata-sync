// moesekai-sync：Project SEKAI masterdata 同步服务（设计文档 docs/design.md）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/ghapp"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/master"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/nuverse"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/publish"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/schema"
)

const usage = `用法：
  moesekai-sync decode --region cn --info <master-data-N.info> --out <目录> [--app-meta <地址>]
      离线：用 app-meta 里当前版本的密钥和结构文件解密、还原一份本地 master，写到 <目录>
  moesekai-sync once --region cn (--out <目录> | --repo <仓库地址> [--work <目录>]) [--app-meta <地址>] [--force]
      跑一轮：查当前版本，有新 master 就下载、还原、写出。--repo 时提交并推送（不检出工作目录）

环境变量：
  MOESEKAI_CN_UID（或 _FILE）              cn login 用的已存在账号 id
  GITHUB_APP_ID、GITHUB_APP_PRIVATE_KEY（或 _FILE）   推送到 GitHub 时用
`

// 提交身份：GitHub App moesekai-bot（与 pjsk-meta-builder 相同）。
var botIdentity = gitrepo.Identity{Name: "moesekai-bot[bot]", Email: "354789470+moesekai-bot[bot]@users.noreply.github.com"}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(64)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "decode":
		err = cmdDecode(ctx, os.Args[2:])
	case "once":
		err = cmdOnce(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(64)
	}
	if err != nil {
		slog.Error("失败", "err", err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(64)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// secret 读环境变量 NAME，或 NAME_FILE 指向的文件。
func secret(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE：%w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

func appMetaClient(base string) *appmeta.Client {
	c := appmeta.NewClient()
	if base != "" {
		c.Base = base
	}
	return c
}

func cmdDecode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	region := fs.String("region", "", "区服")
	info := fs.String("info", "", "本地 master 文件")
	out := fs.String("out", "", "输出目录")
	base := fs.String("app-meta", "", "app-meta 地址（默认 GitHub raw；file:///路径 表示本地目录）")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *region == "" || *info == "" || *out == "" {
		return usageError{"decode 需要 --region、--info、--out"}
	}
	b, err := appMetaClient(*base).Latest(ctx, *region)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*info)
	if err != nil {
		return err
	}
	tables, err := decodeNuverse(b, raw)
	if err != nil {
		return err
	}
	res, err := master.WriteTables(master.DirSink(*out), tables.Tables)
	if err != nil {
		return err
	}
	logTables(tables, res)
	return nil
}

// decodeNuverse 解密 + 解码 + 按结构文件还原。
func decodeNuverse(b *appmeta.Bundle, raw []byte) (*schema.Result, error) {
	if b.Schema == nil {
		return nil, fmt.Errorf("%s 当前版本没有 schema.json", b.Meta.Region)
	}
	s, err := schema.Parse(b.Schema)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	plain, err := b.Keys.API().Decrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("解密 master：%w", err)
	}
	v, err := msgpack.Decode(plain)
	if err != nil {
		return nil, fmt.Errorf("解码 master：%w", err)
	}
	plain = nil
	t1 := time.Now()
	res, err := s.Restore(v)
	if err != nil {
		return nil, fmt.Errorf("还原 master：%w", err)
	}
	slog.Info("解码完成", "bytes", len(raw), "decode", t1.Sub(t0).Round(time.Millisecond), "restore", time.Since(t1).Round(time.Millisecond))
	return res, nil
}

func logTables(tables *schema.Result, res *master.WriteResult) {
	slog.Info("表已生成", "tables", len(res.Produced), "changed", len(res.Written), "unchanged", res.Unchanged,
		"unknown", tables.Unknown, "dropped", tables.Dropped, "expanded", len(tables.Expanded))
}

// openTarget 根据 --out / --repo 打开写入目标。
func openTarget(ctx context.Context, out, repoURL, work string) (publish.Target, error) {
	switch {
	case out != "" && repoURL != "":
		return nil, usageError{"--out 和 --repo 只能用一个"}
	case out != "":
		return &publish.Dir{Root: out}, nil
	case repoURL == "":
		return nil, usageError{"需要 --out 或 --repo"}
	}
	if work == "" {
		work = filepath.Join(os.TempDir(), "moesekai-sync", strings.NewReplacer("/", "_", ":", "_").Replace(repoURL))
	}
	r := &gitrepo.Repo{Dir: work, URL: repoURL, Author: botIdentity}
	if owner, name, ok := githubRepo(repoURL); ok {
		id := os.Getenv("GITHUB_APP_ID")
		key, err := secret("GITHUB_APP_PRIVATE_KEY")
		if err != nil {
			return nil, err
		}
		if id == "" || key == "" {
			return nil, usageError{"推送到 GitHub 需要 GITHUB_APP_ID 和 GITHUB_APP_PRIVATE_KEY"}
		}
		app, err := ghapp.New(id, key)
		if err != nil {
			return nil, err
		}
		r.Token = func(ctx context.Context) (string, error) { return app.Token(ctx, owner, name) }
	}
	return publish.OpenGit(ctx, r)
}

// githubRepo 从 https://github.com/<owner>/<repo>(.git) 取出 owner、repo。
func githubRepo(url string) (string, string, bool) {
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	if !ok {
		return "", "", false
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func cmdOnce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	region := fs.String("region", "", "区服（第一版只支持 cn）")
	out := fs.String("out", "", "写到本地目录")
	repoURL := fs.String("repo", "", "数据仓库地址（https://github.com/… 或本地裸仓库路径）")
	work := fs.String("work", "", "--repo 时的本地 git 目录（默认在临时目录下）")
	base := fs.String("app-meta", "", "app-meta 地址（默认 GitHub raw；file:///路径 表示本地目录）")
	force := fs.Bool("force", false, "cdnVersion 没变也重新下载、写出")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *region != "cn" {
		return usageError{"once 目前只支持 --region cn（jp 在 N3 实现）"}
	}
	cfg := nuverse.CN
	uidText, err := secret("MOESEKAI_CN_UID")
	if err != nil {
		return err
	}
	if uidText != "" {
		if cfg.UserID, err = strconv.ParseUint(uidText, 10, 64); err != nil {
			return usageError{"MOESEKAI_CN_UID 不是数字"}
		}
	}
	b, err := appMetaClient(*base).Latest(ctx, *region)
	if err != nil {
		return err
	}
	target, err := openTarget(ctx, *out, *repoURL, *work)
	if err != nil {
		return err
	}
	commit, err := syncNuverse(ctx, nuverse.New(cfg), b, target, nil, *force)
	if err != nil {
		return err
	}
	if commit != "" {
		slog.Info("已推送", "commit", commit)
	}
	return nil
}

// syncNuverse 跑一轮 Nuverse 服：login → 比较 cdnVersion → 下载、还原、写出 → 提交。
// guard 为 nil 时不删除任何表（单次运行没有「连续两次缺失」的依据）。
func syncNuverse(ctx context.Context, c *nuverse.Client, b *appmeta.Bundle, t publish.Target, guard *master.DeleteGuard, force bool) (string, error) {
	region := b.Meta.Region
	st, err := c.Login(ctx, b)
	if err != nil {
		return "", err
	}
	slog.Info("login", "region", region, "version", b.Meta.NuverseAppVersion(), "http", st.HTTPStatus,
		"status", st.AppVersionStatus, "dataVersion", st.DataVersion, "cdnVersion", st.CDNVersion)
	if st.Outdated() {
		// N4：这里改成刷新 app-meta、必要时请求 builder（设计文档 §6）
		return "", fmt.Errorf("app-meta 里的 %s %s 已被服务器淘汰（HTTP %d，%s），需要新版本的元数据",
			region, b.Meta.NuverseAppVersion(), st.HTTPStatus, st.AppVersionStatus)
	}
	cur, err := t.Current(ctx)
	if err != nil {
		return "", err
	}
	if cur != nil && cur.CDNVersion >= st.CDNVersion && !force {
		slog.Info("没有新的 master", "region", region, "current", cur.CDNVersion)
		return "", nil
	}
	raw, url, err := c.DownloadMaster(ctx, st.CDNVersion)
	if err != nil {
		return "", err
	}
	slog.Info("已下载", "url", url, "bytes", len(raw))
	tables, err := decodeNuverse(b, raw)
	if err != nil {
		return "", err
	}
	raw = nil
	existing := master.TablesIn(t.Files())
	res, err := master.WriteTables(t, tables.Tables)
	if err != nil {
		return "", err
	}
	logTables(tables, res)
	if guard != nil {
		for _, name := range guard.Decide(existing, res.Produced) {
			slog.Warn("删除表", "region", region, "table", name)
			t.Delete("master/" + name + ".json")
		}
	}
	v := master.Version{
		SystemProfile:    "production",
		AppVersion:       st.AppVersion,
		MultiPlayVersion: st.MultiPlayVersion,
		DataVersion:      st.DataVersion,
		AssetVersion:     st.AssetVersion,
		AppHash:          b.Meta.AppHash,
		AppVersionStatus: st.AppVersionStatus,
		CDNVersion:       st.CDNVersion,
	}
	if err := master.WriteVersion(t, v); err != nil {
		return "", err
	}
	return t.Finish(ctx, "Sekai master data version "+st.DataVersion)
}
