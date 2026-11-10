# C5 审计报告：TUN 外部代理恢复与稳定性专项

- 基线提交：`1f48bbd75cc25e17f1d416cc29440b21618b0011`
- 审查方式：逐函数通读实际控制流（`service.go` / `internal/*`），不采信既有整改报告的结论
- 审查范围：TUN、Windows 系统代理、PAC 恢复、进程身份识别、内核升级、开机自启
- 结论：**P1 存在 6 项真实缺陷（其中 3 项可导致用户网络配置永久丢失或谎报），P2 存在 4 项缺陷**。全部已在本阶段修复，详见 `C5-REMEDIATION.md`

---

## 一、P1：TUN 与系统代理恢复

### P1-1（Critical）快照只在内存，未先持久化

- 位置：`service.go` `setTUN()`（基线 L2149-L2161）、`saveAppState()`（基线 L4844）
- 代码事实（修改前）：

```go
var previousRegistry proxyBackup          // 仅内存
prepErr := s.withProxyTx(0, nil, func() error {
    previousRegistry = s.captureProxySnapshot("")
    ...
    if err := s.proxyOps().Disable(); err != nil {   // 已经改动注册表
    ...
})
...
s.tunPrevRegistry = previousRegistry
s.saveAppState()                                   // 内核重启成功后才落盘
```

- 触发条件：用户开着外部代理（Clash Verge / v2rayN，如 `127.0.0.1:7897`）→ 开启 TUN → 在"`Disable()` 执行成功"到"内核重启完成并 `saveAppState()`"之间，进程被任务管理器结束、崩溃或系统断电。
- 影响：快照只存在于进程内存，随进程消失。下次启动 `tunPrevRegistry` 为零值，用户的系统代理被永久关闭且无恢复依据——表现为"开过一次 TUN 之后代理再也回不来"。
- 违反要求：1、3。

### P1-2（Critical）关闭 TUN 时先删快照，后恢复且无回读校验

- 位置：`service.go` `setTUN(on=false)`（基线 L2259-L2295）
- 代码事实（修改前）：

```go
s.mu.Lock()
s.tunPrevRegistry = proxyBackup{}    // 先清空快照
s.mu.Unlock()
s.saveAppState()                      // 先落盘"已删除"

...
if err := s.withProxyTx(0, nil, func() error {
    return s.applyProxySnapshot(prevRegistry)     // 只写，不回读
}); err != nil {
    s.dbg("TUN 关闭后恢复用户原系统代理失败: %v", err)  // 仅记日志
}
```

- 触发条件：
  - 还原过程崩溃/断电 → 快照已删除，无法重试；
  - `applyProxySnapshot` 部分失败（例如写 Override 失败）→ 只打日志返回成功，UI 显示"TUN 已关闭"，实际代理处于半还原状态；
  - 恢复失败后重启 → 无 Pending 标记，启动流程不重试。
- 影响：用户代理停留在半关闭状态；代理状态被谎报为已恢复。
- 违反要求：4、5、6。

### P1-3（High）PAC-only 配置无法恢复

- 位置：`service.go` `setTUN(on=false)`（基线 L2289）、`applyProxySnapshot()`（基线 L1679-L1684）
- 代码事实（修改前）：

```go
if !wantProxy && prevRegistry.Saved && prevRegistry.Enabled && prevRegistry.Server != "" {
    ... // Server 为空直接跳过
}
```

```go
if bk.Enabled && bk.Server != "" {
    errs = append(errs, ops.Enable(bk.Server))
} else {
    errs = append(errs, ops.SetServer(bk.Server), ops.Disable())  // 把 Enabled 也关了
}
```

- 触发条件：用户使用 PAC（`ProxyEnable=1`、`ProxyServer` 为空、`AutoConfigURL` 有值）→ 开 TUN → 关 TUN。
- 影响：跳过恢复，PAC 配置被永久丢弃；即便走到 `applyProxySnapshot`，也会把 `ProxyEnable` 错误地置 0。
- 违反要求：7。

### P1-4（High）关 TUN 前不判断外部修改，会覆盖用户新设置

- 位置：`service.go` `setTUN(on=false)`（基线 L2289-L2291）
- 触发条件：TUN 运行期间用户启动另一个代理软件（或手动打开系统代理）→ 关闭 TUN。
- 影响：`applyProxySnapshot(prevRegistry)` 无条件写回旧值，覆盖用户刚设置的代理。与文件自身在 `proxyStillOurs` 注释中声明的"绝不能覆盖用户的新设置"原则相矛盾。
- 违反要求：8。

### P1-5（High）快照来源错误：本程序接管时取到的是自己的旧端口

- 位置：`service.go` `setTUN(on=true)`（基线 L2152）
- 代码事实（修改前）：

```go
previousRegistry = s.captureProxySnapshot("")   // 读的是"当前注册表"
```

- 触发条件：上一会话本程序接管过系统代理（注册表指向 `127.0.0.1:17890`），本会话开 TUN。
- 影响：快照记录的是本程序自己的地址，而不是用户原始代理；关 TUN 时会把用户代理还原成一个可能已失效的本程序端口（死端口）。
- 关联要求：1、2。

### P1-6（High）开 TUN 前置失败时，快照未落盘就回滚，恢复依据丢失

