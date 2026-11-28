# RoutePilot 内核生命周期与并发专项审计（子 Agent 2）

## 一、头部

**审计范围**：mihomo 内核进程的启停、watchdog 自动恢复、Job Object 归属、PID 身份、退出清理与相关异步任务。

**基线**：`39ca02c1aa9303fc14fe8264d2715432cd50f030`（`git rev-parse HEAD` 实测一致，`git status --short` 为空，工作区干净）。审计期间**未做任何代码写操作**，唯一写入文件为本报告。

**审阅文件清单（`wc -l` 实测）**

| 文件 | 行数 | 说明 |
|---|---|---|
| `internal/kernel/manager.go` | 770 | 核心状态机、Start/Stop/Restart/Close、Wait 收尾 |
| `internal/kernel/watchdog.go` | 341 | 退避、上限、定时器序号、Pause/Resume |
| `internal/kernel/api.go` | 289 | 控制 API 客户端（探活用的 `/version`） |
| `internal/kernel/job_windows.go` | 118 | Job Object（kill-on-close） |
| `internal/safego/safego.go` | 62 | panic 兜底 |
| `internal/procid/procid.go` / `tracker.go` / `source_windows.go` | 258 / 260 / 131 | 进程身份与 PID 复用判定（分流用，非内核句柄） |
| `main.go` | 513 | 进程生命周期、单实例、退出路径 |
| `service.go` | 6054 | 只读其中内核启停相关段（L608-721、L1046-1240、L1327-1565、L2689-2972） |
| 相关测试 | lifecycle_test 533 / lifecycle_barrier_test 175 / start_exit_race_test 132 / repro_concurrency_test 190 / watchdog_test 293 / watchdog_r1_test 304 | 覆盖度评估 |

**执行过的命令与结果**

| 命令 | 结果 |
|---|---|
| `git rev-parse HEAD` / `git status --short` | 基线一致，无改动 |
| `wc -l internal/kernel/*.go internal/safego/safego.go internal/procid/*.go main.go service.go` | 见上表 |
| `go test -count=1 -race -run 'TestConcurrentStartOnlyOneInstance\|TestStartStopConcurrentConsistency\|TestRestartStopConcurrent\|TestConcurrentRestartAllSucceed\|TestStopDuringStartingWaitsForTransaction' ./internal/kernel/` | `ok routepilot/internal/kernel 2.741s`（这些用例本身不暴露数据竞争） |
| `grep -rn "OpenProcess\|TerminateProcess\|taskkill\|KillProcessByName" --include=*.go .` | 除 `job_windows.go:84` 外无按 PID/名字杀进程的实现 → 见 Q3 |
| `grep -rn "safego.Go\|safego.Run\|goSafe\|tickSafe"` | 内核侧仅 `manager.go:430`（report=nil）；service 侧全部为"循环 + tickSafe 单轮" |
| `git log --oneline -18` | 确认 e9172ff/bbf912e/1f48bbd 已整改过看门狗调度与启动竞态，本报告结论均落在当前行号上的**残留缺陷** |

**结论摘要**：P0 = 0，P1 = 0，**P2 = 3**，**P3 = 8**。

最严重一条：watchdog 的定时器一旦触发，其"重启内核"的动作无法被随后的用户 Start/Stop/Restart 撤销（`Watchdog.fire` 的门禁只检查触发瞬间的 paused，`restartKernel` 只检查 quitting），导致"用户点了停止/启动之后，内核又被看门狗拉起或再重启一次"。

---

## 二、10 个必答问题的结论

**Q1 并发状态竞争 / 互斥 / 幂等 / 代际**：Manager 层由 `opMu`（`manager.go:254`、`349`、`613`、`663`、`673`）串行化全部生命周期事务，`mu`（`256`）只护状态快照；Service 层由 `lifeMu`（`service.go:233`、`1433`、`1473`、`1522`、`1328`）串行化 Start/Stop/Restart/Shutdown。代际（`gen`/`genCancel`，`service.go:277-278`）只保护 afterStart 的**副作用**，不保护"谁来启停内核"。**未加锁窗口只有一处实质缺陷**：status/pre-check 与实际执行之间的"意图"没有被记录，于是 watchdog 与人工操作可以互相覆盖（A2-01）。另有一处非原子字段赋值（A2-11）。幂等性：Stop 幂等（`manager.go:620-623`），Start 幂等靠 `run != nil || state ∈ {running, starting}`（`360-363`）。

**Q2 是否可能同时存在两个内核进程**：**结构上不会**。Start 全程持 `opMu`，`m.run` 只在"确认进程已死"后清空（`462-464`、`550-552`、`583`），Job 分配失败/启动失败都先 kill 再 Wait（`406-413`、`538-545`）。唯一后果是 A2-01 的"多余的一次重启"（进程逐个串行，不会并存）。

**Q3 Stop 是否误杀**：不会。全部杀进程走 `exec.Cmd.Process.Kill()`（`407`、`538`、`636`），即 CreateProcess 时的进程句柄，与 PID 复用无关；仓库内不存在按镜像名/`taskkill` 杀进程的代码（grep 实测）。Job Object 只包含本 Manager `assign` 过的 PID（`job_windows.go:73-98`），范围不过宽。`assign` 用 `OpenProcess(pid)` 二次取句柄，存在理论上的"cmd.Start 与 assign 之间 PID 被回收"窗口，但 Windows PID 需整体回绕才复用，实际不可达（见"已排除"）。

**Q4 readiness 超时或 secret 不对是否留半死进程**：不留。`abortStart` 先置 `intentional`，再 `Kill`，再等 `r.done` 最多 5s（`530-557`）；`TestStartTimeoutCleansProcess` 已覆盖。secret 不对时 probe 返回 401 → 15s 后 `classifyStartFailure` 归类 `ErrAPIUnreachable`（`517-520`）→ 同一清理路径。唯一残留问题：5s 内没清理干净时保留 `run`，导致后续 Start 报"已在运行"（A2-08）。

