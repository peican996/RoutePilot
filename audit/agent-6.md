# RoutePilot 审计报告 · 子 Agent 6：测试体系、架构与代码质量专项

## 1. 头部

### 1.1 审计范围

以「测试是否真实有效」为核心，不数覆盖率：

- 全部 Go 测试（263 个 Test 函数 / 53 个测试文件 / 8542 行测试代码，占全仓 Go 代码 20498 行的 41.7%），含 c1_/c3_/c4_/c5_ 系列、repro_*、healthcheck、startup_restore_plan、process_identity、process_targets、site_rules、subinfo、datadir、procname、service_generation，以及 `internal/**/*_test.go`。
- 构建/CI/Lint：`.github/workflows/ci.yml`、`release.yml`、`Taskfile.yml`、`build.ps1`、`.golangci.yml`、`package.py`、`frontend/package.json`。
- 模块边界与依赖：`go.mod`、`go list ./...`、各包 import、`service.go`（6054 行）职责边界。
- 错误处理与可观测性：`_ =` 全量清点、`recover()` 全量清点、`dbg()`、`LogBuffer`、`safego`。

不覆盖（属其它子 Agent）：注册表语义细节、TUN 事务状态机正确性、订阅解析安全策略自身的正确性。凡与 Agent 1 结论重叠处只做交叉引用，不重复定级。

### 1.2 基线

- commit `39ca02c1aa9303fc14fe8264d2715432cd50f030`（`gofmt: 对齐测试表字段`），工作区干净。
- 主控已实测：`go build ./...` / `go vet ./...` / `go test -count=1 ./...` / `go test -race -count=1 ./...` 全绿。
- `go.mod` 实际写的是 `go 1.26.0`（任务书上写 1.27），CI 用 `go-version: '1.26'`，历史 TEST-REPORT.md 记的是 1.27.3 —— 版本口径不统一，不影响本次结论。

### 1.3 审阅文件清单（实际 Read 全文）

| 文件 | 行数 | 说明 |
|---|---|---|
| `service.go` | 6054 | 全文分区间读取（1-260、460-580、600-780、660-740、1080-1180、1430-1780、1820-2110、2600-2680、2800-3060、3376-3520、3700-3996、5355-5480、5867-5990） |
| `main.go` | 513 | 1-160（单实例/启动） |
| `procgen.go` | 327 | 全文 |
| `config_files.go` | 104 | 1-50 |
| `c1_proxy_test.go` | 417 | 全文 |
| `c1_1_proxy_test.go` | 111 | 全文 |
| `c1_1_r1_proxy_test.go` | 535 | 全文 |
| `c3_persist_test.go` | 55 | 全文 |
| `c4_config_test.go` | 215 | 全文 |
| `c4_procgen_test.go` | 235 | 全文 |
| `c4_subscription_test.go` | 178 | 全文 |
| `c4_tun_test.go` | 250 | 全文 |
| `c5_1_proxy_test.go` | 335 | 全文 |
| `c5_tun_proxy_test.go` | 368 | 全文 |
| `healthcheck_test.go` | 75 | 全文 |
| `startup_restore_plan_test.go` | 61 | 全文 |
| `service_generation_test.go` | 155 | 全文 |
| `site_rules_test.go` | 101 | 全文 |
| `subinfo_test.go` | 37 | 全文 |
| `datadir_test.go` | 77 | 全文 |
| `procname_test.go` | 34 | 全文 |
| `repro_t12_test.go` | 47 | 全文 |
| `repro_persist_test.go` | 110 | 全文 |
| `realkernel_procgen_test.go` | 127 | 全文 |
| `internal/kernel/lifecycle_test.go` | 533 | 全文 |
| `internal/kernel/lifecycle_barrier_test.go` | 175 | 全文 |
| `internal/kernel/start_exit_race_test.go` | 132 | 全文 |
| `internal/kernel/repro_concurrency_test.go` | 190 | 全文 |
| `internal/kernel/watchdog.go` | 341 | 全文 |
| `internal/kernel/watchdog_test.go` | 293 | 全文 |
| `internal/kernel/watchdog_r1_test.go` | 304 | 120-300 |
| `internal/kernel/manager.go` | 770 | 80-200、400-520 |
| `internal/kernel/manager_test.go` | 48 | 全文 |
| `internal/kernel/api.go` | 289 | 30-200 |
| `internal/kernel/update.go` | 1005 | 446-500、583-650、905-950 |
| `internal/kernel/upgrade_test.go` | 225 | 全文 |
| `internal/kernel/download_test.go` | 102 | 全文 |
| `internal/kernel/update_test.go` | 89 | 全文 |
| `internal/kernel/update_fallback_test.go` | 114 | 全文 |
| `internal/kernel/realkernel_test.go` | 125 | 1-40 |
| `internal/sysproxy/sysproxy_windows.go` | 275 | 全文 |
| `internal/sysproxy/sysproxy_windows_test.go` | 177 | 全文 |
| `internal/subscription/guard_test.go` | 281 | 全文 |
| `internal/subscription/fetch_test.go` | 47 | 全文 |
| `internal/subscription/repro_t17_test.go` | 34 | 全文 |
| `internal/safego/safego.go` | 62 | 全文 |
| `internal/configgen/gen_test.go` | 296 | 全文 |
| `internal/procid/procid_test.go` | 167 | 全文 |
| `internal/procid/tracker_test.go` | 187 | 150-187 |
| `internal/procid/source_windows_test.go` | 42 | 全文 |
| `internal/autostart/c5_taskstate_test.go` | 122 | 全文 |
| `internal/geoip/geoip.go` | 108 | 全文 |
| `.github/workflows/ci.yml` / `release.yml` / `.golangci.yml` / `Taskfile.yml` / `build.ps1` / `package.py` / `frontend/package.json` | — | 全文 |

### 1.4 执行过的命令与结果

只读审计，未修改任何源码/测试/CI，未做 git 写操作。

| 命令 | 结果 |
|---|---|
| `git log --oneline -1` / `git status --porcelain` | 基线 `39ca02c`，工作区干净 |
| `go list ./...` | 10 个包（根 + 9 个 internal） |
| `grep -rn '^func Test' --include=*_test.go . \| wc -l` | 263 |
| 按文件统计 Test 数 | 根 100 / kernel 73 / subscription 29 / procid 22 / autostart 12 / configgen 12 / sysproxy 8 / safego 7 |
| `go list -deps`、逐包 grep `routepilot/internal` | 依赖图见 §5 |
| `grep -rn 'recover()' --include=*.go .` | 全仓只有 `safego.go:43,54` 两处 |
| `grep -cE '^\s+go ' service.go main.go internal/*/*.go` | 除 `safego.go:25` 外无裸 goroutine |
| 逐函数比对「service.go 函数名是否出现在任何测试文件」 | 约 130 个函数名未在测试中出现（多数经被测函数间接覆盖，见 §3） |
| `grep 'fail[A-Z]\|injected\|os.ErrPermission\|appStateWriter =' _test.go` | 9 个文件、共 74 处注入点 |
| 未执行：全量 `go test` / `-race`（基线已跑，遵循纪律 5）；未做任何源码改动（遵循纪律 6） |

### 1.5 结论摘要

| 等级 | 数量 | 编号 |
|---|---|---|
| P0 | 0 | — |
| P1 | 0 | — |
| P2 | 9 | A6-01 ~ A6-09 |
| P3 | 8 | A6-10 ~ A6-17 |

一句话结论：**这套测试的质量明显高于「为绿而写」的水平**——真实子进程 + Job Object + 真实 HKCU 沙箱 + httptest + 注入钩子的失败路径覆盖都在，`-race` 也确实压住了核心生命周期并发；但缺口高度集中在「不在 Go 单测可行范围内、又没被任何机制兜住」的几处：**升级包完整性校验（摘要/大小/白名单）零测试**、**WinHTTP 全链路只有假实现且从不断言**、**单实例锁零测试**、**Boot()/StartKernel() 编排零覆盖**，以及 **`.golangci.yml` 配好了却从未在 CI 里跑过一次**（errcheck 一旦打开，§6 里那些 `_ =` 会立刻暴露）。

---

## 2. 问题清单

### A6-01 · P2 · 升级包完整性校验（摘要/大小/URL 白名单）没有任何测试

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/update.go` |
| 函数/测试 | `DownloadCore`（583-632）、`validateDownloadURL`（464-477）、`downloadCandidates`（487-493）；测试侧无对应用例 |
| 代码位置 | 摘要比对 `update.go:615-620`；大小比对 `608-611`；URL 白名单 `472-475`；白名单表 `450-459` |
| 描述 | 升级链路唯一的安全性依赖是「下载到的 zip 原文 sha256 == Release 声明的 digest」。这三段（大小不符 / 摘要不符 / 域名不在白名单）实现都在，但**零测试**：全仓 grep `摘要不匹配`、`大小不符`、`validateDownloadURL`、`DownloadCore` 在 `_test.go` 中命中数为 0。已有测试只覆盖了「digest 为空必须拒绝」（`upgrade_test.go:149` `TestDownloadRejectsMissingDigest`）和 `downloadCandidates` 的拼接顺序（`download_test.go:14,32`）。 |
| 触发条件 | 任何人改动 `DownloadCore`（比如把 `continue` 改成 `break`、把校验挪到 `extractMihomo` 之后、或让镜像源跳过校验）——现有测试全绿。 |
| 实际影响 | 这条路径在 TUN 场景下是**管理员权限下替换并执行任意 PE**。校验一旦被改坏，客户端会静默安装被中间人替换的内核；而 `-tags repro` 的 CI 任务仍会通过。这是本次审计里「测试有效性」与「风险」差距最大的一处。 |
| 根因 | `DownloadCore` 把 HTTP 下载与校验写在同一个函数里，httptest 可覆盖但作者只测了"缺摘要"这一个最省事的分支。 |
| 修复建议 | 不改实现即可测：用 `httptest` 返回错误字节 + 正确 digest → 断言 `err` 含「摘要不匹配」；返回正确字节 + 错误长度 + 正确 digest → 断言「大小不符」；把 `repoAPIEndpoints` 换成返回 `http://` 或非白名单域名的资产 → 断言被拒。 |
| 测试方案 | 新增 `internal/kernel/download_integrity_test.go`：4 个 table 用例（摘要不符 / 大小不符 / 非 https / 域名不在白名单），并断言 `exe` 返回值为 nil（不能"校验失败但仍返回数据"）。另加一条：镜像源返回坏数据、直连源返回好数据 → 必须自动回退到直连并成功（验证 `continue` 语义）。 |
| 置信度 | 高（源码与 grep 双向确认） |

### A6-02 · P2 · WinHTTP 接管/还原：全部实现零测试，事务内也从不被断言

