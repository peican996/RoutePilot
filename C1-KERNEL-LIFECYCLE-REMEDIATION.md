# C1 内核生命周期治理整改报告

- 日期：2026-11-09
- 仓库：`peican996/RoutePilot`（本地 `nexuslink_review` 克隆）
- 分支：`main`，基线 `040b6b4`（已从 `a12da32` 快进，工作区开始前干净、无 stash）
- 范围：仅 C1（内核生命周期治理）。C2 进程身份、C3 订阅/升级/持久化、C4 系统级异常恢复均未改动。
- 环境：Windows 10/11 x64、Go 1.27.3、`CGO_ENABLED=0`、内置 mihomo v1.19.33

---

## 1. 结论摘要

C1 的四个问题均已按「状态机 + 串行化事务 + 可验证就绪 + 明确资源所有权」重做，并补充了并发与故障注入回归测试：

1. **生命周期并发安全**：`Start/Stop/Restart/Close` 由 `opMu` 串行化，状态机明确为 `stopped / starting / running / stopping / failed`；进程资源绑定在 `processRun` 上，不再用 `cmd != nil` 推断运行状态。
2. **Watchdog**：改为独立组件，10 分钟滑动窗口、指数退避（2s→60s）、最多 5 次；区分正常退出/主动停止/异常退出；暂停与恢复与用户操作联动，避免退出后又被拉起。
3. **API 端口与就绪校验**：启动必须通过控制 API `/version` 探测才进入 `running`；端口冲突、启动超时、API 不可访问分别返回不同错误；失败自动清理本次进程。
4. **Job Object**：创建、配置、分配进程的每个 Win32 调用都检查错误；Job 由 Manager 独占并在 `Close` 时释放；分配失败直接中止启动，不再静默降级。

所有新增与既有测试通过，真实 mihomo 冒烟测试通过。**改动尚未提交**，全部位于工作区，等待人工审查。

---

## 2. 修改文件清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/kernel/manager.go` | 修改 | 生命周期状态机、串行化事务、就绪校验、失败清理、资源回收 |
| `internal/kernel/job_windows.go` | 重写 | Job Object 所有权、错误检查、关闭时机 |
| `internal/kernel/watchdog.go` | 新增 | 滑动窗口 + 指数退避 + 上限 + 暂停/恢复 |
| `internal/kernel/api.go` | 修改 | 新增 `CloseIdleConnections`，清理旧内核的空闲连接 |
| `service.go` | 修改 | 接入启动就绪校验、看门狗、手动停止暂停恢复、退出清理 |
| `frontend/src/utils.ts` | 修改 | 状态文案补充 `stopping / failed`，移除 `error` |
| `internal/kernel/lifecycle_test.go` | 新增 | 并发、启动失败、端口、Job、资源清理回归测试 |
| `internal/kernel/watchdog_test.go` | 新增 | 退避、窗口、上限、暂停/恢复测试 |
| `internal/kernel/realkernel_test.go` | 新增 | 真实 mihomo 生命周期冒烟测试（`-tags realkernel`） |
| `internal/kernel/repro_concurrency_test.go` | 修改 | 适配新的 `OnExit(ExitInfo)` 与 Job 错误返回 |

统计：6 个已跟踪文件 `+660 / -183`，另有 4 个新增测试/实现文件（约 1084 行）。

---

## 3. 问题根因与修改前后差异

### 3.1 Start / Stop / Restart 并发安全

**根因**

- `Start` 只在锁内判断 `m.cmd != nil`，随后立即解锁再执行 `cmd.Start()`。两个 goroutine 可以同时通过检查，产生两个 mihomo。
- `Stop` 与 `Restart` 没有互斥：`Restart = Stop + sleep + Start`，期间可被其他 `Start/Stop` 插入。
- `state` 在 `cmd.Start()` 成功后立刻置为 `running`，与「内核真的可用」无关。
- `cmd`、`exited`、`started`、`stopping` 分散在 Manager 上，新旧两次启动会互相覆盖；收尾依赖轮询 `cmd == nil`。

**修改后**

- 新增 `opMu` 串行化 `Start/Stop/Restart/Close`；并发调用按获取锁的顺序线性化，`Restart` 在同一个事务内完成 Stop + Start。
- 状态机：`stopped/failed → starting → running`；`running → stopping → stopped`；`starting → failed`；`running → failed`（异常退出）。
- 每次启动创建独立的 `processRun{cmd,pid,started,exited,done,intentional,exitErr}`，Manager 只持有当前 run；`waitProcess` 是唯一 `cmd.Wait()` 调用点，负责关闭日志、清理 cmd/PID、状态迁移。
- `Starting` 状态下重复 `Start` 返回 `ErrAlreadyRunning`，不会创建进程；`Stop` 会等待当前事务结束，再按新状态执行。
- `Stop` 等待 `run.done`（而不是轮询），超时进入 `failed` 并返回 `ErrStopTimeout`，不伪造成功。
- `IsRunning()` 只看状态 `running`，不再看 `cmd` 是否为 nil。
- `Close()` 停止进程、关闭 Job，并禁止后续 `Start`（`ErrManagerClosed`）。

