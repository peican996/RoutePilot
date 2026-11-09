# C1 系统代理与网络恢复整改报告

- 阶段：C1（网络安全与恢复）
- 范围：系统代理备份/恢复事务、`StopKernel` 错误处理、TUN 切换回滚、异常退出恢复、WinHTTP 恢复错误上报
- 不在范围：C2 内核生命周期、C3 进程身份、C4 持久化与订阅安全、C5 架构、C6 CI

---

## 1. 修改文件清单

| 文件 | 变更 |
|---|---|
| `service.go` | 代理接管事务、恢复验证、Pending 机制、TUN 切换回滚、`StopKernel`/`Shutdown`/`failBoot` 错误处理、原子写工具、系统操作注入点 |
| `internal/sysproxy/sysproxy_windows.go` | 新增 `SetServer`；`RestoreWinHTTP` 删除失败不再吞错 |
| `c1_proxy_test.go`（新增） | T01-T04、T10（代理）、T16 故障注入测试 |
| `internal/kernel/repro_concurrency_test.go`（新增） | T05-T09、T18 复现用例（`repro` tag） |
| `repro_persist_test.go`（新增） | T10（订阅）、T11 复现用例（`repro` tag） |
| `repro_t12_test.go`（新增） | T12 复现用例（`repro` tag） |
| `internal/subscription/repro_t17_test.go`（新增） | T17 复现用例（`repro` tag） |
| `T01-T18-REPRODUCTION.md`（新增） | 第二阶段复现报告 |

未修改：前端、订阅解析逻辑、配置生成规则语义、进程规则存储格式、`app-state.json` 已有字段语义。

---

## 2. 问题与修复

### 2.1 备份删除时机错误（P1-02）

**修复前**：`restoreSystemProxy` 先 `os.Remove(proxy.json)`，再写注册表；`s.backup` 也在同一时刻清空。

**修复后**：

1. 先把 `Pending=true` 原子写入磁盘；
2. 执行完整还原（地址、启用状态、Override、PAC）；
3. `verifyProxySnapshot` 回读注册表逐项比对；
4. 全部一致后才删除 `proxy.json`；
5. 任一步失败则保留备份与 Pending，返回明确错误。

这样"恢复中崩溃"在磁盘上是一个可识别的中间态，下次启动会重试，而不是丢掉唯一还原点。

### 2.2 恢复失败被静默忽略（P1-01）

- `StopKernel`：`SetSystemProxy(false)` 失败不再被丢弃，而是中止停止内核并返回错误。理由：内核仍在运行时代理链路可用，强行停内核只会留下死端口。
- `Shutdown`：恢复失败写 `debug.log`，保留备份，下次启动重试（退出阶段无法阻止进程结束）。
- `failBoot`：启动失败时若恢复也失败，追加到 `bootErr`，用户可见。
- `restoreWinHTTPIfPending`：改为返回 `error`；普通权限下返回"需要管理员权限"而不是静默 return；失败保留 `winHTTPPending` 与备份。
- `RestoreWinHTTP`：删除不存在以外的失败不再被当作成功。

### 2.3 TUN 切换回滚不完整（P1-03）

**修复前**：开 TUN 时 `sysproxy.Disable()` 与删除 `proxy.json` 的错误被忽略；失败回滚只 `Enable(prevBackup.Addr)`，丢失 Override/PAC/WinHTTP；关 TUN 失败时 `tunOn` 仍回滚为 `true`。

**修复后**：

开 TUN 的顺序：

1. 采集注册表快照（`captureProxySnapshot`）；
2. 若系统代理由本程序接管，先完整还原（失败则中止切换，不进入 TUN）；
3. 关闭系统代理（失败则按快照回滚并返回）；
4. 还原 WinHTTP（失败则同上）；
5. 重启内核；失败时先判断内核是否还活着：
   - 活着 → 重新接管系统代理；
   - 已停止 → 只还原用户原始配置，`tunOn` 置 `false`（因为 TUN 实际不存在）。

关 TUN：成功重启后，如果开 TUN 前系统代理由本程序接管，自动重新打开；失败则保留 `tunPrevProxyOn` 供下次启动处理。