**Q5 内核自行退出后状态是否及时同步**：及时。`waitProcess` 在 `cmd.Wait` 返回后立刻在 `mu` 内改状态（`579-592`），前端 1.2s 轮询 `Status()`（`service.go:1389-1395`）即可见；崩溃原因写入 `lastErr`。**例外**：非主动的 exit code 0 被当成"正常停止"，状态静默变成 `stopped` 且不回调（A2-05）。

**Q6 watchdog 是否可能无限重启**：不会失控。`MaxRestarts=5`（`service.go:2895`）+ 10 分钟窗口 + `StableAfter=1min` 重新计数（`watchdog.go:133-144`、`261-274`）+ 指数退避 2s→60s（`326-341`），且没有忙循环。耗尽是终态，只有人工 Start/Restart（`Resume`）能清零（`241-252`）。

**Q7 goroutine 是否泄漏**：没有可增长的泄漏。每次 Start 只多一个 `waitProcess` goroutine（`430`）与 exec 内部 copier，两者都随进程退出结束；`afterStart` 任务有 15s ctx 上界（`1124`）且 Shutdown 会 `afterWG.Wait()`（`1108`）。详见"已排除"。

**Q8 panic 恢复后后台任务是否永久停止**：service 侧不会——所有周期任务都是"外层 for + `tickSafe` 单轮"（`service.go:2998-3007`、`3855-3870`、`5490-5505`、`main.go:473-511`）。内核侧有一处例外：`manager.go:430` 的 report 传 `nil`，panic 后收尾逻辑半途而废会把内核管理永久卡死（A2-03）；另有 watchdog 定时器回调没有 recover（A2-04）。

**Q9 PID 复用是否造成误判**：不会。Manager 从不按 PID 认进程（只持句柄）；`procid` 侧用 `Created` 时间戳与 `FileID` 判 PID 复用/文件替换（`tracker.go:213-223`、`source_windows.go:21-39`），并在采纳快照路径前实时复核（`tracker.go:139-151`）。这套机制只服务"连接→进程身份"，不参与内核判断。

**Q10 应用退出时是否有资源未释放**：主路径完整——watchdog 先 Stop（`1340`）、代际失效并等待后置任务（`1345-1346`）、还原系统代理（`1348-1357`）、关闭 HTTP 空闲连接（`1358-1360`）、`mgr.Close()` 停进程并关 Job 句柄（`1361-1365`）；`stopLocked` 超时的情况下 Job 的 kill-on-close 兜底（`job_windows.go:100-118`）。未释放项只有"Stop 超时后不等进程真正死亡就返回"（A2-07），以及 `Manager.Close` 在 `stopErr != nil` 时吞掉 `jobErr`（`685-688`，CloseHandle 失败概率可忽略）。

---

## 三、问题清单

### A2-01（P2）watchdog 已触发的重启无法被人工操作取代 —— "停止后被拉起 / 启动后被二次重启"

- **文件 / 函数**：`internal/kernel/watchdog.go` `Watchdog.fire`；`service.go` `restartKernel` / `StartKernel` / `StopKernel`
- **代码位置**：`watchdog.go:147-172`（fire 的门禁与 `restarting=true`）、`watchdog.go:306-312`（schedule）；`service.go:1525-1527`（restartKernel 只检查 quitting/shuttingDown）、`service.go:1543-1553`（无条件 `mgr.Restart`）、`service.go:1464-1468`（resume 在 Start 之后）、`service.go:1507-1511`（pause 在 Stop 之后）
- **描述**：`fire` 只在"进入 fire 的瞬间"检查 `stopped/paused/exhausted`（`156-159`），一旦通过就置 `restarting=true` 并释放 `w.mu` 去调用 `Restart()`。此刻起的任何 `Pause()/Resume()/Stop()` 都无法撤销这次重启：`Pause` 只能 `stopTimer`（`228-237`，但 timer 已被 fire 置 nil），`restartKernel` 内部也不看 watchdog 状态，只检查 `Quitting/IsShuttingDown`。于是"崩溃 → 2s 定时器 → 定时器到点 → 重启"这条链，会跨过用户在此期间发起的手动操作。
- **触发条件（因果链，确定性）**：
  1. T0 内核异常退出，`OnExit` → `wd.ReportExit` → 排 2s 定时器（`watchdog.go:122-124`）。
  2. T0+Δ（Δ<2s）用户点"启动"。`StartKernel` 持 `lifeMu` 进入 `mgr.Start`，探测就绪需要 200ms~1.5s（`readyPollInterval=200ms`，`manager.go:69`），最坏 15s（`service.go:694`）。
  3. T0+2s 定时器到点，`fire` 门禁通过（此时 `paused=false`）→ `restarting=true` → `restart()` → `restartKernel(false)` **阻塞在 `lifeMu`**。
  4. `mgr.Start` 成功，`StartKernel` 调 `resumeWatchdog()`（`Resume` 只停了已被消费的 timer）、返回并释放 `lifeMu`。
  5. `restartKernel(false)` 拿到 `lifeMu`：quitting=false → `mgr.Restart(confPath)` → **把刚启动好的内核杀掉再起一个**。