| 字段 | 内容 |
|---|---|
| 文件 | `internal/sysproxy/sysproxy_windows.go`、`service.go` |
| 函数/测试 | `EnableWinHTTP`（268-275）、`WinHTTPRaw`（223-241）、`RestoreWinHTTP`（245-264）、`takeOverWinHTTP`（1948-1980）、`restoreWinHTTPIfPendingLocked`（1989-2011） |
| 代码位置 | 唯一相关的测试是 `sysproxy_windows_test.go:173` `TestWinHTTPBypassMatchesSystemProxyBypass`——它只比较两个**常量**是否相等；`c1_proxy_test.go:124-129` 里 `WinHTTPRaw` 恒返 `nil,nil`、`EnableWinHTTP` 恒成功、`RestoreWinHTTP` 恒成功，且 `winHTTPCalls` 字段声明后**从未被任何断言读取**。 |
| 描述 | 三件事全无覆盖：① HKLM 打开失败 / `netsh` 失败的降级分支；② `WinHTTPRaw` 在「值不存在」与「权限错误」两种 err 下的不同语义（235-239 明确区分，却无测试）；③ `winHTTPPending` / `winHTTPBackup` 是否被正确落盘（`saveAppState` 里 `WinHTTPPending: s.winHTTPPending`（5416）没有任何测试读过）。 |
| 触发条件 | 管理员场景下打开系统代理 → `takeOverWinHTTP` 被调用 → 只写 `dbg` 日志；还原失败只写 `dbg`。测试里这条链路跑了，但断言为零，等于没测。 |
| 实际影响 | 与 Agent 1 的 A1-06 是同一处缺陷，但**从测试角度更严重**：即使把 `takeOverWinHTTP` 整个函数体改成 `return`，全套测试（含 `-race`）依然全绿。这意味着"服务流量是否真的走了代理"这一用户可见承诺，没有任何自动化防线。 |
| 根因 | WinHTTP 需要 HKLM 写权限，作者判断"测不了"就直接没写；实际上 `takeOverWinHTTP` 的失败分支完全可以靠 `fakeRegistry`（即 `systemProxyOps` 注入）覆盖，不需要真 HKLM。 |
| 修复建议 | 在 `fakeRegistry` 里把 `WinHTTPRaw` / `RestoreWinHTTP` / `EnableWinHTTP` 改成可注入失败（加 `failWinHTTPRaw`、`failEnableWinHTTP`、`failRestoreWinHTTP` 三个字段），并断言 `winHTTPPending` 的置位与清除。 |
| 测试方案 | ① `TestWinHTTPTakeoverBackupPersisted`：admin=true → 断言 `svc.winHTTPPending==true` 且 `app-state.json` 里 `winHTTPPending:true`；② `TestWinHTTPEnableFailureKeepsPendingFalse`：`failEnableWinHTTP` → 断言 `winHTTPPending==false` 且无备份残留（当前实现如此，锁住这个契约）；③ `TestWinHTTPRestoreFailureKeepsPending`：`failRestoreWinHTTP` → 断言返回 error 且 `winHTTPPending` 仍 true（下次启动可重试）；④ `TestWinHTTPRestoreRequiresAdmin`：admin=false 且 pending=true → 断言返回"需要管理员权限"错误。 |
| 置信度 | 高 |

### A6-03 · P2 · 单实例锁（互斥体 + 端口）零测试

| 字段 | 内容 |
|---|---|
| 文件 | `main.go` |
| 函数/测试 | `acquireInstanceMutex`（100-119）、`acquireSingleInstance`（122-141） |
| 代码位置 | 30×150ms 重试循环（124-133）、端口兜底（137-139）、`ERROR_ALREADY_EXISTS` 与「创建失败」的区分（110-118） |
| 描述 | 这个函数决定"能不能起第二个实例"。它的三条分支——取到锁 / 已有实例（`ERROR_ALREADY_EXISTS`）/ 创建失败（`h==0`）——**没有任何测试**，而这三条分支一旦接错，用户看到的是完全不同的两个弹窗（`showAlreadyRunning` vs `showStartFailure`）。注：`runtime.LockOSThread()` 那几行注释说明了这块曾经踩过坑（LastError 线程局部性）。 |
| 触发条件 | 改动 `procCreateMx.Call` 的返回值判断、把 `lastErr` 换成 `err`、或改重试次数。 |
| 实际影响 | 双实例后果：两个内核、两套端口轮换、两个进程同时往同一个 `app-state.json` / `proxy.json` / `procgen` 目录写。`atomicWriteFile` 的 `os.Rename` 在目标被占用时会失败（`service.go:2071`），于是第二个实例的落盘持续失败，只写 `dbg`（`saveAppState` 只记日志）。这是"配置文件莫名回滚"的经典成因，目前**没有任何测试或断言**能发现它。 |
| 根因 | 依赖 Windows 内核对象，作者认为不可测；其实可以在同进程内先 `acquireInstanceMutex` 一次拿到句柄，再调一次断言返回 `(false,false)`——同一进程重复 CreateMutexW 同名对象同样会置 `ERROR_ALREADY_EXISTS`。 |
| 修复建议 | 把 `acquireInstanceMutex` 的 syscall 抽成 `var createMutex = func(name string) (handle, errno, ok)` 供注入；保留 `regPath` 式的最小可测缝。 |
| 测试方案 | ① `TestAcquireInstanceMutexSecondCallFails`：连续调用两次，第二次断言 `(false,false)`；② `TestAcquireSingleInstancePortConflictIgnored`：先用 `net.Listen` 占住 `127.0.0.1:47914`，断言单实例锁仍然成功（端口只是补充信号，`main.go:135-139` 的注释承诺了这个行为）；③ `TestAcquireSingleInstanceGivesUpAfterRetry`：注入恒失败的 createMutex，断言 `createFailed==true`（走 `showStartFailure` 而不是 `showAlreadyRunning`）。 |
| 置信度 | 高 |