### 3.2 Watchdog 自动恢复

**根因**

- `failCount` 由回调闭包持有，重启成功后立刻清零；「启动即崩」会被无限快速重启。
- 退避在 `OnExit` 回调里 `time.Sleep`，多个退出事件会生成多个休眠 goroutine，可能并发重启。
- 没有窗口概念，失败次数只在进程生命周期内有效；`Quitting` 检查与重启之间存在竞态。
- 重启失败没有明确状态与日志闭环。

**修改后**

- 新增 `kernel.Watchdog`：10 分钟窗口、最多 5 次、`2s/4s/8s/16s/32s/60s` 指数退避、上限 60s。
- 失败只在窗口内累计；一次运行持续时间 ≥ 1 分钟后再次崩溃，视为新故障重新计数，而不是「启动成功即清零」。
- 同一时刻最多一个待执行重启定时器；重复退出事件只计数不叠加重启。
- `Pause()`：用户手动停止内核时取消待执行重启，并忽略暂停期间的退出事件，解决「停止与崩溃并发后又把内核拉起」。
- `Resume()`：只由用户手动 Start/Restart 调用；看门狗自身重启不清零计数，避免死循环。
- `Stop()`：退出程序时永久停止；`MarkQuitting` 与 `Shutdown` 都会先调用。
- 重启失败计入次数并记录日志；超过上限后发出一次 `kernel-crashed` 事件，状态由 Manager 置为 `failed`。
- 回调在释放 Manager 锁之后执行，回调内阻塞不会卡住 `Restart/Stop`（有专门测试）。

### 3.3 mihomo API 端口管理

**根因**

- `pickPort` 只在 Boot 时执行一次，`Start` 成功即认为内核可用；端口被占用、进程活着但 API 不可达都表现为「运行中」。
- 启动失败没有清理进程，`afterStart` 异步等待 15 秒后静默返回。
- Stop 后立即 Start 没有验证端口是否真正释放。

**修改后**

- `Manager.SetStartupValidation(addr, probe, timeout)`：启动后轮询真实 API（RoutePilot 使用 `/version`），成功才进入 `running`；`starting` 期间 `IsRunning()` 为 false。
- 三类错误明确区分，可用 `errors.Is` 判断：
  - `ErrPortInUse`：端口被其他监听占用（启动前快速失败；或超时后确认端口仍被监听）。
  - `ErrStartTimeout`：超时、地址可绑定但没有监听。
  - `ErrAPIUnreachable`：端口能建立 TCP 连接，但 HTTP 健康检查失败。
  - `ErrProcessExited`：进程在就绪前退出（携带退出原因，不等满超时）。
- 就绪失败统一走 `abortStart`：标记主动结束、Kill、等待 `done`、清理 `run`，状态置 `failed`；只有确认收尾才清空进程引用，避免旧进程存活时再启动第二个。
- 端口预检查对 Windows `TIME_WAIT` 做了容错：绑定失败但无法建立 TCP 连接时，不直接判占用，最终以启动后的 API 探测为准。
- 停止/重启/异常退出后调用 `Client.CloseIdleConnections()`，避免复用到旧进程的 keep-alive 连接。

### 3.4 Windows Job Object

**根因**

- 旧实现 `attachToJob(pid)` 返回 void，完全忽略 `CreateJobObjectW`、`SetInformationJobObject`、`OpenProcess`、`AssignProcessToJobObject` 的返回值；Job 句柄是包级全局变量，无所有权、无关闭路径；失败静默降级。

**修改后**

- `jobObject` 由 Manager 独占，惰性创建，`Close()` 时关闭；`KILL_ON_JOB_CLOSE` 保证客户端被强杀时内核一并结束。
- 所有 Win32 调用都检查错误并返回可诊断信息；分配失败时 `Start` 会 Kill 并回收进程，返回 `ErrJobObjectFailed`，不再假装受管。
- 使用 `golang.org/x/sys/windows` 的强类型 API，避免手写结构体与 `uintptr` 计算错位。
- 兼容性：Windows 8+ 支持嵌套 Job，Win10/11 正常；若宿主进程处于禁止嵌套的 Job（主要是 Win7），分配会失败并中止启动，错误会记录到日志。

---

## 4. 新增测试用例

