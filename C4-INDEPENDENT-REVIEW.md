# C4 独立验收与回归审查

审查方式：不引用 `C4-REMEDIATION.md` 的结论，重新阅读当前代码、逐项对照基线
`e9172ff` 的差异，并实际执行测试。

---

## 1. 逐项验收

### 1.1 TUN 与系统代理并发

**修改前控制流**

```
setTUN: 关代理(无锁) → tunOn=true(提前) → RestartKernel
SetSystemProxy(true): 只检查 tunOn → 在窗口期可成功打开代理
```

**修改后控制流**

```
setTUN: mu 内置 tunChanging=true → proxyMu 内关代理/WinHTTP
      → 释放 proxyMu → RestartKernel
      → 成功: tunOn=true, proxyOn=false
      → 失败: 按 kernelAlive 回滚注册表/内存，保留 restoreProxy 意图
SetSystemProxy(true): 持有 proxyMu 后读 tunChanging/tunOn → 拒绝
reconcileProxy: tunChanging 时直接返回
```

关键代码：`service.go` `setTUN()`、`setSystemProxyLocked()`、
`reconcileProxyLocked()`、`withProxyTx()`。

**测试**：`c4_tun_test.go` 7 项，全部实际执行通过。

**残余触发路径**：
- 外部代理软件在"准备段结束 → 重启完成"之间改注册表：程序不会覆盖它，
  但也不会重新校验；TUN 成功后系统代理保持外部值。属于可接受行为（不破坏用户设置）。
- `tunChanging` 只在内存中，崩溃后不会残留（进程结束即消失），由 `app-state.json`
  的已提交状态恢复。

**结论：已解决，有确定性测试。**

---

### 1.2 Manager 启动成功与进程退出竞态

**修改前**

```go
m.mu.Lock()
if m.run == r { m.state = StateRunning; return nil }
```

**修改后**

```go
m.mu.Lock()
if m.run != r { ... return ErrProcessExited }
select {
case <-r.exited:            // 进程已退出（waitProcess 可能还没拿到锁）
    m.run = nil
    m.state = StateFailed
    return ErrProcessExited
default:
    m.state = StateRunning
}
```

`r.exitErr` 在 `close(r.exited)` 之前写入，读取有 happens-before 保证。
先摘除 `m.run` 还避免了随后 `waitProcess` 把 Failed 改回 Stopped、
或让后续 Start 误判"已在运行"。

**测试**：`TestStartRejectsExitBeforeRunningCommit`、
`TestStartAfterExitBeforeRunningCanRetry`。使用真实子进程 + `before-running`
hook，等待 `r.exited` 关闭后再放行，没有 sleep 或超时掩盖。

**残余路径**：`Stop`/`abortStart` 路径未改动，仍由 `opMu` 串行化；
本次只补了"提交 Running"这一处缺失的同步检查。

**结论：已解决，有确定性测试。**

---

### 1.3 订阅重定向凭据泄漏

**修改前**：`http.Client` 自动跟随，`CheckRedirect` 只校验 URL，
Referer 由 Go 自动写入且包含原始查询串。

**修改后**：
- `CheckRedirect` 返回 `ErrUseLastResponse`；
- `doWithDeps` 手动循环，每一跳都用全新 Header（仅 UA/Accept）；
- `redirectAllowed`：默认拒绝跨站；禁止 https→http；禁止 userinfo；
- 每跳继续执行 `validateSubscriptionURL`，拨号层继续做 DNS 全量校验。

**测试**：`internal/subscription/redirect_test.go` 8 项，全部通过。
其中跨站用例断言第三方服务器收到 **0 次请求**；同站用例断言 `Referer` 为空、
跳转后的查询串不包含原始 token。

**残余路径 / 行为变化**：
- 合法但依赖跨站 CDN 的订阅会被拒绝。这是有意的默认安全策略，
  用户需要填写最终地址。属于 **产品决策风险**，不是漏洞。