### A6-04 · P2 · 崩溃检测 goroutine 传 `nil` reporter，panic 后静默消失、看门狗永不触发

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/manager.go` |
| 函数/测试 | `Manager.Start` → `safego.Go("内核进程等待", func() { m.waitProcess(r, w) }, nil)`，第 430 行 |
| 代码位置 | `manager.go:430`（唯一 reporter 为 `nil` 的 `safego.Go` 调用；对比 `watchdog.go:128,291` 都传了 `w.reportPanic`） |
| 描述 | `safego.Go` 的第三个参数是 panic 上报回调；传 `nil` 时 `protect` 在 `safego.go:49-51` 直接 `return`，**panic 被完全吞掉，栈也不取**（`debug.Stack()` 在 48 行取了但 49 行直接丢）。`waitProcess` 是唯一调用 `cmd.Wait()` 的地方，也是 `r.exited` / `r.done` 的关闭方与 `OnExit` 回调的触发方。 |
| 触发条件 | `waitProcess` 内部任何 panic（状态机切换、`exitReason`、回调 `m.OnExit` 里的用户代码、`w.file.Close()` 等）。 |
| 实际影响 | panic 之后：`r.done` 不关闭 → `awaitReady` 里 `<-r.done`（`manager.go:498`）永久阻塞（若此时正在启动）；`OnExit` 不触发 → `Watchdog.ReportExit` 收不到 → **不会自动重启，系统代理继续指向死端口**；UI 侧 `Status()` 仍显示 Running。用户看到的是"内核在跑但所有网站打不开"，日志里一个字都没有。这是问题 7（无意义的 recover）在本仓的唯一实例，且后果比"任务永久消失"更重，因为它同时把断网兜底（看门狗）也一起废掉了。 |
| 根因 | 写这行时只想到"panic 不能带走整个客户端"，没想到"panic 之后必须让状态机收敛"。 |
| 修复建议 | 改为 `safego.Go("内核进程等待", ..., m.reportPanic)`；并在 `waitProcess` 里用 `defer` 保证 `r.exitErr`/`close(r.exited)`/`close(r.done)` 三条收尾无论如何都执行（panic 路径下也要把状态推进到 Failed）。 |
| 测试方案 | ① 单测 `safego.Go(..., nil)` 的语义（断言 report 不被调用且不 panic）——已有 `safego_test.go` 可扩展；② 给 Manager 加 `waitProcessHook func()`（仅供测试，在 `waitProcess` 入口调用），注入一个会 panic 的 hook，断言：`Start` 返回错误或状态转为 `failed`、`r.done` 已关闭、`OnExit` 被调用或至少状态不是 `Running`、`m.Logs` 里有一条 panic 记录。③ 断言 `reportPanic` 被调用（当前为 nil 时无任何可观测痕迹）。 |
| 置信度 | 高（源码确证；是否真的会 panic 无法证明，故不定 P1） |

### A6-05 · P2 · `writeProcRulesYAML` 的返回在 3 处被丢弃：进程分流规则静默回退到旧版本

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数/测试 | `Boot`（702）、`applySubscriptionExpect`（4247）、`applyInactiveSubscription`（4633） |
| 代码位置 | `service.go:702` `_ = s.writeProcRulesYAML()`；`4247`；`4633` |
| 描述 | 三处都是「订阅/规则变了 → 重写 procgen 发布 → 重启内核」这条链里的关键一步。写成 `_ =` 后，发布失败时 `s.procProxyPath` 仍指向上一次成功的 generation（`procgen.go:131` 只在成功时 `setProcGenPaths`），而 `options()`（755-756）会把这个**过期路径**写进内核配置。 |
| 触发条件 | procgen 目录不可写（磁盘满、被杀软锁、`current.json` 被占用导致 `os.Rename` 失败，`procgen.go:173`）。 |
| 实际影响 | 用户刚改的进程分流规则**不生效也不报错**，内核按上一代规则跑；`Boot` 这一处更糟——它是启动路径，`failBoot` 的其它每一步都在报错（661、671、704、710），唯独这一处吞掉，于是"启动成功但进程规则是旧的"，用户在 UI 里看到规则已保存、实际流量走错出口。 |
| 根因 | `publishProcRules` 返回 error 后，调用方觉得"规则文件而已，失败就失败"。 |
| 修复建议 | `Boot` 处改为 `s.failBoot(err)` 或至少 `s.appendBootErr(...)`（已有 `appendBootErr`，见 1283）；订阅相关两处把 error 并入返回值（`applySubscriptionExpect` 本来就有 error 返回）。 |
| 测试方案 | 已有 `procPublishHook` 注入点（`procgen.go:57`），直接复用：`procPublishHook = func(stage string) error { if stage=="before-manifest" {return errTestInjected} }` → 断言 `Boot` 后 `svc.bootErr` 非空、`applySubscriptionExpect` 返回 error。 |
| 置信度 | 高 |

### A6-06 · P2 · `.golangci.yml` 配了但 CI 从未执行 → errcheck/staticcheck/unused 从未生效

| 字段 | 内容 |
|---|---|
| 文件 | `.golangci.yml`、`.github/workflows/ci.yml`、`Taskfile.yml` |
| 函数/测试 | CI 无 lint 步骤 |
| 代码位置 | `ci.yml` 全文（120 行）无 `golangci`、无 `staticcheck`、无 `errcheck`；`Taskfile.yml` 无 lint task；`build.ps1` 无 lint |
| 描述 | `.golangci.yml` 明确启用了 `errcheck`、`govet`、`ineffassign`、`staticcheck`、`unused`、`misspell`、`revive`，并把 `_test.go` 的 errcheck 排除——说明作者是认真配置过的。但全仓 grep 只有这份配置文件本身提到 `golangci`。历史文档 `T01-T18-REPRODUCTION.md:5` 直接写明环境"无 golangci-lint"。 |
| 触发条件 | 任何人提交新的吞错代码。 |
| 实际影响 | 本次审计在 `service.go` 里数出的 30+ 处 `_ =`（见 §6）之所以能长期存在，直接原因就是这条检查从未运行。换言之：**配置存在给了"我们有静态检查"的错觉，实际防线为零**。这是 P2 里"投入产出比最高"的一条。 |
| 根因 | 本地没装 golangci-lint，就没往 CI 里加。 |
| 修复建议 | 在 `ci.yml` 的 `check` job 里加一步：`uses: golangci/golangci-lint-action@v6` + `version: v1.61`（Windows runner 支持），首次跑大概率会报几十条，先只对 `errcheck` 做 `--new-from-rev` 增量门禁，再逐步收紧。 |
| 测试方案 | 加一个"负向"自检：在 CI 里故意提交一个 `_ = fmt.Errorf("x")` 的临时文件，确认门禁会红（可在 PR 模板里说明，不必长期保留）。 |
| 置信度 | 高 |

### A6-07 · P2 · `-race` 与 `-tags repro` 从不同时出现：所有 repro 并发测试都没跑过竞态检测

| 字段 | 内容 |
|---|---|
| 文件 | `.github/workflows/ci.yml` |
| 函数/测试 | `check` job 的 `Go test (repro)`（第 56-58 行）、`race` job 的 `Go test -race`（第 116 行） |
| 代码位置 | `ci.yml:59-60`：`go test -count=1 -tags repro ./...`（无 `-race`）；`ci.yml:101-102`：`go test -race -count=1 ./...`（无 `-tags repro`） |
| 描述 | 仓库里最"并发"的几个测试恰好都在 `repro` 标签下：`internal/kernel/repro_concurrency_test.go`（并发 Start/Stop/Restart）、`repro_persist_test.go`（12×2 goroutine 并发写订阅文件）。它们在 CI 里跑的是**不带 race detector 的版本**。 |
| 触发条件 | 新增或修改任何被 repro 测试覆盖的并发逻辑。 |
| 实际影响 | `repro_persist_test.go:66-72` 那 24 个并发 `writeSubscriptionFiles` 调用，正是最可能暴露共享 map 读写的地方，而它偏偏永远在无 race 模式下跑。`-race` job 覆盖的是 lifecycle_test 的 100 goroutine Start，那部分确实是真并发（不是"跑了全量所以覆盖了"），所以问题不是"race 没覆盖并发"，而是"**最脏的那几个并发用例被排除在 race 之外**"。 |
| 根因 | 两个 job 独立配置，作者为了省时间没做矩阵。 |
| 修复建议 | race job 改两行：`go test -race -count=1 ./...` + `go test -race -count=1 -tags repro ./...`。也可与 A6-06 一起把 job 拆成 `{tags: "", repro}` × `{race: on}` 的 2 格矩阵。 |
| 测试方案 | 合并后在本地先跑一次 `go test -race -tags repro ./internal/...` 确认没有既有竞态（若有，正好是本次审计的额外收获）。 |
| 置信度 | 高 |

### A6-08 · P2 · 测试未隔离数据目录，直接写开发机真实 `%APPDATA%\RoutePilot`

| 字段 | 内容 |
|---|---|
| 文件 | `c3_persist_test.go` |
| 函数/测试 | `TestSetProcessRuleWriteFailureLeavesMemoryUnchanged`，第 11-29 行 |
| 代码位置 | `c3_persist_test.go:12` `svc := NewCoreService()` —— **没有** `setDataDirOverride(t.TempDir())` |
| 描述 | 同文件另一个测试 `TestWriteFileAtomicLeavesNoTemp` 用了 `t.TempDir()`；同包其它测试（`c1_proxy_test.go:133`、`c4_config_test.go:209`、`c4_procgen_test.go:14`）都统一用 `configTestService` / `newC1Service` 做隔离。只有这一个测试漏了。`SetProcessRule` → `persistProcRuleSnapshot` → `publishProcRules` → `procGenDir()` = `dataDir()/procgen`，其中 `dataDir()`（`service.go:465-474`）在没有 override 时返回真实的 `%APPDATA%\RoutePilot`。 |
| 触发条件 | 在任意一台**装过 RoutePilot**的开发机上跑 `go test ./...`。 |
| 实际影响 | ① 会 `os.MkdirAll` 真实数据目录下的 `procgen`（`procgen.go:77`）；② 若该机器上 `writeFileAtomic` 的注入失效或被移除（例如有人重构掉这个 var），测试会**真的把一条 `path:c:\apps\b\b.exe → direct` 的规则发布进用户真实配置**，并触发 `cleanupProcGensLocked` 删掉真实用户的旧 generation；③ 在 `%APPDATA%` 只读的 CI 沙箱里，测试会因为"创建目录失败"而红——红的原因跟被测逻辑无关，属于假红。 |
| 根因 | 复制粘贴 `newC1Service` 时只留了 `NewCoreService()`。 |
| 修复建议 | 加 `old := getDataDirOverride(); setDataDirOverride(t.TempDir()); t.Cleanup(...)`（与 `c4_procgen_test.go:14-20` 完全一致），或直接改用同包的 `procGenTestService(t)`。 |
| 测试方案 | 加一条守卫：在 `c5WriteAppState` 里已有类似防呆（`c5_tun_proxy_test.go:48`）；建议把同样的守卫放进 `publishProcRules` 的测试路径，`if getDataDirOverride() == "" { t.Fatal(...) }`，从此不可能再漏。 |
| 置信度 | 高 |

### A6-09 · P2 · `Boot()` 与 `StartKernel()` 的编排零覆盖，`failBoot` 的 6 个分支全部未被执行

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数/测试 | `Boot`（608-721）、`failBoot`（1270）、`startupRestorePlan`（1248）、`afterStart`（1119） |
| 代码位置 | `failBoot` 调用点：661-662（EnsureBinary）、671-673（randomSecret）、704-706（configgen.Write）、710-711（mgr.Start）；`appendBootErr` 调用点：1283、1304、1310；`startupRestorePlan` 的决策在 1248，被 `TestStartupRestorePlan` 覆盖，但**它的调用方（`afterStart` 内 1199-1245）是否按 plan 执行**没有任何测试。 |
| 描述 | 启动编排是"数据目录迁移 → 释放内核与 wintun → 释放 geo 数据 → 生成密钥 → 选端口 → 加载订阅/规则 → 启看门狗/采样/健康检查 → 恢复 TUN/代理"的 15 步流水线。测试只覆盖了其中**纯函数**部分（`startupRestorePlan`、`copyDirAll`、`loadAppState` 的损坏分支）与**下游异步任务**部分（`afterStart` 的代际、`service_generation_test.go`）。整条 `Boot` 在测试中一次都没跑过（`Boot` 在 `_test.go` 中零出现）。 |
| 触发条件 | 调整 Boot 的步骤顺序（例如把 `recoverPendingProxyRestore` 挪到 `afterStart` 之后）、或改 `failBoot` 的语义。 |
| 实际影响 | 这条路径决定"重启电脑后用户还能不能上网"。`startupRestorePlan` 的核心格子（上次开 TUN、这次没管理员 → 必须回落系统代理）有单测，但**"plan 算出来了，代码有没有真的照着做"没有测试**——`afterStart` 里 `startupRestorePlan` 返回的 `wantTUN/wantProxy` 到真正的 `setTUN/setSystemProxy` 调用之间那 40 多行（`service.go:1199-1245`）是纯手工连线，没有任何断言。历史 C1/C2 整改的回归全部集中在 Boot 上，却只能在真机上用眼睛验。 |
| 根因 | `Boot` 强依赖 `embeddedKernel` / `wintunDLL` / `geoipEmbed` 三个 embed 变量与 `%APPDATA%` 实路径，作者判断"没法测"就整体放过了。 |
| 修复建议 | 把 Boot 的内部步骤抽成 `bootSteps` 接口或函数字段（`ensureBinary`, `ensureAux`, `ensureGeodata`, `startMgr`），测试注入空实现；`migrateDataDir` 接收 base 目录参数而不是内部调 `os.UserConfigDir()`。这是测试可及性问题，不是重构洁癖。 |
| 测试方案 | ① `TestBootFailBootSetsBootErr`：`mgr.Start` 注入失败 → 断言 `Status().BootErr` 非空、`api` 未就绪、后台任务（看门狗/采样器）**未启动**；② `TestBootRestorePlanApplied`：`restoreTUN=true, admin=false` → 断言 `setSystemProxy` 被调用、`setTUN` 未调用、`bootErr` 含告警文案；③ `TestBootMigrateFailureDoesNotTouchNewDir`（配合 `migrateDataDir` 参数化）。 |
| 置信度 | 高 |

### A6-10 · P3 · `service.go` 是 6054 行 / 178 个方法 / 13 把锁的上帝对象，是绝大多数测试缺口的根因

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数/测试 | `type CoreService struct`（141 起，结构体定义 194 行）；`func (s *CoreService)` 共 178 个 |
| 代码位置 | 结构体字段与锁：`mu`/`idLogMu`/`lifeMu`/`proxyMu`/`subsMu`/`rulesMu`/`procGenMu`/`afterMu`/`trafficMu`/`histMu`/`delayMu`/`verMu`/`connMu` 共 13 把 + `afterWG` + `afterClosed` |
| 描述 | 一个类型同时承担：系统代理事务（1569-2110）、TUN 事务（2208-2680）、内核生命周期（1432-1565）、看门狗接线（2884-2976）、订阅仓库（4083-4700）、进程规则仓库（4994-5300）、站点规则（5098-5130、5659-5850）、应用状态持久化（5283-5460）、流量历史（3743-3914）、延迟历史（2826-2850）、窗口位置（5448-5460）、自动更新（5491-5560）、连接管理（3584-3738）、UI 适配（各种 View）。 |
| 触发条件 | — |
| 实际影响 | 可测性的**结构性原因**，逐条对应前 9 个问题：① 13 把锁里只有 4 把有并发测试（`proxyMu`、`lifeMu`、`subsMu`、`rulesMu`）；`procGenMu`、`histMu`、`trafficMu`、`connMu`、`delayMu`、`verMu`、`idLogMu`、`afterMu`（部分）**没有任何并发用例**。② 任何测试只要 `NewCoreService()` 就必须连带准备 13 把锁与 embed 依赖，于是出现了 A6-08 那种"少写一行隔离就污染真实目录"的脆弱性。③ `Boot`/`StartKernel` 因为要触碰全部 9 个子系统而不可能单测（A6-09）。 |
| 根因 | 增量修补：每轮整改（C1→C5.1）都往同一个类型加字段加方法，没人拆。 |
| 修复建议 | 不需要大重构，先做**三个低风险切分**，每个都能立刻换来测试：① `internal/appstate`（`appState` 结构 + `saveAppStateErr`/`loadAppState`/`readConfigJSON`，约 200 行）；② `internal/substore`（`subs`/`subMeta`/`loadSubs`/`saveSubs`/`removeSubscription`，约 400 行）；③ `internal/proxytx`（`captureProxySnapshot`/`applyProxySnapshot`/`verifyProxySnapshot`/`restoreProxyTransactionLocked`/`proxyStillOurs`，约 300 行）。切出去之后 `service_test` 不再需要 13 把锁的上下文，A6-01/02/09 的测试都能独立写。 |
| 测试方案 | 切分本身不改行为，用现有的 `c1_*`/`c5_*` 测试作为安全网（它们全部通过 fakeRegistry，天然不依赖 `CoreService` 的其它字段）。 |
| 置信度 | 高 |

### A6-11 · P3 · repro 系列测试空转：断言的是测试自己造的数据，或旧 bug 的现象

| 字段 | 内容 |
|---|---|
| 文件 | `repro_persist_test.go`、`internal/kernel/repro_concurrency_test.go` |
| 函数/测试 | `TestReproT10SubsTruncatedIsOverwritten`（17-44）、`TestReproT11ConcurrentSubscriptionWrite`（48-103）、`TestReproT18ReplaceLeavesNoKernel`（161-190）、`TestReproT05/T06/T07`（48-126） |
| 代码位置 | T10：30 行 `svc.loadSubs()`、35 行 `svc.saveSubs()`、40 行断言 `!strings.Contains(got, "SECRET")`；T11：76-81 行是**测试自己** `os.WriteFile` 到 `subConfigFile`；T18：172-182 行全程只调 `os.Remove`/`os.Rename`，**没有调用任何生产代码** |
| 描述 | 这三个是"复现脚本"转正成测试时留下的残骸，且它们都在 CI 的 `-tags repro` 里跑、都通过。 |
| 触发条件 | — |
| 实际影响 | ① **T10 断言的是 bug 本身**（"损坏的 subs.json 会被空数据覆盖"）。`c4_config_test.go` 的 `TestCorruptConfigIsPreservedAndReported` 已经断言了正确行为（损坏文件被改名保留）。于是同一份代码上，一个测试要求"必须覆盖"，另一个要求"必须保留"——两者都通过，因为 T10 只检查 `got` 里**没有** `SECRET`，而修复后 `saveSubs()` 写的新文件同样不含 `SECRET`。**无论修没修，T10 都是绿的**，而且它还会 `t.Logf("FAIL: ...")` 打出误导性输出。② **T11 断言的是 NTFS 的写原子性**，不是 `writeSubscriptionFiles` 的行为；它自己当写入方，产品代码只是被顺带调用了一次。③ **T18 完全没碰产品代码**，`t.Logf("FAIL: ...")` 同样误导。 |
| 根因 | 复现脚本的职责是"证明 bug 存在"，修完之后本该删除或改写成"证明 bug 不存在"，实际只改了注释（`// T10（...）` 前缀都不一致，T10 注释没写"已修复"）。 |
| 修复建议 | T10 改写为断言修复后的契约（损坏文件被 `.corrupt-*` 保留 + `bootErr` 非空 + 后续 save 不破坏备份）——即与 `TestCorruptConfigIsPreservedAndReported` 合并去重；T11 改为对 `applySubscriptionExpect` 做真并发（已经有 `c4_subscription_test.go:93` 覆盖，可删）；T18 删除（真实实现由 `upgrade_test.go:106` `TestInstallBinaryRenameFailureKeepsOld` 覆盖）。 |
| 测试方案 | —— |
| 置信度 | 高 |

