# C5 测试报告

基线：`1f48bbd75cc25e17f1d416cc29440b21618b0011`

所有命令均在 `C:\Users\wpc\Documents\Trae-CN\default\nexuslink_review` 下执行，结果取自实际运行输出。

---

## 一、执行结果总览

| # | 验证项 | 命令 | 结果 |
|---|---|---|---|
| 1 | 全量单元测试 | `go test -count=1 ./...` | ✅ 通过 |
| 2 | 重复执行（含 kernel/root） | `go test -count=3 ./internal/kernel .` | ✅ 通过（kernel 29.2s / root 7.4s） |
| 3 | repro 构建标签 | `go test -count=1 -tags repro ./...` | ✅ 通过 |
| 4 | 数据竞争检测 | `CGO_ENABLED=1 go test -race -count=1 ./...` | ✅ 通过（见 3.1） |
| 5 | 静态检查 | `go vet ./...` | ✅ 无输出 |
| 6 | 格式检查 | `gofmt -l .` | ✅ 无输出 |
| 7 | 前端构建 | `npm run build`（frontend） | ✅ 通过（67 modules，299.14 kB） |
| 8 | Windows amd64 编译 | `GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H windowsgui"` | ✅ 通过（87.2 MB） |
| 9 | 真实 mihomo 回归 | `go test -tags realkernel -run TestRealKernel ./internal/kernel` | ✅ 3/3 通过 |

第 1 项实际输出：

```
ok  routepilot                          3.457s
ok  routepilot/internal/autostart       0.828s
ok  routepilot/internal/configgen       0.821s
?   routepilot/internal/elevate         [no test files]
?   routepilot/internal/geoip           [no test files]
ok  routepilot/internal/kernel         11.573s
ok  routepilot/internal/procid          0.879s
ok  routepilot/internal/subscription    2.552s
?   routepilot/internal/sysproxy        [no test files]
```

第 9 项实际输出（真实 mihomo v1.19.33）：

```
--- PASS: TestRealKernelDistinguishesSameNameProcessPaths (0.28s)
--- PASS: TestRealKernelSmartRoutingRulesStart (0.22s)
--- PASS: TestRealKernelLifecycle (0.21s)
ok  routepilot/internal/kernel  2.837s
```

---

## 二、新增测试清单

### 2.1 TUN 外部代理（`c5_tun_proxy_test.go`）

| 测试 | 覆盖要求 | 断言要点 |
|---|---|---|
| `TestC5TunSnapshotPersistedBeforeDisable` | 1、2 | 在 fake 的 `disableHook`（Disable 执行瞬间）读取 app-state，断言快照已落盘且四字段完整 |
| `TestC5TunCrashBeforeCommitRestoresOnBoot` | 3、6 | 构造"TUN 未提交 + 快照在盘"的崩溃现场，启动补偿后代理恢复、快照被清 |
| `TestC5TunOnBootKeepsSnapshot` | 3 | TUN 仍生效时不打开系统代理、保留快照 |
| `TestC5TunOffRestoreFailureKeepsPendingThenRetries` | 4、6 | 注入 `SetOverride` 失败 → 报错 + Pending + 快照保留；故障消失后重试成功 |
| `TestC5TunRestorePACOnly` | 7 | `Enabled=true, Server="", PAC 有值` 完整恢复且不写 ProxyServer |
| `TestC5TunExternalChangeNotOverwritten` | 8 | TUN 期间外部把代理改成 `127.0.0.1:1080`，关 TUN 后该值原样保留 |
| `TestC5TunSnapshotDeleteFailureReported` | 5 | 注入 app-state 落盘失败：恢复成功但删除快照失败必须报错、内存快照回滚；重试幂等收敛 |
| `TestC5TunAbortsWhenSnapshotCannotPersist` | 1、3 | 快照无法落盘时中止，`Disable` 调用次数为 0，注册表不变 |
| `TestC5TunSnapshotUsesOriginalFromTakeoverBackup` | 1、2（P1-5） | 已有接管备份时快照取用户原始值（7897），不取本程序旧端口 |

### 2.2 SemVer（`internal/kernel/c5_semver_test.go`）

- `TestCompareSemver`：27 组边界，覆盖 `v` 前缀、缺省段、四段版本、`alpha/beta/rc`、数字 vs 字母标识符、`alpha.2 < alpha.10`、`rc.2 < rc.10`、build metadata 忽略、5 组非法输入。
- `TestVersionNewerSemantics`：11 组，验证"已装更新版本不提示升级""预发布低于正式版""无法解析时不提示升级"。

### 2.3 进程身份（`internal/procid/c5_identity_test.go`）

| 测试 | 覆盖 |
|---|---|
| `TestLookupNamePIDReuseForcesRefresh` | 快照未过期但 PID 被新实例复用 → 必须刷新到新路径 |
| `TestLookupNameStaleInstanceDowngradesWithLog` | 实例校验持续失败 → `LookupAmbiguous` 且日志含"降级为名称匹配" |
| `TestLookupNameSameNameDifferentPathIsAmbiguous` | 同名不同路径 → 歧义，不生成路径规则 |
| `TestRefreshFailureIsLogged` | 刷新失败返回错误且留痕 |

### 2.4 开机自启（`internal/autostart/c5_taskstate_test.go`）

