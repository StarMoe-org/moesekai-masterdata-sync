package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/builder"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/syncer"
)

type fakeMeta struct {
	version string
	calls   int
	err     error
}

func (f *fakeMeta) get(context.Context, string) (*appmeta.Bundle, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	b := &appmeta.Bundle{}
	b.Meta.Region, b.Meta.AppVersion, b.Meta.AppHash = "jp", f.version, "h-"+f.version
	return b, nil
}

type fakeBuilder struct{ calls int }

func (f *fakeBuilder) RequestBuild(_ context.Context, region string) (*builder.Request, error) {
	f.calls++
	return &builder.Request{RequestID: fmt.Sprintf("sync-%s-%d", region, f.calls), Status: "queued"}, nil
}

func newWorker(meta *fakeMeta, fb *fakeBuilder, round func(b *appmeta.Bundle) (*syncer.Result, error)) (*Worker, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	tm := DefaultTiming(5 * time.Minute)
	tm.Jitter = 0
	w := &Worker{
		Region:  "jp",
		Timing:  tm,
		Runner:  RunnerFunc(func(_ context.Context, b *appmeta.Bundle) (*syncer.Result, error) { return round(b) }),
		AppMeta: meta.get,
		Builder: fb,
		Now:     func() time.Time { return now },
	}
	return w, &now
}

func TestNormalAndBackoff(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMeta{version: "7.1.0"}
	var err error
	w, now := newWorker(meta, &fakeBuilder{}, func(*appmeta.Bundle) (*syncer.Result, error) {
		if err != nil {
			return nil, err
		}
		return &syncer.Result{DataVersion: "7.1.0.60", Updated: true, Commit: "c"}, nil
	})
	if d := w.Step(ctx); d != 5*time.Minute {
		t.Errorf("正常间隔 = %v", d)
	}
	if st := w.Status(); st.AppVersion != "7.1.0" || st.DataVersion != "7.1.0.60" || st.Updates != 1 {
		t.Errorf("状态 = %+v", st)
	}
	// app-meta 只在启动和每 30 分钟读
	w.Step(ctx)
	if meta.calls != 1 {
		t.Errorf("app-meta 读了 %d 次", meta.calls)
	}
	*now = now.Add(31 * time.Minute)
	w.Step(ctx)
	if meta.calls != 2 {
		t.Errorf("30 分钟后应重读 app-meta：%d", meta.calls)
	}

	err = errors.New("网络错误")
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if d := w.Step(ctx); d != want {
			t.Errorf("第 %d 次退避 = %v，应为 %v", i+1, d, want)
		}
	}
	if st := w.Status(); st.ConsecutiveFailures != 7 || st.LastError == "" {
		t.Errorf("状态 = %+v", st)
	}
	err = fmt.Errorf("jp：%w", syncer.ErrMaintenance)
	if d := w.Step(ctx); d != 5*time.Minute {
		t.Errorf("维护 = %v", d)
	}
	err = nil
	w.Step(ctx)
	err = errors.New("再次出错")
	if d := w.Step(ctx); d != time.Minute {
		t.Errorf("成功后退避应重置：%v", d)
	}
}

func TestOutdatedFlow(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMeta{version: "7.1.0"}
	fb := &fakeBuilder{}
	rounds := 0
	w, now := newWorker(meta, fb, func(b *appmeta.Bundle) (*syncer.Result, error) {
		rounds++
		if b.Meta.AppVersion == "7.1.0" {
			return nil, fmt.Errorf("jp 7.1.0：%w", syncer.ErrOutdated)
		}
		return &syncer.Result{DataVersion: "7.2.0.10"}, nil
	})
	// 过期、app-meta 没有新版本 → 请求 builder，进入等待
	if d := w.Step(ctx); d != time.Minute {
		t.Errorf("等待间隔 = %v", d)
	}
	if fb.calls != 1 || !w.Status().WaitingBuilder {
		t.Fatalf("应请求 builder 并等待：%d %+v", fb.calls, w.Status())
	}
	// 等待期间：不跑同步、不重复请求，按退避检查 app-meta
	*now = now.Add(time.Minute)
	if d := w.Step(ctx); d != 2*time.Minute || rounds != 1 || fb.calls != 1 {
		t.Errorf("等待中：%v rounds=%d builder=%d", d, rounds, fb.calls)
	}
	// 回调说可以重试 → 下一次检查时重新请求
	w.Notify(&builder.Callback{Region: "jp", Status: "failed", Error: &struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}{"not_ready", "QooApp 还没上架"}})
	w.Step(ctx)
	if fb.calls != 2 {
		t.Errorf("可重试的失败应重新请求：%d", fb.calls)
	}
	// 回调成功、app-meta 已更新 → 换上新版本，恢复同步
	meta.version = "7.2.0"
	w.Notify(&builder.Callback{Region: "jp", Status: "succeeded", Result: "advanced", AppVersion: "7.2.0"})
	if d := w.Step(ctx); d != 5*time.Minute {
		t.Errorf("恢复后应按正常间隔：%v", d)
	}
	if st := w.Status(); st.WaitingBuilder || st.AppVersion != "7.2.0" || st.DataVersion != "7.2.0.10" {
		t.Errorf("状态 = %+v", st)
	}
}

func TestOutdatedButAppMetaAlreadyNewer(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMeta{version: "7.1.0"}
	fb := &fakeBuilder{}
	w, _ := newWorker(meta, fb, func(b *appmeta.Bundle) (*syncer.Result, error) {
		if b.Meta.AppVersion == "7.1.0" {
			meta.version = "7.2.0" // builder 已经发布了新版本，只是还没到刷新时间
			return nil, syncer.ErrOutdated
		}
		return &syncer.Result{DataVersion: "x"}, nil
	})
	if d := w.Step(ctx); d != 0 {
		t.Errorf("app-meta 已有新版本时应立即重试：%v", d)
	}
	if d := w.Step(ctx); d != 5*time.Minute || fb.calls != 0 {
		t.Errorf("不应请求 builder：%v %d", d, fb.calls)
	}
}

func TestNonRetryableFailure(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMeta{version: "7.1.0"}
	fb := &fakeBuilder{}
	w, now := newWorker(meta, fb, func(*appmeta.Bundle) (*syncer.Result, error) { return nil, syncer.ErrMismatch })
	w.Step(ctx)
	w.Notify(&builder.Callback{Region: "jp", Status: "failed", Error: &struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}{"data", "格式不认识"}})
	for range 5 {
		*now = now.Add(30 * time.Minute)
		w.Step(ctx)
	}
	if fb.calls != 1 {
		t.Errorf("不可重试的失败不应自动重发：%d", fb.calls)
	}
	*now = now.Add(4 * time.Hour)
	w.Step(ctx)
	if fb.calls != 2 {
		t.Errorf("超过 RebuildAfter 应再试一次：%d", fb.calls)
	}
}

func TestStartupAppMetaFailure(t *testing.T) {
	meta := &fakeMeta{err: errors.New("GitHub 不可用")}
	w, _ := newWorker(meta, nil, func(*appmeta.Bundle) (*syncer.Result, error) {
		t.Fatal("没有元数据时不应同步")
		return nil, nil
	})
	w.Builder = nil
	if d := w.Step(context.Background()); d != time.Minute {
		t.Errorf("启动时读不到 app-meta 应退避：%v", d)
	}
}
