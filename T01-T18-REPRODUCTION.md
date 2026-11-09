# NexusLink T01-T18 专项故障复现报告

- 执行日期：2026-11-09
- 代码基线：`70adff3` + 本阶段 C1 修改（工作区未提交）
- 环境：Windows 11 专业版 x64，非管理员（`elevated=False`），Go 1.27.3 windows/amd64，无 CGO / gcc，无 `golangci-lint`
- 复现方式：故障注入单元/集成测试（可执行）、本地真实 mihomo v1.19.33（T14）
- 判定约定：
  - **FAIL（已复现）**：测试实际执行并观察到错误行为，已证明缺陷存在。
  - **PASS**：测试实际执行，当前实现满足预期。
  - **PARTIAL**：部分路径已实际执行验证，剩余路径无法在不改代码的前提下验证。
  - **BLOCKED**：当前环境无法执行，未标记为通过。

复现用例文件（均带 `//go:build repro`，默认 CI 不执行，需显式 `-tags repro`）：

| 文件 | 覆盖 |
|---|---|
| `c1_proxy_test.go` | T01-T04、T10（代理状态）、T16 |
| `internal/kernel/repro_concurrency_test.go` | T05-T09、T18 |
| `repro_persist_test.go` | T10（订阅）、T11 |
| `repro_t12_test.go` | T12 |
| `internal/subscription/repro_t17_test.go` | T17 |

---

## 汇总表

| 编号 | 场景 | 结果 | 复现次数 | 关联问题 | 最终等级 |
|---|---|---|---|---|---|
| T01 | 接管后强杀，下次启动恢复 | PASS（修复后） | 2/2 | P1-10, P1-07 | P1（已修） |
| T02 | 恢复失败备份保留且可重试 | PASS（修复后） | 1/1 + 重试 | P1-01, P1-02 | P1（已修） |
| T03 | TUN 开启失败完整恢复 | PASS（修复后） | 1/1 | P1-03 | P1（已修） |
| T04 | TUN 关闭失败 UI 与实际一致 | PASS（修复后） | 1/1 | P1-03 | P1（已修） |
| T05 | 并发 Start 最多一个内核 | **FAIL 已复现** | 10/10 均成功 | P1-05 | **P1（未修，C2）** |
| T06 | Start/Stop 并发无孤儿 | PARTIAL | 1/1 | P1-05 | P1（C2） |
| T07 | 并发 Restart 串行化 | PASS（本环境） | 5 并发 | P1-05 | P1（风险保留，C2） |
| T08 | 崩溃有限次恢复 | PARTIAL | 1/1（主动停止不误报） | P1-06 | P1（C2） |
| T09 | Job Object 失败不留孤儿 | **FAIL 已复现**（静默失败） | 1/1 | P2-19 | P2（C2） |
| T10 | 磁盘写失败保留有效配置 | **FAIL 已复现**（订阅）；代理 PASS | 1/1、1/1 | P1-10, P1-11 | P1（代理已修，订阅未修） |
| T11 | 更新期间切换订阅不串写 | PASS（未触发交错） | 12 轮 | P1-12 | P1（风险保留，C2/C4） |
| T12 | 两个同名 EXE 不误伤 | **FAIL 已复现** | 1/1 | P1-08 | P1（未修，C3） |
| T13 | PID 复用不继承旧身份 | BLOCKED | - | P1-08 | P1（C3） |
| T14 | API 端口冲突正确识别 | **FAIL 已复现**（真实内核） | 1/1 | P1-13 | P1（未修，C2） |
| T15 | 休眠唤醒后接管状态可恢复 | BLOCKED | - | P1-04 | 未定级 |
| T16 | 外部改代理不误覆盖 | PASS（修复后） | 1/1 | P1-04 | P1（已修） |
| T17 | 订阅重定向不访问未授权目标 | **FAIL 已复现**（SSRF） | 1/1 | P2-11 | **P1（升级）** |
| T18 | 更新内核强退不损坏二进制 | **FAIL 已复现** | 1/1 | P2-07 | **P1（升级）** |

---

## T01 系统代理接管后强杀，下次启动恢复

