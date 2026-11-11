// Package worker 是每个区服的主循环（设计文档 §6、§10 O4）：按间隔跑一轮同步，出错退避；
// 版本过期或数据对不上结构文件时刷新 app-meta，没有新版本就请求 builder，暂停到回调或 app-meta 更新。
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/builder"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/syncer"
)

// Runner 用给定的 app 元数据跑一轮同步。
type Runner interface {
	Round(ctx context.Context, b *appmeta.Bundle) (*syncer.Result, error)
}

// RunnerFunc 让普通函数实现 Runner。
type RunnerFunc func(ctx context.Context, b *appmeta.Bundle) (*syncer.Result, error)

func (f RunnerFunc) Round(ctx context.Context, b *appmeta.Bundle) (*syncer.Result, error) {
	return f(ctx, b)
}

// BuildRequester 是 builder.Client 的接口（测试可替换）。
type BuildRequester interface {
	RequestBuild(ctx context.Context, region string) (*builder.Request, error)
}

// Timing 是各种间隔，默认值见 DefaultTiming。
type Timing struct {
	Interval       time.Duration // 正常轮询间隔
	Jitter         float64       // ±比例
	MinBackoff     time.Duration // 出错后第一次等待
	MaxBackoff     time.Duration
	Maintenance    time.Duration // 503 后固定等待
	AppMetaRefresh time.Duration // 正常时多久刷新一次 app-meta（兜底漏掉的回调）
	RebuildAfter   time.Duration // 等 builder 期间多久重新请求一次（上次失败且不可重试时也会重发）
}

// DefaultTiming：设计文档 O4。
func DefaultTiming(interval time.Duration) Timing {
	return Timing{
		Interval:       interval,
		Jitter:         0.1,
		MinBackoff:     time.Minute,
		MaxBackoff:     30 * time.Minute,
		Maintenance:    5 * time.Minute,
		AppMetaRefresh: 30 * time.Minute,
		RebuildAfter:   6 * time.Hour,
	}
}

// Status 是对外展示的状态（/metrics、/healthz）。
type Status struct {
	Region              string
	AppVersion          string
	DataVersion         string
	WaitingBuilder      bool
	Rounds, Updates     int64
	Failures            int64 // 累计失败轮数
	ConsecutiveFailures int
	LastSuccess         time.Time
	LastUpdate          time.Time
	LastError           string
}

// Worker 是一个区服的循环。
type Worker struct {
	Region  string
	Timing  Timing
	Runner  Runner
	AppMeta func(ctx context.Context, region string) (*appmeta.Bundle, error)
	Builder BuildRequester // 可为 nil：不请求 builder，只等 app-meta 更新
	// Heavy 是各区服共用的锁：同一时间只有一个区服在跑一轮（下载、解码占内存）。
	Heavy *sync.Mutex
	Now   func() time.Time

	wake chan struct{}

	mu            sync.Mutex
	st            Status
	bundle        *appmeta.Bundle
	nextMeta      time.Time
	forceMeta     bool
	backoff       time.Duration
	buildAt       time.Time // 上次请求 builder 的时间；零值表示这次等待还没请求
	buildRetry    bool      // 回调说可以重试：下次检查时重新请求
	lastFailedCB  *builder.Callback
	lastRequestID string
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) init() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wake == nil {
		w.wake = make(chan struct{}, 1)
	}
	w.st.Region = w.Region
	if w.Heavy == nil {
		w.Heavy = &sync.Mutex{}
	}
}

// Status 返回当前状态的副本。
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.st
	s.Region = w.Region
	return s
}

