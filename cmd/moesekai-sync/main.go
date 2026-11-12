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

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/ghapp"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/master"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/nuverse"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/publish"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/sega"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/syncer"
)

// version 由构建时的 -ldflags "-X main.version=…" 设置。
var version = "dev"

const usage = `用法：
  moesekai-sync serve
      常驻服务：按间隔同步各区服并推送到数据仓库；HTTP 提供 /healthz /metrics 和 builder 回调（配置见 README）
  moesekai-sync once --region jp|en|cn|hmt|kr (--out <目录> | --repo <仓库地址> [--work <目录>]) [--app-meta <地址>] [--force] [--register]
      跑一轮：查当前版本，有新 master 就下载、还原、写出。--repo 时提交并推送（不检出工作目录）
      jp / en 没有账号时，加 --register 才会注册一个新账号（先保存到账号文件再使用）
  moesekai-sync decode --region cn --info <master-data-N.info> --out <目录> [--app-meta <地址>]
      离线：用 app-meta 里当前版本的密钥和结构文件解密、还原一份本地 master，写到 <目录>
  moesekai-sync version

环境变量（once）：
  MOESEKAI_<区服>_UID（或 _FILE）          cn / hmt / kr login 用的已存在账号 id
  MOESEKAI_<区服>_API                      hmt / kr：覆盖 login 的 API 地址（可选）
  MOESEKAI_<区服>_ACCOUNT_FILE             jp / en 账号文件（JSON，0600；没有时配合 --register 注册）
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
	case "serve":
		err = cmdServe(ctx)
	case "once":
		err = cmdOnce(ctx, os.Args[2:])
	case "decode":
		err = cmdDecode(ctx, os.Args[2:])
	case "version":
		fmt.Println(version)
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

// nuverseConfig 取某个 Nuverse 服的配置：uid 来自 MOESEKAI_<区服>_UID（必填），
// login 的 API 地址可用 MOESEKAI_<区服>_API 覆盖。
func nuverseConfig(region string) (nuverse.Config, error) {
	cfg, ok := nuverse.Configs[region]
	if !ok {
		return cfg, usageError{region + " 不是 Nuverse 服"}
	}
	up := strings.ToUpper(region)
	uidText, err := secret("MOESEKAI_" + up + "_UID")
	if err != nil {
		return cfg, err
	}
	if uidText != "" {
		if cfg.UserID, err = strconv.ParseUint(uidText, 10, 64); err != nil {
			return cfg, usageError{"MOESEKAI_" + up + "_UID 不是数字"}
		}
	}
	if api := os.Getenv("MOESEKAI_" + up + "_API"); api != "" {
		cfg.APIBase = api
	}
	return cfg, nil
}

// segaRegion / nuverseRegion 判断区服属于哪套协议。
func segaRegion(r string) bool    { _, ok := sega.Configs[r]; return ok }
func nuverseRegion(r string) bool { _, ok := nuverse.Configs[r]; return ok }

// githubApp 读 GitHub App 配置；没配置时返回 nil。
func githubApp() (*ghapp.App, error) {
	id := os.Getenv("GITHUB_APP_ID")
	key, err := secret("GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	if id == "" && key == "" {
		return nil, nil
	}
	if id == "" || key == "" {
		return nil, usageError{"GITHUB_APP_ID 和 GITHUB_APP_PRIVATE_KEY 要一起配置"}
	}
	return ghapp.New(id, key)
}

// newRepo 创建数据仓库的本地副本；GitHub 地址用 App 换只针对该仓库的 token。
func newRepo(repoURL, work string, app *ghapp.App) (*gitrepo.Repo, error) {
	r := &gitrepo.Repo{Dir: work, URL: repoURL, Author: botIdentity}
	if owner, name, ok := githubRepo(repoURL); ok {
		if app == nil {
			return nil, usageError{"推送到 GitHub 需要 GITHUB_APP_ID 和 GITHUB_APP_PRIVATE_KEY"}
		}
		r.Token = func(ctx context.Context) (string, error) { return app.Token(ctx, owner, name) }
	}
	return r, nil
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
	tables, err := syncer.DecodeNuverse(b, raw)
	if err != nil {
		return err
	}
	res, err := master.WriteTables(master.DirSink(*out), tables.Tables)
	if err != nil {
		return err
	}
	syncer.LogTables(*region, tables, res)
	return nil
}

func cmdOnce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	region := fs.String("region", "", "区服：jp en cn hmt kr")
	out := fs.String("out", "", "写到本地目录")
	repoURL := fs.String("repo", "", "数据仓库地址（https://github.com/… 或本地裸仓库路径）")
	work := fs.String("work", "", "--repo 时的本地 git 目录（默认在临时目录下）")
	base := fs.String("app-meta", "", "app-meta 地址（默认 GitHub raw；file:///路径 表示本地目录）")
	force := fs.Bool("force", false, "版本没变也重新下载、写出")
	register := fs.Bool("register", false, "jp / en：没有账号时注册一个")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if !segaRegion(*region) && !nuverseRegion(*region) {
		return usageError{"未知区服（可选：jp en cn hmt kr）"}
	}

	var target publish.Target
	switch {
	case *out != "" && *repoURL != "":
		return usageError{"--out 和 --repo 只能用一个"}
	case *out != "":
		target = &publish.Dir{Root: *out}
	case *repoURL == "":
		return usageError{"需要 --out 或 --repo"}
	}

	// 先检查各区服自己的配置，再访问网络
	var nvCfg nuverse.Config
	var store sega.AccountStore
	if nuverseRegion(*region) {
		var err error
		if nvCfg, err = nuverseConfig(*region); err != nil {
			return err
		}
	} else {
		f := os.Getenv("MOESEKAI_" + strings.ToUpper(*region) + "_ACCOUNT_FILE")
		if f == "" {
			return usageError{*region + " 需要 MOESEKAI_" + strings.ToUpper(*region) + "_ACCOUNT_FILE"}
		}
		store = sega.FileStore(f)
	}

	b, err := appMetaClient(*base).Latest(ctx, *region)
	if err != nil {
		return err
	}
	if target == nil {
		app, err := githubApp()
		if err != nil {
			return err
		}
		dir := *work
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "moesekai-sync", strings.NewReplacer("/", "_", ":", "_").Replace(*repoURL))
		}
		r, err := newRepo(*repoURL, dir, app)
		if err != nil {
			return err
		}
		if target, err = publish.OpenGit(ctx, r); err != nil {
			return err
		}
	}

	opt := syncer.Options{Force: *force} // 单次运行不删除任何表（没有「连续两次缺失」的依据）
	var res *syncer.Result
	if nuverseRegion(*region) {
		res, err = syncer.Nuverse(ctx, nuverse.New(nvCfg), b, target, opt)
	} else {
		res, err = syncer.Sega(ctx, sega.New(sega.Configs[*region], b), store, *register, target, opt)
	}
	if err != nil {
		return err
	}
	if res.Commit != "" {
		slog.Info("已推送", "commit", res.Commit)
	}
	return nil
}