- **实际影响**：用户刚点"启动"成功的内核在数百毫秒~数秒后被无名重启一次；`State` 经历 `running→stopping→starting→running` 抖动，前端 1.2s 轮询可能看到 `failed`/`starting`；`OnRetry` 在 attempt≥2 时还会弹"内核反复退出"。同一根因在 Stop 方向表现为"点了停止之后内核又自己起来"（窗口更窄：`mgr.Stop` 在进程已死时只需几毫秒，但 `SetSystemProxy(false)` 的注册表写入与 `waitAfterTasks` 会把窗口拉到数十毫秒）。
- **根因**：`Watchdog` 只对"待执行的定时器"负责，对"已进入执行的回调"没有撤销点；`restartKernel` 没有"本次崩溃是否已被人工操作取代"的判据（缺少意图 epoch/代际）。
- **修复建议**：给 `Watchdog` 增加单调自增的 `intentEpoch`（`Pause/Resume/Stop` 内自增），`ReportExit` 时把当时的 epoch 随回调下传；`restartKernel(epoch uint64)` 在**取得 lifeMu 之后**比对 `wd.Epoch() == epoch`，不等则 `return nil`（视为已被人工操作取代）。备选：在 `StartKernel/StopKernel/restartKernel` 的**开头**（持 lifeMu 后）调用 `wd.CancelInflight()`，并让 `fire` 通过一个 `aborted chan` 在调用 `restart()` 前后各查一次。
- **测试方案**：见 T-A/T-B/T-C（`service.go` 增加仅测试用的 `watchdogBaseDelay` 注入点即可确定性复现）。
- **置信度**：高（代码路径确定），线上触发频率：中（崩溃后 0.5~2s 内点启动/重启是最自然的人类操作）。

### A2-02（P2）Boot 失败路径不创建 watchdog，之后手动启动成功的内核永久没有自动恢复

- **文件 / 函数**：`service.go` `Boot`、`startKernelWatchdog`、`resumeWatchdog`/`pauseWatchdog`
- **代码位置**：`service.go:710-713`（`mgr.Start` 失败 → `failBoot` 后 **return**）、`service.go:718`（`startKernelWatchdog` 只在成功路径）、`service.go:2940`（`mgr.OnExit` 只在这里赋值）、`service.go:2955-2972`（resume/pause 对 nil 静默返回）
- **描述**：`s.watchdog` 与 `mgr.OnExit` 的唯一赋值点在 `startKernelWatchdog`，而它位于 Boot 成功路径的末尾。Boot 中任何一步失败（`EnsureBinary`、`randomSecret`、`configgen.Write`、`mgr.Start` 端口冲突/超时）都会 `failBoot` 后直接 return，watchdog 永久缺席。
- **触发条件**：首次启动失败（例如上次异常退出残留的 mihomo 占着 API 端口，或订阅/配置生成失败），用户在界面上修复后点"启动"成功。
- **实际影响**：该会话内内核崩溃后**没有人自动重启**：`mgr.OnExit` 为 nil（`manager.go:600` 直接跳过）、`pauseWatchdog/resumeWatchdog` 是 no-op。前端只看到状态变 `failed`，且不会收到 `kernel-crashed`。同时 `startAutoUpdate/startHistorySampler/startNetworkHealthCheck` 也一并缺失（`716-719`），后台能力整体降级，且**无任何提示**。
- **根因**：后台基础设施的安装与"内核是否启动成功"耦合在同一个顺序流程里，缺少"确保安装"的幂等入口。
- **修复建议**：把 `startKernelWatchdog`（及另三个后台任务）提到 `mgr.EnsureBinary` 成功之后、`mgr.Start` 之前调用，或在 `StartKernel/restartKernel` 里做 `if s.watchdog == nil { s.startKernelWatchdog() }` 的惰性安装（`OnExit` 赋值需幂等）。
- **测试方案**：T-G。
- **置信度**：高（纯代码路径），触发频率：低-中（取决于 Boot 失败率）。

### A2-03（P2）`waitProcess` 的 panic 兜底是空的，且收尾未放 defer → panic 后内核管理永久卡死

- **文件 / 函数**：`internal/kernel/manager.go` `startLocked` / `waitProcess`；`internal/safego/safego.go` `protect`
- **代码位置**：`manager.go:430`（`safego.Go("内核进程等待", …, nil)`）；`safego.go:43-51`（`report == nil` 时直接 return，连日志都没有）；`manager.go:571-597`（`m.run = nil`、`close(r.exited)`、`close(r.done)` 都在函数体中段，没有 defer）
- **描述**：注释（`429`）明确写"收尾逻辑里有状态机切换，panic 不能把整个客户端带走"，但报告器传 nil，panic 被静默吞掉；更关键的是 panic 一旦发生在 `close(r.exited)`（`577`）之前，`run` 不会清空、`exited/done` 都不会关闭。
- **触发条件**：`waitProcess` 内任何 panic（`cmd.Wait`、`w.file.Close`、`close(r.exited)` 双关、`exitReason`）。
- **实际影响**：① 之后每次 `Start` 永远返回 `ErrAlreadyRunning`（`360-363`）；② `Stop`/`abortStart` 等 `r.done` 5s 后返回 `ErrStopTimeout`（`640-649`）；③ `OnExit` 不触发 → 无自动恢复；④ 日志里查不到任何线索。等于一次 panic 把内核管理砖化到进程退出。
- **根因**：兜底只做了"进程不死"，没做"资源仍被正确归还"；关键收尾不是 defer。
- **修复建议**：① 传真正的 reporter（复用 `m` 上的日志回调 / `service.bgReport`）；② `waitProcess` 用 `defer` 保底执行 `close(r.exited)`、`close(r.done)`、状态归一与 `w.file.Close()`；③ 在 `Start` 的拒绝分支里区分"僵尸 run"与"真的在运行"（与 A2-08 合修）。
- **测试方案**：T-D（需要新增一个 `m.callHook("wait-process")` 注入点）。
- **置信度**：机制确定；panic 发生概率低 → 定为 P2。

### A2-04（P3）watchdog 定时器回调没有 panic 兜底，重启回调 panic 会终止整个进程