| 测试 | 覆盖 |
|---|---|
| `TestClassifyTaskQuery` | 6 组：present / 英文不存在 / 中文不存在 / 另一种不存在文案 / 权限不足 / 执行异常 |
| `TestSetUnknownTaskStateNeverWritesRunValue` | 权限不足时开启（普通/管理员）均报错且 `writeRunValue` 调用次数为 0 |
| `TestSetWithExistingTaskConvergesRunValue` | 任务存在时提权开启：不写 Run，且清理 Run |
| `TestSetNonElevatedWithExistingTaskRejected` | 任务存在时非提权开启：报错、不写 Run |
| `TestUpgradeIfElevatedUnknownStateIsNoop` | 查询失败时升级：报错且不改动 Run |

---

## 三、-race 执行记录与发现

### 3.1 环境准备

本机初始无 C 编译器。通过 winget 安装 WinLibs（真实 GCC，UCRT）：

```powershell
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e `
  --accept-source-agreements --accept-package-agreements --disable-interactivity
gcc --version
# gcc.exe (MinGW-W64 x86_64-ucrt-posix-seh, built by Brecht Sanders, r3) 16.2.0
```

安装路径已写入用户 PATH；构建时显式前置该目录：
`C:\Users\wpc\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin`

### 3.2 首次执行发现的问题（已修复）

首次 `-race` 执行失败：

```
WARNING: DATA RACE
Write at ... by goroutine 9:
  routepilot.r1Service.func1()  c1_1_r1_proxy_test.go:20   // 测试 Cleanup 恢复全局
Previous read at ... by goroutine 12:
  routepilot.dataDir()          service.go:425
  routepilot.(*CoreService).restoreProxyTransactionLocked()
  routepilot.(*CoreService).startKernelWatchdog.func3()
  routepilot/internal/kernel.(*Watchdog).exhaustLocked.gowrap1()
--- FAIL: TestR1WatchdogExhaustedRestoreSerializedWithUser
```

根因：`dataDirOverride` 是测试用全局变量，看门狗熔断恢复的 goroutine 会在测试结束后仍执行代理还原并调用 `dataDir()`，与 `t.Cleanup` 恢复变量并发读写。

修复：`service.go` 将全局改为受 `sync.RWMutex` 保护的 `setDataDirOverride` / `getDataDirOverride`，所有测试改用访问器；生产路径的 `dataDir()` 语义不变。

判定：属于**测试基础设施的数据竞争**，不是产品逻辑缺陷（`dataDir()` 在生产环境为常量，不存在并发写）。

### 3.3 修复后执行结果

```
$env:Path = '<winlibs>\mingw64\bin;' + $env:Path
$env:CGO_ENABLED='1'
go test -race -count=1 ./...

ok  routepilot                          5.940s
ok  routepilot/internal/autostart       1.680s
ok  routepilot/internal/configgen       1.662s
ok  routepilot/internal/kernel         14.488s
ok  routepilot/internal/procid          1.710s
ok  routepilot/internal/subscription    2.765s
```

**数据竞争检测通过，无 WARNING: DATA RACE。**

### 3.4 真实注册表验证

按要求，所有系统代理测试使用 `fakeRegistry`，**未对开发机真实注册表做任何写入**。
TUN 相关用例全部通过 `svc.sys = fake.ops()` 注入，且 `c5WriteAppState` 带防呆断言：若 `dataDirOverride` 为空（会写到真实 `%APPDATA%`）直接 `t.Fatal`。

### 3.5 TUN 真实网卡验证

TUN 需要管理员权限与真实虚拟网卡，本阶段未执行真实 TUN 数据面验证，仅验证控制流、快照与注册表事务。见第 4 节遗留项。

---

## 四、尚未解决的问题

1. **内核未运行时开启 TUN**：`setTUN(on=true)` 会先关闭系统代理并把 `tunOn` 置 true，但不会启动内核；若用户此前手动停过内核，会出现"无代理且无 TUN"的窗口。属于既有设计缺陷，不在本阶段范围内。
2. **真实 TUN 数据面回归**：需要在管理员环境实际开启 TUN、走系统代理切换、再关闭，验证 WinINET 与 TUN 无叠加。
3. **双故障叠加场景**：外部修改代理 + 上一次恢复已失败（`pending=true`）时，重试会优先还原原始快照（可能覆盖外部改动）。当前取舍是"保证部分写入可收敛"，已在 `restoreTunExternalProxyLocked` 注释中说明。
4. **`afterDisable` 的 override 精确性**：若 TUN 期间有软件只修改 ProxyOverride 而不改启用态，会被判定为外部修改并放弃还原（保守策略，不会覆盖用户设置）。

---

## 五、Release Candidate 判定

| 条件 | 状态 |
|---|---|
| P1 全部缺陷修复且有确定性测试 | ✅ |
| P2 版本比较符合 SemVer 2.0.0 并覆盖边界 | ✅ |
| P2 进程身份降级显式记录 | ✅ |
| P2 自启三态化，杜绝双实例 | ✅ |
| 全量测试 / vet / gofmt / 前端构建 / amd64 编译 | ✅ |
| `-race` 数据竞争检测 | ✅ 通过（含修复测试钩子竞争） |
| 真实 TUN 数据面回归 | ⚠️ 未执行 |

**结论：代码与自动化测试层面达到 Release Candidate 标准（含 `-race` 全绿）。仅剩真实 TUN 数据面回归需在管理员环境人工验证；该验证不涉及代码变更风险，建议发布前补做，其余无阻塞项。**
