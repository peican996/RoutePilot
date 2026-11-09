# C1.1 + C2 测试报告

- 日期：2026-11-10
- 环境：Windows 10/11 x64、Go 1.27.3、Node 20、`CGO_ENABLED=0`、无 gcc、内置 mihomo v1.19.33
- 基线：`0c38642`
- 结论：单元/并发/复现/真实内核测试通过；**race detector 因缺少 gcc 无法执行**，未宣称通过。

---

## 1. 自动化测试命令与结果

### 1.1 全量单元测试

```powershell
go test -count=1 ./...
```

结果：

```
ok   routepilot
ok   routepilot/internal/autostart
ok   routepilot/internal/configgen
?    routepilot/internal/elevate      [no test files]
?    routepilot/internal/geoip        [no test files]
ok   routepilot/internal/kernel
ok   routepilot/internal/procid
ok   routepilot/internal/subscription
?    routepilot/internal/sysproxy     [no test files]
```

### 1.2 生命周期与 C1.1 用例重复执行

```powershell
go test -count=3 ./internal/kernel .
```

结果：

```
ok   routepilot/internal/kernel  27.513s
ok   routepilot              1.305s
```

### 1.3 复现套件

```powershell
go test -count=1 -tags repro ./...
```

结果：全部通过（包含 T01-T18 复现用例，T17 SSRF 已于上一阶段修复）。

### 1.4 真实 mihomo（Windows 实机）

```powershell
go test -tags realkernel -count=1 -run "TestRealKernel" -v ./internal/kernel
```

结果：

```
=== RUN   TestRealKernelDistinguishesSameNameProcessPaths
--- PASS (0.28s)  A=200 direct, B=502 REJECT
=== RUN   TestRealKernelSmartRoutingRulesStart
--- PASS (0.55s)
=== RUN   TestRealKernelLifecycle
    真实内核已就绪: pid=12204 version=v1.19.33
--- PASS (0.22s)
```

### 1.5 静态检查

```powershell
go vet ./...
gofmt -l .
```

结果：无输出（无问题）。

### 1.6 前端

```powershell
cd frontend
npm run build
```

结果：`tsc` + `vite build` 成功（67 modules，产物约 299KB）。
仓库没有独立 lint 脚本，`tsc` 已作为构建前置检查。

### 1.7 Windows amd64 生产编译

```powershell
go build -tags production -trimpath -buildvcs=false -ldflags "-w -s -H windowsgui" -o bin\RoutePilot.exe .
```

结果：成功，生成约 90MB 的 `bin\RoutePilot.exe`。

### 1.8 Race Detector（未通过，环境限制）

```powershell
$env:CGO_ENABLED=1
go test -race -count=1 ./...
```

结果：

```
# runtime/cgo
cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%
FAIL routepilot [build failed]
...
```

**说明**：本机没有 gcc，`-race` 无法构建；这不是测试通过。并发正确性由确定性 barrier/channel 测试与多轮重复执行覆盖，但不能替代 race detector。

---

## 2. 并发与故障测试矩阵

| 场景 | 测试 | 控制方式 | 结果 |
|---|---|---|---|
| 100 个并发 Start | `TestConcurrentStartOnlyOneInstance` | 100 goroutine | 通过，仅 1 个成功 |
| Start / Stop 并发 | `TestStartStopConcurrentConsistency` | 20 轮并发 | 通过，状态与 PID 一致 |
| 多个 Restart 并发 | `TestConcurrentRestartAllSucceed` | 5 goroutine | 通过 |
| Restart 与 Stop 并发 | `TestRestartStopConcurrent` | barrier/并发 | 通过 |
| Start 在 process-started 时 Stop | `TestStopDuringStartingWaitsForTransaction` | Manager hook + channel | 通过，Stop 等待事务结束，无孤儿 |
| Start 在 process-started 时 Close | `TestCloseDuringStartingLeavesNoProcess` | Manager hook + channel | 通过，Close 后无进程 |
| Start 等待 API 时 Stop | `TestStopWhileWaitingAPIReady` | 300ms probe + 并发 | 通过，超时清理、PID 清空 |
| Restart 等旧进程退出 | `TestRestartWaitsForOldProcessExit` | PID 对比 | 通过，旧 PID 不再存活 |
| 回调阻塞时 Restart | `TestExitCallbackConcurrentWithRestart` | 回调 channel 阻塞 | 通过，未死锁 |
| API 超时清理 | `TestStartTimeoutCleansProcess` | PID 文件 + 300ms 超时 | 通过，进程被清理 |
| API 不可达分类 | `TestAPIUnreachableClassified` | 端口监听 + 无 HTTP | 通过，`ErrAPIUnreachable` |
| 端口占用 | `TestStartFailsWhenAPIPortInUse` | 占用监听 | 通过，`ErrPortInUse` |
| 进程提前退出 | `TestProcessExitDuringStartup` | fake exit | 通过，`ErrProcessExited` |
| Job 分配失败 | `TestJobObjectAssignInvalidPID` | 无效 PID | 通过，返回错误 |
| Job 绑定成功记录 | `TestJobAttachedForStartedProcess` | 实际子进程 | 通过 |
| Watchdog 连续崩溃熔断 | `TestWatchdogExhaustsAfterMaxRestarts` | 小窗口定时器 | 通过 |
| Watchdog 重启失败计数 | `TestWatchdogRestartFailureCountsTowardLimit` | 失败 restart | 通过 |
| Watchdog 稳定运行重置 | `TestWatchdogStableRunResetsCounter` | Uptime 2 分钟 | 通过 |
| Watchdog Stop 取消待重启 | `TestWatchdogStopCancelsPendingRestart` | timer + Stop | 通过 |
| 旧 afterStart 改新实例 | `TestStaleAfterStartCannotReadNewInstance` | API server + hook | 通过，未读取 `/configs`、未改端口 |
| 旧任务延迟返回 | `TestStaleAfterTaskCannotMutateNewGeneration` | channel | 通过 |
| Shutdown 等待异步任务 | `TestShutdownWaitsForAfterTasksAndClosesGate` | channel + WaitGroup | 通过，Shutdown 等待且拒绝新任务 |
| 用户关闭代理恢复失败 | `TestReconcileClosedProxyFailureKeepsBackup` | fakeRegistry 注入 | 通过，备份保留，重试成功 |
| 恢复成功清理备份 | `TestReconcileClosedProxySuccessClearsBackup` | fakeRegistry | 通过 |
| 恢复成功但删备份失败 | `TestRestoreSuccessRemoveFailureKeepsBackup` | `removeProxyState` 注入 | 通过，保留并可重试 |
| 既有代理事务 T01/T02/T10/T16 | `c1_proxy_test.go` | fakeRegistry + 写失败注入 | 通过 |