- **文件 / 函数**：`internal/kernel/watchdog.go` `fire` / `scheduleLocked`
- **代码位置**：`watchdog.go:310`（`time.AfterFunc(delay, func() { w.fire(seq) })`）、`147-215`（`fire` 无 recover）、`127-128`（注释承诺"任何 panic 都不该带走整个进程"，但只对 `OnRetry` 用了 `safego.Go`）
- **描述**：`fire` 运行在 `time.AfterFunc` 自己的 goroutine 里，未 recover 的 panic 会直接 `panic: … goroutine …` 终止进程。而真正"重启内核"的调用恰好在 `restart()`（`171`）这一唯一未受保护的路径上。
- **触发条件**：`restartKernel` → `configgen.Write` / `s.mgr.Restart` / `afterStart` 链路上任意 panic。
- **实际影响**：窗口直接消失，系统代理可能停在死端口（下次启动靠 Pending 恢复兜底）。
- **根因**：保护范围只覆盖"通知类"回调，没覆盖"执行类"回调。
- **修复建议**：`fire` 首行 `defer w.reportPanic("内核自动重启", recover(), debug.Stack())`，并把 `w.restarting = false` 放进 defer（否则 panic 后 `restarting` 永久为 true，即使进程侥幸存活也会让所有后续重启被 `pendingAgain` 吞掉）。
- **测试方案**：T-E（`Restart` 内 panic，期望进程不退出且后续 ReportExit 仍能调度）。
- **置信度**：机制确定，概率低。

### A2-05（P3）非主动的 exit code 0 被当作"正常停止"，不告警、不恢复

- **文件 / 函数**：`internal/kernel/manager.go` `waitProcess`
- **代码位置**：`manager.go:585-591`（`if intentional || err == nil { state = StateStopped }`）、`600`（`err != nil && !intentional` 才回调）
- **描述**：只有"非零退出"才算崩溃。内核被第三方以优雅方式结束（`WM_CLOSE`/CTRL_C，或 mihomo 某些 fatal 路径自行 `os.Exit(0)`）时，Manager 认为"用户停的"，状态变 `stopped`、`lastErr` 清空、`OnExit` 不触发。
- **触发条件**：外部工具/安全软件优雅结束 mihomo；或 mihomo 内部以 0 退出。
- **实际影响**：内核静默消失，watchdog 不介入，前端显示"已停止"而非"异常"，用户会以为是自己的操作。属于"状态撒谎"。
- **根因**：用退出码推断意图，而真实意图已有 `intentional` 标志。
- **修复建议**：非 `intentional` 的退出一律视为异常（`err == nil` 时上报 `ExitCode: 0, Reason: "unexpected exit"`），或在 `StateStopped` 之外引入"外部结束"的标记字段供 UI 提示。
- **测试方案**：T-F。
- **置信度**：代码行为确定；真实世界触发频率需真机验证 mihomo 退出码分布 → 中低。

### A2-06（P3）`Failed` 状态下 Stop 不归一为 `Stopped`，`lastErr` 残留

- **文件 / 函数**：`internal/kernel/manager.go` `stopLocked`
- **代码位置**：`manager.go:620-623`（`(Stopped||Failed) && run==nil` 直接 `return nil`，状态与 `lastErr` 都不动）
- **描述**：内核崩溃后（`state=failed, run=nil`）用户点"停止"，`Stop` 返回成功但状态仍是 `failed`、`lastErr` 仍是上次崩溃原因。
- **实际影响**：UI 可能继续显示红色错误态/错误横幅，用户以为停止没生效；`Status().Error` 会一直误导前端。
- **修复建议**：`Failed + run==nil` 时把 `state` 归一为 `Stopped` 并清 `lastErr`（保留一条历史事件另存）。
- **测试方案**：T-H。
- **置信度**：高（行为确定），影响面小。

### A2-07（P3）`Stopping → Failed` 允许"进程还活着"，随后又被改写为 `Stopped` 并丢掉诊断

- **文件 / 函数**：`internal/kernel/manager.go` `stopLocked` / `waitProcess`
- **代码位置**：`manager.go:640-649`（超时 → `state=Failed, lastErr="停止内核超时"`，但 `m.run` 保留）、`579-591`（进程稍后真死 → `current && intentional` → `state=Stopped, lastErr=""`）
- **描述**：kill 后 5s 内 `done` 未关闭就宣告 `Failed`，此时进程可能仍存活（`Status().PID` 仍非 0）。之后进程死了，`waitProcess` 把 `Failed` 覆盖成 `Stopped` 并清空 `lastErr`。
- **实际影响**：① 存在"状态 Failed 但进程活着"的矛盾窗口（前端/用户无法判断）；② 停止超时的诊断信息被静默抹掉，事后无法复盘。
- **修复建议**：区分"停止超时（进程可能存活）"与"启动失败"两种 Failed，例如增加 `lastErr` 保留策略（`waitProcess` 不覆盖既有 `lastErr`，或追加而不是替换）。
- **置信度**：高（行为确定），触发概率低（kill 是 `TerminateProcess`，几乎不会超时）。

### A2-08（P3）`abortStart` 超时保留 `run` → 后续 Start 报"内核已在运行"，语义误导

- **文件 / 函数**：`internal/kernel/manager.go` `abortStart` / `startLocked`
- **代码位置**：`manager.go:540-556`（`cleaned=false` 时保留 `m.run`，但仍写 `state=Failed`）、`360-363`（`run != nil` → `ErrAlreadyRunning`）
- **描述**：启动清理 5s 未完成时，设计上"保留句柄阻止二次启动"（注释 `548-549`），但对外错误与真实语义不符：状态是 `failed`、`IsRunning()` 为 false，点启动却得到"内核已在运行"。
- **实际影响**：用户被卡在"启动→已在运行→状态又是失败"的循环里；唯一出路是 Stop（`stopLocked` 会再杀一次）或重启应用。若前端在 `failed` 状态隐藏了"停止"按钮，就成了死胡同（需前端确认）。
- **修复建议**：为"僵尸 run"单独返回 `ErrLifecycleBusy`/新增 `ErrCleanupPending`，并在 `Status` 里暴露 `cleanupPending` 供前端显示"正在清理"。
- **置信度**：高（行为确定），触发概率极低。

### A2-09（P3）API/混合/DNS 端口只在 Boot 选一次，重启不重选，也不复核归属