### A6-12 · P3 · 5 处重复实现（含"改一处漏一处"的真实风险）

| 字段 | 内容 |
|---|---|
| 文件 | 多处 |
| 函数/测试 | 见下 |
| 代码位置 | ① 原子写两份：`service.go:2041 atomicWriteFile` 与 `internal/configgen/gen.go:426 writeFileAtomic`（逻辑几乎逐行相同，configgen 那份甚至连失败路径测试都没有）；② fake kernel 两套：`internal/kernel/lifecycle_test.go:19 init()`（`ROUTEPILOT_FAKE_KERNEL`）与 `internal/kernel/repro_concurrency_test.go:14 TestMain`（`NEXUSLINK_FAKE_KERNEL`，且用 `os.Setenv` 而非 `t.Setenv`）；③ repro 测试与 lifecycle 测试重复：`TestReproT05/06/07` vs `TestConcurrentStartOnlyOneInstance`/`TestStartStopConcurrentConsistency`/`TestConcurrentRestartAllSucceed`；④ "是否仍属于我"三份：`proxyStillOurs`（1915）、`tunRestoreUserChanged`（2617）、`tunRegistryChangedExternally`（2638）；⑤ 进程规则两套写路径：`publishProcRules`（procgen.go:65，gen 目录 + manifest）与 `writeProcRuleSnapshotFiles`（service.go:5271，旧版平铺），两条路径同时存活。 |
| 描述 | ④ 是最值得担心的：三个函数都在做"逐字段比对注册表是否被别人改了"，但允许值集合不同（1 个值 / 2 个值 / 1 个值 + 空串语义）。Agent 1 的 A1-10 已经指出 `proxyStillOurs` 把 `cur==""` 一律判为我方，而 `tunRestoreUserChanged` 不会。三份实现没有任何一张表驱动测试同时锁住它们的语义。 |
| 触发条件 | 修改其中一份的比较规则。 |
| 实际影响 | ④ 会导致"关系统代理会覆盖用户清空的设置，但关 TUN 不会"这种同一产品内不一致的行为。② 两套 harness 让 `-tags repro` 下同时存在两个假内核约定，新写测试时容易选错（选错后子进程不按预期退出，测试变成 flaky）。⑤ 两份写路径意味着 procgen 的哈希校验在降级路径上不生效。 |
| 根因 | 增量修补的副产物。 |
| 修复建议 | ④ 抽成一个 `sameAsAny(cur string, allowed ...string) bool` + 一张 table 测试跑遍三个入口；② 删掉 repro 的 `TestMain`，统一用 `init()` + `ROUTEPILOT_FAKE_KERNEL`；① 把 configgen 的实现替换为对 `internal/fsutil.AtomicWrite` 的调用（或反过来）。 |
| 测试方案 | ④：`TestProxyOwnershipSemantics` table，行=场景（我方端口/用户原值/空串/第三个值），列=三个函数，要求三者对同一场景给出显式一致的结论（或显式记录差异理由）。 |
| 置信度 | 高 |

### A6-13 · P3 · `internal/geoip`、`internal/elevate`、`internal/kernel/api.go` 零测试

| 字段 | 内容 |
|---|---|
| 文件 | `internal/geoip/geoip.go`（108 行）、`internal/elevate/elevate_windows.go`（77 行）、`internal/kernel/api.go`（289 行） |
| 函数/测试 | `geoip.extractCode`（46-60）、`geoip.Location`（99-108）；`elevate.RelaunchAsAdmin`；`Client.CloseConnection/CloseAllConnections/Rules/Proxies/Select/Delay/ProviderProxies/DelayProviderProxy`（148-289） |
| 代码位置 | `go test ./...` 输出里 geoip / elevate 都是 `[no test files]`；`api.go` 只有 `Version` 与 `Configs` 被 `service_generation_test.go:60-70` 的 httptest 间接跑到，其余 14 个方法零覆盖。 |
| 描述 | `extractCode` 是纯函数、判空与 5 个字段名兜底，测起来 10 行；`Location` 是纯查表。`api.go` 的 `do()`（39-77）承载"状态码非 2xx 要带 body 报错"、"JSON 解析失败要区分"两条契约，却没有一个 `httptest` 直接针对它——明明 `download_test.go`、`update_fallback_test.go` 已经在同包内大量用 httptest，成本极低。 |
| 触发条件 | 修改 GeoIP 记录的字段名兜底顺序、或改 `do()` 的错误包装。 |
| 实际影响 | `HomeLocation`（`service.go:3156`）依赖 geoip；`identifyConnection`（3667）依赖 API 返回的 `processPath`。`api.do` 的错误包装是 `logIdentityFallback`、`checkTrafficAlertToday` 的可诊断性来源，改坏了只会表现为"日志变空"。风险中等偏低，但**投入产出比极高**（每个用例 5-15 行）。`internal/elevate` 是 ShellExecute "runas" 提权重启，无法单测进程启动，但可以测"参数拼装"部分（若把拼装抽出来）。 |
| 根因 | 两个包体量小、作者认为"不值得"。 |
| 修复建议 | 给 `api.go` 加 `api_test.go`：一张表覆盖 `do` 的 4 种返回（200+JSON / 200+坏 JSON / 500+body / context 超时），加 `Delay` 的 `PathEscape`（节点名带 `/`、`?`、中文时必须被转义，`api.go:158`）。给 geoip 加 `extractCode` 的 4 种记录形状 + `Location("cn")` 大小写。 |
| 测试方案 | 同上。 |
| 置信度 | 高 |