- `sameSiteHost` 用标签边界比较，未引入 Public Suffix List；
  `a.co.uk → co.uk` 会被视为同站。实际可利用性低（需要控制公共后缀域），
  标记 P3。

**结论：已解决；跨站订阅兼容性需产品确认。**

---

### 1.4 订阅提交串行化

**修改前**：刷新结果直接写 `subURL/activeIdx/subConfig`，与切换/删除并发时互相覆盖。

**修改后**：
- `subsMu` 覆盖"落盘 + 内存提交"，网络下载在锁外；
- `applySubscriptionExpect` 在锁内校验 `expectActive`，不匹配返回
  `errSubscriptionChanged`；
- 刷新/自动更新传 `expectActive`，用户显式添加/切换不传；
- 删除/清空/切换使用同一把锁，且释放后才调用 `RestartKernel`。

**测试**：`TestRefreshDoesNotOverrideConcurrentSwitch` 精确复现"下载中切换"；
`TestSubscriptionOpsConcurrentConsistency` 7 个并发操作无死锁、无重复条目。

**残余风险**：
- 两个用户操作（显式 Subscribe A 与 Switch B）并发时，后者提交顺序决定结果，
  这是"最后操作生效"，不是数据损坏。标记 P3。
- 已修复审查中发现的一个小问题：下载期间用户切换"忽略证书校验"，
  提交时曾会被旧值覆盖；现在保留内存中的最新标志。

**结论：已解决。**

---

### 1.5 配置损坏保护

**修改前**：解析失败静默返回，后续保存覆盖原文件。

**修改后**：`readConfigJSON` 返回 `missing/empty/corrupt/unsupported/ok`；
损坏文件改名为 `.corrupt-<时间>`；`bootErr` + `debug.log` 双通道提示；
新增 `version` 字段识别未来版本。

**测试**：7 个文件的非法 JSON/类型错误/截断，外加空文件、版本不兼容、
缺失文件、保存后备份不被破坏，全部通过。

**残余风险**：
- `proxy.json` 走的是独立读取路径（保持原逻辑：损坏则保留原文件并提示，
  不做改名），因此不会被后续保存覆盖，但也不会自动备份成 `.corrupt-*`。
  标记 P3。
- 损坏的 `app-state.json` 无法恢复 WinHTTP 备份内容；原文件保留在
  `.corrupt-*` 中供人工取回。标记 P2（信息可恢复，但需要人工介入）。

**结论：已解决（不静默、不覆盖、可诊断）。**

---

### 1.6 多文件配置一致性

**修改后**：generation 目录 + manifest 提交点；current 校验失败回退 prev，
再回退旧版平铺文件；发布前写文件并逐个回读哈希。

**测试**：发布/解析、manifest 未发布、current 损坏、文件截断、旧版回退、
清理、路径穿越，全部通过；真实 mihomo 能从 procgen 加载 provider。

**残余风险**：
- 旧版平铺文件在 manifest 发布后 best-effort 更新；若此时崩溃，平铺文件可能是
  混合版本。当前版本不读它们，只有降级到旧版本时才有影响。标记 P3。
- 每次启动都会发布一个新 generation（规则不变也发），清理只保留 2 代；
  磁盘开销可忽略，但属于可以优化的 P3。

**结论：已解决。**

---

## 2. 锁与状态图

### 2.1 实际嵌套关系

```
lifeMu  ──(Stop/Restart/Shutdown)──> proxyMu ──> s.mu
subsMu  ──(提交/切换/删除)─────────> s.mu
procGenMu ──(发布/解析)────────────> s.mu
```

`lifeMu` 与 `subsMu` **从不嵌套**：订阅操作在释放 `subsMu` 后才调用
`RestartKernel`（需要 `lifeMu`）。文档中的链式顺序描述的是单次嵌套路径，
不是同时持有。

### 2.2 TUN 状态机

```
                    ┌─────────── tunChanging=true ───────────┐
关闭 ──开TUN──> 准备(proxyMu) ──> 重启(无 proxyMu) ──> 提交/回滚
                    │
                    └─ 并发 SetSystemProxy(true) → 拒绝
                    └─ 并发 reconcileProxy      → 跳过
```

