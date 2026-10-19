package app

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestPollLoginPendingKeepsPolling 锁 pollLogin 的 pending 分支（v2.9.2 回归）：
// wrapPoll / 表内 inline Poll 在「用户尚未在浏览器完成授权」时返回
// (done=nil, unfinished=true, perr=nil)。pollLogin 曾只判 perr==nil 就调用 done ——
// nil 函数值直接 panic，登录流程在首个 tick（2s）静默死亡；defer 复位 login_busy 后
// 面板把复位误读成「登录完成，正在同步账号…」，实际账号从未落盘。
//
// 断言刻意不依赖 panic 捕获：pending 被正确消化时 Poll 才会被调到第 2 次、
// 且流程以「登录已取消」收尾；回归形态下两者分别退化为「只调 1 次」与「loginErr 为空」。
func TestPollLoginPendingKeepsPolling(t *testing.T) {
	kind := provider.WorkBuddy
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var polls atomic.Int32
	orig := loginFlows[kind]
	loginFlows[kind] = &loginFlow{
		Poll: func(*http.Client, string) (func(*App) error, bool, error) {
			if polls.Add(1) >= 2 {
				cancel() // 第二个 tick 仍 pending：流程跨 tick 存活，可以收尾了
			}
			return nil, true, nil // 用户始终没在浏览器完成授权
		},
	}
	t.Cleanup(func() { loginFlows[kind] = orig })

	a := &App{loginKind: kind, loginCtx: ctx, loginCancel: cancel}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.pollLogin(ctx) // 回归形态：首个 tick（2s）即 nil 函数调用 panic，流程死亡
	}()

	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatalf("pollLogin 未在超时内退出（Poll 被调 %d 次）", polls.Load())
	}
	if n := polls.Load(); n < 2 {
		t.Fatalf("Poll 只被调 %d 次：首个 pending tick 就中断了（nil 函数调用 panic 回归）", n)
	}
	if got := a.loginErr; got != "登录已取消" {
		t.Fatalf("期望以「登录已取消」收尾（pending 期间流程持续存活），得到 %q", got)
	}
}
