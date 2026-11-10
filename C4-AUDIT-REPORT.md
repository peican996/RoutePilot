# C4 稳定性与故障恢复审计报告

- 基线：`main` @ `e9172ff`
- 审计范围：TUN/系统代理并发、内核生命周期、订阅安全与事务、配置持久化、多文件一致性
- 结论：六项问题在基线上**全部真实存在**，均已定位到具体代码路径并在本轮修复。

---

## 1. TUN 与系统代理并发控制

**风险等级：P1（系统状态失控）**

代码位置：`service.go` `setTUN()`（原 2039-2150 行）

原始问题：

1. `setTUN()` 完全没有使用 `proxyMu`，注册表的还原、关闭、WinHTTP 还原都是裸写。
2. `tunOn` 在重启内核**之前**就被置为 `true`（原 2090-2096 行），提交早于实际操作。
3. 在"系统代理已关闭、`tunOn` 尚未置位"的窗口内，`SetSystemProxy(true)` 只检查
   `s.tunOn`，会成功重新打开系统代理；随后 TUN 重启成功，形成 TUN + 系统代理双接管。
4. `reconcileProxy()` 在同一窗口被 afterStart 调用时，也会按旧状态重新接管代理。
5. 关闭 TUN 失败时，`tunPrevProxyOn` 被用掉但恢复意图没有写入启动恢复流程，
   下次启动无法补做代理恢复。

根因：TUN 开关是一个跨"注册表 + 内核重启 + 内存状态"的复合事务，
却被拆成无锁的多段代码，且状态提交顺序错误。

---

## 2. Manager 启动成功与进程退出竞态

**风险等级：P1（错误报告运行状态）**

代码位置：`internal/kernel/manager.go` `startLocked()` 407-420 行、`waitProcess()` 516-555 行

原始问题：

```
waitProcess:  r.exitErr = err; close(r.exited);  ← 先关退出信号
              m.mu.Lock(); m.run = nil; state = Failed/Stopped  ← 后清理
startLocked:  m.mu.Lock(); if m.run == r { state = Running }   ← 只看 run
```

进程在探测成功与提交 Running 之间退出时，`startLocked` 可能在 `waitProcess`
拿到锁之前进入临界区，看到 `m.run == r` 就提交 Running 并返回 nil。
调用方据此认为内核已就绪，实际上进程已经退出。

根因：启动成功判定依赖"run 指针仍存在"，而不是"进程仍存活"；
`exited` 通道提供的同步关系没有被使用。

---

## 3. 订阅跨主机重定向凭据泄漏

**风险等级：P1（凭据泄漏）**

代码位置：`internal/subscription/guard.go` `newGuardedClient()` 224-235 行、
`internal/subscription/fetch.go` `doWithDeps()`

原始问题：

- 使用 `http.Client` 默认重定向，`CheckRedirect` 只重新校验 URL，**不限制跨主机**。
- Go 的默认重定向会把上一跳 URL 写入 `Referer`；订阅 URL 通常是
  `https://panel/api/v1/client/subscribe?token=SECRET`，于是 token 会以
  `Referer: https://panel/...?token=SECRET` 的形式发送给第三方域名。
- 没有显式处理 `Authorization` / `Cookie`；重定向目标带 userinfo 时会被
  Go 自动转成 Basic Auth。
- `https → http` 降级没有被禁止。

根因：把安全边界完全交给 `http.Client` 默认行为，而默认行为为了兼容性
会主动携带 Referer。

---

## 4. 订阅操作缺少提交串行化

**风险等级：P2（数据一致性 / 半提交）**

代码位置：`service.go` `Subscribe()`、`SwitchSubscription()`、
`removeSubscription()`、`ClearSubscription()`、`applySubscription()`、
`maybeAutoRefresh()`

原始问题：

- 所有订阅变更都只依赖 `s.mu` 保护内存字段，没有跨"文件落盘 + 内存提交 +
  激活切换"的事务边界。
- 刷新 A 的网络请求返回后直接调用 `applySubscription(A)`，期间用户若
  切到 B，A 的结果会把 `subURL/activeIdx/subConfig` 全部覆盖回 A。
- 切换、删除、清空与自动更新并发时，可能出现 `subs` 列表、`activeIdx`、
  `subConfig` 与磁盘文件来自不同版本。
- 自动更新已经有一段"重新读 active"的逻辑，但它是"检查后再写"，
  不构成原子提交。

根因：缺少统一的提交锁与"期望激活项"校验。

---

## 5. 配置损坏被静默当作空配置

**风险等级：P1（用户配置永久丢失）**

代码位置：`service.go` `loadSubs()`、`loadProcRules()`、`loadSiteRules()`、
`loadIgnored()`、`loadAppState()`、`migrateSingleSub()`

原始问题：

- JSON 解析失败时直接 `return`，内存保持空值；程序继续运行。
- 之后任何一次保存（`saveSubs` / `saveProcRules` / `saveAppState`）都会
  `writeFileAtomic` 覆盖原文件，损坏前的用户数据永久丢失。
- `loadAppState` 损坏时同样静默返回，TUN / 系统代理 / WinHTTP 的恢复线索
  一起消失，可能留下指向死端口的系统代理。
- 没有区分"文件不存在 / 空文件 / 截断 / 非法字段 / 版本不兼容"。

根因：加载失败没有可诊断的返回状态，也没有"先保留再继续"的保护。

---

## 6. 多文件配置提交不一致

**风险等级：P2（内核读到自相矛盾的规则）**

代码位置：`service.go` `persistProcRuleSnapshot()`、`writeProcRuleSnapshotFiles()`、
`writeProcRulesYAML()`；`internal/configgen/gen.go` 264/271 行固定路径

原始问题：

- 进程规则由三个文件组成：`process-rules.json`、`process-proxy.yaml`、
  `process-direct.yaml`。
- 三者分别原子写入，单个文件不会截断，但**跨文件没有事务**：
  写第一个后崩溃，会留下"新 JSON + 旧 provider"或两个 provider 新旧混合。
- 内核启动时直接按固定路径读取，无法判断这三个文件是否属于同一版本。

根因：把"单文件原子"误当成"多文件一致"。

---

## 7. 影响范围

| 问题 | 受影响功能 | 用户可见后果 |
| --- | --- | --- |
| TUN/代理并发 | TUN、托盘代理开关、启动恢复 | 双代理、断网、TUN 已开但代理仍指向旧端口 |
| Manager 竞态 | 内核启动、看门狗、UI 状态 | 显示"运行中"但实际已退出，流量全部失败 |
| 重定向泄漏 | 订阅更新 | 机场 token 泄漏给第三方域名 |
| 订阅并发 | 多订阅切换/刷新/自动更新 | 切换到 B 又被刷新 A 覆盖，配置与 UI 不一致 |
| 配置损坏 | 订阅、进程规则、网站规则、忽略列表、运行状态 | 一次异常断电后用户配置永久丢失 |
| 多文件一致性 | 进程分流 | 内核读到混合版本规则，分流行为不可预测 |

---

## 8. 修复优先级与本轮处理

| 序号 | 问题 | 等级 | 本轮 |
| --- | --- | --- | --- |
| 1 | TUN/系统代理并发 | P1 | 已修复 |
| 2 | Manager 启动/退出竞态 | P1 | 已修复 |
| 3 | 订阅重定向凭据泄漏 | P1 | 已修复 |
| 4 | 订阅提交串行化 | P2 | 已修复 |
| 5 | 配置损坏保护 | P1 | 已修复 |
| 6 | 多文件一致性 | P2 | 已修复（generation + manifest） |
