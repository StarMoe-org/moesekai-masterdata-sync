// Package syncer 是一轮同步：问游戏服务器当前版本 → 有新 master 就下载、还原、写出 → 提交。
// once 和 serve 共用；调度、退避和与 builder 的配合在 worker 包里。
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/master"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/nuverse"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/publish"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/schema"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/sega"
)

var (
	// ErrOutdated：app-meta 里的版本已被服务器淘汰（426 / not_available），需要新的元数据（设计文档 §6）。
	ErrOutdated = errors.New("app-meta 里的版本已被服务器淘汰")
	// ErrMaintenance：服务器维护中（503）。
	ErrMaintenance = errors.New("服务器维护中")
	// ErrMismatch：master 和结构文件对不上（Nuverse，§5.3），同样需要新的元数据。
	ErrMismatch = errors.New("master 和结构文件对不上")
)

// Options 控制一轮同步。
type Options struct {
	Force bool                // 版本没变也重新下载、写出
	Guard *master.DeleteGuard // nil 时不删除任何表
}

// Result 是一轮的结果。
type Result struct {
	DataVersion string // 服务器当前的 dataVersion
	Updated     bool   // 这轮下载并写出了 master
	Changed     int    // 内容变化的表数
	Commit      string // 推送的提交（没有变化或写本地目录时为空）
}

// Nuverse 跑一轮 Nuverse 服：login → 比较 cdnVersion → 下载、还原、写出 → 提交。
func Nuverse(ctx context.Context, c *nuverse.Client, b *appmeta.Bundle, t publish.Target, opt Options) (*Result, error) {
	region := b.Meta.Region
	st, err := c.Login(ctx, b)
	if errors.Is(err, nuverse.ErrMaintenance) {
		return nil, fmt.Errorf("%s：%w", region, ErrMaintenance)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("login", "region", region, "version", b.Meta.NuverseAppVersion(), "http", st.HTTPStatus,
		"status", st.AppVersionStatus, "dataVersion", st.DataVersion, "cdnVersion", st.CDNVersion)
	if st.Outdated() {
		return nil, fmt.Errorf("%s %s（HTTP %d，%s）：%w", region, b.Meta.NuverseAppVersion(), st.HTTPStatus, st.AppVersionStatus, ErrOutdated)
	}
	res := &Result{DataVersion: st.DataVersion}
	cur, err := t.Current(ctx)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.CDNVersion >= st.CDNVersion && !opt.Force {
		slog.Info("没有新的 master", "region", region, "current", cur.CDNVersion)
		return res, nil
	}
	raw, url, err := c.DownloadMaster(ctx, st.CDNVersion)
	if err != nil {
		return nil, err
	}
	slog.Info("已下载", "region", region, "url", url, "bytes", len(raw))
	tables, err := DecodeNuverse(b, raw)
	if err != nil {
		return nil, err
	}
	raw = nil
	existing := master.TablesIn(t.Files())
	wr, err := master.WriteTables(t, tables.Tables)
	if err != nil {
		return nil, err
	}
	LogTables(region, tables, wr)
	deleteMissing(region, t, opt.Guard, existing, wr.Produced)
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
	return finish(ctx, t, v, res, len(wr.Written))
}

// DecodeNuverse 解密 + 解码 + 按结构文件还原。解密、解码或还原失败都算 ErrMismatch（密钥或结构文件不对）。
func DecodeNuverse(b *appmeta.Bundle, raw []byte) (*schema.Result, error) {
	if b.Schema == nil {
		return nil, fmt.Errorf("%s 当前版本没有 schema.json：%w", b.Meta.Region, ErrMismatch)
	}
	s, err := schema.Parse(b.Schema)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	plain, err := b.Keys.API().Decrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("解密 master：%v：%w", err, ErrMismatch)
	}
	v, err := msgpack.Decode(plain)
	if err != nil {
		return nil, fmt.Errorf("解码 master：%v：%w", err, ErrMismatch)
	}
	plain = nil
	t1 := time.Now()
	res, err := s.Restore(v)
	if err != nil {
		return nil, fmt.Errorf("还原 master：%v：%w", err, ErrMismatch)
	}
	slog.Info("解码完成", "region", b.Meta.Region, "bytes", len(raw), "decode", t1.Sub(t0).Round(time.Millisecond), "restore", time.Since(t1).Round(time.Millisecond))
	return res, nil
}