### 4.1 生命周期并发（`internal/kernel/lifecycle_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestConcurrentStartOnlyOneInstance` | 100 个 goroutine 同时 Start，仅 1 个成功；PID 有效；Stop 后进程不存活 |
| `TestStartWhileStartingRejected` | `starting` 期间再次 Start 返回 `ErrAlreadyRunning`，PID 不变，`IsRunning()` 为 false |
| `TestStartStopConcurrentConsistency` | 20 轮 Start/Stop 并发，最终状态与 PID 始终一致，无中间态残留 |
| `TestRestartStopConcurrent` | Restart 与 Stop 并发不产生半停半起 |
| `TestConcurrentRestartAllSucceed` | 5 个并发 Restart 全部成功，最终单实例运行 |
| `TestIntentionalStopNotReported` | 主动 Stop 不触发 OnExit |
| `TestNormalExitNotReported` | exit code 0 正常退出不触发自动恢复 |
| `TestUnexpectedExitReportsInfo` | 非零退出上报退出码与运行时长，状态 `failed` |
| `TestExitCallbackConcurrentWithRestart` | OnExit 回调阻塞时 Restart 仍可完成，证明回调不持锁 |

### 4.2 启动失败与资源清理

| 用例 | 验证点 |
|---|---|
| `TestStartFailsWhenAPIPortInUse` | 端口占用时快速返回 `ErrPortInUse`，不产生进程 |
| `TestStartTimeoutCleansProcess` | API 不可达超时返回 `ErrStartTimeout`；根据 PID 文件确认进程被清理 |
| `TestAPIUnreachableClassified` | 端口有监听但 HTTP 不健康，返回 `ErrAPIUnreachable` |
| `TestProcessExitDuringStartup` | 进程提前退出返回 `ErrProcessExited`，不等待满超时 |
| `TestJobObjectAssignInvalidPID` | 无效 PID 返回错误，不再静默 |
| `TestStartAfterCloseRejected` | Close 后 Start 返回 `ErrManagerClosed` |

### 4.3 Watchdog（`internal/kernel/watchdog_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestBackoffDelay` | 2/4/8/16/30/30 秒指数退避与封顶 |
| `TestWatchdogSingleRestartForBurst` | 连续 5 个退出事件只触发 1 次重启 |
| `TestWatchdogStopsOnUserExit` | Stop 后不再重启 |
| `TestWatchdogExhaustsAfterMaxRestarts` | 超过上限停止重启，只通知一次 |
| `TestWatchdogRestartFailureCountsTowardLimit` | 重启失败计入上限，不会无限重试 |
| `TestWatchdogStableRunResetsCounter` | 长稳运行后崩溃重新计数 |
| `TestWatchdogPrunesExpiredFailures` | 窗口外失败被清理 |
| `TestWatchdogPauseCancelsRecovery` | 手动停止后取消待执行重启，并忽略后续退出 |
| `TestWatchdogResumeReenables` | 手动启动后恢复自动恢复 |

### 4.4 真实内核（`internal/kernel/realkernel_test.go`，构建标签 `realkernel`）

用仓库内置 `assets/mihomo.exe` 启动真实内核，验证：API 就绪后进入 `running`、`/version` 可访问、Stop 后进程不存活、API 端口释放、Manager 可关闭。

---

## 5. 测试执行命令与结果

基线（修改前）已执行，全部通过：

```powershell
go test -count=1 ./...
```

修改后：

```powershell
# 全量测试
go test -count=1 ./...
# ok routepilot
# ok routepilot/internal/autostart
# ok routepilot/internal/configgen
# ok routepilot/internal/kernel
# ok routepilot/internal/subscription

# 内核包连续 3 轮，检查并发用例稳定性
go test -count=3 ./internal/kernel/...
# ok routepilot/internal/kernel  12.358s

# 既有复现用例（T05-T09 等，构建标签 repro）
go test -count=1 -tags repro ./internal/kernel
# ok routepilot/internal/kernel  5.412s

# 真实 mihomo 冒烟
go test -tags realkernel -count=1 -run TestRealKernelLifecycle -v ./internal/kernel
# === RUN   TestRealKernelLifecycle
#     realkernel_test.go:75: 真实内核已就绪: pid=20148 version=v1.19.33
# --- PASS: TestRealKernelLifecycle (0.26s)

# 静态检查
go vet ./...
# 无输出
gofmt -l .
# 无输出

# 前端构建（状态文案变更）
cd frontend; npm run build
# ✓ built in 146ms
```

**Race Detector 不可用**：当前 `CGO_ENABLED=0`，`go test -race` 直接失败（`-race requires cgo`）。本轮通过串行化设计、锁内状态断言与高并发重复测试覆盖，但未获得 race detector 证据。

---

## 6. 尚未解决的问题