合法组合只有：`(false,false)`、`(false,true)`、`(true,false)`。
`(true,true)` 由两层拦截（tunChanging + tunOn）共同排除。

---

## 3. 审查中新发现并已修复的问题

| # | 问题 | 等级 | 处理 |
| --- | --- | --- | --- |
| N1 | 内核未运行时关闭 TUN，`tunPrevProxyOn` 被清空，恢复意图丢失 | P2 | 成功分支改为写入 `restoreProxy` 并落盘；新增测试 |
| N2 | `procgen/current.json` 被手工改成 `../` 路径可读取目录外文件 | P3 | 校验文件名必须是纯 basename；新增测试 |
| N3 | 下载期间切换"忽略证书校验"，提交时被旧值覆盖 | P3 | 提交时保留内存最新 `Insecure` |

---

## 4. 仍未解决的问题（按等级）

### P0
无。

### P1
无（本轮范围内的已知 P1 均已修复并有用例）。

### P2
1. **跨站重定向默认拒绝可能影响部分机场订阅**：需要产品确认是否提供
   显式白名单；当前行为是安全优先。
2. **TUN 关闭失败且内核已死**：本次已把恢复意图写入启动流程，
   但真正恢复要等到下次启动，期间系统代理处于"用户原配置"状态。
   这是能力限制，不是状态谎言。
3. **配置损坏需要人工介入**：程序保证不覆盖、可诊断，但不会自动从
   `.corrupt-*` 合并恢复。
4. **TUN/系统代理的真实注册表与网卡行为未在本机实机验证**：
   自动化只覆盖 fakeRegistry 与真实 mihomo（不含 TUN 网卡）。

### P3
1. `sameSiteHost` 未使用 Public Suffix List。
2. `proxy.json` 损坏路径没有 `.corrupt-*` 副本（保留原文件、不覆盖）。
3. 系统休眠/唤醒、网络切换没有专门监听。
4. 每次启动发布新 generation，存在可优化的磁盘 churn。
5. 本机无法执行 `-race`；CI race job 尚未实际跑过一次。

---

## 5. 测试真实性核对

| 声明 | 是否真实执行 |
| --- | --- |
| `go test ./...` | 是 |
| `-count=3 ./internal/kernel .` | 是 |
| `-tags repro ./...` | 是 |
| `-tags realkernel`（3 项 + procgen） | 是，真实 mihomo 进程 |
| `go vet` / `gofmt` | 是 |
| 前端生产构建 | 是 |
| Windows amd64 编译 | 是（`go build ./...`，未覆盖运行中的 bin exe） |
| `go test -race` | **否**：本机无 gcc；未标记为通过 |
| Windows 实机 TUN/注册表 | **否**：未执行，已列出步骤 |

---

## 6. 发布条件判断

| 条件 | 状态 |
| --- | --- |
| 所有 P0/P1 已修复并有测试 | 是（本轮范围内） |
| TUN/系统代理无已知竞态 | 代码路径上已排除；实机未验证 |
| 内核生命周期无已知状态竞争 | 是（启动/退出、Stop/Restart 均有确定性测试） |
| 订阅安全边界无已知绕过 | 是（跨站/降级/userinfo/SSRF 均有测试） |
| 配置不会因解析/写入失败被静默覆盖 | 是 |
| 关键故障矩阵有明确结果 | 部分：12 项中 9 项有自动化或既有测试，3 项明确未实机 |
| 自动化测试通过 | 是（除 race 未执行） |

**结论：可以作为发布候选，但不建议直接宣布 GA。阻断项如下：**

1. 在具备 CGO 的环境实际跑通 `go test -race ./...`（CI 已配置，需一次成功记录）。
2. 管理员实机完成 TUN + 系统代理并发、崩溃恢复、procgen 回退三项验证。
3. 产品确认"跨站重定向默认拒绝"是否可接受，或补充显式白名单。

在上述三项完成前，标记为 **Release Candidate / 待实机验收**。