### A6-14 · P3 · `ResetCore` / `UpdateCore` 吞掉 `RestartKernel` 的失败：用户以为在回滚/降级，实际没有内核

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数/测试 | `UpdateCore`（3468-3473）、`ResetCore`（3517-3531） |
| 代码位置 | `3471` `_ = s.RestartKernel()`（InstallCore 失败后拉回服务）；`3521` `_ = s.StopKernel()`（ResetCore 第一步） |
| 描述 | ① 安装新内核失败后，代码想"把服务拉回来"，但 `_ = s.RestartKernel()` 失败时用户只会看到安装错误，内核其实没起来——系统代理状态此时取决于调用方是否已经 `StopKernel`（3464 确实停过），于是"代理开着但内核没跑"。② `ResetCore` 第一步 `_ = s.StopKernel()`：StopKernel 会先 `SetSystemProxy(false)`，失败时会**中止**停止流程并返回错误（1496-1501），被这里吞掉后代码继续删 `mihomo.exe`——Windows 上删运行中的 exe 会失败（`_ =` 再吞一次，3523），随后 `EnsureBinary` 的 `os.Rename` 也会失败，最终报的是"恢复内置内核失败"，与真实原因（停内核失败/文件被占用）完全无关。 |
| 触发条件 | 系统代理恢复失败（注册表被锁）时点"重置内核"；或 `InstallCore` 失败后自动重启也失败。 |
| 实际影响 | 错误信息把用户和排障者引向错误方向；`ResetCore` 可能出现"旧内核还在跑、自定义标记已清、内置内核没写上"的三重不一致状态。 |
| 根因 | 这两处的错误被当成"已经尽力了"。 |
| 修复建议 | `_ = s.StopKernel()` → 失败即返回 `fmt.Errorf("停止内核失败，无法重置: %w", err)`；`_ = s.RestartKernel()` → 把 restartErr 并入返回的错误（同 `5957-5959` 已有的写法，那里是对的）。 |
| 测试方案 | 已有 `c3_persist_test.go` 的 `writeFileAtomic` 注入与 `c1_*` 的 `fakeRegistry`，`StopKernel` 的失败可以用 `failSetOverride` 注入（`TestR1StopKernelWaitsForProxyTx` 已证明这条失败路径存在）：`fake.failSetOverride=true` → `ResetCore()` 必须返回错误且**不得删除** `bin/mihomo.exe`。 |
| 置信度 | 高 |

### A6-15 · P3 · `ensureGeodata` 吞掉释放失败：内核可能因缺 geoip 而启动失败，但错误与其无关

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数/测试 | `ensureGeodata`（568-580），调用点 `Boot:669` |
| 代码位置 | `service.go:578` `_ = writeFileAtomic(p, data, 0o644)` |
| 描述 | 两个 embed 的 geo 库（geoip.metadb / geosite.dat）写盘失败时完全静默。对比同一段 Boot 里 `EnsureBinary` 失败会 `failBoot`（661）。 |
| 触发条件 | 数据目录只读、磁盘满、杀软拦截大文件写入。 |
| 实际影响 | 内核启动时报"找不到 geoip"或规则匹配全部走默认出口，用户看到的是"分流全错"，而 `debug.log`（`dbg` 的落点）里没有任何线索。`Boot` 里其它每一步都留了痕，唯独这一步没有。 |
| 根因 | "geo 数据不是启动必需"的主观判断——实际 `configgen` 生成的规则含 `GEOIP,CN`，缺库时内核行为不可预期。 |
| 修复建议 | 改为返回 error 并由 Boot 决定是 `failBoot` 还是 `appendBootErr`（建议后者：geo 缺失不该阻止启动，但要告知）。 |
| 测试方案 | 注入 `writeFileAtomic` 失败 → `Boot` 后断言 `bootErr` 含 "geo"；或单独测 `ensureGeodata` 返回值。 |
| 置信度 | 高 |

### A6-16 · P3 · 测试对真实时间与真实端口的依赖：潜在 flaky 面

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/lifecycle_test.go`、`lifecycle_barrier_test.go`、`watchdog_test.go`、`watchdog_r1_test.go`、`internal/procid/tracker_test.go`、`internal/subscription/guard_test.go` |
| 函数/测试 | `freeAddr`（60-71）、`TestAPIUnreachableClassified`（449-474）、`TestStopWhileWaitingAPIReady`（107-139）、`TestWatchdogStableRunResetsCounter`（183-206）、`TestTrackerTTLRefreshes`（174-187）、`TestSSRFTimeout`（247-262） |
| 代码位置 | `freeAddr` 的 TOCTOU：`net.Listen` → 取端口 → `ln.Close()`（62-68），端口在返回后到 `m.Start` 之间可能被别人抢走；`TestAPIUnreachableClassified` 用 `time.Sleep(150ms)` 后再 listen（453），若机器负载高、listen 晚于 700ms 的探测窗口，分类会变成 `ErrStartTimeout` 而非 `ErrAPIUnreachable`；`tracker_test.go:183` 用 `Sleep(30ms)` 跨过 10ms TTL；`guard_test.go:249` 的 500ms 处理 + 100ms 超时。 |
| 描述 | 全仓无 `t.Parallel()`（这点很好，见 §7），但有 31 处 `time.Sleep`，其中 6 处是"靠 sleep 制造先后关系"而不是靠信号。 |
| 触发条件 | 高负载 CI runner（GitHub 的 windows-latest 是共享 2 核）。 |
| 实际影响 | `TestAPIUnreachableClassified` 是最可能偶发红的一个：它断言的是错误**分类**（`ErrAPIUnreachable` vs `ErrStartTimeout`），分类错了测试就红，而根因只是写监听线程晚了 50ms。狼来了几次之后，大家会开始习惯性重跑，进而忽略真正的失败。 |
| 根因 | 少数用例没有可注入的钩子（`Manager` 对 `net.DialTimeout` 是硬编码调用）。 |
| 修复建议 | ① `freeAddr` 改为不关监听、把 listener 传给 `SetStartupValidation` 的变体（或加 `m.portFreeHook`）；② `TestAPIUnreachableClassified` 改为先 listen 再启动 Start（端口已被占用时 `classifyStartFailure` 走的是 `DialTimeout` 成功分支，不需要 sleep）；③ 其余保持——`watchdog_*` 的 sleep 是测真实定时器，属于合理。 |
| 测试方案 | 加 `-shuffle=on -count=3` 到 CI 的 test 步骤（成本几乎为零，能提前暴露顺序依赖）。 |
| 置信度 | 中（未实测复现，属静态推断） |

### A6-17 · P3 · release 流程：无代码签名、tag 上不跑任何测试、`realkernel` 测试永不执行

| 字段 | 内容 |
|---|---|
| 文件 | `.github/workflows/release.yml`、`ci.yml`、`internal/kernel/realkernel_test.go`、`realkernel_procgen_test.go`、`internal/kernel/realkernel_smart_test.go`、`internal/kernel/realkernel_procpath_test.go` |
| 函数/测试 | `release.yml` 全文；4 个 `//go:build realkernel` 测试文件 |
| 代码位置 | `release.yml:19-79`（整个 job 无 `go test`/`go vet`）；`release.yml:63-70`（只 `sha256sum *.zip`，无 `signtool`/Authenticode）；`ci.yml` 的 tags 触发分支只在 `release.yml`（`ci.yml:4-6` 仅 `push: branches: [main]`）；`realkernel` 标签不在任何 workflow 里 |
| 描述 | ① 打 tag 直接发版，**不跑任何测试**——虽然 main 上跑过 CI，但 tag 可以打在任意 commit 上（`workflow_dispatch` 还允许指定任意 tag）。② 二进制无签名：本程序要接管系统代理并安装 wintun 驱动，未签名的 exe 会被 SmartScreen 拦截，且用户无法校验发布者。③ `sha256sum` 只防传输损坏，不防仓库被入侵。④ 4 个 `realkernel` 集成测试（真实 mihomo 起停、provider 加载、smart routing、进程路径）**从不执行**，等于死代码——它们依赖 `assets/mihomo.exe`，而 CI 用 `--placeholders` 造了假文件，所以就算加进 CI 也只会 `t.Skip`。 |
| 触发条件 | 发版。 |
| 实际影响 | ① 一次误 tag 就能把未测代码发出去；② 用户侧信任成本；③ realkernel 测试会随 mihomo 版本升级而静默腐烂（断言里的 `/providers/rules` 响应结构、`config.yaml` 字段名），等真要用时已经跑不起来。 |
| 根因 | 发版流程与验证流程是两套独立配置。 |
| 修复建议 | ① `release.yml` 在打包前加 `go vet ./... && go test -count=1 ./...`（或 `workflow_run` 依赖 CI 成功）；② 加 `signtool sign /fd sha256 /tr <ts> /td sha256`（需要 secrets，可先只做 CI 侧"未签名则告警"）；③ 加一个 `schedule: cron` 的 nightly job：`fetch-assets.py --download && go test -tags realkernel ./...`，让 rust 有机会暴露；④ 补 `SHA256SUMS.txt` 的签名（GPG）或至少把 digest 写进 Release 正文。 |
| 测试方案 | —— |
| 置信度 | 高 |

---

## 3. 关键路径测试覆盖打勾表

判定标准：「有」= 有直接调用被测逻辑、且断言最终状态/副作用（不只看 mock 调用次数）的用例；「只有 mock」= 被测逻辑本身被替身替换，或只断言替身被调用；「无」= 该路径无任何自动化用例。

