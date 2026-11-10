# C4 整改报告

基线：`main` @ `e9172ff`
原则：不新增功能、不做大规模重构，只修稳定性/安全/一致性。

---

## 1. 修改文件清单

| 文件 | 类型 | 内容 |
| --- | --- | --- |
| `service.go` | 修改 | TUN 事务、代理状态机、订阅提交锁、配置损坏处理、procgen 接入 |
| `config_files.go` | 新增 | `readConfigJSON` / `configReadState` / 损坏文件保留与上报 |
| `procgen.go` | 新增 | generation 目录 + manifest 发布、校验、回退、清理 |
| `internal/kernel/manager.go` | 修改 | 启动成功前检查 `r.exited` |
| `internal/subscription/guard.go` | 修改 | 手动重定向策略、跨站拒绝、Referer/凭据剥离 |
| `internal/subscription/fetch.go` | 修改 | 手动跟随重定向循环、总超时、逐跳校验 |
| `internal/configgen/gen.go` | 修改 | 支持版本化 provider 路径（默认兼容旧路径） |
| `.github/workflows/ci.yml` | 修改 | repro、Windows amd64 构建、race job |
| `c4_tun_test.go` | 新增 | TUN 与代理并发/回滚测试 |
| `c4_subscription_test.go` | 新增 | 刷新与切换并发、提交一致性测试 |
| `c4_config_test.go` | 新增 | 损坏/空/版本不兼容/不覆盖测试 |
| `c4_procgen_test.go` | 新增 | 发布、崩溃点、回退、清理测试 |
| `internal/kernel/start_exit_race_test.go` | 新增 | 启动与退出竞态确定性测试 |
| `internal/subscription/redirect_test.go` | 新增 | 跨站重定向与 Referer 泄漏测试 |
| `realkernel_procgen_test.go` | 新增 | 真实 mihomo 加载 procgen provider |
| `internal/subscription/guard_test.go` | 修改 | 私有 DNS 重定向用例改用同站点子域，保留 DNS 校验覆盖 |

---

## 2. 锁设计

### 2.1 锁清单与顺序

单次嵌套路径（不是同时持有全部）：

```
lifeMu  ──> proxyMu ──> s.mu
subsMu  ──> s.mu
procGenMu ──> s.mu
```

- `lifeMu`：Start/Stop/Restart/Shutdown 生命周期事务。
- `proxyMu`：系统代理注册表事务（开关、校正、还原、WinHTTP、TUN 的注册表部分）。
- `subsMu`：订阅"落盘 + 内存提交"事务；网络下载**不持锁**。
- `procGenMu`：进程规则 generation 发布/解析/清理。
- `s.mu`：只保护内存字段，持有期间禁止注册表 I/O、文件 I/O、网络 I/O。

`lifeMu` 与 `subsMu` 从不嵌套：订阅操作在释放 `subsMu` 后才调用
`RestartKernel`（需要 `lifeMu`）。

不允许的反向路径：

- 持有 `s.mu` 时不能取 `proxyMu/subMu/procGenMu`。
- 持有 `subsMu` 时不能调用 `RestartKernel()`（会取 `lifeMu`），因此重启都在释放后执行。
- `afterStart` 只取 `proxyMu`，绝不取 `lifeMu`，避免与 Stop/Restart 形成环。

### 2.2 TUN 为什么不能整段持有 proxyMu

TUN 切换包含"重启内核"，而重启后的 `afterStart` 需要 `proxyMu` 做端口校正。
如果 `setTUN` 持锁重启，`waitAfterTasks` 会等待一个永远拿不到锁的任务，形成死锁。

因此 TUN 事务拆成三段：

1. **准备段（持 proxyMu）**：还原并关闭系统代理、还原 WinHTTP。
2. **重启段（不持 proxyMu）**：`tunChanging=true` 阻止代理事务进入。
3. **提交/回滚段**：按重启结果提交 `tunOn`，或恢复注册表与内存状态。

`tunChanging/tunTarget` 是这段窗口的显式状态，不是用 sleep 掩盖。

---

## 3. 状态转换

### 3.1 TUN

```
关闭 --开TUN--> [tunChanging=true, target=true]
                  ├─ 准备失败 ──────────────→ tunOn=false, 备份/Pending 保留
                  ├─ 重启失败 + 内核存活 ───→ tunOn=false, 重新接管系统代理
                  ├─ 重启失败 + 内核已死 ───→ tunOn=false, 恢复用户原代理, restoreProxy 记录
                  └─ 重启成功 ──────────────→ tunOn=true, proxyOn=false

开启 --关TUN--> [tunChanging=true, target=false]
                  ├─ 重启失败 + 内核存活 ───→ tunOn=true, 不碰系统代理
                  ├─ 重启失败 + 内核已死 ───→ tunOn=false, restoreProxy=true
                  └─ 重启成功 ──────────────→ tunOn=false → 按记录恢复系统代理
```