- **文件 / 函数**：`service.go` `Boot` / `restartKernel`；`internal/kernel/manager.go` `checkAddrFree` / `awaitReady`
- **代码位置**：`service.go:679-681`（`pickPort` 后永不变更）、`service.go:688-695`（probe 固定指向 `127.0.0.1:apiPort`）、`manager.go:732-743`（`checkAddrFree` 允许"暂时绑定失败"通过）
- **描述**：端口在启动时选一次并写进配置，之后所有 Restart 复用。若某次重启期间该端口被外部程序占用：`checkAddrFree` 只有在"能建连"时才报占用（`737-741`），若对方尚未 listen 则放行；随后内核绑定失败/或 mihomo 顺延，而 probe 永远指向旧地址 → `ErrPortInUse`/`ErrStartTimeout` 反复失败，并且不会换端口。watchdog 试 5 次后熔断。
- **实际影响**：一次瞬时端口抢占 → 本次会话内核再也起不来，只能重启应用（应用重启会重新 `pickPort`）。
- **修复建议**：`startLocked` 在端口类失败时回调上层"重新选端口并重写配置"（`configgen.Write` 已经接受 `Options`，成本很低），或在 `Manager` 暴露 `SetStartupValidation` 重设入口并在失败时调用。
- **置信度**：中（需要 mihomo 在 external-controller 冲突时是退出还是顺延才能定性 → **静态推断，需 Windows 真机验证**）。

### A2-10（P3）`UpdateCore` / `ResetCore` 的"停止 → 换文件 → 重启"不是原子事务

- **文件 / 函数**：`service.go` `UpdateCore` / `ResetCore`
- **代码位置**：`service.go:3464-3483`（`StopKernel()` → `InstallCore` → `RestartKernel()` 三次独立获取 `lifeMu`）、`service.go:3521-3530`（`StopKernel` → 删文件 → `EnsureBinary` → `RestartKernel`）
- **描述**：`lifeMu` 在这三步之间是释放的。窗口内任何其他启停请求（前端点启动、`maybeAutoRefresh` 的 `RestartKernel`、`SetTUN`）都能插进来。
- **实际影响**：① 升级时，插队的 Start 会让内核重新占用 `mihomo.exe`，Windows 下 `InstallCore` 的替换失败 → 升级失败但用户已经断过一次网；② `ResetCore` 删文件后若被插队 Start，会用"已删除的内核"启动并失败。均不会产生双进程。
- **修复建议**：把"停止+换文件+重启"包进一个 `lifeMu` 事务（例如新增 `withKernelDown(fn func() error)`），或至少在 `InstallCore` 前后用 `mgr` 的状态断言"内核已停止"并在失败时重试一次。
- **置信度**：高（结构确定），触发需并发，概率低。属相邻风险，非本次 10 问直接项。

### A2-11（P3）`Manager.OnExit` 非原子赋值与持锁读取构成数据竞争

- **文件 / 函数**：`service.go` `startKernelWatchdog`；`internal/kernel/manager.go` `waitProcess`
- **代码位置**：`service.go:2940`（`mgr.OnExit = func…`，**不持** `m.mu`）、`manager.go:593`（`cb := m.OnExit`，**持** `m.mu`）
- **描述**：写入方既不持锁也不与读取方建立 happens-before。Boot 顺序保证"赋值发生在首次 `mgr.Start` 之后、任何内核退出之前"只是时序巧合：`Boot` 在 `mgr.Start` 成功（`710`）到 `startKernelWatchdog`（`718`）之间会执行 `beginKernelGeneration`/`startAutoUpdate`/`startHistorySampler`，若内核在这段毫秒级窗口内自行退出，`waitProcess` 就会并发读该字段。
- **实际影响**：竞态窗口极窄；`-race` 命中需要精巧时序。后果是读到 nil 回调 → 漏掉一次自动恢复。
- **修复建议**：给 `Manager` 增加 `SetOnExit(fn)` 并在 `mu` 内赋值，`waitProcess` 已在 `mu` 内读，天然解决。
- **置信度**：机制确定，概率极低。

---

## 四、状态机矩阵（以代码中真实常量为准：`stopped / starting / running / stopping / failed`，`manager.go:32-38`）

代码里**没有** `Restarting`；"重启中"对外表现为 `running → stopping → stopped → starting → running`（`662-669`）。

| From ↓ / To → | stopped | starting | running | stopping | failed |
|---|---|---|---|---|---|
| **stopped** | ✅ 幂等空操作 `620-623`；`waitProcess` 归一 `653-655` | ✅ `374`（前置校验 `360-368`；opMu `349`） | — 无路径（必经 starting） | — 无路径（`run==nil` 时 `stopLocked` 直接归 `stopped` `624-630`） | — 无路径（`failStart` 仅在 `starting` 生效 `562`） |
| **failed** | ❌ **无路径**：`620-623` 早退，状态与 `lastErr` 都不动 → **A2-06** | ✅ `360-363` 只拦 running/starting，允许 failed 重试 | — 无路径 | ⚠️ 仅当 `run != nil`（`abortStart` 超时残留，**A2-08**）时可达 `632-633` | ✅ 幂等（`620-623`；`failStart` `565`） |
| **starting** | ⚠️ 瞬时：`abortStart` 置 `intentional` 后 `waitProcess` 写 `586`，随即被 `553` 改成 failed，不可作为终态观察 | ❌ `360-363` → `ErrAlreadyRunning`（`TestStartWhileStartingRejected` 覆盖） | ✅ `471`，双重校验 `run==r`（`444`）+ `exited` 未关闭（`458-470`） | ✅ `534`（仅 `abortStart`，对外不可见） | ✅ `465`（提交前退出）/ `553`（清理后）/ `563`（未产生进程） |
| **running** | ✅ `586`（`intentional` 或被 kill；**exit 0 也走这里 → A2-05**） | ❌ 无路径（Restart 必须先停 `665-668`） | ❌ `360-363` → `ErrAlreadyRunning` | ✅ `632-633`（`intentional` 同步置位） | ✅ `589`（`waitProcess` 异常退出）；提交后毫秒级死亡属不可阻止窗口，见下 |
| **stopping** | ✅ `586` / `653-655` | ❌ `364-367` → `ErrLifecycleBusy`（因 opMu 串行，实际不可达，防御性死代码） | — 无路径 | ❌ 无路径 | ⚠️ `645`（kill 后 5s 未收尾）→ 进程可能仍存活，**A2-07** |

