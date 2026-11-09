# C2 内核生命周期统一串行化整改报告

- 日期：2026-11-10
- 仓库：`peican996/RoutePilot`（本地 `nexuslink_review`）
- 基线：`0c38642`
- 范围：仅 C2（内核生命周期状态机、并发协调、Watchdog、Job Object、API 就绪与回滚）
- 状态：改动在工作区，未提交

---

## 1. 结论摘要

上一轮已经建立了 `internal/kernel.Manager` 的状态机与串行化事务；本轮按任务书逐项复核，并补齐了 **Service 层的生命周期串行化与启动代际**，解决 Manager 进程锁保护不到的 afterStart 副作用。

核心结论：

1. Start / Stop / Restart / Close 由 Manager `opMu` 串行化；状态机为 Stopped / Starting / Running / Stopping / Failed。
2. Start 只有在 API 探测成功后才进入 Running；失败会 Kill 进程、等待收尾并进入 Failed。
3. Stop 在 Starting 中不会误判；测试用 hook 卡住“进程已创建”阶段验证 Stop/Close 会等待事务完成。
4. Restart 是 `stopLocked + startLocked` 的完整事务，没有固定 Sleep；新进程创建前旧进程必须已退出。
5. Watchdog 使用滑动窗口、指数退避、最大次数与稳定窗口，不会在重启成功后立即清零。
6. Job Object 创建/配置/分配全部检查错误；分配失败直接阻止启动并返回 `ErrJobObjectFailed`。
7. API 就绪与启动失败回滚：`ProcessStarted`、`APIReady`、`KernelReady` 三阶段在 Manager 内完成；失败清理进程、日志、临时资源和 Job 归属。
8. Shutdown 与 Start/Stop/Restart 串行；Shutdown 关闭 afterStart 入口、等待任务、再恢复代理与关闭 Manager。

---

## 2. 状态机设计

```
Stopped ──Start──▶ Starting ──API Ready──▶ Running
   ▲                  │                       │
   │                  └──失败/超时──▶ Failed   ├──Stop──▶ Stopping ──进程退出──▶ Stopped
   │                                          │
   └────────────── Stop（幂等） ◀──────────────┘
Running ──异常退出──▶ Failed
Failed ──Start（人工/看门狗）──▶ Starting
```

合法转换：

| 当前 | 允许操作 | 结果 |
|---|---|---|
| Stopped | Start | Starting → Running / Failed |
| Failed | Start | Starting → Running / Failed |
| Running | Start | `ErrAlreadyRunning` |
| Starting | Start | `ErrAlreadyRunning`（串行等待后） |
| Stopping | Start | `ErrLifecycleBusy` |
| Running | Stop | Stopping → Stopped |
| Starting | Stop | 等待 Start 事务结束，再停止 |
| Running/Stopped | Restart | 完整 Stop + Start 事务 |
| Starting/Running | Close | 等待事务结束、停止、关闭 Job |

`IsRunning()` 只认 `Running`，不再用 `cmd != nil` 推断。

---

## 3. 锁与并发协调

### 3.1 Manager 层

- `opMu`：生命周期操作串行化锁，覆盖完整的 Start / Stop / Restart / Close。
- `mu`：状态、当前 `processRun`、lastErr 的短临界区保护；不持锁执行进程启动、等待或回调。
- `processRun`：每次启动独立的 cmd / pid / exited / done / intentional / exitErr，避免新旧实例字段互相覆盖。
- `waitProcess` 是唯一 `cmd.Wait()` 调用点；回调在释放 `mu` 后执行，允许回调重入 Manager 而不会死锁（有测试）。
- 启动期间 `rate readiness` 由 `awaitReady` 在 `opMu` 内等待；其他 Start 串行等待，不会创建第二个进程。
- Stop 在 Starting 中通过 `opMu` 等待，不会因为 `run == nil` 或 `cmd == nil` 直接返回。

### 3.2 Service 层（本轮补强）

Manager 的锁保护不了 afterStart，因此在 `CoreService` 新增：

- `lifeMu`：串行化 Service 的 StartKernel / StopKernel / restartKernel / Shutdown。
- `gen` / `genCancel`：每次成功启动的唯一代际；旧代际取消。
- `afterMu` / `afterWG` / `afterClosed`：afterStart 任务登记、Shutdown 入口关闭与等待。

关键顺序：