`tunPrevProxyOn` 是新增的 app-state 字段（`omitempty`），旧配置读取时为零值，向后兼容。

### 2.4 异常退出后的恢复可靠性（P1-10、P1-07）

- 新增 `recoverPendingProxyRestore()`，在 `Boot` 读取状态后立即执行，优先重试上次未完成的恢复，并清掉旧的 `restoreProxy` 意图，避免 `afterStart` 又把代理打开。
- 新增 `atomicWriteFile`：同目录临时文件 → `Chmod` → `Write` → `Sync` → `Close` → `Rename`，失败清理临时文件。`proxy.json` 与 `app-state.json` 改用它。
- 启动时若发现"没有备份但注册表仍指向本程序端口"，判定为上次崩溃遗留，直接关闭并提示，不把死端口当成用户原配置保存。
- 接管前若 `Enable` 或清 PAC 失败，按快照回滚，避免半接管状态。

### 2.5 外部修改不覆盖（P1-04）

`reconcileProxy` 现在区分四种情况：

| 注册表状态 | 行为 |
|---|---|
| 地址 = 我们上次写入的 `Addr` | 校正到当前内核端口，更新备份 |
| 地址 = 当前内核端口但不等于 `Addr` | 视为端口漂移，更新 `Addr` |
| 地址是其它值且非本程序端口 | 外部接管，删除备份、放弃接管、退出不写注册表 |
| `ProxyEnable=0` | 还原地址/Override/PAC，清除备份与 `restoreProxy` 意图 |
| 备份 `Pending=true` | 交给启动恢复流程，这里不改注册表 |
| TUN 已开 | 完全不处理系统代理 |

---

## 3. 状态模型（C1 范围）

C1 引入的是"接管记录"级别的状态，不改变内核生命周期：

```
无记录（NotTakenOver）
  └─ Enable 成功 + 快照落盘 → TakenOver(Saved, Pending=false)
        ├─ 用户/外部改地址 → Relinquished（删除记录，不再写注册表）
        ├─ 用户关闭代理   → Released（还原地址/Override/PAC，删除记录）
        └─ 开始恢复       → Restoring(Saved, Pending=true)
              ├─ 还原+校验成功 → 删除记录 → NotTakenOver
              └─ 失败          → Restoring 保留，下次启动重试
```

`proxyOn` 只作为 UI 与运行时意图的镜像；是否"归我们管"以 `proxy.json` 与注册表实际值为准。

---

## 4. 故障场景、回滚与验证

| 故障 | 回滚 | 验证 |
|---|---|---|
| 开启代理时写注册表失败 | 按快照还原全部字段 | 代码路径 + T02 同类注入 |
| 清 PAC 失败 | 按快照还原 | T03 覆盖 PAC 恢复 |
| 快照落盘失败 | 按快照还原，不置 `proxyOn` | T10 原子写失败用例 |
| 恢复写注册表失败 | 保留备份 + Pending | T02 |
| 恢复后校验不一致 | 保留备份 + Pending | T02（Override 不符） |
| 恢复中崩溃 | 下次启动重试 | T01 |
| 开 TUN 前还原失败 | 不切换，恢复原状态 | T03 前置分支 |
| 开 TUN 重启失败（内核活着） | 重新接管系统代理 | T03 |
| 开 TUN 重启失败（内核已死） | 还原用户原配置，`tunOn=false` | T04 同源 |
| 关 TUN 重启失败 | `tunOn=false`，保留 `tunPrevProxyOn` | T04 |
| 外部改代理 | 放弃接管，不写注册表 | T16 |
| WinHTTP 无管理员权限 | 保留 pending + 备份，提示提权 | 代码路径 + 编译验证（本机非管理员，未实机写 HKLM） |

---

## 5. 测试命令与结果

```
> gofmt -l .
（无输出）

> go vet ./...
（无输出，exit=0）

> go test -count=1 ./...
ok  	proxdeck	                2.573s
ok  	proxdeck/internal/autostart	1.298s
ok  	proxdeck/internal/configgen	1.293s
ok  	proxdeck/internal/elevate	[no test files]
ok  	proxdeck/internal/geoip  	[no test files]
ok  	proxdeck/internal/kernel 	1.664s
ok  	proxdeck/internal/subscription	1.580s
ok  	proxdeck/internal/sysproxy	[no test files]
exit=0

> go build -ldflags "-s -w -H windowsgui" -o bin/NexusLink.exe .
exit=0
```