| 关键业务 | 结论 | 证据（文件:行 / 测试名） | 说明 |
|---|---|---|---|
| 系统代理·接管（写注册表 + 落备份） | **有（事务层 mock + 底层真注册表）** | `c1_proxy_test.go:163`、`c5_1_proxy_test.go:163`；底层 `sysproxy_windows_test.go:37`（真实 HKCU 沙箱） | 事务层用 `fakeRegistry` 真实执行 `setSystemProxyLocked` 全部分支；注册表读写用真实 `HKCU\Software\RoutePilotTest_*`。**唯一的洞是 WinHTTP 分支（A6-02）**。 |
| 系统代理·恢复（Pending→写→校验→删） | **有** | `c1_1_r1_proxy_test.go:37,85,117,150,192,239`、`c1_1_proxy_test.go:12,47,72`、`c5_1_proxy_test.go:48` | 覆盖 4 个字段的写失败、回读不一致、删除失败、外部改写、幂等、重启重试。质量高。 |
| 系统代理·外部改动不覆盖 | **有** | `c1_proxy_test.go:353` `TestT16`、`c1_1_r1_proxy_test.go:192` | 4 字段 × 2 入口（恢复/重试）都有。 |
| WinHTTP 接管/还原 | **无（只有假实现，且零断言）** | `c1_proxy_test.go:124-129`（恒成功）；`sysproxy_windows_test.go:173` 只比常量 | **A6-02**。连 `winHTTPCalls` 字段都没人读。 |
| TUN·事务状态机（fake 下游） | **有** | `c4_tun_test.go:27,78,105,142,182,204,232`、`c5_tun_proxy_test.go:71,105,129,155,193,227,258,306,347`、`c5_1_proxy_test.go:214,245,286,312` | 开/关/失败回滚/快照落盘失败/崩溃恢复/外部改动/PAC-only 全覆盖，质量很高。 |
| TUN·真实数据面（wintun 驱动、路由、DNS、IP 层） | **无** | 全仓无任何测试触碰 `wintun.dll` 或 TUN 网卡 | 结构性缺口：需要管理员 + 真实网卡。见 §8 真机清单。 |
| 内核启停（真实子进程） | **有** | `lifecycle_test.go:119,167,198,223,249`、`lifecycle_barrier_test.go:12,61,107,141,164`、`start_exit_race_test.go:15,70` | 用测试二进制自身伪装内核 + 真实 `Job Object` + 真实 `OpenProcess` 判存活。这是本仓最硬的一块测试。 |
| 内核启停·崩溃后自动恢复（Watchdog） | **有** | `watchdog_test.go:28,55,79,103,151,183,209,235,268`、`watchdog_r1_test.go:153,182,221,254` | 退避、熔断、Pause/Resume、定时器序号作废、Stop 竞态全覆盖。 |
| 内核启停·看门狗与用户操作串行（CoreService 层） | **部分有** | `c1_1_r1_proxy_test.go:445`（熔断恢复 vs 用户开代理） | "看门狗 Restart vs 用户 Restart"在 CoreService 层无用例，但 `lifeMu` 串行 + Manager 层并发已测（`TestRestartStopConcurrent`），风险可接受。 |
| 订阅下载·SSRF 防线 | **有** | `guard_test.go:43,70,78,95,115,133,153,168,191,209,225,247,264` | 字面量/DNS/混合/重定向/次数/体积/TLS/超时全覆盖，用真实 httptest + 注入 dial。 |
| 订阅下载·内容校验（解析、节点数） | **有** | `parse_test.go`、`fetch_test.go:7,33`、`subinfo_test.go:11` | 解析与 userinfo 头解析有 table。 |
| 升级·替换与回滚 | **有** | `upgrade_test.go:35,54,98,106,149,157,179` | 真实 PE 头、真实文件占用（`CreateFile` 不共享 delete）、真实"新内核起不来→回滚→旧内核起得来"。 |
| 升级·包完整性（摘要/大小/域名） | **无** | `update.go:608-620` 无任何测试 | **A6-01**。 |
| 配置生成（规则顺序/字段剥离/Base 不被污染） | **有** | `gen_test.go:11,39,49,79,113,127,140,175,207,220,240,286` | 含一条真实回归（`TestBuildDoesNotMutateBase`）。 |
| 配置生成·写盘失败 | **无** | `gen.go:407-460` 无失败注入；`gen_test.go:49` 只测成功 | 低风险（`configgen.Write` 内部有自己的原子写，失败时 Boot 会 `failBoot`）。 |
| 进程规则（procgen 代际/manifest/哈希回退/路径穿越） | **有** | `c4_procgen_test.go:30,58,89,123,156,178,201` | 7 个用例，含 manifest 损坏、文件截断、路径穿越。 |
| 站点规则 / 进程身份解析 | **有** | `site_rules_test.go:8,37,64,84`、`procid_test.go`（11 个）、`tracker_test.go` | 纯函数 + 真实 Windows 进程快照（`source_windows_test.go`）。 |
| 应用状态持久化（损坏/空/未来版本） | **有** | `c4_config_test.go:11,114,136,160,181` | 覆盖 7 种文件 × 3 种损坏形态。 |
| 单实例 | **无** | `main.go:100-141` | **A6-03**。 |
| 启动编排 Boot / failBoot | **无** | `service.go:608-721`，`Boot` 在测试中零出现 | **A6-09**。 |
| 托盘/窗口/前端绑定 | **无** | `main.go:285-513` | 属 UI，可接受（但 `clampToVirtualScreen` 是纯函数，可测）。 |
| 前端类型检查 | **有** | `ci.yml:29-34`（`npm run lint` = `tsc --noEmit`；`npm run build` = `tsc && vite build`） | 无前端单测，但对 5 万行 TS 应用只有 tsc 是常见取舍。 |

---

## 4. 测试有效性抽检表（10 个用例）

判定方法：只读测试断言与被测实现，推断"把被测函数改成明显错误的实现，这个测试会不会红"。**未做任何代码改动**（纪律 6）。

| # | 测试 | 被测对象 | 会红/不会红 | 推断依据 |
|---|---|---|---|---|
| 1 | `c1_1_r1_proxy_test.go:37` `TestR1RestoreWriteFailuresKeepBackup` | `restoreSystemProxy` → `applyProxySnapshot` 四字段 | **会红** | 4 个 subtest 分别把 `failSetServer`/`failSetOverride`/`failSetPAC`/`failClearPAC` 打开，然后断言 `err != nil`。若 `applyProxySnapshot` 少写任一字段，对应注入不会被触发 → `err == nil` → 第 68 行 `t.Fatal` 立即红。同时断言磁盘 `Saved && Pending` 与内存 `Saved && Pending`，同时锁住"失败要保留依据"这一语义。 |
| 2 | `c5_tun_proxy_test.go:71` `TestC5TunSnapshotPersistedBeforeDisable` | `setTUN(true)` 的快照落盘时序 | **会红** | 用 `fake.disableHook`（在 `Disable` 成功后触发）读盘断言 `TunPrevRegistry != nil`。若把 `saveAppStateErr` 挪到 `Disable` 之后，hook 读到的仍是旧盘 → 84 行红。断言 4 个字段内容（Server/Enabled/Override），少写一个也红。这条测试专门锁"先落盘后改注册表"的顺序，是本仓最好的一个用例。 |
| 3 | `c5_1_proxy_test.go:48` `TestC51PendingRetryPreservesUserNewProxy` | `retryTunProxyRestore` 的字段级归属判定 | **会红** | 4 个 subtest 各改一个字段（`enabled`/`server`/`override`/`pac`），重启后断言"用户新值不被覆盖"**且**"未改字段不被写回旧值"（150-152）。若 `tunRestoreUserChanged` 少比一个字段，对应 subtest 的红；若改成整体快照写回，`ProxyServer` subtest 红。 |
| 4 | `c4_config_test.go:11` `TestCorruptConfigIsPreservedAndReported` | `readConfigJSON` + `preserveCorruptFile` | **会红** | 7 种文件 × 损坏形态，逐个断言：原文件已不存在、`.corrupt-*` 恰好 1 个且内容逐字节相等、`bootErr` 含文件名。若改成"静默当空配置"，第 89 行红；若改成"直接删除"，第 96-98 行红；若忘记写 `bootErr`，103 行红。三重断言很扎实。 |
| 5 | `c4_subscription_test.go:43` `TestRefreshDoesNotOverrideConcurrentSwitch` | `RefreshSubscription` 与 `SwitchSubscription` 的提交互斥 | **会红** | 用 `fetchSubscription` 替换网络层 + 两个 channel 精确卡住"下载中"窗口，期间切到 B，然后断言刷新返回 `errSubscriptionChanged` **且** `currentSubURL()==urlB`。若去掉提交前的那次"激活项是否还是我"的校验，刷新会把 A 写回 → 84 行红。断言的是最终状态不是调用次数。 |
| 6 | `c4_procgen_test.go:58` `TestProcGenManifestFailureKeepsPrevious` | `publishProcRules` 的 manifest 提交点 | **会红** | `procPublishHook("before-manifest")` 注入失败，断言返回错误 **且** `resolvePublishedProcRules` 仍解出上一代内容（`direct` 而非 `proxy`）。若把 manifest 写挪到文件写之前、或失败时不 `cleanup`，后者红。 |
| 7 | `watchdog_test.go:28` `TestWatchdogSingleRestartForBurst` | `ReportExit` 的去重（同一次故障只排一个定时器） | **会红** | 连发 5 次 `ReportExit`，断言 `restarts == 1`。若去掉 `if w.timer != nil { return }` 这段（`watchdog.go:116-120`），5 次都会排定时器 → 计数 5 → 红。注意它是靠 `Sleep(300ms)` 等定时器落地，属"真实时间"依赖，但断言本身有效。 |
| 8 | `lifecycle_test.go:119` `TestConcurrentStartOnlyOneInstance` | `Manager.Start` 的生命周期锁 | **会红（但有调度依赖）** | 100 goroutine 同时 Start，断言 `success == 1` 且其余全是 `ErrAlreadyRunning`，随后 `Stop` 并断言进程真的死了（`processAlive`）。若去掉锁，大概率多个成功 → 红。小概率下所有 goroutine 串行执行、只剩一个成功——但即便那样，`Stop` 后 `processAlive(pid)==false` 仍能抓到"Stop 没真杀进程"。因此**即使并发没触发，也有一半断言有效**。 |
| 9 | `repro_persist_test.go:17` `TestReproT10SubsTruncatedIsOverwritten` | 名义上测 `loadSubs` + `saveSubs` | **不会红** | 它断言的是**旧 bug 的现象**（"原订阅已被空数据覆盖"）：`if strings.Contains(got,"SECRET") { t.Fatal }`。修复后 `loadSubs` 把损坏文件改名为 `.corrupt-*`，`saveSubs` 写出的新文件同样不含 `SECRET` → 绿。**把修复回退，它依然绿**（回退后 saveSubs 会用空数据覆盖，也不含 SECRET）。对被测逻辑的正反两向都不敏感，且第 43 行还 `t.Logf("FAIL: ...")` 输出误导信息。属"为了通过测试而写的假测试"典型。 |
| 10 | `repro_persist_test.go:48` `TestReproT11ConcurrentSubscriptionWrite` | 名义上测共享 `subscription.yaml` 的并发写 | **不会红** | 真正的并发写是测试**自己**在第 78-79 行 `os.WriteFile(...)` 到 `subConfigFile` 做的，产品代码只被顺带调了一次 `writeSubscriptionFiles`（写的是 per-URL 独立文件，`service.go:4091` 用 `subFileName(url)` 区分，本来就不会互相覆盖）。断言"头部是 #A 或 #B"、"body 长度等于 pad 长度"全部由测试自己的写入决定。把 `applySubscription` 的整个写入路径删掉，这个测试依然绿。 |

**抽检结论**：10 个里 8 个是有效测试（其中 #1/#2/#4/#5 属高质量，断言的是最终持久化状态而不是 mock 调用次数）；2 个无效测试**都集中在 `repro` 系列**，且它们正好是 CI 里唯一被单独跑一遍的那组。这说明"整改测试"（c1_/c3_/c4_/c5_）的质量是可信的，而"复现测试转正的残留"（repro_*）需要清理。

**补充抽检（不计入 10 个）**：