- StopKernel：`lifeMu` → 失效代际 → 等待 afterStart → 恢复系统代理 → Manager.Stop → 暂停 Watchdog。
- RestartKernel：`lifeMu` → 失效代际 → 等待 afterStart → Manager.Restart → 新代际 → afterStart。
- Shutdown：`lifeMu` → `shuttingDown=true` → 停止 Watchdog → 失效代际 → 关闭任务入口并等待 → 恢复 WinINET/WinHTTP → 关闭 API 连接 → Manager.Close。

---

## 4. Start / Stop / Restart / Shutdown 执行流程

### Start

1. 取得 `opMu`；检查 closed / state / exePath。
2. 原子设置 Starting。
3. 端口预检查（仅快速失败）。
4. `cmd.Start()`。
5. Job Object 分配；失败则 Kill + Wait + 关闭日志 + Failed。
6. 创建 `processRun`，启动唯一 Wait goroutine。
7. 如配置了 probe：轮询 API；超时/进程退出/API 不可达分别分类。
8. 成功后才置 Running；探测成功但进程已退出则 Failed。

### Stop

1. 取得 `opMu`。
2. 若已 Stopped/Failed 且无 run → 幂等返回。
3. 标记 run.intentional，状态 Stopping。
4. Kill；等待 `run.done`（真正回收 cmd/PID/日志）。
5. 超时进入 Failed，返回 `ErrStopTimeout`，不伪造成功。

### Restart

1. 取得 `opMu`。
2. `stopLocked` 完整停止并等待旧进程退出。
3. 直接 `startLocked`，不插入固定 Sleep。
4. 只有 API Ready 才 Running；失败清理并 Failed。

### Shutdown

1. 取得 `opMu`。
2. `stopLocked` + 关闭 Job 句柄；之后 Start 返回 `ErrManagerClosed`。
3. 无固定 Sleep。

---

## 5. Watchdog 设计

`internal/kernel/watchdog.go`：

- 滑动窗口（默认 10 分钟）统计失败，窗口外记录清理。
- 指数退避 2s/4s/8s/16s/32s，封顶 60s。
- 最大连续重启 5 次；超过即熔断，只通知一次。
- 只有运行时间 ≥ 稳定窗口（1 分钟）后的下次退出才重置计数；**重启成功不立即清零**。
- 单一 timer 防止多个退出事件叠加多个重启。
- 看门狗自身重启失败也计入失败次数。
- `Pause` / `Resume`：用户手动停止内核后暂停；手动启动恢复；看门狗自身重启不清零。
- `Stop`：Shutdown/退出时取消等待中的重启。
- 重启通过 `Service.restartKernel(false)` → Manager.Restart，不绕过 Manager。
- 熔断后 Manager 保持 Failed，UI 通过事件与 status.error 获取状态。
- 看门狗回调检查 `s.app != nil`、`s.Quitting()`；Shutdown 先 Stop 看门狗。

---

## 6. Windows Job Object 策略

`internal/kernel/job_windows.go`：

- `CreateJobObject` 失败 → 返回错误，启动中止。
- `SetInformationJobObject`（KILL_ON_JOB_CLOSE）返回值检查；失败关闭句柄并返回错误。
- `OpenProcess` / `AssignProcessToJobObject` 错误全部返回。
- Job 由 Manager 独占，`Close()` 释放；不静默降级。
- 分配失败时：Kill 本次进程、Wait、关闭日志、返回 `ErrJobObjectFailed`，状态 Failed。
- `processRun.jobAttached` 记录绑定成功；`Manager.JobAttached()` 可供测试/诊断。
- 兼容性：Windows 8+ 支持嵌套 Job；Win10/11 已实测启动成功。若宿主 Job 禁止嵌套，Assign 失败会明确阻止启动并写日志。
- 进程退出后由 Wait 收尾；Job 句柄在 Manager.Close 时释放，KILL_ON_JOB_CLOSE 兜底强杀场景。

---

## 7. API 就绪与失败回滚

三阶段：

| 阶段 | 判定 | 失败处理 |
|---|---|---|
| ProcessStarted | `cmd.Start` 成功 | Kill/Wait/关闭日志/Failed |
| APIReady | probe（`/version`）成功 | 超时/端口冲突/API 不可达/进程退出分类 |
| KernelReady | Manager 状态 Running，且 `m.run == r` | 进程期间退出则 Failed |