- 关联问题：P1-07、P1-10
- 前置条件：`proxy.json` 存在且 `Pending=true`；注册表仍指向本程序旧端口。
- 操作：构造"恢复中崩溃"的磁盘状态，调用 `recoverPendingProxyRestore()`。
- 预期：还原用户原始 `ProxyServer`/`Enabled`/`Override`/PAC，删除备份，清除 `restoreProxy` 意图。
- 实际：全部满足；`bootErr` 为空。
- 第二个场景：正常接管中崩溃（`Pending=false`，注册表指向旧端口） → `reconcileProxy` 校正到新端口并更新备份。
- 证据：`TestT01CrashRecoveryRestoresOriginal`、`TestT01CrashRecoveryReconcilesActiveTakeover` 实际通过。
- 替代保护：Job Object 只保护内核进程，不保护注册表状态；此前无其他保护。
- 结论：修复前 `proxy.json` 非原子写且恢复前删除备份，崩溃后可能永久断网（AUDIT P1-10）。C1 已修复，**P1 降为已修复**。

## T02 恢复失败时备份保留

- 关联问题：P1-01、P1-02
- 操作：注入 `SetOverride` 失败，调用 `restoreSystemProxy()`；随后解除故障再次调用。
- 预期：首次失败返回明确错误、`proxy.json` 保留且 `Pending=true`、内存备份保留；重试成功并删除备份。
- 实际：首次返回 `恢复系统代理失败（备份已保留）`，文件与内存均保留；重试后注册表完整还原。
- 证据：`TestT02RestoreFailureKeepsBackupAndRetries` 通过。
- 说明：修复前缺陷是"先 `os.Remove` 再恢复"，无法在保留生产代码的前提下执行旧实现，故未计为线上复现，仅记录代码路径；风险等级维持 P1。
- 结论：**已修复**。`StopKernel` 现在遇恢复失败会中止停止内核。

## T03 TUN 开启失败完整回滚

- 关联问题：P1-03
- 操作：系统代理处于接管态，`setTUN(true, restart)` 注入重启失败。
- 预期：`tunOn=false`、系统代理重新指向本程序端口、Override 回到接管值、PAC 重新清空、备份重建为用户原始地址。
- 实际：全部满足。
- 证据：`TestT03TUNEnableFailureRollsBackSystemProxy` 通过。
- 结论：**已修复**（含 PAC/Override/WinHTTP 与注册表快照回滚）。

## T04 TUN 关闭失败 UI 与实际一致

- 关联问题：P1-03
- 操作：`tunOn=true`，`setTUN(false, restart)` 中让重启先杀掉旧内核再返回失败。
- 预期：内核已死 → TUN 实际不存在 → `tunOn=false`；恢复意图 `tunPrevProxyOn` 保留。
- 实际：满足。
- 证据：`TestT04TUNDisableFailureKeepsUIInSync` 通过。
- 结论：**已修复**（不再把 `tunOn` 回滚成 true）。

## T05 并发 Start

- 关联问题：P1-05
- 操作：10 个 goroutine 并发 `Manager.Start`。
- 预期：最多 1 次成功。
- 实际：**10 次全部成功**。
- 证据：

```
=== RUN   TestReproT05ConcurrentStart
    repro_concurrency_test.go:70: FAIL: 并发 Start 成功 10 次，预期最多 1 次（会启动多个内核）
```
- 根因：`m.cmd` 在 `cmd.Start()` 后才赋值，检查与赋值之间存在启动窗口。
- 用户影响：并发操作（托盘 + 主窗口 + 看门狗）可拉起多个 mihomo，争抢端口、产生孤儿。
- 替代保护：单实例互斥只限制 NexusLink 自身进程，不限制内核实例数。
- 结论：**FAIL 已复现，维持 P1，C2 修复**。

## T06 Start 与 Stop 并发

- 关联问题：P1-05
- 操作：并发 `Start` 与 `Stop`，随后再 `Stop` 收尾，检查 `IsRunning`。
- 实际：收尾后 `IsRunning=false`，未观察到错误。
- 局限：未枚举系统进程数，不能证明"零孤儿"。Job Object 是否真的托管未知（见 T09）。
- 结论：**PARTIAL**，风险保留，C2 用进程计数与 Job 检查补齐。

## T07 并发 Restart

- 关联问题：P1-05
- 操作：5 个 goroutine 并发 `Restart`。
- 实际：本轮 5 次均返回 nil，最终内核在运行。
- 局限：`Restart` 先 `Stop` 再 `Start`，`Stop` 的等待掩盖了部分竞态；T05 已证明 Start 窗口存在，Restart 仍可能在 `Stop` 返回后并发 `Start`。
- 结论：**PASS（本环境）**，但缺陷未被消除，保留 P1，C2 串行化。

## T08 崩溃恢复上限