合法状态组合（`tunChanging=false` 时）：

| tunOn | proxyOn | 含义 |
| --- | --- | --- |
| false | false | 都不接管 |
| false | true | 仅系统代理接管 |
| true | false | 仅 TUN 接管（合法且唯一允许的 TUN 组合） |
| true | true | **禁止**，由 `tunChanging` 与 SetSystemProxy 拒绝共同保证 |

### 3.2 内核状态

`Stopped/Failed → Starting → Running` 的提交新增了退出信号检查：

```go
select {
case <-r.exited:
    m.run = nil
    m.state = StateFailed
    return ErrProcessExited
default:
    m.state = StateRunning
}
```

`exited` 关闭发生在 `r.exitErr` 写入之后，因此读取 `exitErr` 有 happens-before 保证。

### 3.3 订阅

```
下载（无锁） → subsMu → 检查 expectActive → 写文件 → 提交内存 → 释放 → 重启内核
```

- `Subscribe`：`expectActive=""`，用户明确添加/更新，允许激活。
- `RefreshSubscription` / 自动更新：`expectActive=当前激活 URL`，期间被切换则返回
  `errSubscriptionChanged`，不覆盖用户操作。
- 切换/删除/清空：全程 `subsMu`，文件与内存同一事务。

---

## 4. 持久化协议变化

### 4.1 配置文件读取

新增 `readConfigJSON` 返回明确状态：

| 状态 | 含义 | 行为 |
| --- | --- | --- |
| `missing` | 文件不存在 | 正常首次启动，不报错 |
| `empty` | 存在但只有空白 | 视为截断：保留备份 + 启动错误 |
| `corrupt` | JSON 解析失败 / 字段类型非法 | 保留备份 + 启动错误 |
| `unsupported` | `version` 高于程序支持 | 保留备份 + 启动错误，不按旧结构解释 |

保留方式：把原文件改名为 `<name>.corrupt-YYYYMMDD-HHMMSS`，后续保存写入新文件，
原内容不会被覆盖。

`appState` 与 `subsStore` 新增 `version` 字段（当前为 1，旧文件缺省为 0）。

### 4.2 进程规则 generation

新布局：

```
procgen/
  proxy-<gen>.yaml
  direct-<gen>.yaml
  rules-<gen>.json
  current.json          # 提交点：文件名 + sha256 列表
  current.json.prev     # 上一代提交点
```

发布顺序：

1. 三个文件写入新 generation 目录；
2. 逐个回读并校验 SHA-256；
3. 保存上一代 `current.json` 为 `.prev`；
4. 原子 rename 发布 `current.json`；
5. 尽力同步旧版平铺文件（降级兼容）；
6. 清理不在 current/prev 中的旧 generation。

加载顺序：`current` → `current.json.prev` → 旧版 `process-rules.json`。
任何一代只有"manifest 存在 + 三个文件哈希全部匹配"才会被采用。

内核配置通过 `configgen.Options.ProcessProxyPath/ProcessDirectPath` 指向
`./procgen/xxx.yaml`；路径为空时回退旧的 `./process-*.yaml`。

---

## 5. 订阅重定向策略

明确策略（`redirectAllowed`）：

| 场景 | 处理 |
| --- | --- |
| 同主机 / 父子域跳转 | 允许 |
| 跨站跳转（不同注册域） | 拒绝 `ErrCrossHostRedirect` |
| `https → http` | 拒绝 `ErrInsecureRedirect` |
| 目标带 userinfo | 拒绝 `ErrBlockedAddress` |
| 目标 IP/内网/数字主机 | 仍由 `validateSubscriptionURL` + 拨号校验拦截 |

实现方式：`http.Client.CheckRedirect` 返回 `ErrUseLastResponse`，由
`doWithDeps` 手动循环跟随；每一跳都新建请求，Header 只包含
`User-Agent` 与 `Accept`，不携带 Referer/Authorization/Cookie。
总超时覆盖所有跳转，最多 5 跳。

---

## 6. 兼容性说明

1. **旧数据可直接启动**：没有 `procgen/` 时回退旧版平铺文件；旧 JSON 没有
   `version` 字段按 0 处理。
2. **降级兼容**：每次发布仍会尽力更新旧版平铺文件，回退到旧版本程序时规则可用。
3. **路径格式**：配置中的 procgen 路径统一使用正斜杠，Windows 下 mihomo 可正常读取。
4. **跨站重定向行为变化**：以前会跟随任意公网跳转，现在默认拒绝。若用户订阅确实
   依赖跨站 CDN，需要直接填写最终地址。这是有意的安全收紧。
5. **损坏文件处理变化**：以前静默清空，现在会改名保留并在界面显示启动错误；
   用户可从 `.corrupt-*` 文件手工恢复。
6. **新增 `version` 字段**对旧版本是无害的未知字段，旧版本会忽略。