- `c5_tun_proxy_test.go:193` `TestC5TunRestorePACOnly`：**会红**。断言 `!fake.enabled`、`fake.pac == 原值`、`fake.server == ""`。若把 `applyProxySnapshot` 里的 `SetServer` 改成"仅当非空才写"，`fake.server` 仍为空 → 不红；但若把 `SetEnabled` 挪到 `SetServer` 之前（顺序错），`!fake.enabled` 仍成立 → 也不红。**它对字段写入顺序不敏感**，但 `c4_tun_test.go` 的 `TestTUNEnableFailureRestoresProxyTakeover` 会抓到（断言 enabled+server 同时正确）。
- `gen_test.go:49` `TestWriteConfigAtomic`：**不会红**。只断言"文件非空 + 无残留 tmp"。若把 `Write` 改成写到一个随机文件名并返回该路径，测试照样绿。属于弱测试（但 `gen_test.go:11` 的规则顺序测试很强，掩盖了这个弱点）。
- `c1_proxy_test.go:239` `TestR1RestoreIsIdempotent`：**会红**，但断言的是 `enableCalls` 计数——这是少见的"断言调用次数"用例，此处合理（幂等的定义就是"不再产生写入"）。
- `startup_restore_plan_test.go:7`：**会红**，5 行决策矩阵 + 一条"默认不接管"。但它只测了纯函数，**"Boot 有没有照着 plan 执行"没测**（A6-09）。

---

## 5. 依赖图与分层

### 5.1 import 图（`go list ./...` + 逐包 grep `routepilot/internal`）

```
routepilot (package main: main.go, service.go, procgen.go, config_files.go)
├── internal/autostart      （叶子，仅 x/sys/windows）
├── internal/configgen      （叶子）
├── internal/elevate        （叶子）
├── internal/geoip          （叶子，maxminddb）
├── internal/kernel    ──→  internal/safego
├── internal/procid         （叶子，仅 x/sys/windows）
├── internal/safego         （叶子）
├── internal/subscription   （叶子）
└── internal/sysproxy       （叶子，仅 x/sys/windows/registry）
```

- 内部包之间**只有一条边**：`kernel → safego`。
- 无循环依赖。
- 9 个 internal 包全部只被根包引用（`elevate`/`geoip` 各 1 处引用）。

### 5.2 分层评价

**加分项**：`internal/*` 的分层是干净的——没有 `internal/a → internal/b → internal/a`，没有 internal 反向 import 根包，`elevate`/`geoip`/`procid`/`safego` 四个包是纯叶子且无业务耦合。这比多数 6000 行单包项目好得多。

**违规/混乱清单**：

| # | 位置 | 问题 | 影响 |
|---|---|---|---|
| L1 | `internal/kernel`（2523 行生产代码） | 一个包塞了 6 个职责：进程生命周期（`manager.go` 770）、看门狗状态机（`watchdog.go` 341）、升级/下载/PE 校验（`update.go` 1005）、REST 客户端（`api.go` 289）、Job Object（`job_windows.go` 118）、日志环形缓冲（`manager.go:91`） | `api.go` 因此拿不到自己的 `api_test.go`（A6-13）；`update.go` 的下载校验与文件替换耦合在一个文件里，导致 A6-01 |
| L2 | `internal/subscription`（796 行） | fetch（509）+ SSRF guard（287）+ parse + redirect 四件事同包，**全部只暴露顶层 `Fetch`** | 内部函数（`doWithDeps`、`checkPublicIP`）需要 `_test.go` 同包才能测（当前正是这么做的），外部无法单独引用；可接受但不利于复用 |
| L3 | 根包 `service.go` | 13 个领域（代理事务、TUN 事务、内核生命周期、订阅仓库、进程规则仓库、站点规则、状态持久化、流量历史、延迟历史、窗口、自动更新、连接、UI View）共用 13 把锁（A6-10） | 测试必须携带全部上下文；A6-08 这类隔离遗漏的土壤 |
| L4 | 根包 `procgen.go` + `service.go:5252-5279` + `service.go:4091` | 文件布局知识（`procgen/`、`subscription.yaml`、`subs.json`、`app-state.json`）散落在 3 个文件里，没有单一"存储层" | 改动文件名要搜 3 处 |
| L5 | `service.go:90-107 defaultSystemProxyOps` | 根包直接持有 `sysproxy.*` 函数值并组装成 `systemProxyOps` 接口 | 这是好设计（为了可注入），但它让"注册表语义"的知识（哪些字段要一起写、`SetServer("")` 等于删值）横跨 `sysproxy` 与 `service.go` 两层，A6-02 的 WinHTTP 就是这样两边都"以为对方管了" |

**建议的最小切分**（与 A6-10 相同，按投入产出排序）：`appstate`（状态持久化）→ `proxytx`（代理事务，把 `service.go:1569-2110` 搬走）→ `substore`（订阅仓库）→ `apiclient`（`kernel/api.go` 独立成包，这样它能有自己的测试文件而不必藏在 kernel 里）。

---

## 6. 吞掉错误的代码清单（问题 6 的直接回答）

`grep -n '^\s*_ = \|_, _ = '` 在 `service.go` 命中 30 处（另有 `internal/*` 若干）。逐条判定：

### 6.1 真问题（掩盖了会影响用户的失败）

| 位置 | 代码 | 判定理由 |
|---|---|---|
| `service.go:702` | `_ = s.writeProcRulesYAML()` | Boot 唯一不报错的一步；失败后内核配置指向过期 generation（A6-05） |
| `service.go:4247` | `_ = s.writeProcRulesYAML()` | 订阅切换后进程规则不更新，静默回退（A6-05） |
| `service.go:4633` | `_ = s.writeProcRulesYAML()` | 同上（inactive 订阅路径） |
| `service.go:3471` | `_ = s.RestartKernel()` | 升级失败后"拉回服务"可能没拉起（A6-14） |
| `service.go:3521` | `_ = s.StopKernel()` | 重置内核时内核可能还在跑，后续删除/替换失败的错误信息全错（A6-14） |
| `service.go:3523` | `_ = os.Remove(.../mihomo.exe)` | 与上一行叠加；被占用时静默失败（A6-14） |
| `service.go:578` | `_ = writeFileAtomic(p, data, 0o644)` | geo 库释放失败无痕（A6-15） |
| `service.go:1678` | `_ = ops.Disable()` | 检测到"备份丢失但有遗留接管"时关代理，关不掉也只返回"已关闭遗留代理"的错误文案——文案与事实可能不符 |
| `service.go:1691/1697/1704/1708` | `_ = s.applyProxySnapshot(bk)` | 回滚失败被吞。Agent 1 已定级 A1-09（P3），此处仅补测试视角：**这 4 个分支零测试**（要注入"Enable 失败 + 回滚也失败"的双重故障，`fakeRegistry` 完全支持） |
| `service.go:3831` | `_ = writeFileAtomic(s.historyPath(), ...)` | 流量历史落盘失败静默；`TrafficHistory()` 会显示断档，用户以为统计坏了（`sampleTrafficOnce` 也无测试） |
| `service.go:4158` | `_ = writeFileAtomic(...subFileName(m.URL), cb, ...)` | 旧版单订阅迁移落盘失败 → 迁移"完成"但原配置丢失（`migrateSingleSub` 无测试） |
| `service.go:5372` | `_ = f.Truncate(0)` | `dbg` 的 2MB 截断失败 → 日志无限增长（`debug.log` 无大小上限兜底）；且 `dbg` 每次调用都 `OpenFile`+`Stat`+可能的 `Truncate`，并发调用时两个 goroutine 可能同时判定超限并截断，丢掉另一方的行 |
| `internal/kernel/manager.go:430` | `safego.Go(..., nil)` | reporter 为 nil，panic 完全静默（A6-04） |

### 6.2 合理忽略（可保留）

| 位置 | 理由 |
|---|---|
| `service.go:506/510` `_ = os.RemoveAll(...)` | 迁移清理，失败留给下次启动 |
| `service.go:599` `_ = ln.Close()` | 端口探测，只为取号 |
| `service.go:2053/2055` 临时文件 `Close`/`Remove` | 清理路径 |
| `service.go:2565-2583` `_ = s.clearTunSnapshot()` | TUN 回滚路径上的尽力清理（快照删除失败下次启动幂等重试） |
| `service.go:4102/4373/4374/4389/4390/4676/4679` `_ = os.Remove(...)` | 删除订阅/配置文件的尽力而为 |
| `internal/sysproxy/sysproxy_windows.go:33-34` `_, _, _ = procInternetSetOptionW.Call(...)` | 通知系统刷新，失败无法补救 |
| `internal/kernel/update.go:445` `_ = os.Remove(mark)` | 标记文件清理 |
| 全部 `_test.go` 里的 `_ =` | `.golangci.yml:27` 已显式排除 |

> **关键结论**：如果 A6-06（CI 跑 golangci-lint）修好，6.1 里的 13 处会立刻被 `errcheck` 全部点名——这正是"静态检查缺位"与"吞错"之间的因果关系。

### 6.3 panic/recover（问题 7）

全仓 `recover()` 只有 2 处，都在 `safego.go`（43、54 行），**设计是正确的**：`protect` 取栈 → 调 reporter → reporter 自己再包一层 recover 防二次 panic；`Run` 用于"每轮"粒度，`Go` 用于一次性任务；注释明确解释了"不能把整个循环包进 Go，否则一次 panic 后循环永远停了"。调用侧也规范：`startNetworkHealthCheck`（3006）、`startHistorySampler`（3860）都写成 `for { ticker.C; tickSafe(...) }`，`spawnAfterTask` 走 `goSafe`。

**唯一的问题就是 A6-04**：`manager.go:430` 传了 `nil`，把"有意兜底"退化成了"静默吞掉"。这不是多余，而是**接错线**。

---

## 7. 已排除的疑点