- 关联问题：P1-06
- 已执行：验证"主动 Stop 不被误报为崩溃"（`TestReproT08ActiveStopNotReportedAsCrash` 通过，300ms 内回调 0 次）。
- 未执行：`failCount` 在重启成功后立即清零，缺少时间窗口上限；该逻辑位于 `startKernelWatchdog` 闭包内，无注入点，未能在不改代码的前提下独立驱动。
- 结论：**PARTIAL**。静态结论不作为"已复现"，但"成功即清零"可导致无限重启，维持 P1，C2 修复并补可注入的计数测试。

## T09 Job Object 分配失败

- 关联问题：P2-19（原报告）
- 操作：`attachToJob(0x7FFFFFF0)`（不存在的 PID）。
- 实际：函数静默返回；`CreateJobObjectW`、`SetInformationJobObject`、`AssignProcessToJobObject` 返回值全部被忽略，调用方无法感知未托管。
- 证据：

```
=== RUN   TestReproT09JobObjectFailureIsSilent
    repro_concurrency_test.go:154: attachToJob 对不存在的 PID 静默返回，调用方无法区分已托管/未托管（T09 复现）
```
- 用户影响：Job 创建/分配失败时不报错，进程被强杀后内核可能残留并占端口。
- 结论：**FAIL（静默失败）已复现**，维持 P2（若与 T05 叠加可升 P1），C2 修复（返回错误并记录）。

## T10 磁盘写入失败保留有效配置

### 代理状态（已修复）

- 操作：`proxy.json` 已有有效内容；把 `writeFileAtomic` 替换为必失败实现后调用 `persistProxyState`。
- 实际：返回错误；旧文件字节完全不变；无 `.tmp-` 残留。
- 证据：`TestT10ProxyStateAtomicWriteFailureKeepsOldFile` 通过。

### 订阅（未修复，已复现）

- 操作：把 `subs.json` 截断成前 20 字节，`loadSubs()` 后调用 `saveSubs()`。
- 实际：加载静默返回空；随后保存把文件覆盖为 `{"items": null, "active": 0}`，原订阅（含 token）不可恢复。
- 证据：

```
=== RUN   TestReproT10SubsTruncatedIsOverwritten
    repro_persist_test.go:43: FAIL: 损坏的 subs.json 被空数据覆盖，原订阅不可恢复
```
- 结论：代理部分**已修复**；订阅/进程规则部分**FAIL 已复现**，维持 P1，C4 修复。

## T11 订阅更新期间切换订阅

- 关联问题：P1-12
- 操作：并发写两个订阅文件，再用 12 轮 2MB 级内容并发覆盖共享的 `subscription.yaml`。
- 实际：本轮未观察到交错或长度异常，文件始终是完整的 A 或 B。
- 局限：`os.WriteFile` 对小/中等内容通常单次写入，交错概率低，本轮未触发；但代码层面无互斥，且并发 `applySubscription`/`SwitchSubscription` 都会触发 `RestartKernel`，内核层竞态已由 T05 证明。
- 结论：**PASS（未触发）**，风险保留为 P1，C2/C4 修复（串行化 + 原子写）。

## T12 两个同名 EXE

- 关联问题：P1-08
- 操作：验证路径型进程名被拒绝；生成同名进程目标规则；检查配置中是否存在 `PROCESS-PATH`。
- 实际：路径被 `sanitizeProcName` 拒绝；只生成一条 `PROCESS-NAME,chrome.exe,...`；配置中无路径规则。
- 证据：

```
=== RUN   TestReproT12SameNameExeCannotBeDistinguished
    repro_t12_test.go:45: FAIL: 同名不同路径的 exe 会命中同一条规则（T12 复现）
```
- 用户影响：绿色版与安装版同名程序共享同一策略，无法分别指定节点。
- 替代保护：无。
- 结论：**FAIL 已复现，维持 P1，C3 修复**（引入 ProcessIdentity + 规则版本迁移）。

## T13 PID 复用

- 状态：**BLOCKED**。
- 原因：连接 API 不暴露 PID 生命周期；需要在真实内核下高频创建/销毁进程并采样连接，当前环境无法保证时序可重复，且无法在不改代码的前提下取证。
- 现有评估：规则按 EXE 名匹配，不缓存 PID→身份，理论上不会"继承旧身份"，但缺少正面证据，不标记 PASS。
- 结论：维持待验证，C3 补充可观测字段与测试。

## T14 本地 API 端口冲突