**未被阻止的格子与后果**

1. `running → failed` 的毫秒级竞态：`startLocked` 在 `471` 提交 `running` 之后、调用方收到成功之前，进程可能刚好退出（`select` 在 `458` 那一瞬还没关闭）。`Start` 会返回成功，随后 `waitProcess` 立刻改写为 `failed` 并触发 `OnExit`。**后果可接受**（watchdog 补偿），但存在"Start 报成功、状态立刻 failed"的瞬时不一致；`start_exit_race_test.go` 只覆盖了 hook 卡住的更早窗口。
2. `stopping → failed` 允许"`Failed` 但进程存活"的语义矛盾（A2-07）。
3. `failed → stopped` 缺失（A2-06），以及 `failed + run!=nil → starting` 被 `360-363` 拦成"已在运行"（A2-08），是"状态集合与生命周期事实不同步"的两处缺口。

结论：状态机本身**闭合**（没有"代码允许但语义非法"的转换），所有转换点都在 `mu` 内、且除 `waitProcess` 外都在 `opMu` 内执行；缺陷集中在"状态标签的含义"而非"并发写入"。

---

## 五、并发时序风险表

| # | 场景 | 交错顺序 | 是否安全 | 依据 |
|---|---|---|---|---|
| 1 | N 个 Start 并发 | 首者 `starting`，余者见 `362` | 安全（只有一个进程，`TestConcurrentStartOnlyOneInstance` 100 goroutine 覆盖） | `manager.go:349-352, 360-363` |
| 2 | Start ∥ Stop | opMu 线性化；Stop 在后则最终 `stopped`，Start 在后则最终 `running` | 安全 | `349` / `613`；`lifecycle_barrier_test.go:13-58` |
| 3 | Start ∥ Close | Close 等 Start 事务结束再停进程并置 `closed` | 安全（`TestCloseDuringStartingLeavesNoProcess`） | `672-679` |
| 4 | 5 个 Restart 并发 | 逐个 `stop→start` | 安全（`TestConcurrentRestartAllSucceed`） | `662-668` |
| 5 | 进程在 `before-running` 退出 | hook 放行后 `select <-r.exited` 命中 → `ErrProcessExited` | 安全（`TestStartRejectsExitBeforeRunningCommit`） | `441-470` |
| 6 | 进程在提交 `running` 后立刻退出 | 短暂 `running` → `failed` | 竞态存在但后果可接受 | `471` vs `589` |
| 7 | 崩溃 ∥ Stop | `intentional` 在 `waitProcess` 读之前置位 → 不误报崩溃 | 安全（`TestIntentionalStopNotReported`） | `632` / `581` / `585` |
| 8 | **watchdog fire ∥ StopKernel** | fire 通过门禁后阻塞在 lifeMu；Stop 先 `mgr.Stop` 再 `pause` → 释放 lifeMu 后重启执行 | **不安全**：停止后被拉起 | `watchdog.go:156-167` + `service.go:1507-1511` → A2-01 |
| 9 | **watchdog fire ∥ StartKernel / RestartKernel** | 人工启动持 lifeMu 期间定时器到点 → 启动完成后立即被 `mgr.Restart` 杀一次 | **不安全**：二次重启、状态抖动 | `service.go:1460-1468, 1543-1563` → A2-01 |
| 10 | watchdog fire ∥ Shutdown | `quitting`/`shuttingDown` 在 `restartKernel` 入口拒绝；`wd.Stop` 先于 `mgr.Close` | 安全 | `1525-1527` / `2947` / `1328-1346` |
| 11 | StartKernel ∥ Shutdown | 同持 lifeMu；Shutdown 在后则 Close 停掉，Shutdown 在前则 `shuttingDown` 拒绝 | 安全，且不会留下双进程 | `1433` / `1328-1333` / `1436` |
| 12 | `OnExit` 赋值 ∥ `waitProcess` 读取 | 无锁写 vs 持锁读 | **数据竞争（窄窗口）** | `2940` vs `593` → A2-11 |
| 13 | Stop 超时 ∥ Start | `run` 保留 → Start 被 `360-363` 拒绝 | 安全（抑制双进程），但错误语义误导 | `548-552` → A2-08 |
| 14 | afterStart ∥ Stop/Restart/Shutdown | 代际失效 + ctx 取消 + `afterWG.Wait` | 安全（`service_generation_test.go` 覆盖） | `1073-1081` / `1345-1346` |
| 15 | Boot 失败 ∥ 之后的手动 Start | watchdog 从未创建 | 功能缺失（无自动恢复），非并发错误 | `710-718` → A2-02 |

---

## 六、必须补的测试清单

> 说明：`service.go` 侧测试可用现成的 `r1NewService`/`r1Service`（`c1_1_r1_proxy_test.go:30-32`）构造 `CoreService`，并像 `c1_1_r1_proxy_test.go:449-450` 那样直接注入 `svc.mgr`（同包，可赋值）与 `svc.watchdog`；内核侧复用 `newFakeManager`（`lifecycle_test.go:40`）与 `m.hook`。