---

## 3. Windows 实机验证结果

已在当前 Windows 主机实际执行：

- 真实 mihomo v1.19.33 启动、API `/version` 就绪、Stop 后进程/端口释放：通过。
- 真实 mihomo 按路径区分同名 `same.exe`，A 直连 200、B REJECT 502：通过。
- 真实 mihomo 加载内置 `GEOSITE,cn` / `GEOIP,CN` 数据库并启动：通过。
- 子进程创建、Job Object 分配、并发 Start/Stop/Close barrier 测试：通过（使用真实 test binary 作为假内核）。
- Windows 文件占用场景（升级目标独占打开）下的替换失败测试：通过（属于上一阶段，本轮回归仍通过）。
- Windows amd64 生产构建：通过。

**未做**：

- 管理员权限下真实 WinINET/WinHTTP 注册表恢复（测试使用 fakeRegistry，避免改动本机网络）。
- TUN 模式、系统休眠/唤醒、真实断网重连。
- 图形界面连续点击 Start/Stop、托盘退出与崩溃恢复。
- 长时间稳定性（小时级）。

---

## 4. 未完成项

1. `go test -race` 未执行成功：缺少 gcc/CGO。
2. 真实注册表系统代理事务未做管理员实机演练。
3. WinHTTP 与 WinINET 混合异常的真实注册表演练未做（代码路径已接入，测试用 fake）。
4. UI/TUN/休眠唤醒不在本阶段范围。
5. `Shutdown` 等待异步任务没有超时上限；依赖 API 15s 超时与注册表调用快速返回，属于正确性优先取舍。

---

## 5. 已知风险

| 风险 | 说明 | 缓解 |
|---|---|---|
| 无 race detector 证据 | 本机无 gcc | 确定性 barrier/channel 测试 + count=3 重复；CI/有 gcc 环境应补跑 `-race` |
| 外部孤儿内核占用 API 端口 | mihomo `/version` 不含 PID | 端口预检查、Manager 串行化、探测后 `m.run == r` 复核；占用端口会拒绝启动 |
| Shutdown 可能变慢 | 等待 afterStart 真正退出 | API 15s 上限；代理副作用必须等待，避免退出后改注册表 |
| 真实 WinHTTP 恢复失败 | 仅管理员可写 HKLM | 失败保留 `app-state` 备份，下次提权启动重试 |
| Job Object 嵌套限制 | 宿主已有禁止嵌套的 Job（主要 Win7） | Assign 失败明确阻止启动并报错，不静默降级 |
| Watchdog 熔断后代理恢复 | UI 事件与 debug.log 记录 | 熔断同时还原 WinINET/WinHTTP；失败保留备份 |

---

## 6. 与任务书验收标准对照

- “任何并发操作不能产生多个 mihomo”：Manager `opMu` + 100 并发 Start 测试通过。
- “Stop 不能误判 Starting”：barrier 测试通过，Stop 等待 Start 事务。
- “Restart 不允许旧进程未退出就启动新进程”：PID 测试通过。
- “Shutdown 返回后无残留进程”：Close/Shutdown barrier 测试通过。
- “Watchdog 不会无限重启”：窗口、退避、上限、稳定窗口测试通过。
- “Job 失败不静默”：错误返回 + 启动中止测试通过。
- “API 超时清理进程”：PID 文件与 `ErrStartTimeout` 测试通过。
- “代理恢复失败保留备份”：三类恢复失败测试通过。
- “旧异步任务不能改新实例”：代际 + 真实 afterStart hook 测试通过。

报告完成，暂停等待人工审查。