- `awaitReady` 轮询间隔 200ms，超时默认 15s。
- 失败统一走 `abortStart`：标记 intentional、Kill、等待 done、清理 run，状态 Failed。
- 端口冲突、启动超时、API 不可达、进程提前退出分别返回不同 sentinel error。
- 探测成功后再次核对 `m.run == r`，避免进程退出后仍标记 Running。
- API 探测绑定本次 `processRun`；Service 层 afterStart 另用 generation 防止旧任务修改新实例。
- 启动失败不会接管系统代理；afterStart 中的代理恢复失败会写 bootErr 并保留备份。

---

## 8. 测试覆盖矩阵

| 任务书场景 | 测试 | 结果 |
|---|---|---|
| 100 个并发 Start | `TestConcurrentStartOnlyOneInstance` | 通过 |
| Start 与 Stop 并发 | `TestStartStopConcurrentConsistency` | 通过 |
| 多个 Restart 并发 | `TestConcurrentRestartAllSucceed` | 通过 |
| Start 与 Shutdown 并发 | `TestCloseDuringStartingLeavesNoProcess` | 通过 |
| API 超时 | `TestStartTimeoutCleansProcess` / `TestStopWhileWaitingAPIReady` | 通过 |
| Watchdog 连续崩溃 | `TestWatchdogExhaustsAfterMaxRestarts` / `TestBackoffDelay` | 通过 |
| Job Object 绑定失败 | `TestJobObjectAssignInvalidPID` / `TestReproT09JobObjectFailureIsReported` | 通过 |
| 代理恢复失败 | `TestReconcileClosedProxyFailureKeepsBackup` / `TestT02...` | 通过 |
| 代理恢复后再次启动 | `TestReconcileClosedProxyFailureKeepsBackup` 二次恢复 | 通过 |
| 旧异步任务晚到 | `TestStaleAfterStartCannotReadNewInstance` / `TestStaleAfterTaskCannotMutateNewGeneration` | 通过 |
| Stop 在 Starting 中 | `TestStopDuringStartingWaitsForTransaction` | 通过 |
| 回调重入 | `TestExitCallbackConcurrentWithRestart` | 通过 |
| 真实内核 | `TestRealKernelLifecycle` | 通过 |

完整执行命令与结果见 `TEST-REPORT.md`；完整 Git diff 见 `C1.1-C2-CHANGES.diff`。

---

## 9. 修改文件与 diff 摘要

```
 internal/kernel/manager.go        +34   hook / jobAttached / JobAttached
 internal/kernel/watchdog_test.go  +24   Stop 取消待执行重启
 service.go                        +266  generation / lifeMu / after 任务 / 代理事务加固
 新增：
 internal/kernel/lifecycle_barrier_test.go
 service_generation_test.go
 c1_1_proxy_test.go
 C1.1-REMEDIATION.md
 C2-LIFECYCLE-REMEDIATION.md
 TEST-REPORT.md
```

重要改动原因：

- `service.go` 代际与 afterStart 登记：Manager 锁无法保护异步副作用；Stop/Restart/Shutdown 必须等待并阻止旧任务。
- `Manager.hook`：用 barrier 替代随机 Sleep，确定性复现 Start 中间态。
- `processRun.jobAttached` / `JobAttached()`：让“是否真正受 Job 托管”可验证。
- 生命周期串行化：保证 Service 层操作顺序与 Manager 状态一致。
- Watchdog/Job/API readiness：复核既有实现并补齐测试与边界。

---

## 10. 未解决与风险

1. **race detector 不可执行**：本机无 gcc，`CGO_ENABLED=1 go test -race` 在 runtime/cgo 失败；不能声称 race 通过。
2. **外部孤儿进程的 API 身份**：mihomo `/version` 不返回 PID；防护依赖端口预检查、Manager 串行化和 `m.run == r` 核对。极端情况下外部进程占用同一 API 端口会被判为端口冲突而拒绝启动，不会误认旧实例为新实例。
3. **Shutdown 等待无上限**：afterStart 中的 API 有 15s 超时，注册表调用通常很快；若系统调用挂起，Shutdown 会等待。正确性优先。
4. **真实 WinINET/WinHTTP 注册表恢复**：测试使用 fakeRegistry，避免改动本机网络；需要管理员实机验收。
5. **UI/TUN/休眠唤醒**：不在本轮 C1.1/C2 范围，未做完整实机矩阵。

本阶段到此暂停，等待人工审查。