1. **C2/C3/C4 未执行**：进程身份仍以 EXE basename 为主；订阅 SSRF、内核升级原子性、系统代理持久化、休眠/唤醒与断电恢复不在本次范围。
2. **未做完整实机验收**：未验证 UI 连续点击 Start/Stop、管理员权限 + TUN、系统代理接管/还原、休眠唤醒、长时间稳定性；这些属于 C4。
3. **启动耗时增加**：真实内核代码路径现在会等待 API 就绪，正常约 0.2-1 秒；若进程存活但 API 永久不可用，`Boot` 最多阻塞 15 秒后才显示失败。属于正确性优先的取舍，若需要可后续改为后台就绪。
4. **端口预检查不是绝对保证**：检查与真正绑定之间仍有竞态，最终判定以启动后 API 探测为准；Windows `TIME_WAIT` 已做容错，但极端 backlog 场景仍可能延迟发现。
5. **Job Object 兼容性**：Win10/11 嵌套 Job 正常；Win7 等不支持嵌套且宿主已在 Job 中时会拒绝启动。需在目标系统矩阵中确认。
6. **旧文档命名冲突**：仓库根目录既有的 `C1-REMEDIATION.md` 针对的是上一轮「系统代理事务」，与本阶段 C1（内核生命周期）不是同一事项，本次未修改该文件。

---

## 7. 兼容性风险

| 变更 | 风险 | 处理 |
|---|---|---|
| `State` 由 `error` 改为 `failed` | 外部脚本或旧前端若匹配 `"error"` 会失效 | 前端 `STATE_TEXT` 已更新；托盘只判断是否 `running`；无其他后端消费者 |
| `Manager.OnExit` 由 `func(string)` 改为 `func(ExitInfo)` | 仅内部调用 | 已更新 `service.go` 与 `_test.go`，`go build ./...` 通过 |
| Job 分配失败由静默降级改为中止启动 | 极端宿主 Job 限制下无法启动内核 | 返回 `ErrJobObjectFailed` 并写日志；Win10/11 正常 |
| Restart 去掉固定 150ms 等待 | 依赖 `cmd.Wait()` 与就绪探测保证端口释放 | 真实内核冒烟与并发 Restart 测试通过 |
| `Stop` 失败现在返回错误并保留 `failed` | 上层需要处理错误 | 现有调用方已返回错误；不会伪造成功 |
| 启动期间 `IsRunning()` 为 false | UI 会显示「启动中」而非「运行中」 | 已有 `starting` 状态文案 |

---

## 8. 是否需要人工实机验证

需要，且不能用单元测试替代：

- 管理员权限下开启 TUN，重复 Start/Stop/Restart，确认无残留 `mihomo.exe`、无端口占用。
- 系统代理开启时强杀客户端，确认 Job Object 生效、下次启动能恢复代理（与 C3 联动）。
- 内核运行中手动结束 `mihomo.exe`，观察 Watchdog 的退避、提示与上限行为；确认客户端退出时不会重启。
- 重复点击 UI 启动/停止按钮，确认不会出现两个内核或状态卡死。
- 休眠/唤醒、网络断开/恢复后的内核状态与 API 可用性（C4）。
- 内核升级中断、替换失败后的回滚（C3，本次未改动）。

---

## 9. Git diff 摘要

```
 frontend/src/utils.ts                     |   3 +-
 internal/kernel/api.go                    |   9 +
 internal/kernel/job_windows.go            | 141 ++++----
 internal/kernel/manager.go                | 512 ++++++++++++++++++++++++------
 internal/kernel/repro_concurrency_test.go |  17 +-
 service.go                                | 161 ++++++++--
 6 files changed, 660 insertions(+), 183 deletions(-)

 新增（未跟踪）：
 internal/kernel/lifecycle_test.go   471 行
 internal/kernel/watchdog.go         252 行
 internal/kernel/watchdog_test.go    246 行
 internal/kernel/realkernel_test.go  115 行
```

关键提交前状态：

- 分支：`main`，HEAD `040b6b4`（与 `origin/main` 一致）。
- 工作区：上述修改与新增文件全部**未提交**。
- 未执行 `git add` / `git commit` / `git push`。
- 未修改 `C2`、`C3`、`C4` 相关代码；未改动订阅、升级、系统代理事务逻辑。

---

## 10. 提交状态明细

| 内容 | 状态 |
|---|---|
| 内核状态机与生命周期串行化 | 工作区，未提交 |
| 启动就绪/端口错误分类与失败清理 | 工作区，未提交 |
| Job Object 加固 | 工作区，未提交 |
| Watchdog 重写与 Service 接入 | 工作区，未提交 |
| API 空闲连接清理 | 工作区，未提交 |
| 前端状态文案 | 工作区，未提交（`frontend/dist` 已重新构建，产物被 gitignore） |
| 回归测试与真实内核冒烟测试 | 工作区，未提交 |

本阶段到此暂停，等待人工审查后再决定是否提交与进入 C2。
