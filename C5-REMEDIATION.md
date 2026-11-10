# C5 整改报告

基线：`1f48bbd75cc25e17f1d416cc29440b21618b0011`
整改原则：优先保证用户网络配置绝不丢失、代理状态绝不谎报、进程分流绝不错误匹配。

---

## 一、P1：TUN 与系统代理快照

### 1.1 数据结构扩展

`service.go` `CoreService` 新增：

```go
tunPrevRegistry proxyBackup // 开 TUN 前用户原始系统代理（落盘）
tunAfterDisable proxyBackup // TUN 生效后注册表样貌（判定外部修改）
tunProxyPending bool        // 已关闭 TUN 但原始代理未恢复成功（落盘重试）
```

`appState` 同步新增 `tunPrevRegistry` / `tunAfterDisable` / `tunProxyPending` 三个字段，`loadAppState` / `saveAppStateErr` 全量读写。

### 1.2 新增错误哨兵

```go
errTUNSnapshotSaveFailed // 快照无法落盘 → 必须中止开启 TUN
errTUNRollbackFailed     // 前置失败且注册表回滚失败 → 保留快照待启动恢复
```

### 1.3 开启 TUN：控制流（修改后）

`service.go:2157 setTUN(on=true)`

```
tunChanging = true（拒绝并发切换）
  ↓
withProxyTx（持有 proxyMu，与其它代理事务串行）
  1. previousRegistry = prevBackup（若本程序接管过，备份即用户原始配置）
     否则 previousRegistry = captureProxySnapshot()
  2. tunPrevRegistry = previousRegistry; tunAfterDisable = 空; pending = false
  3. saveAppStateErr() —— 失败则立刻中止（errTUNSnapshotSaveFailed）
  4. 若本程序接管过：restoreSystemProxyLocked()
  5. proxyOps().Disable()
     失败 → applyProxySnapshot(previousRegistry) 回滚；
            回滚也失败 → errTUNRollbackFailed（保留快照 + pending）
  6. restoreWinHTTPIfPendingLocked()
  7. afterDisable = captureProxySnapshot()（TUN 生效后的样貌）
  ↓
restart()（内核重启，配置含 TUN）
  失败 → 回滚注册表；快照按"回滚是否成功"决定保留或清除
  ↓
成功 → tunOn = true; tunPrevProxyOn = 原接管状态;
        tunPrevRegistry = previousRegistry; tunAfterDisable = afterDisable
        saveAppState()
```

关键改进：

- 快照在**任何注册表写入之前**落盘；落盘失败直接中止，绝不关闭用户代理（要求 1、3）。
- 快照来源改为"本程序接管备份"优先，避免记录自己的旧端口（P1-5）。
- 前置失败时，只有回滚成功才删除快照；回滚失败保留 `pending`，交给启动恢复（P1-6）。

### 1.4 关闭 TUN：控制流（修改后）

`service.go:2157 setTUN(on=false)` → `service.go:2432 restoreTunExternalProxyLocked()`

```
提交 tunOn = false（先落盘；快照暂不删除）
  ↓
wantProxy（原先由本程序接管）
  → SetSystemProxy(true)：成功后清快照；失败置 restoreProxy 并清快照（避免还原到旧端口）
  ↓
!wantProxy（用户外部代理）
  → restoreTunExternalProxyLocked(prev, after)
      1) 若 pending=false：比对当前注册表与 after
         （enabled / server / override / PAC 全字段）
         不一致 → 判定外部已接管 → 放弃还原（绝不覆盖用户新设置）
         pending=true 时跳过该判定（上一次恢复已写过部分注册表，
         中间态本就不同于 after，再判外部会把自己的半成品误认为用户配置）
      2) pending = true; saveAppStateErr()  ← 先落盘恢复意图
      3) applyProxySnapshot(prev)
      4) verifyProxySnapshot(prev)         ← 回读校验
      5) clearTunSnapshot()                ← 校验通过才删除
```

`service.go:2407 clearTunSnapshot()`：落盘失败时**回滚内存快照**并返回错误，调用方如实上报"删除依据失败"，不谎报成功。

### 1.5 PAC-only 支持

`service.go:1700 applyProxySnapshot()` 重排写入顺序：

```
按 bk.Enabled 调 Enable(server) 或 Disable()
  → SetServer(bk.Server)     （空则删除值）
  → SetOverride(bk.Override)
  → SetAutoConfigURL 或 ClearAutoConfigURL
```

- `Enabled=true + Server=""` 的 PAC-only 组合不再被错误地 `Disable()`。
- `verifyProxySnapshot` 只在 `Server != ""` 时校验地址，PAC-only 校验启用态、绕过列表与 PAC 地址。

### 1.6 启动与退出补偿

`service.go` `Boot()`（`recoverPendingProxyRestore` 之后）：

```go
if err := s.retryTunProxyRestore(); err != nil {
    s.appendBootErr("上次 TUN 关闭时系统代理未恢复完成: " + err.Error())
}
```

`service.go:2501 retryTunProxyRestore()`：

- `tunOn == true`（TUN 仍生效）→ 直接返回，避免双代理；
- 否则进入完整恢复事务（含外部修改判定、Pending、回读校验）。

`Shutdown()` 增加同样的补偿调用。

---

## 二、P2：SemVer 2.0.0 版本比较

`internal/kernel/update.go`

新增 `parseSemver` / `compareSemver`（L189 / L235），替换原 `versionNewer` 的字符串逻辑：