| 疑点 | 排除依据 |
|---|---|
| `LogBuffer.Clear()` 把 `seq` 归零会让前端日志永久停止更新 | `manager_test.go:24-46` 明确断言"清空后 `Since(oldSeq)` 返回空且 `seq=0`"；前端 `frontend/src/App.tsx:246-252` 有 `if (nextSeq < logSeqRef.current)` 分支做"整段替换"，`onClearLogs`（1021-1033）还先 `logGenRef.current += 1` 作废在途响应。三处配合完整。 |
| 测试用了 `t.Parallel()` 与全局变量冲突 | 全仓 `grep t.Parallel()` 命中 **0**。所有全局（`regPath`、`dataDirOverride`、`repoAPIEndpoints`、`procPublishHook`、`writeFileAtomic`、`removeProxyState`、`appStateWriter`）都靠 `t.Cleanup` 恢复，串行执行下安全。 |
| `-race` 只是"跑了全量所以覆盖了" | 不成立。`lifecycle_test.go:119`（100 goroutine Start）、`:198`（20 轮 Start/Stop 对撞）、`start_exit_race_test.go:15`（精确卡在 before-running 与进程退出之间）、`c1_1_r1_proxy_test.go:269/317/363/400/445`（5 个真并发用例，用 channel 而非 sleep 制造交错）都是**真并发用例**。缺陷只在于 repro 那组没进 race（A6-07）。 |
| `sysproxy` 测试会污染开发机的系统代理 | 不会。`sysproxy_windows_test.go:18-34` 用 `withSandbox` 把 `regPath` 改到 `HKCU\Software\RoutePilotTest_<纳秒>` 并在 Cleanup 里删键。真正的隔离问题是 A6-08（根包的 `c3_persist_test.go`）。 |
| `service.go` 里 178 个方法全都无测试 | 不成立。约 130 个函数名未在 `_test.go` 中直接出现，但其中多数是被覆盖函数的内部步骤（如 `setSystemProxyLocked` 被 `SetSystemProxy` 调用、`reconcileProxyLocked` 被 `reconcileProxy` 调用、`captureProxySnapshot` 被接管路径调用）。真正零覆盖的是 §3 表里标"无"的几项。 |
| `TestStartTimeoutCleansProcess` 会留下孤儿进程 | 不会。它用 `ROUTEPILOT_FAKE_PIDFILE` 拿到子进程 PID，用 `OpenProcess` + `GetExitCodeProcess`（`lifecycle_test.go:101-116`）轮询确认已退出，并给 2s 上限。这是全仓最好的一条"不留垃圾"断言。 |
| `dbg()` 会泄露订阅 token | 未发现。`dbg` 的 66 处调用里，与订阅相关的是 `logIdentityFallback`（3712）与 `save app state failed`（5380），均不打 URL；`process identity degraded: key=%s` 打的是 `path:`/`name:` 键。`subscription` 包内没有任何日志调用。`debug.log` 权限 0644、位于 %APPDATA%，属可接受。 |
| `dbg()` 的 2MB 截断会导致日志被无限重写 | 不是无限——超过 2MB 时 `Truncate(0)` 后继续追加，逻辑上界在 2MB。但存在"两个 goroutine 同时判定超限、同时截断、丢掉对方刚写的行"的竞态（无锁），以及 `_ = f.Truncate(0)` 吞错（6.1）。 |
| `ResetCore` 删不掉 `mihomo.exe` 会导致"回退了但还在用自定义内核" | 不会。`ResetCore:3522` 先 `ClearCustomMark`，`EnsureBinary`（`update.go:923`）因此不会走"保留磁盘版本"分支，而是走哈希比对 → 必然重写 → 写不走会在 `os.Rename` 处报错并被 `ResetCore:3527-3529` 转成"恢复内置内核失败"。真正的问题只是错误文案失真（A6-14）。 |
| `atomicWriteFile` 会在崩溃时留下半截文件 | 不会。`service.go:2041-2079` 写 tmp → `Chmod` → `Write` → `Sync` → `Close` → `Rename`，`defer` 里清理 tmp，`c3_persist_test.go:32` 与 `c1_proxy_test.go:385` 都断言了"失败后旧文件完好 + 无残留 tmp"。 |
| `internal/kernel` 与根包存在循环依赖 | 没有。`kernel` 只 import `safego`，根包 import `kernel`，单向。 |

---

## 8. 未覆盖部分 + 需 Windows 真机执行的测试清单

### 8.1 本机（普通权限）可执行但当前没有自动化的项

| 项 | 建议方式 | 对应问题 |
|---|---|---|
| `DownloadCore` 完整性（摘要/大小/白名单） | `httptest`，无需管理员 | A6-01 |
| `validateDownloadURL` 的 4 种拒绝 | 纯函数 table | A6-01 |
| `internal/kernel/api.go` 全部方法 | `httptest` + 坏 JSON/500/超时 | A6-13 |
| `geoip.extractCode` / `Location` | 纯函数 table（可用假 MMDB 或直接测 `extractCode` 的 4 种 map 形状） | A6-13 |
| 单实例互斥体（同进程重复创建） | 直接调两次 `acquireInstanceMutex` | A6-03 |
| 单实例端口兜底（先占端口再取锁） | `net.Listen` + 调 `acquireSingleInstance` | A6-03 |
| `Boot` 的 failBoot 分支 | 注入 mgr/EnsureBinary 失败 | A6-09 |
| `ConfigGen.Write` 失败（目录只读） | `os.Chmod(0)` 或注入 | §3 |
| WinHTTP 失败分支 | 需要 `fakeRegistry` 增加注入字段（不需要真 HKLM） | A6-02 |
| `CloseConnection` / `CloseAllConnections` | `httptest` 校验 DELETE 路径与 3s 超时 | A6-13 |
| `StopKernel` 失败时 `ResetCore` 的返回 | `fakeRegistry.failSetOverride` | A6-14 |
| `migrateDataDir`（rename / copy / 失败不加锁） | 需要把 `os.UserConfigDir()` 参数化 | A6-09 |
| `clampToVirtualScreen` | 纯函数 | §3 |

### 8.2 需要提权 / 真机 / 真实网卡（**必须人工验收**）

| 项 | 为什么无法自动化 | 验收要点 |
|---|---|---|
| TUN 开启（wintun 驱动加载、虚拟网卡创建、路由表注入、DNS 劫持） | 需管理员 + 驱动安装 + 真实网卡；CI 无提权 runner | 开 TUN 后 `route print` 出现 0.0.0.0/0 指向 wintun；`nslookup` 走 127.0.0.1:<dnsPort>；断开后路由/DNS 复原 |
| TUN 与系统代理互斥（真实注册表） | 需要真实 HKCU + 真实 TUN | 开 TUN 后 `ProxyEnable` 必须为 0；关 TUN 后按快照恢复 |
| TUN 模式下进程分流是否真的生效（`processPath` 由内核上报） | 需真实内核 + 真实进程流量 | 用 `curl.exe` 与 `chrome.exe` 分别验证出口 |
| WinHTTP 接管（HKLM 写 + `netsh winhttp set proxy`） | 需管理员 | `netsh winhttp show proxy` 与 UI 状态一致；还原后为"直接访问" |
| 非管理员启动但上次开过 TUN（回落系统代理） | 需真实权限切换 + 重启 | 提示文案 + 系统代理被接管 + 浏览器可用 |
| Job Object 在**父进程被强杀**时是否真带走内核 | 需任务管理器结束进程 | 结束后 `tasklist` 无 mihomo.exe |
| 内核 exe 被占用时升级（杀软/正在运行） | 需真实文件锁 | 升级失败后旧内核仍可启动，系统代理不变 |
| 单实例：真实双击两次 | 需真实进程 | 第二个实例弹"已在运行"而不是"创建锁失败" |
| 数据目录迁移（ProxDeck/NexusLink → RoutePilot） | 需在 `%APPDATA%` 造旧目录 | 迁移后订阅/规则完整，旧目录按预期删除或保留 |
| 注销/关机时的代理还原（`Shutdown` 与 Windows 关机回调竞争） | 需真实关机流程 | 关机后再登录，注册表无残留指向 127.0.0.1:7890 的死代理 |
| `-tags realkernel` 的 4 个集成测试 | 需 `assets/mihomo.exe`（真实二进制） | `go test -tags realkernel ./...` 应全绿；建议 nightly |

### 8.3 完全放弃自动化的部分（可以接受，但要写进 CONTRIBUTING）

托盘菜单（`main.go:365-513`）、窗口位置恢复、图标设置、`RelaunchElevated` 的 UAC 弹窗、前端 5 万行 TS 的交互逻辑（只有 `tsc --noEmit` 把关）。

---

## 附：CI 缺口表（问题 10 的直接回答）

| 检查项 | 现状 | 缺口 | 建议 |
|---|---|---|---|
| Windows runner | **有**（`ci.yml:10`, `ci.yml:70` `runs-on: windows-latest`，check 与 race 两个 job） | 只有 `windows-latest`（当前 = Server 2022），未覆盖 Win10；无 arm64 | 可选加 `windows-2022` + `GOARCH=arm64` 交叉编译（只编译不测） |
| `go vet` | **有**（`ci.yml:41-42`） | — | — |
| `gofmt -l` | **有**（`ci.yml:45-53`） | — | — |
| `go test` | **有**（`ci.yml:55-56`） | 无 `-shuffle=on`、无 `-count=2`、无 `-cover` | 加 `-shuffle=on`（暴露顺序依赖，见 A6-16）；覆盖率可只上传不门禁 |
| `-tags repro` | **有**（`ci.yml:59-60`） | 不带 `-race`（A6-07） | 见 A6-07 |
| `-race` | **有**（`ci.yml:98-102`，含 MinGW 安装） | 不带 `-tags repro`；`CGO_ENABLED=1` 只在 race job 设，check job 未显式设 0 | 见 A6-07；check job 显式 `CGO_ENABLED: '0'`（发布链路用 0，保证一致） |
| `-tags realkernel` | **无** | 4 个集成测试永不执行（A6-17） | nightly `schedule` job，先 `fetch-assets.py --download` |
| golangci-lint | **无**（配置存在但从不执行，A6-06） | errcheck/staticcheck/unused 全程缺位 | `golangci/golangci-lint-action@v6`，先增量门禁 |
| 前端 `tsc` | **有**（`npm run lint` = `tsc --noEmit`；`npm run build` = `tsc && vite build`，`ci.yml:29-34`） | 无 ESLint（`package.json` 无 eslint 依赖）；无前端单测 | 可接受；若加 eslint 需先修存量 |
| 前端产物先于 go 编译 | **有**（注释明确，`ci.yml:27-28`） | — | — |
| 交叉编译 | **只有** `GOOS=windows GOARCH=amd64`（`ci.yml:63-67`） | 无 arm64 / 386；无 `-tags repro` 或 `realkernel` 的构建验证 | 至少加 `GOARCH=arm64 go build`（Windows on ARM 用户存在） |
| 发布前跑测试 | **无**（`release.yml` 只 build 不 test） | tag 可打在任意 commit 上（A6-17） | 加 `go vet && go test`，或 `needs` 依赖 CI |
| 代码签名 | **无**（只有 `sha256sum *.zip`） | 无 Authenticode；TUN 驱动 + 代理接管类应用的未签名 exe 会被 SmartScreen 拦 | 订阅证书后加 `signtool sign /fd sha256 /tr <ts>`；至少把 SHA256 写进 Release 正文 |
| SBOM / 依赖审计 | **无** | `go.mod` 只 4 个直接依赖，风险低，但 `frontend` 有 100+ 传递依赖 | 可选 `govulncheck ./...` + `npm audit --omit=dev` |
| 覆盖率门禁 | **无** | 覆盖率下跌无感知（但覆盖率本身不是本次目标） | 可选，不强制 |
| macOS/Linux runner | **无** | `internal/*` 多个文件带 `//go:build windows`，跨平台本来就不支持 | 符合产品定位，无需补 |