// Wake 让循环立刻跑下一步。
func (w *Worker) Wake() {
	w.init()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Notify 处理 builder 的回调（只处理本区服的）。
func (w *Worker) Notify(cb *builder.Callback) {
	w.init()
	w.mu.Lock()
	switch {
	case cb.Status == "succeeded":
		// advanced 时 app-meta 已经更新；其他结果也重新读一次，以仓库为准
		w.forceMeta = true
		w.lastFailedCB = nil
		slog.Info("builder 回调", "region", w.Region, "requestId", cb.RequestID, "result", cb.Result, "appVersion", cb.AppVersion)
	case cb.Retryable():
		w.buildRetry = true
		slog.Warn("builder 处理失败，稍后重新请求", "region", w.Region, "requestId", cb.RequestID, "kind", cb.Error.Kind, "message", cb.Error.Message)
	default:
		w.lastFailedCB = cb
		kind, msg := "", ""
		if cb.Error != nil {
			kind, msg = cb.Error.Kind, cb.Error.Message
		}
		slog.Error("builder 处理失败，需要人工处理", "region", w.Region, "requestId", cb.RequestID, "kind", kind, "message", msg)
	}
	w.mu.Unlock()
	w.Wake()
}

// Run 一直跑到 ctx 结束。
func (w *Worker) Run(ctx context.Context) {
	w.init()
	for {
		delay := w.Step(ctx)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-w.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Step 跑一步，返回下一步之前要等多久。导出给测试用。
func (w *Worker) Step(ctx context.Context) time.Duration {
	w.init()
	now := w.now()

	w.mu.Lock()
	needMeta := w.bundle == nil || w.forceMeta || w.st.WaitingBuilder || !now.Before(w.nextMeta)
	w.mu.Unlock()
	if needMeta {
		changed, err := w.refreshMeta(ctx)
		if err != nil {
			w.mu.Lock()
			noBundle := w.bundle == nil
			w.mu.Unlock()
			if noBundle {
				return w.fail(err)
			}
			slog.Warn("刷新 app-meta 失败，沿用当前版本", "region", w.Region, "err", err)
		} else if changed {
			w.mu.Lock()
			w.st.WaitingBuilder, w.buildAt, w.buildRetry, w.lastFailedCB = false, time.Time{}, false, nil
			w.mu.Unlock()
		}
	}

	w.mu.Lock()
	waiting, b := w.st.WaitingBuilder, w.bundle
	w.mu.Unlock()
	if waiting {
		w.maybeRequestBuild(ctx)
		return w.nextBackoff()
	}

	w.Heavy.Lock()
	res, err := w.Runner.Round(ctx, b)
	w.Heavy.Unlock()
	if ctx.Err() != nil {
		return 0
	}

	switch {
	case err == nil:
		w.mu.Lock()
		w.st.Rounds++
		w.st.ConsecutiveFailures, w.st.LastError, w.backoff = 0, "", 0
		w.st.LastSuccess = w.now()
		w.st.DataVersion = res.DataVersion
		if res.Updated {
			w.st.Updates++
			w.st.LastUpdate = w.st.LastSuccess
		}
		w.mu.Unlock()
		if res.Updated {
			slog.Info("已更新", "region", w.Region, "dataVersion", res.DataVersion, "changed", res.Changed, "commit", res.Commit)
		}
		return w.jitter(w.Timing.Interval)

	case errors.Is(err, syncer.ErrOutdated) || errors.Is(err, syncer.ErrMismatch):
		w.record(err)
		slog.Warn("需要新的 app 元数据", "region", w.Region, "err", err)
		// 先看 app-meta 有没有更新的版本：有就直接换上重试
		if changed, merr := w.refreshMeta(ctx); merr == nil && changed {
			return 0
		}
		w.mu.Lock()
		w.st.WaitingBuilder = true
		w.backoff = 0
		w.mu.Unlock()
		w.maybeRequestBuild(ctx)
		return w.nextBackoff()

	case errors.Is(err, syncer.ErrMaintenance):
		w.record(err)
		slog.Info("服务器维护中", "region", w.Region)
		return w.Timing.Maintenance

	default:
		return w.fail(err)
	}
}

// refreshMeta 重新读 app-meta；appVersion 或 appHash 变了返回 true。
func (w *Worker) refreshMeta(ctx context.Context) (bool, error) {
	b, err := w.AppMeta(ctx, w.Region)
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forceMeta = false
	if err != nil {
		w.nextMeta = now.Add(w.Timing.MinBackoff)
		return false, err
	}
	w.nextMeta = now.Add(w.Timing.AppMetaRefresh)
	if w.bundle != nil && w.bundle.Meta.AppVersion == b.Meta.AppVersion && w.bundle.Meta.AppHash == b.Meta.AppHash {
		return false, nil
	}
	if w.bundle != nil {
		slog.Info("app 元数据已更新", "region", w.Region, "from", w.bundle.Meta.AppVersion, "to", b.Meta.AppVersion)
	}
	w.bundle = b
	w.st.AppVersion = b.Meta.AppVersion
	return true, nil
}

// maybeRequestBuild 在等待期间按需请求 builder：第一次、回调说可重试、或距上次超过 RebuildAfter。
func (w *Worker) maybeRequestBuild(ctx context.Context) {
	if w.Builder == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	due := w.buildAt.IsZero() || w.buildRetry || now.Sub(w.buildAt) >= w.Timing.RebuildAfter
	if due && w.lastFailedCB != nil && now.Sub(w.buildAt) < w.Timing.RebuildAfter {
		due = false // 不可重试的失败：等人处理，或到 RebuildAfter 再试
	}
	w.mu.Unlock()
	if !due {
		return
	}
	req, err := w.Builder.RequestBuild(ctx, w.Region)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		slog.Warn("请求 builder 失败，稍后再试", "region", w.Region, "err", err)
		return
	}
	w.buildAt, w.buildRetry, w.lastRequestID = now, false, req.RequestID
	if req.Existing {
		slog.Info("builder 已在处理这个区服，等它的回调", "region", w.Region, "requestId", req.RequestID, "status", req.Status)
	} else {
		slog.Info("已请求 builder", "region", w.Region, "requestId", req.RequestID)
	}
}

func (w *Worker) record(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Rounds++
	w.st.Failures++
	w.st.ConsecutiveFailures++
	w.st.LastError = err.Error()
}

func (w *Worker) fail(err error) time.Duration {
	w.record(err)
	d := w.nextBackoff()
	slog.Error("同步失败", "region", w.Region, "err", err, "retryIn", d.Round(time.Second))
	return d
}

// nextBackoff：MinBackoff 起翻倍，最长 MaxBackoff，带抖动。
func (w *Worker) nextBackoff() time.Duration {
	w.mu.Lock()
	if w.backoff == 0 {
		w.backoff = w.Timing.MinBackoff
	} else {
		w.backoff = min(w.backoff*2, w.Timing.MaxBackoff)
	}
	d := w.backoff
	w.mu.Unlock()
	return w.jitter(d)
}

func (w *Worker) jitter(d time.Duration) time.Duration {
	if w.Timing.Jitter <= 0 {
		return d
	}
	f := 1 + w.Timing.Jitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * f)
}