- 关联问题：P1-13
- 操作：先用 `TcpListener` 占用 `127.0.0.1:19095`，再用真实 mihomo v1.19.33 以该 `external-controller` 启动。
- 实际：内核进程存活（`exited=False`），但 `/version` 请求超时，API 不可用。
- 证据：

```
exited=False
api-failed: 操作超时。
```
- 用户影响：界面显示"运行中"，但节点、连接、测速、模式切换全部不可用；`afterStart` 15 秒后静默返回，`bootErr` 为空。
- 替代保护：`pickPort` 只在启动前探测，存在 TOCTOU，无法覆盖本次冲突。
- 结论：**FAIL 已复现（真实内核）**，维持 P1，C2 修复（启动后校验实际监听，失败重选端口或明确报错）。

## T15 休眠唤醒

- 状态：**BLOCKED**。
- 原因：需要真实睡眠/唤醒，且会中断当前会话；本机为正在使用的开发机，无法安全执行。
- 结论：未定级，留给最终验收（Windows 实机）。

## T16 外部软件修改系统代理

- 关联问题：P1-04
- 操作：`proxy.json` 记录 `Addr=127.0.0.1:17890`，把注册表改成外部 `127.0.0.1:8888`，调用 `reconcileProxy()` 后再 `Shutdown()`。
- 预期：不覆盖外部地址、不删除外部设置、退出不写注册表。
- 实际：外部地址保持不变，`Enable` 调用 0 次，旧备份被删除，`proxyOn=false`。
- 证据：`TestT16ExternalProxyChangeIsNotOverwritten` 通过。
- 说明：修复前 `reconcileProxy` 只要 `proxy.json` 存在就 `Enable(want)`，会覆盖外部设置；该缺陷属旧实现本身，未单独执行旧代码。
- 结论：**已修复**，P1 降为已修复。

## T17 订阅重定向到本机

- 关联问题：P2-11
- 操作：订阅返回 302 指向 `httptest` 本机服务，`Fetch` 未限制重定向目标。
- 实际：成功跟随重定向并解析出节点。
- 证据：

```
=== RUN   TestReproT17RedirectToLoopback
    repro_t17_test.go:28: 订阅被重定向到本机地址并成功解析（SSRF 复现）: nodes=1
```
- 用户影响：恶意/被劫持面板可让客户端访问本机与内网端点，构成 SSRF；订阅 token 也可能随重定向泄露。
- 结论：**FAIL 已复现，等级由 P2 上调为 P1**，C4 修复（重定向白名单/私网拦截）；不属于 C1 范围，本轮不修。

## T18 内核替换中断

- 关联问题：P2-07
- 操作：让 `os.Remove(target)` 失败、`os.Rename(tmp,target)` 失败（目标被占位目录占住），模拟替换窗口中断。
- 实际：报 `Access is denied`，目标内核文件不存在，`.new` 临时文件残留。
- 证据：

```
=== RUN   TestReproT18ReplaceLeavesNoKernel
    FAIL: 替换失败后没有可用的内核文件（rename ...: Access is denied.），残留 ...mihomo.exe.new
```
- 用户影响：下次启动只能回退内置内核；若内置版本与自定义版本差异大，等于升级失败且无提示。
- 结论：**FAIL 已复现，等级由 P2 上调为 P1**，C4 修复（替换前后校验、保留 `.bak`、失败回滚）；不属于 C1 范围。

---

## 分级修正

| 问题 | 原等级 | 修正后 | 依据 |
|---|---|---|---|
| P1-01/P1-02/P1-03/P1-04/P1-07/P1-10（代理部分） | P1 | 已验证修复 | T01-T04、T16 实际通过 |
| P1-05 | P1 | P1 | T05 10/10 复现 |
| P1-06 | P1 | P1 | T08 部分执行，上限缺陷仍在 |
| P1-08 | P1 | P1 | T12 复现 |
| P1-12 | P1 | P1 | T11 本轮未触发，但无互斥 + T05 佐证 |
| P2-11 | P2 | **P1** | T17 实际跟随重定向到本机 |
| P2-07 | P2 | **P1** | T18 替换失败后内核缺失 |
| P2-19 | P2 | P2 | T09 静默失败复现 |

---

## 未执行 / 未通过清单

- T13：BLOCKED，缺少可重复的 PID 复用采证手段。
- T15：BLOCKED，需真实睡眠/唤醒与管理员环境。
- T08：崩溃计数上限未独立驱动（闭包不可注入）。
- T06：未做系统级孤儿进程枚举。
- T11：并发交错本轮未触发。

以上均未标记 PASS。
