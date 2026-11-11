package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/builder"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/gitrepo"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/httpapi"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/kube"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/master"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/nuverse"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/publish"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/sega"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/syncer"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/worker"
)

// workDirLimit：数据仓库本地副本超过这个大小就清空重来（emptyDir 申请 2Gi）。
const workDirLimit = 512 << 20

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// serveConfig 是 serve 的配置（设计文档 §9）。
type serveConfig struct {
	Regions     []string
	Poll        map[string]time.Duration
	Repos       map[string]string
	WorkDir     string
	AppMetaBase string
	BuilderURL  string // "none" 表示不请求 builder
	CallbackURL string
	HMACSecret  []byte
	AccountName string // jp 账号 Secret
	AccountFile string // 本机调试：用文件代替 Secret
	Register    bool
	Listen      string
}

func loadServeConfig() (*serveConfig, error) {
	c := &serveConfig{
		Poll:        map[string]time.Duration{},
		Repos:       map[string]string{},
		WorkDir:     env("MOESEKAI_WORK_DIR", "/work"),
		AppMetaBase: os.Getenv("APP_META_BASE"),
		BuilderURL:  env("BUILDER_URL", "http://pjsk-meta-builder.apps.svc.cluster.local:8080"),
		CallbackURL: env("SYNC_CALLBACK_URL", "http://moesekai-masterdata-sync.apps.svc.cluster.local:8080/v1/app-meta/callback"),
		AccountName: env("ACCOUNT_SECRET_NAME", "moesekai-masterdata-sync-accounts"),
		AccountFile: os.Getenv("MOESEKAI_JP_ACCOUNT_FILE"),
		Register:    env("MOESEKAI_JP_REGISTER", "true") == "true",
		Listen:      ":" + env("PORT", "8080"),
	}
	for _, r := range strings.Split(env("MOESEKAI_REGIONS", "jp,cn"), ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if r != "jp" && r != "cn" {
			return nil, usageError{fmt.Sprintf("MOESEKAI_REGIONS：不支持的区服 %q（目前只有 jp、cn）", r)}
		}
		up := strings.ToUpper(r)
		d, err := time.ParseDuration(env("MOESEKAI_POLL_"+up, "5m"))
		if err != nil || d < 30*time.Second {
			return nil, usageError{fmt.Sprintf("MOESEKAI_POLL_%s 要是不小于 30s 的时长", up)}
		}
		c.Regions = append(c.Regions, r)
		c.Poll[r] = d
		c.Repos[r] = env("MOESEKAI_REPO_"+up, "https://github.com/StarMoe-org/moesekai-masterdata-"+r)
	}
	if len(c.Regions) == 0 {
		return nil, usageError{"MOESEKAI_REGIONS 为空"}
	}
	s, err := secret("MOE_HMAC_SECRET")
	if err != nil {
		return nil, err
	}
	if len(s) < 32 {
		return nil, usageError{"MOE_HMAC_SECRET（或 _FILE）至少 32 个字符，与 builder 共用"}
	}
	c.HMACSecret = []byte(s)
	return c, nil
}

func cmdServe(ctx context.Context) error {
	cfg, err := loadServeConfig()
	if err != nil {
		return err
	}
	app, err := githubApp()
	if err != nil {
		return err
	}
	meta := appMetaClient(cfg.AppMetaBase)
	var bc worker.BuildRequester
	if cfg.BuilderURL != "none" {
		bc = &builder.Client{Base: cfg.BuilderURL, Callback: cfg.CallbackURL, Secret: cfg.HMACSecret}
	}
	heavy := &sync.Mutex{}
	workers := map[string]*worker.Worker{}
	for _, region := range cfg.Regions {
		r, err := newRepo(cfg.Repos[region], filepath.Join(cfg.WorkDir, region), app)
		if err != nil {
			return err
		}
		runner, err := newRunner(region, cfg, r)
		if err != nil {
			return err
		}
		workers[region] = &worker.Worker{
			Region:  region,
			Timing:  worker.DefaultTiming(cfg.Poll[region]),
			Runner:  runner,
			AppMeta: meta.Latest,
			Builder: bc,
			Heavy:   heavy,
		}
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&httpapi.Server{Version: version, Secret: cfg.HMACSecret, Workers: workers}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	slog.Info("已启动", "version", version, "listen", cfg.Listen, "regions", cfg.Regions, "builder", cfg.BuilderURL != "none")

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	wg.Wait()
	slog.Info("已停止")
	return err
}

// newRunner 为一个区服创建每轮的同步函数：每轮重新拉取数据仓库的目录树；元数据换版本时换新的客户端。
func newRunner(region string, cfg *serveConfig, repo *gitrepo.Repo) (worker.Runner, error) {
	guard := master.NewDeleteGuard()
	after := func(res *syncer.Result) {
		if res == nil || res.Commit == "" {
			return
		}
		if reset, err := repo.ResetIfLarger(workDirLimit); err != nil {
			slog.Warn("清理本地仓库副本失败", "region", region, "err", err)
		} else if reset {
			slog.Info("本地仓库副本过大，已清空（下轮重新拉取目录树）", "region", region)
		}
	}
	switch region {
	case "cn":
		ncfg, err := cnConfig()
		if err != nil {
			return nil, err
		}
		if ncfg.UserID == 0 {
			return nil, usageError{"cn 需要 MOESEKAI_CN_UID（或 _FILE）"}
		}
		client := nuverse.New(ncfg)
		return worker.RunnerFunc(func(ctx context.Context, b *appmeta.Bundle) (*syncer.Result, error) {
			t, err := publish.OpenGit(ctx, repo)
			if err != nil {
				return nil, err
			}
			res, err := syncer.Nuverse(ctx, client, b, t, syncer.Options{Guard: guard})
			after(res)
			return res, err
		}), nil

	case "jp":
		var store sega.AccountStore
		if cfg.AccountFile != "" {
			store = sega.FileStore(cfg.AccountFile)
		} else {
			sc, err := kube.InCluster(cfg.AccountName)
			if err != nil {
				return nil, fmt.Errorf("jp 账号存储：%w（本机运行请设 MOESEKAI_JP_ACCOUNT_FILE）", err)
			}
			store = &kube.AccountStore{Secret: sc, Region: region}
		}
		var client *sega.Client
		return worker.RunnerFunc(func(ctx context.Context, b *appmeta.Bundle) (*syncer.Result, error) {
			if client == nil || client.Bundle != b {
				client = sega.New(sega.JP, b) // 新版本：重新查 game-version、拿 cookie
			}
			t, err := publish.OpenGit(ctx, repo)
			if err != nil {
				return nil, err
			}
			res, err := syncer.Sega(ctx, client, store, cfg.Register, t, syncer.Options{Guard: guard})
			after(res)
			return res, err
		}), nil
	}
	return nil, fmt.Errorf("不支持的区服 %s", region)
}