| 编号 | 用例 | 注入点 | 期望 | 断言要点 |
|---|---|---|---|---|
| **T-A** | `TestWatchdogPauseDuringFiredRestartAborts` | `kernel.Watchdog`：`Restart` 回调先 `close(entered)` 再阻塞在 `release`；`BaseDelay=10ms` | 调用 `Pause()` 后再放行，**最终不得产生重启效果** | 修复后：`Restart` 拿到"已被取代"信号直接返回，`restartEffect` 计数为 0；当前代码此用例会 FAIL（ documenting A2-01）。需给 `Restart` 回调传入 epoch/abort 通道，否则无法表达期望 |
| **T-B** | `TestStartKernelNotDoubleRestartedByPendingWatchdog` | `CoreService` 新增仅测试用 `watchdogBaseDelay`（默认 2s，测试设 50ms）；`svc.mgr` 用 e2e 假内核（`newFakeManager` 的等价物，写 PID 文件）；`SetStartupValidation` 的 probe 故意 `sleep 200ms` 再成功 | `StartKernel()` 返回后 1s 内不再产生新 PID | 假内核 PID 文件内容在 `StartKernel` 返回后 500ms、1000ms 两次读取必须相同；`mgr.Restart` 未被调用（可用 PID 变化 + `Status().StartedAt` 双证） |
| **T-C** | `TestStopKernelNotResurrectedByInflightWatchdog` | 同上；先让内核崩一次触发定时器，再在定时器到点前 `go StopKernel()` 并用 `proxyMu` 占锁把 Stop 拖长（`svc.proxyMu.Lock()` 模拟注册表慢） | `StopKernel` 返回后内核保持停止 | 500ms 后 `svc.mgr.Status().State != running`，且无新 PID |
| **T-D** | `TestPanicInWaitProcessDoesNotWedgeManager` | `manager.go:waitProcess` 首行新增 `m.callHook("wait-process")`；hook 内 `panic("boom")` | panic 后 `Stop()` 在 1s 内成功、`Status().State ∈ {stopped, failed}`、`run == nil` | `Stop()` 不返回 `ErrStopTimeout`；随后 `Start()` 不返回 `ErrAlreadyRunning`；日志里能看到 panic 记录（当前 `report=nil` → 无记录，属 A2-03） |
| **T-E** | `TestWatchdogRestartPanicKeepsProcessAlive` | `Restart: func() error { panic("boom") }` | 进程不退出，且下一次 `ReportExit` 仍能调度重启 | 用 `defer/recover` 在测试内捕获不到（跨 goroutine），改为断言：panic 后 `watchdogHasTimer(w)` 或后续 `ReportExit` 能触发第二次 `Restart`；`restarting` 必须已复位 |
| **T-F** | `TestUnexpectedZeroExitIsReported` | `newFakeManager(t, "exit")`（100ms 后 `os.Exit(0)`），设置 `OnExit` | `OnExit` 至少触发一次，或状态为 `failed` 且带原因 | 当前实现：`OnExit` 不触发、状态 `stopped`（A2-05）；修复后断言 `info.ExitCode==0 && info.Reason!=""` |
| **T-G** | `TestBootFailureStillEnablesWatchdog` | 让 `Boot` 在 `mgr.Start` 处失败（预先把 `s.apiPort` 占住，或在 `configgen.Write` 的目标路径上制造不可写） | `Boot` 失败后 `svc.watchdog != nil` 且 `svc.mgr.OnExit != nil` | 直接读 `svc.watchdog`（同包）；再手动 `StartKernel()` 成功后 `Process.Kill()` → 期望 `kernel-crashed` 事件或状态自动恢复 |
| **T-H** | `TestStopFromFailedNormalizesState` | `newFakeManager(t,"crash")` 启动后等 `failed`，再 `Stop()` | `Status().State == stopped` 且 `Error == ""` | 当前实现保持 `failed`（A2-06） |
| **T-I** | `-race` 稳定性回归 | 无（现有用例） | 连续 20 轮无 race 报告 | `go test -race -count=20 -run 'TestConcurrentStartOnlyOneInstance\|TestStartStopConcurrentConsistency\|TestRestartStopConcurrent\|TestConcurrentRestartAllSucceed' ./internal/kernel/` |
| **T-J** | `TestOnExitAssignmentRace`（可选） | 在 `mgr.Start` 成功后立刻 kill 假内核，同时并发赋值 `OnExit` | `-race` 无告警 | 需要 A2-11 修复（`SetOnExit` 持锁）后才有意义 |

---

## 七、已排除的疑点（逐条给代码依据）

