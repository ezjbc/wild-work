# wild-work v2.6.1 — 修复小浣熊「浏览器授权登录」不可用 + 登录失败不再谎报成功

> 发布主题：**修复 v2.6.0 小浣熊渠道的浏览器授权登录完全不可用**（协议回调入口未接线），
> 以及一个把**所有渠道**的登录失败都显示成「登录完成」的 UX 缺陷。
> 另含面板渠道按钮改为两行五列、渠道名「商汤小浣熊」简化成「小浣熊」。

---

## 🐛 严重修复

### 小浣熊「浏览器授权登录」完全不可用（v2.6.0 引入）

**现象**：在面板点「＋ 小浣熊」→ 浏览器打开官方授权页 → 点授权 → 回到 wild-work，
**账号没有加进来**，界面也无任何报错，等了 5 分钟后提示「登录完成」但账号列表不变。

**根因**：协议回调的**入口从未接进 `main.go`**。

小浣熊的授权码经自定义深链 `office-raccoon://auth/callback?code=…` 回传，
实现方式是登录期间临时改写注册表：

```
HKCU\Software\Classes\office-raccoon\shell\open\command
  (Default) = "D:\...\wild-work.exe" --raccoon-callback "%1"
```

`internal/raccoon` 包把 `CallbackFlag = "--raccoon-callback"` 和 `SaveCallback()` 都写好并测过，
但 **`cmd/wild-work/main.go` 从未解析这个 flag**。于是链路变成：

1. 用户点授权 → Windows 按注册表命令行唤起 `wild-work.exe --raccoon-callback "<深链>"`
2. 该 exe **不知道**自己是回调子进程 → 又启动了一整份 daemon
3. 新实例监听 7863 失败（`bind: Only one usage of each socket address`，日志里能看到），
   更致命的是它的**启动自愈**发现「上次登录残留的协议改写」→ 立刻
   `RestoreProtocol()` + `ClearCallback()`，把注册表恢复并把回调文件删掉
4. 常驻进程的轮询永远读不到授权码 → 一路等到 5 分钟超时

全程**不报错、不崩溃**，只是功能静默失效。

**修法**：`main.go` 在 `os.Chdir(workDir())` 之后、**任何初始化之前** 拦下该入口：

```go
if handled, err := handleRaccoonCallback(os.Args[1:]); handled {
    …  // 只落盘并 os.Exit，绝不启动第二份服务/托盘
}
```

并把「回调已落盘 / 处理失败」追加写进 `data/app.log`
（Windows 的 `windowsgui` 构建**无控制台**，否则失败时无任何线索可查）。

**实测**（完整链路）：回调子进程 **52ms** 退出、不产生第二份 daemon、不抢端口、
注册表在兑换后正确恢复；用假授权码跑完整链路能拿到上游的真实错误
（`authorization_code_not_found_error`），证明「回调 → 落盘 → 主进程兑换」已打通。

### 所有渠道的登录失败都被显示成「登录完成」（长期存在）

**现象**：任何渠道登录失败/超时/被拒，面板轮询到 `login_busy=false` 后一律 toast
「**登录完成**，正在同步账号…」，用户以为是成功的，直到发现账号没出现。
本次小浣熊的问题之所以难定位，正是被这条消息掩盖了。

**根因**：`/api/state` 只回传 `login_busy` 布尔值，**没有终态错误通道**；
前端 `startLoginPoll` 只看这一个字段就宣告成功。

**修法**：

- 后端新增 `login_error` 字段（`pollLogin` 的超时/取消/终态失败均写入，
  下一次 `startLogin` 时清空）；读取**非破坏式**（`/api/state` 有多个调用方，
  若读取即清空会被别的调用偷走）。
- 前端轮询到 `!login_busy` 时先看 `login_error`：非空则 toast 真实的失败原因
  （如「登录超时（未收到授权回调），请重新发起登录」）并且**不再显示成功**。

---

## 🔧 其它变更

- **面板渠道按钮改为两行五列**（新渠道向右扩展，不再起第三行）：
  - 行1 = WorkBuddyCN / QoderCN / TraeWork / MonkeyCode / Loomy
  - 行2 = WorkBuddyAI / QoderCOM / 千问办公 / 小浣熊 / 智谱清言
- 渠道显示名「商汤小浣熊」→「**小浣熊**」（`CH_LABEL` / 按钮 / README / AGENTS）。
  注意：`internal/raccoon/protocol*.go` 里的 `商汤小浣熊.exe` **保持不变**——
  它是官方客户端真实文件名，是注册表回调的识别目标。

---

## 🧪 回归测试

新增 `cmd/wild-work/raccoon_callback_test.go`，锁定这类「**声明了却没接线**」的疏漏
（与 R34「新增渠道漏了 `go xxxSch.Run()`」同族）：

- `TestRaccoonCallbackEntryPointIsHandled`：静态检查 `main.go` 确实调用了
  `handleRaccoonCallback`、位置在任何服务初始化之前、分支里有 `os.Exit`、
  判据引用常量而非字面量。（去掉修复即失败。）
- `TestRaccoonCallbackHandlerBehavior`：参数不匹配不接管；缺深链报错；
  非法深链照常落盘（带 Err）；正常深链可被 `raccoon.LoadCallback` 读回。

---

## 📎 升级提示

- **v2.6.0 用户请升级**：小浣熊授权登录在 v2.6.0 下不可用（「从客户端导入」不受影响，一直是好的）。
- 无需迁移任何数据；`state-*.json` / `auths/` 格式未变。