- 位置：`service.go` `setTUN(on=true)` 的 `prepErr` 分支（基线 L2172-L2188）
- 触发条件：`restoreSystemProxyLocked` 或 `Disable` 失败，且回滚 `applyProxySnapshot` 也失败。
- 影响：注册表处于未知状态，但内存/磁盘都没有可用快照，只能等下次启动的 `proxy.json` 兜底；若 `proxy.json` 已被恢复事务清掉，则无依据。
- 关联要求：3、6。

### P1-7（Medium）启动流程不处理"TUN 已关闭但快照未恢复"

- 位置：`service.go` `Boot()`（基线 L575 `recoverPendingProxyRestore` 之后）、`Shutdown()`
- 触发条件：关 TUN 后进程崩溃；或非管理员启动时 `tunOn` 被强制置 false（TUN 实际未生效）。
- 影响：磁盘上的快照永远不被消费，用户代理保持关闭。
- 关联要求：6。

---

## 二、P2：版本比较

### P2-1（High）非 SemVer 比较，可误判并引导降级

- 位置：`internal/kernel/update.go` `versionNewer()`（基线 L174-L218）、`CheckUpdate()`（基线 L160）
- 代码事实（修改前）：按 `.` 分段 `Atoi`；`-`/`+` 之后一律当作"预发布"，只比较核心数字；`hasUpdate = lat != cur` 的语义被替换成"核心段不同"。
- 触发条件：
  - `1.19.0-alpha.2` 与 `1.19.0-alpha.10`：数字段不比较，先出现差异的核心段相同 → 判为相等；
  - `1.19.0-rc1` 与 `1.19.0-rc2` 同理；
  - 本地装的是比 latest 更新的预发布版时，仍可能提示升级；
  - build metadata 差异（`+build.5` / `+build.6`）会干扰判断。
- 影响：升级提示失真，最坏情况把用户从新版本降级到旧版本。
- 违反要求：1-5。

---

## 三、P2：进程身份识别

### P2-2（Medium）PID 复用时名称与路径可能来自两个进程

- 位置：`internal/procid/source_windows.go` `listWindowsProcesses()` / `queryProcess()`（基线 L59-L98）
- 代码事实（修改前）：名称取自 Toolhelp 快照的 `ExeFile`，路径/创建时间取自随后按 PID 的 `OpenProcess`，两者之间无一致性校验。
- 触发条件：枚举与 `OpenProcess` 之间该 PID 退出并被复用。
- 影响：拼出不存在的 `Name+Path` 组合，破坏后续 `Created` 复用检测，可能生成错误的 `PROCESS-PATH` 规则。

### P2-3（Medium）快照刷新失败被静默吞掉

- 位置：`internal/procid/tracker.go` `refreshLocked()`（基线 L156-L160）
- 触发条件：`Source.List()` 返回错误（权限不足、系统繁忙）。
- 影响：沿用旧快照但无任何留痕，调用方无法知道本次解析基于陈旧数据；与"身份降级必须显式记录"的要求不符。

### P2-4（Low）陈旧快照下的实例校验缺失

- 位置：`internal/procid/tracker.go` `LookupName()`（基线 L102-L130）
- 说明：基线在采纳唯一匹配路径前没有实时校验进程实例；C4 阶段虽有校验钩子，但未针对"同名进程在 TTL 内被替换"写测试。

---

## 四、P2：开机自启

### P2-5（High）计划任务查询结果只有"存在/不存在"二态

- 位置：`internal/autostart/autostart_windows.go` `taskExists()`（基线 L135-L138）、`Set()`（基线 L41-L70）
- 代码事实（修改前）：

```go
func taskExists() bool {
    cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
    return cmd.Run() == nil     // 权限不足同样返回 false
}
...
if taskExists() { ... } else { return setRunValue() }   // 于是写了 Run 项
```

- 触发条件：计划任务真实存在，但当前用户无权限查询（或 `schtasks` 异常）。
- 影响：判定"不存在" → 追加 HKCU Run 项 → 任务与 Run 并存 → 登录时双实例启动，输者弹出"已经在运行"。
- 违反要求：三态化。

---

## 五、已核实无问题（避免重复劳动）

- `withProxyTx` 代际校验与 `proxyMu` 串行化：当前实现无 TOCTOU 缺口。
- `proxy.json` 恢复事务（落盘 Pending → 写注册表 → 回读 → 删除 → 提交内存）：顺序正确，删除失败保留备份。
- `captureProxySnapshot` 覆盖 ProxyEnable/ProxyServer/ProxyOverride/AutoConfigURL 四字段。
- `sysproxy` 的注册表错误处理：`SetServer`/`RestoreWinHTTP` 已区分 `ERROR_FILE_NOT_FOUND`。
- `configgen` 组名过滤、订阅 scheme 白名单：C4 修复有效。

---

## 六、风险结论

| 维度 | 审计前 | 审计后 |
|---|---|---|
| 用户网络配置丢失风险 | 高（P1-1/P1-2/P1-3/P1-5） | 低 |
| 代理状态谎报风险 | 高（P1-2） | 低 |
| 进程分流错误匹配风险 | 中（P2-2/P2-3） | 低 |
| 升级降级风险 | 中（P2-1） | 低 |
| 双实例启动风险 | 中（P2-5） | 低 |

未解决的遗留项见 `C5-TEST-REPORT.md` 第 4 节。