| 规则 | 实现 |
|---|---|
| 前导 v/V | 忽略 |
| build metadata `+xxx` | 截断，不参与比较 |
| 预发布 `-alpha` / `-beta` / `-rc.N` | 低于同核心正式版 |
| 预发布标识符 | 数字按数值比较；数字低于字母；字母按 ASCII；字段少者优先级低 |
| 缺省段 | 按 0 补齐（`1.19` == `1.19.0`） |
| 非法版本 | 返回 `ok=false`，保守不提示升级（避免降级） |

`CheckUpdate`（L148）：

```go
hasUpdate := false
if cur != "" && lat != "" {
    if cmp, ok := compareSemver(lat, cur); ok {
        hasUpdate = cmp > 0
    }
}
```

---

## 三、P2：进程身份识别

`internal/procid/tracker.go`

- `refreshLocked` 刷新失败时 `t.log("进程快照刷新失败，沿用上一份快照: %v", err)`，降级不再静默。
- `LookupName` 在采纳唯一路径前调用注入的 `verify`（`SetVerifyFunc`）实时校验 PID 实例；不一致则强制刷新一次，仍不一致返回 `LookupAmbiguous` 并记录"降级为名称匹配"。

`internal/procid/source_windows.go`

- `queryProcess`：`QueryFullProcessImageName` 得到的 basename 必须与 Toolhelp 快照里的 `ExeFile` 一致（大小写不敏感），否则丢弃路径、只保留名称——避免 PID 复用时拼出"旧名 + 新路径"的错误身份。
- `ProcessMatches`（L21）：`OpenProcess` + `GetProcessTimes` 比对创建时间，供 `Tracker` 作为 `verify` 使用。

`service.go` 构造函数里接线：`s.procTracker.SetVerifyFunc(procid.ProcessMatches)`。

---

## 四、P2：开机自启三态查询

`internal/autostart/autostart_windows.go`

```go
type taskState int
const (
    taskUnknown taskState = iota  // 查询失败/权限不足
    taskNotFound                  // 明确不存在
    taskPresent                   // 明确存在
)
```

- `classifyTaskQuery`（L53）：仅当输出包含 `cannot find` / `找不到` / `does not exist` 才判 `taskNotFound`，其余非零退出判 `taskUnknown` 并返回错误。
- `Set`（L94）：
  - 查询失败 → 返回错误；`enable=false` 时仅清理 Run 项，绝不新增启动项；
  - 仅 `taskNotFound` 才允许创建另一种自启；
  - `taskPresent` + 非提权 → 报错引导提权，不写 Run；
  - `taskPresent` + 提权 → 清理 Run，保持单一自启动源。
- `UpgradeIfElevated`（L142）：查询失败直接返回错误，不改动 Run 项；成功路径始终收敛 Run 项。
- `Enabled`（L77）：查询失败时按"可能已启用"处理，避免界面谎报关闭。
- 注入点 `queryTask` / `writeRunValue` / `deleteRunValue` 供测试模拟，不触碰真实注册表。

---

## 五、测试注入点（仅测试可见影响）

| 注入点 | 位置 | 用途 |
|---|---|---|
| `fakeRegistry` | `c1_proxy_test.go` | 系统代理注册表全量仿真 |
| `appStateWriter` | `service.go:5095` | 注入 app-state 落盘故障 |
| `disableHook` / `enableHook` | `c1_proxy_test.go` | 在注册表动作瞬间读快照，验证顺序 |
| `queryTask` / `writeRunValue` / `deleteRunValue` | `internal/autostart` | 三态查询与双实例行为 |
| `SetVerifyFunc` | `internal/procid` | 注入 PID 实例校验 |
| `mutableSource` | `internal/procid/tracker_test.go` | 动态进程快照 |

所有注入点在 `t.Cleanup` 中恢复，生产路径默认指向真实实现。

---

## 六、未做改动的部分及理由

- **TUN 开启时内核未运行**：当前实现会先关闭系统代理再置 `tunOn=true`，存在短时间无代理窗口。这是既有设计（UI 通常要求内核运行），本阶段不扩大改动面，已记入遗留项。
- **`reconcileProxy` 的端口校正**：C4 已修复，本轮复核未发现新问题。
- **WinHTTP 恢复**：沿用既有事务，新增的 TUN 快照不与其交叉。

---

## 七、兼容性

- `app-state.json` 新增字段均为 `omitempty`，旧版本文件可直接读取，缺失字段按零值处理。
- 旧版本写下的 `proxy.json` 语义未变。
- 无配置版本号提升，无需迁移。

---

## 八、补充修复：`-race` 发现的测试钩子竞争

本机安装 MinGW-w64（WinLibs GCC 16.2.0）后首次执行 `go test -race ./...`，报出：

```
Write at ... r1Service.func1()            c1_1_r1_proxy_test.go:20  // t.Cleanup 恢复全局
Previous read ... dataDir()               service.go:425
  restoreProxyTransactionLocked → startKernelWatchdog.func3
  → kernel.(*Watchdog).exhaustLocked.gowrap1()
```

- 根因：`dataDirOverride` 是测试专用全局；看门狗熔断恢复的 goroutine 在测试用例结束后仍会继续执行代理还原并调用 `dataDir()`，与 Cleanup 的写并发。
- 修复：`service.go` 用 `sync.RWMutex` 保护该全局，新增 `setDataDirOverride` / `getDataDirOverride`；9 个测试文件共 14 处改为访问器。
- 影响面：仅测试基础设施，生产 `dataDir()` 行为不变。
- 结果：`go test -race -count=1 ./...` 全绿，无 DATA RACE。