// LogTables 记录一次还原和写出的统计。
func LogTables(region string, tables *schema.Result, wr *master.WriteResult) {
	slog.Info("表已生成", "region", region, "tables", len(wr.Produced), "changed", len(wr.Written), "unchanged", wr.Unchanged,
		"unknown", tables.Unknown, "dropped", tables.Dropped, "expanded", len(tables.Expanded))
}

// Sega 跑一轮 CP 服：（第一次时）game-version + cookie → 登录 → 比较 dataVersion → 逐个分片写出 → 提交。
// 同一个 Client 跨轮复用：cookie 失效时 Login 会自己重新获取。
func Sega(ctx context.Context, c *sega.Client, store sega.AccountStore, register bool, t publish.Target, opt Options) (*Result, error) {
	region, m := c.Config.Region, c.Bundle.Meta
	if !c.Prepared() {
		if err := c.Prepare(ctx); err != nil {
			return nil, mapSega(region, err)
		}
	}
	acct, created, err := sega.EnsureAccount(ctx, c, store, register)
	if err != nil {
		return nil, mapSega(region, err)
	}
	if created {
		slog.Warn("已注册新账号并保存", "region", region)
	}
	s, err := c.Login(ctx, acct)
	if err != nil {
		return nil, mapSega(region, err)
	}
	slog.Info("login", "region", region, "version", m.AppVersion, "status", s.AppVersionStatus,
		"dataVersion", s.DataVersion, "assetVersion", s.AssetVersion, "splits", len(s.SplitPaths))
	if s.AppVersionStatus == "not_available" {
		return nil, fmt.Errorf("%s %s（%s）：%w", region, m.AppVersion, s.AppVersionStatus, ErrOutdated)
	}
	res := &Result{DataVersion: s.DataVersion}
	cur, err := t.Current(ctx)
	if err != nil {
		return nil, err
	}
	if cur != nil && master.CompareVersions(cur.DataVersion, s.DataVersion) >= 0 && !opt.Force {
		slog.Info("没有新的 master", "region", region, "current", cur.DataVersion)
		return res, nil
	}
	existing := master.TablesIn(t.Files())
	total := &master.WriteResult{Produced: map[string]bool{}}
	t0 := time.Now()
	err = c.Master(ctx, func(split *msgpack.Map) error {
		for _, k := range split.Keys {
			if name, _ := k.(string); total.Produced[name] {
				return fmt.Errorf("表 %s 出现在多个分片里", name)
			}
		}
		wr, err := master.WriteTables(t, split)
		if err != nil {
			return err
		}
		for n := range wr.Produced {
			total.Produced[n] = true
		}
		total.Written = append(total.Written, wr.Written...)
		total.Unchanged += wr.Unchanged
		return nil
	})
	if err != nil {
		return nil, mapSega(region, err)
	}
	slog.Info("表已生成", "region", region, "tables", len(total.Produced), "changed", len(total.Written),
		"unchanged", total.Unchanged, "took", time.Since(t0).Round(time.Millisecond))
	deleteMissing(region, t, opt.Guard, existing, total.Produced)
	v := master.Version{
		Layout:             master.LayoutSega,
		AppHash:            m.AppHash,
		SystemProfile:      c.Profile(),
		AppVersion:         s.AppVersion,
		MultiPlayVersion:   s.MultiPlayVersion,
		AssetVersion:       s.AssetVersion,
		AppVersionStatus:   s.AppVersionStatus,
		DataVersion:        s.DataVersion,
		AssetHash:          s.AssetHash,
		RemoveAssetVersion: s.RemoveAssetVersion,
	}
	return finish(ctx, t, v, res, len(total.Written))
}

func mapSega(region string, err error) error {
	switch {
	case errors.Is(err, sega.ErrOutdated):
		return fmt.Errorf("%s：%v：%w", region, err, ErrOutdated)
	case errors.Is(err, sega.ErrMaintenance):
		return fmt.Errorf("%s：%w", region, ErrMaintenance)
	}
	return err
}

func deleteMissing(region string, t publish.Target, guard *master.DeleteGuard, existing []string, produced map[string]bool) {
	if guard == nil {
		return
	}
	for _, name := range guard.Decide(existing, produced) {
		slog.Warn("删除表", "region", region, "table", name)
		t.Delete("master/" + name + ".json")
	}
}

func finish(ctx context.Context, t publish.Target, v master.Version, res *Result, changed int) (*Result, error) {
	if err := master.WriteVersion(t, v); err != nil {
		return nil, err
	}
	commit, err := t.Finish(ctx, "Sekai master data version "+v.DataVersion)
	if err != nil {
		return nil, err
	}
	res.Updated, res.Changed, res.Commit = true, changed, commit
	return res, nil
}