1. **重复内核进程**：`Start` 全程持 `opMu`（`manager.go:349-352`），进程创建到提交 `run` 之间不可能被第二次 Start 插入（`396-426` 都在锁内）；`run` 仅在 `waitProcess`（`583`）、`abortStart` 已收尾（`550-552`）、提交前已退出（`462-464`）三种"进程确认已死"的情形下清空。`TestConcurrentStartOnlyOneInstance`（100 goroutine）与 `-race` 实测通过。
2. **按名字杀进程 / 误杀**：全仓库 grep 无 `taskkill`/`TerminateProcess`/按镜像名枚举；唯一 kill 点是 `exec.Cmd.Process.Kill()`（`407`、`538`、`636`），走 CreateProcess 句柄。`job_windows.go:84-96` 的 `OpenProcess(pid)` 是为 `AssignProcessToJobObject` 单独开句柄，理论上有 PID 回收窗口，但需要在 `cmd.Start`→`assign` 的微秒级间隔内发生 PID 回绕，实际不可达。
3. **Job Object 范围过宽**：Job 由单个 Manager 独占创建（`job_windows.go:33-70`），只 `assign` 本 Manager 启动的 PID，kill-on-close 作用于其中进程；嵌套 Job 失败会中止启动并给出可诊断错误（`job_windows.go:22-25`、`manager.go:406-413`），不会静默降级。
4. **readiness 超时留半死进程**：`abortStart` 走 `Kill` + 等 `done`（`530-557`），`TestStartTimeoutCleansProcess` 断言进程确实消失；Job 分配失败路径也 kill+Wait（`406-411`）。
5. **secret 错误 / API 401**：probe 失败 → `classifyStartFailure` 先 dial 再 `bindAddr` 三分归类（`509-526`），最终走同一条 `abortStart` 清理。15s 的等待是设计成本，不是泄漏。
6. **退出状态同步不及时**：`waitProcess` 在 `Wait` 返回即改状态（`579-592`），不依赖轮询；`TestUnexpectedExitReportsInfo` 覆盖退出码与 `failed` 状态。
7. **watchdog 无限重启 / 打满 CPU**：`MaxRestarts` + Window + StableAfter（`watchdog.go:67-84`、`133-144`、`261-274`）+ 指数退避（`326-341`）；`awaitReady` 是 200ms ticker 且同时 select `r.exited`（`496-504`），无忙循环。`TestWatchdogExhaustsAfterMaxRestarts`/`TestBackoffDelay` 覆盖。
8. **goroutine 泄漏**：每次 Start 一个 `waitProcess`（`430`，随进程结束返回）+ exec 内部 copier（`Stdout/Stderr` 非 `*os.File`，`Wait` 会等 copier 结束，见 `os/exec` 语义）；日志泵没有独立 goroutine；`afterStart` 任务有 15s ctx 上界（`service.go:1124`）并被 `afterWG` 追踪（`1085-1108`）。
9. **panic 导致后台任务永久停摆（service 侧）**：全部周期任务都是"外层 for + `tickSafe` 单轮"：网络健康检查 `2998-3007`、流量采样 `3855-3870`、订阅自动更新 `5490-5505`、托盘刷新 `main.go:473-511`。内核侧的缺口另见 A2-03/A2-04。
10. **PID 复用误判**：Manager 不按 PID 认进程；`procid.Tracker` 以 `Created` 判复用（`tracker.go:213-223`）、`ProcessMatches` 用 `GetProcessTimes` 实时复核（`source_windows.go:21-39`）、`LookupName` 在快照失效时强制刷新并降级（`tracker.go:139-151`），且规则键不含 PID/创建时间（`tracker.go:57-58`）。
11. **`stopped → starting` 的 pre-check 竞争**：Service 的 `Status()` 预检（`service.go:1445-1450`）与 `mgr.Start` 之间有窗口，但 `lifeMu` 已把 service 层的启停串行化，且 `Manager.startLocked` 在 `mu` 内做最终判定（`360-363`），不依赖预检。
12. **`StateStopping` 的 `ErrLifecycleBusy` 分支（`364-367`）**：`opMu` 保证 `stopLocked` 返回后不会停留在 `stopping`，故该分支实际不可达，属防御性代码，不算缺陷。
13. **`Manager.Close` 的重复调用**：`stopLocked` 幂等、`closed` 置位、`job.close` 幂等（`job_windows.go:105-108`），测试 `t.Cleanup` + `Shutdown` 双调用安全。
14. **崩溃后不失效内核代际**（曾怀疑）：崩溃时 `gen` 不递增，`afterStart` 旧任务会继续轮询已死内核，但每轮都 `select ctx.Done()` 且总预算 15s（`service.go:1127-1139`），无副作用、有上界，不构成缺陷。
15. **`Boot` 中 `s.mgr = mgr` 未持 `s.mu`（`service.go:659`）**：所有读 `s.mgr` 的 goroutine（`718-719` 起的后台任务、`720` 起的 afterStart）都在该赋值之后创建，写入先于任何并发读者，实际无竞争（若将来 Boot 异步化则需改）。
16. **`awaitReady` 在 `probe` 阻塞期间不响应 `r.exited`**：`probe` 是 `api.Version`（10s 客户端超时，`api.go:26`），最坏把退出检测延迟到 ctx 15s 到期；但连接被拒是瞬时的，只有在"端口上有别的监听者"时才发生，且此时 `classifyStartFailure` 仍归类为 `ErrAPIUnreachable` 并完成清理，不产生半死进程。

---

## 八、未覆盖部分 + 需真机验证项

1. **mihomo 在 `external-controller` 端口冲突时是退出还是顺延**（影响 A2-09 的定级）：需在真机上占住 `apiPort` 后启动内核，观察其退出码与监听端口。**静态推断，需 Windows 真机验证。**
2. **mihomo 自行退出的退出码分布**（影响 A2-05 的实际触发率）：需统计 `kernel.log` 中历史 fatal 场景的退出码。**需真机验证。**
3. **嵌套 Job 的兼容性**：Win7 或由某些启动器（含部分 EDR）启动时 `AssignProcessToJobObject` 会 `ERROR_ACCESS_DENIED`，当前实现会**直接拒绝启动内核**（`job_windows.go:22-25`、`manager.go:406-413`）。是否需要对这类环境降级为"无 Job 但可用"（并显式告警）属产品决策。**需 Windows 真机验证。**
4. **关机/注销路径**：未处理 `WM_QUERYENDSESSION`/`SERVICE_CONTROL_SHUTDOWN`；强杀时 Job kill-on-close 生效，系统代理/注册表靠下次启动的 Pending 机制恢复。不在本次 10 问范围内，但属退出清理的空白。
5. **前端在 `failed` / 僵尸 `run` 状态下是否提供"停止"入口**（决定 A2-08 的后果严重度）：前端代码未纳入本次审阅。
6. **15s readiness 预算对大规则集冷启动是否足够**（`service.go:694`）：首次启动加载数万条规则 + GeoIP 可能超过 15s，超时即被杀，用户会看到"启动超时"。**需真机验证。**
7. **`probe` 成功但响应的不是本进程内核**（端口被前一个残留 mihomo 占住且 `checkAddrFree` 恰好放行）：代码无法用 PID/Job 归属校验 API 响应者。当前依赖 `checkAddrFree` 的"能建连即判占用"（`manager.go:732-743`）间接规避，属**静态推断，需真机验证**该环节是否会漏。
8. **未做**：全量 `go test -race ./...`（主控在跑）、`service.go` 其余 6000 行的代理/TUN 事务逻辑、`update.go` 的下载/替换细节、前端。