C1 专项：

```
> go test -count=1 -run 'TestT0|TestT1' .
ok  	proxdeck	2.272s
```

新增/相关用例：

| 用例 | 结果 |
|---|---|
| `TestT01CrashRecoveryRestoresOriginal` | PASS |
| `TestT01CrashRecoveryReconcilesActiveTakeover` | PASS |
| `TestT02RestoreFailureKeepsBackupAndRetries` | PASS |
| `TestT03TUNEnableFailureRollsBackSystemProxy` | PASS |
| `TestT04TUNDisableFailureKeepsUIInSync` | PASS |
| `TestT16ExternalProxyChangeIsNotOverwritten` | PASS |
| `TestT10ProxyStateAtomicWriteFailureKeepsOldFile` | PASS |

复现用例（`-tags repro`，失败即为缺陷证据，C2-C4 转正）：

```
> go test -tags repro -count=1 -run 'TestReproT05|TestReproT07' -v ./internal/kernel
--- FAIL: TestReproT05ConcurrentStart   并发 Start 成功 10 次
--- PASS: TestReproT07ConcurrentRestart
```

`-race` 仍未执行：本机无 CGO/gcc。CI 需补 Windows runner 的 race 任务（C6）。

---

## 6. 行为变更说明

1. **停止内核可能失败**：系统代理恢复失败时 `StopKernel` 返回错误且内核继续运行。调用方（前端"停止内核"、托盘、内核升级）会看到错误提示，而不是静默断网。
2. **内核升级前置条件变严**：升级流程复用 `StopKernel`，若代理恢复失败会中止升级并保持内核运行。这是有意的安全取舍。
3. **`restoreWinHTTPIfPending` 返回错误**：调用方均改为记录或上报，不再静默。
4. **`app-state.json` 权限从 0644 改为 0600**，并新增 `tunPrevProxyOn` 字段（可选，旧文件可读）。
5. **删除 `proxy.json` 的时机后移**：只有恢复并校验成功后才会删除。

---

## 7. 尚未解决（不在 C1 范围）

| 问题 | 等级 | 计划阶段 |
|---|---|---|
| T05 并发 Start 可启动多个内核 | P1 | C2 |
| T14 API 端口冲突后内核存活但 API 不可用 | P1 | C2 |
| T09 Job Object 失败静默 | P2 | C2 |
| T08 崩溃重启缺少时间窗口上限 | P1 | C2 |
| T12 同名 EXE 无法区分 | P1 | C3 |
| T13 PID 复用未取证 | P1（待验证） | C3 |
| T10 订阅/进程规则非原子写、损坏后覆盖 | P1 | C4 |
| T17 订阅可重定向到本机/内网（SSRF） | P1 | C4 |
| T18 内核替换中断导致二进制缺失 | P1 | C4 |
| T11 订阅操作无串行化 | P1 | C2/C4 |
| T15 休眠唤醒 | 未定级 | 最终验收 |

---

## 8. 已知局限

- 本机非管理员：WinHTTP 的 HKLM 写入/还原未做实机验证，仅做了代码路径与编译验证；未标记为通过。
- 未执行真实强杀 + 重启的端到端验证（T01 用等价磁盘状态注入），实机验收留给最终阶段。
- 系统代理的"用户手动关闭"分支已覆盖，但未在真实注册表上验证 `InternetSetOption` 通知行为。
- `repro` 用例当前以失败形式保存，用于证明缺陷存在；C2-C4 修复后应转为普通测试并去掉 `repro` tag。

---

## 9. 暂停点

C1 已完成，按授权要求在此暂停，不进入 C2。等待人工审查下列内容：

1. `StopKernel` 遇恢复失败即中止的行为是否符合产品预期。
2. 开 TUN 失败且内核已死时"不假装 TUN 开启"的处理是否可接受。
3. 外部修改系统代理时"放弃接管、不写注册表"的策略是否符合预期。
4. 是否允许把 `repro` 失败用例保留在仓库中直到 C2-C4 修复。
