# RoutePilot 生产级代码审查报告（第一阶段）

- 审查日期：2026-11-09
- 审查基线：`70adff3 feat: 地图按真实出口展示多出口路径`
- 审查范围：Go 后端、Wails v3 生命周期、React 前端、Windows 系统集成、构建与 CI
- 审查方式：只读静态审查 + 调用链梳理 + 基线测试；**未修改任何业务代码**
- 本地环境：Windows 10/11 x64、Go 1.26、Node 20.19、无 CGO/gcc、无 `golangci-lint`
- 基线结果：`go test -count=1 ./...` 通过；`go test -race` 因 `-race requires cgo` 无法执行

---

## 1. 结论摘要

本次审查确认了 **13 个 P1、18 个 P2、7 个 P3** 问题，**未确认可稳定复现的 P0**。

最需要优先处理的三类风险：

1. **系统代理恢复事务不完整**：恢复失败时备份已被删除，停止内核仍继续执行，存在"代理指向死端口且无法自动恢复"的断网风险。
2. **内核生命周期缺少串行化与状态机**：`Start/Stop/Restart` 并发可启动多个内核；看门狗可在启动早期访问未初始化的 `app`，且对反复崩溃没有真正的上限。
3. **进程分流身份只依赖 EXE 名**：同名不同路径程序、升级后路径变化、UWP/权限不足进程会共享或丢失规则，符合任务书 T12/T13 的未定义行为。

此外，持久化写入普遍缺少原子替换与错误上抛，大量 `os.WriteFile` 的失败被静默忽略；这与"崩溃后配置可恢复"的验收目标直接冲突。

---

## 2. 仓库结构与核心调用链

```
main.go
  ├─ acquireSingleInstance() → Boot()
  │    ├─ migrateDataDir / loadAppState / restoreWinHTTPIfPending
  │    ├─ EnsureBinary / EnsureAux / ensureGeodata
  │    ├─ loadSavedSubscription / loadProcRules / writeProcRulesYAML
  │    ├─ configgen.Write → Manager.Start
  │    ├─ startAutoUpdate / startHistorySampler
  │    └─ startKernelWatchdog / startNetworkHealthCheck / afterStart
  └─ application.New → Attach(app) → window/tray → app.Run → Shutdown
```

关键状态所有权：

- `CoreService.proxyOn / tunOn / backup / restoreProxy / restoreTUN`：系统接管的状态，但实际注册表/TUN 的真实状态没有独立表示。
- `Manager.cmd / state / stopping / exited`：内核进程状态，但启动窗口期 `cmd == nil`。
- `procRules`：进程规则唯一来源；运行态由 `process-direct.yaml` / `process-proxy.yaml` / 隐藏策略组共同表达。
- `subConfig / subTargets / subProviders`：订阅骨架与进程目标解析结果。
- 磁盘：`proxy.json`、`app-state.json`、`subs.json`、`sub-*.yaml`、`process-rules.json`、`traffic-history.json`。

---

## 3. 状态模型与并发分析

### 3.1 系统接管状态

当前只有两个布尔值 `proxyOn`、`tunOn`，加上一次性的 `restoreProxy` / `restoreTUN` 和 `proxy.json` 备份。缺少：

- `DesiredState`：用户希望系统代理/TUN 处于什么状态。
- `ActualState`：注册表 `ProxyEnable` / PAC / WinHTTP / TUN 网卡的真实状态。
- `TransitionState`：切换进行中，禁止并发切换。
- `RecoveryState`：恢复失败、待下次提权或下次启动处理。

直接后果：

- `reconcileProxy` 只要看到 `proxy.json` 且注册表启用，就认为代理归自己管。
- 恢复失败后没有持久化的"待恢复"状态；备份删除后无法重试。
- `tunOn` 只表示意图，不能证明 TUN 网卡真实存在。

### 3.2 内核生命周期

`Manager.Start` 在 `m.cmd == nil` 时放行，但 `m.cmd` 要等 `cmd.Start()` 成功后才赋值。因此存在明确的启动窗口：

- 两个并发 `Start` 都能通过检查，启动两个 mihomo。
- `Stop` 在窗口期拿到 `cmd == nil`，直接返回；启动仍继续。
- `Restart` 没有串行化，两个重启可交叉 `Stop/Start`。
- `Stop` 等待 `exited` 后，还会轮询 `cmd` 是否清空，但 1 秒后**无条件返回 nil**，此时紧接着的 `Start` 仍可能报"内核已在运行"。

### 3.3 前端并发

前端对模式和进程规则有乐观更新守卫，能防止轮询覆盖本地新值。但 `onSetProc` 没有进入全局 `busy`，快速连点会向后端并发发送多次 `SetProcessRule`，与配置重写/内核重载叠加。

---

## 4. 问题清单

### P0（未确认稳定复现）

无。P1-01 与 P1-02 组合后可能出现"用户断网且备份丢失"，但需要故障注入才能确认是否达到不可恢复级别；在未复现前不认定为 P0。

---

### P1

#### P1-01 停止内核忽略系统代理恢复失败，仍继续停止内核

- 模块：系统代理 / 内核控制
- 位置：`service.go:809-828`（`SetSystemProxy(false)` 错误被 `_ = err` 丢弃，随后 `mgr.Stop()`）；`service.go:1771-1785`（内核升级复用 `StopKernel`）
- 触发条件：注册表写入失败、权限异常、备份损坏，导致 `restoreSystemProxy` 返回错误。
- 根因：`StopKernel` 把代理恢复视为尽力而为，没有中断后续停止流程，也没有记录待恢复状态。
- 影响：内核已停止，但系统代理仍指向 127.0.0.1 的死端口；用户断网。内核升级路径同样会先关闭系统代理，升级完成后不会自动恢复。
- 复现：可通过注入 `sysproxy.Disable`/`SetOverride` 失败稳定复现（需测试钩子）。
- 建议修复：恢复失败时返回错误并保留备份与待恢复标记；是否继续停止内核需由调用方显式决定。升级流程应先保存"升级前 proxyOn"，成功重启后恢复。
- 回归测试：T01/T02；模拟恢复失败，断言 `proxy.json` 保留、内核不停止（或明确进入待恢复态）。

#### P1-02 restoreSystemProxy 在恢复前删除备份，且不验证实际状态

- 模块：系统代理
- 位置：`service.go:925-961`（`os.Remove(proxy.json)` 位于 `sysproxy.Enable/Disable` 之前）
- 触发条件：任意一次恢复失败（注册表权限、值被占用、PAC/Override 写回失败）。
- 根因：备份删除与恢复动作没有事务边界；错误只返回，不保留可重试状态。
- 影响：恢复失败后唯一备份已丢失，下次启动无法识别原始配置；系统代理可能长期指向死端口。内存 `s.backup` 也已提前清空。
- 复现：注入 `sysproxy.SetOverride` 失败即可稳定复现。
- 建议修复：先恢复并回读验证（`Current()` + `Override()` + `AutoConfigURL()`），全部成功后再删除 `proxy.json`；失败时保留备份并设置 `proxyPendingRestore`，下次启动/提权重试。
- 回归测试：T02；断言失败后文件仍在、下次启动能再次尝试。

#### P1-03 TUN 切换回滚不完整，且未验证真实 TUN 状态

- 模块：TUN / 系统接管
- 位置：`service.go:1110-1162`；关键点 `1131-1138`（`sysproxy.Disable`、删除 `proxy.json` 的错误被忽略）、`1142-1156`（失败回滚只 `Enable(prevBackup.Addr)`，未恢复 PAC/Override/WinHTTP，也未处理"旧内核已被 Stop 掉"的情况）
- 触发条件：`RestartKernel` 失败；`Disable` 失败；注册表写入失败。
- 根因：回滚假设旧内核仍在运行且系统代理只需重新启用地址；没有记录完整原状态，也没有 API 验证 TUN 是否真的生效。
- 影响：
  - 开 TUN 失败后，用户原始 PAC/Override/WinHTTP 可能丢失或保持被接管。
  - 关 TUN 失败时，旧内核可能已被停止，`tunOn` 却回滚为 `true`，UI 显示 TUN 已开而实际无内核。
  - 重启返回成功但 mihomo 随后退出时，`tunOn` 仍为 true。
- 复现：T03/T04；让 `RestartKernel` 的 `Start` 失败，或让 mihomo 因 TUN 驱动失败退出。
- 建议修复：引入显式切换事务：保存完整代理快照 → 变更 → 重启 → 通过 `/configs` 验证 TUN 字段 → 提交状态；任一步失败按快照完整回滚，并明确内核是否仍在运行。
- 回归测试：T03/T04；覆盖系统代理→TUN、TUN→系统代理、失败回滚、内核未运行。

#### P1-04 系统代理归属判断过宽，会覆盖外部软件或用户的新设置

- 模块：系统代理
- 位置：`service.go:1054-1099`（`reconcileProxy` 只要 `proxy.json` 存在且 `ProxyEnable=1` 就认领）；`service.go:666-679`（重启后按 `restoreProxy` 无条件重新开启）
- 触发条件：用户或其它代理软件在 RoutePilot 接管期间修改了 `ProxyServer`；用户手动关闭系统代理后退出再启动。
- 根因：`proxy.json` 只记录"曾经接管过"，没有校验当前 `ProxyServer` 是否等于我们上次写入的 `Addr`；`restoreProxy` 是启动时无条件消费的旧意图。
- 影响：
  - 启动时把外部软件的新代理地址改回我们的端口。
  - 退出时把"接管前的旧值"写回，覆盖用户/外部软件的新配置（T16）。
  - 用户手动关闭系统代理，下次启动又被重新打开。
- 复现：T16；手动改注册表 `ProxyServer` 后触发 `reconcileProxy`/退出。
- 建议修复：认领前校验 `Current()` 返回值等于 `backup.Addr` 或明确的本产品地址；增加"用户手动关闭"的持久标记；`restoreProxy` 只在确认仍由本产品接管时执行。
- 回归测试：T16；外部改代理、手动关代理、正常退出三种路径。

#### P1-05 Manager.Start/Stop/Restart 缺少串行化，可产生多个内核

- 模块：内核生命周期
- 位置：`internal/kernel/manager.go:211-254`（`cmd` 在 `cmd.Start` 后才赋值）、`293-332`（Stop 窗口期直接返回）、`335-341`（Restart 无锁）
- 触发条件：并发点击启动/重启、托盘与主窗口同时操作、看门狗重启与用户重启重叠。
- 根因：只靠 `m.cmd != nil` 表示运行中，缺少 `starting/stopping/restarting` 状态与操作互斥。
- 影响：两个 mihomo 争抢端口；旧进程未被回收；Job Object 只挂到其中一个；出现双内核或孤儿进程。
- 复现：T05/T06/T07；并发调用 10 次 `Start`。
- 建议修复：Manager 内引入操作锁 + 状态机；`Start` 在锁内完成"标记 STARTING → 启动 → 提交 cmd"，窗口期禁止第二次 Start；`Stop/Restart` 串行化并等待真实退出。
- 回归测试：T05-T07；`go test -race`。

#### P1-06 看门狗重启无真正上限，且可能在 app 未初始化时 panic

- 模块：内核恢复 / 启动顺序
- 位置：`service.go:1330-1357`；`main.go:148-190`（`Boot()` 先于 `Attach(app)`）；`service.go:1339,1344` 直接访问 `s.app.Event`
- 触发条件：内核在 `Boot` 完成后、`Attach` 前崩溃；或每次重启都短暂成功后又崩溃。
- 根因：
  - `failCount` 在"重启成功"时立即清零，统计的是"重启失败次数"，不是"崩溃次数"。
  - 回调在 `cmd.Wait()` goroutine 中同步 `Sleep + RestartKernel`，未检查 `s.app` 是否为空。
- 影响：反复崩溃可无限重启（每次成功即清零），或启动早期直接 panic；任务要求的"有限次数恢复"未实现。
- 复现：T08；构造一个启动后 1 秒退出的假内核。
- 建议修复：引入时间窗口内的崩溃计数与指数退避上限；回调只做异步调度，不直接重启；`app` 为空时只记录日志。
- 回归测试：T08；崩溃 5 次后停止自动重启。

#### P1-07 afterStart 协程缺少退出/代数保护，可能在 Shutdown 后重新开启代理

- 模块：启动恢复 / 退出
- 位置：`service.go:623-684`（轮询最多 15 秒，末尾可 `SetSystemProxy(true)`）；`service.go:713-726`（Shutdown 不等待该协程）；`main.go:237-241`（`app.Run()` 出错时 `log.Fatal`，跳过 `Shutdown`）
- 触发条件：启动后立即退出；`app.Run` 返回错误。
- 根因：`afterStart` 没有 `Quitting()` 检查或启动代数；退出路径没有统一的 defer/finally。
- 影响：Shutdown 已恢复代理并停止内核，afterStart 晚到后再次打开系统代理，留下指向死端口的代理；`log.Fatal` 路径完全跳过恢复。
- 复现：T01 变体；在 `Version` 就绪前退出。
- 建议修复：所有启动异步任务携带 generation 与 `Quitting` 检查；`main` 用 defer 保证 `Shutdown` 执行；`app.Run` 错误也走 `Shutdown`。
- 回归测试：T01；快速启动后退出，断言代理恢复且不再被重新开启。

#### P1-08 进程身份只依赖 EXE 名，存在同名误分流

- 模块：进程分流
- 位置：`service.go:575`（`PROCESS-NAME,<proc>,<group>`）；`service.go:1893-1898`（连接视图只保留 `process` 名，路径仅展示）；`frontend/src/components/ConnectionsPanel.tsx:60-72`（进程列表按名称聚合）
- 触发条件：两个不同目录下同名 EXE；程序升级后路径变化；UWP/权限不足导致 `process` 为空；同名进程重启。
- 根因：规则与 UI 都以 basename 作为唯一身份，没有 `ProcessIdentity`。
- 影响：给 `chrome.exe` 指定节点会影响所有同名进程；无法区分绿色版/安装版；部分 UWP 统一显示 `ApplicationFrameHost.exe`；无法读取路径的进程回退到"—"后规则无法命中。
- 复现：T12；复制同一 exe 到两个目录并分别联网。
- 建议修复：设计版本化 `ProcessIdentity`（至少 exe 名 + 规范化路径，可选发布者/哈希）；规则存储增加 `version` 并保留旧 `procRules` 迁移；UI 显示路径并提供"按路径匹配"选项。
- 回归测试：T12/T13；同名不同路径、路径变化、无路径进程。

#### P1-09 热更新失败后持久化状态与运行态不一致

- 模块：进程规则
- 位置：`service.go:3441-3532`（先写 `process-rules.json` 与 provider 文件，再 `configgen.Write`/`ReloadConfig`；失败时只返回错误，不回滚内存/磁盘）；`3513-3517`（重载失败再重启，重启也失败则直接返回）
- 触发条件：配置生成失败、`PUT /configs` 失败、重启失败、provider 热刷新部分失败。
- 根因：没有"规则事务"概念；持久化视为已提交，运行态失败视为操作失败，两者没有一致性约束。
- 影响：前端回滚显示旧规则，但磁盘/内存已是新规则；下次重启突然生效。用户看到"设置失败"但实际改了一半。
- 复现：T11 变体；替换 `ReloadConfig` 为失败注入。
- 建议修复：定义热更新成功 = 持久化成功 + 内核加载成功 + 连接缓存失效；失败时回滚到操作前快照（内存、JSON、provider 文件）或返回明确的"待生效"状态。
- 回归测试：规则变更成功、重载失败、重启失败、provider 部分失败。

#### P1-10 proxy.json / app-state.json 非原子写入，且错误被忽略

- 模块：持久化
- 位置：`service.go:1040-1046`（`persistProxyState` 忽略写错误）；`service.go:3224-3255`（`saveAppState` 忽略写错误）
- 触发条件：磁盘满、权限不足、写文件时进程崩溃。
- 根因：直接 `os.WriteFile`，没有临时文件 + Sync + 原子替换；加载端解析失败静默返回。
- 影响：
  - `proxy.json` 截断后 `failBoot` 不会恢复，注册表可能继续指向死端口。
  - `app-state.json` 截断后 `restoreTUN/restoreProxy` 与 WinHTTP 备份丢失；WinHTTP 备份不可重建。
  - 更危险的自指场景：`proxy.json` 丢失而 `app-state.ProxyOn=true` 时，`afterStart` 会把当前注册表里"我们自己留下的死端口"当作原始配置备份，退出后恢复成死端口。
- 复现：T01/T10；写入过程中强杀进程。
- 建议修复：统一原子写工具（临时文件 + fsync + `MoveFileEx`/`ReplaceFile`）；写失败上抛并保留上一份有效备份；解析失败时尝试 `.bak`，并记录恢复日志。
- 回归测试：T10；截断 JSON、只读目录、磁盘空间不足。

#### P1-11 订阅/进程规则文件非原子写入，解析失败后可能静默覆盖

- 模块：持久化
- 位置：`service.go:2260-2275`（订阅原文/provider 文件）；`service.go:3151-3155`（进程规则 YAML）；`service.go:3020-3033` 与 `3037-3053`（加载失败直接 return，内存为空）；`service.go:3493-3495`（进程规则 JSON）
- 触发条件：写订阅/规则时崩溃；YAML/JSON 截断；磁盘错误。
- 根因：无原子替换、无 `.bak`、加载失败不区分"文件不存在"与"文件损坏"。
- 影响：订阅文件损坏后启动没有节点；下一次保存会用空内存覆盖损坏文件，原数据不可恢复。进程规则同理。
- 复现：T10/T11；写入中杀进程，然后修改一次规则触发重写。
- 建议修复：原子写入 + 双份轮换（当前/上一份）；加载失败进入只读保护态，禁止自动覆盖，并提示用户从备份恢复。
- 回归测试：T10；损坏文件后重启，断言保留可恢复副本。

#### P1-12 订阅自动更新、手动切换与内核重启之间缺少操作串行化

- 模块：订阅 / 内核
- 位置：`service.go:2360-2399`（SwitchSubscription）；`service.go:2693-2739`（applySubscription）；`service.go:3316-3363`（maybeAutoRefresh 后台循环）；三者都会 `RestartKernel`
- 触发条件：自动更新正在应用时用户切换订阅/刷新；两个订阅同时到期；用户连续点击切换。
- 根因：没有全局 `subscriptionOp`/`kernelOp` 互斥；`Manager.Restart` 本身也未串行化。
- 影响：配置串写（A 订阅的文件写入后 B 的 `subConfig` 生效）、内核双启动/重启失败、`subs.json` 与内存 `activeIdx` 不一致。
- 复现：T11；在下载等待期间切换订阅。
- 建议修复：订阅操作使用单一互斥/任务队列；应用订阅时校验 URL 仍为当前目标；内核重启纳入同一串行化通道。
- 回归测试：T11；并发刷新/切换/自动更新。

#### P1-13 API 端口缺少启动后真实监听校验

- 模块：内核 API
- 位置：`service.go:438-441`（`pickPort` 选 API 端口后直接创建客户端）；`service.go:623-655`（只等待 `/version`，超时后静默返回，不设置 bootErr）
- 触发条件：API 端口在探测与内核启动之间被其它进程占用；内核无法绑定 external-controller。
- 根因：`pickPort` 是"探测后关闭"的 TOCTOU；启动后没有验证实际监听地址与端口。
- 影响：内核可能在跑但 API 不可用，前端只能看到"运行中"却无节点/连接数据；状态与实际不一致（T14）。
- 复现：T14；先占用目标端口再启动。
- 建议修复：启动后校验 `net.Listen`/连接探测 + `/version`；失败时选择新端口并重载配置，或明确 bootErr；`pickPort` 只作为候选，不作为承诺。
- 回归测试：T14；端口冲突、端口瞬时抢占。

---

### P2

#### P2-01 sysproxy.Enable 部分失败会留下半接管状态

- 位置：`internal/sysproxy/sysproxy_windows.go:34-54`
- 说明：依次写 `ProxyServer`、`ProxyOverride`、`ProxyEnable`；中途失败直接返回，没有回滚已写入的值。若 `ProxyOverride` 写失败，用户的 bypass 已被改成固定串，`ProxyEnable` 仍是旧值。
- 影响：代理配置污染，退出时若原始 Override 为空也不会恢复。
- 建议：按"读取旧值 → 写入 → 失败恢复旧值"实现；`SetSystemProxy` 捕获失败后恢复备份。
- 测试：注入第二步写失败，断言注册表回到原值。

#### P2-02 WinHTTP 恢复删除失败被当作成功

- 位置：`internal/sysproxy/sysproxy_windows.go:183-198`（`_ = k.DeleteValue(...); return nil`）
- 影响：原始 WinHTTP 本来就未设置时，还原"成功"但值仍存在，系统服务继续走死代理。
- 建议：检查 `DeleteValue` 错误；值不存在（`ERROR_FILE_NOT_FOUND`）才视为成功。
- 测试：只读/权限不足时断言返回错误、`winHTTPPending` 保留。

#### P2-03 debug.log 记录完整订阅 URL，且文件权限 0644

- 位置：`service.go:3336`（`s.dbg("auto refresh skipped (%s)", m.URL)`）；`service.go:3209-3221`（日志文件 0644）
- 影响：订阅 URL 通常含 token，可能被同机其他用户/备份工具读取。
- 建议：URL 脱敏（只保留 host + 路径摘要）；敏感日志权限 0600；增加日志级别。
- 测试：日志中不出现完整 token。

#### P2-04 内核升级会关闭系统代理，升级后不恢复

- 位置：`service.go:1771-1785`
- 说明：升级调用 `StopKernel()`，其中 `SetSystemProxy(false)` 会把 `proxyOn` 置 false 并落盘；升级完成后 `RestartKernel` 不会恢复系统代理。
- 影响：用户升级内核后"代理自动关了"，需要手动再开；若恢复失败则断网。
- 建议：升级前记录 `proxyOn`，成功重启后按记录恢复，并在失败时保留待恢复状态。
- 测试：升级流程前后断言系统代理状态不变。

#### P2-05 内核版本比较是字符串不等，不是语义版本

- 位置：`internal/kernel/update.go:129-132`
- 说明：`lat != cur` 即 `HasUpdate`。当前版本比 latest 新（本地自定义版本）时仍提示升级，可能主动降级。
- 建议：解析 semver 比较；无法解析时才退回字符串提示。
- 测试：current=1.19.34、latest=1.19.33 应判否。

#### P2-06 内核下载摘要校验可为空

- 位置：`internal/kernel/update.go:191-229`
- 说明：GitHub release 未提供 `digest` 时不做任何校验，仍以管理员权限替换并执行内核。域名白名单 + TLS 降低了风险，但未满足"必须 SHA256 校验"。
- 建议：无摘要时拒绝自动升级，或使用 manifest 中固定的期望摘要；记录实际哈希。
- 测试：digest 为空时应失败。

#### P2-07 内核替换先删后改名，Windows 上非原子

- 位置：`internal/kernel/update.go:240-250`、`internal/kernel/update.go:320-330`
- 说明：`os.Remove(target)` 失败被忽略，`os.Rename` 失败后临时文件残留；运行中的 exe 无法删除时错误不直观。
- 影响：T18 强制退出可能留下缺失目标；下次启动多数情况能回退到内置内核，但自定义版本可能丢失。
- 建议：使用 `MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)` 或 `ReplaceFile`；保留 `.bak`；失败时清理临时文件。
- 测试：T18；运行中替换、断电模拟。

#### P2-08 pickPort 的 TOCTOU 与全占用回退

- 位置：`service.go:365-375`、`438-441`
- 说明：探测后立即关闭，端口可能被抢占；60 个端口全占用时直接返回 `prefer`（必然冲突），没有 error。
- 建议：启动后校验实际端口；全占用返回明确错误或扩大搜索范围。
- 测试：T14。

#### P2-09 ImportConfig 只替换内存与规则，不同步订阅文件/当前订阅

- 位置：`service.go:1224-1261`
- 说明：导入后 `subs`、`procRules` 被替换，但 `subConfig`、`sub-*.yaml`、`subs.json` 对应的本地文件没有重新拉取/切换；`RestartKernel` 仍使用旧 `subConfig`。
- 影响：导入后界面订阅列表与当前生效节点不一致；切换订阅可能找不到文件。
- 建议：导入后按第一个/激活订阅执行完整 `SwitchSubscription` 或明确要求用户刷新；校验 URL 与规则值。
- 测试：导入包含两个订阅的配置后断言生效订阅与磁盘一致。

#### P2-10 provider 目标失败时静默回退 DIRECT，校验离线过宽

- 位置：`service.go:3537-3568`（内核未运行时，只要存在 provider 就接受任意目标名）；`service.go:593-616`（`Select` 失败仅记日志，回退 DIRECT）
- 影响：用户把进程指向不存在的节点，规则生成成功但实际走直连，UI 仍显示目标节点。
- 建议：provider 节点需在 `/providers/proxies` 中确认存在；选择失败上报前端或标记"未生效"。
- 测试：目标不存在、provider 未加载、节点被删除。

#### P2-11 订阅下载允许 http 且不限制重定向目标

- 位置：`internal/subscription/fetch.go:59-87`（默认 `http.Client` 跟随重定向）、`270-300`（只校验 http/https 前缀）
- 影响：订阅 token 可能明文传输；恶意/被劫持面板可重定向到 `127.0.0.1`/内网地址（T17），形成 SSRF。
- 建议：默认拒绝 http（或显著警告）；重定向后校验 host；对私网/本机地址做策略限制；限制重定向次数。
- 测试：T17；重定向到 localhost 应被拒绝。

#### P2-12 配置文件没有版本字段与兼容策略

- 位置：`service.go:2209-2228`（`appState` 无 version）；`service.go:2246-2249`（`subsStore` 无 version）；`proxyBackup` 无 version
- 影响：新版本读取旧结构只能靠零值兜底；旧版本读取新结构会静默丢弃未知字段。无法判断"文件来自哪个版本、是否可迁移"。
- 建议：所有持久化结构增加 `version` 与迁移函数；未知高版本拒绝写入并保留原文件。
- 测试：旧版文件加载、未来版本文件加载。

#### P2-13 EnsureAux / ensureGeodata 忽略写入错误

- 位置：`internal/kernel/manager.go:197-208`；`service.go:339-351`
- 影响：wintun.dll 或 Geo 数据写失败时内核启动/首次连接可能失败或长时间卡在下载；错误无记录。
- 建议：返回错误并让 Boot 决定是否降级（例如 TUN 不可用提示）；写失败写日志。
- 测试：只读数据目录启动。

#### P2-14 SetMode 切全局失败时忽略 GLOBAL 出口选择错误

- 位置：`service.go:2113-2152`（`Select("GLOBAL", group)` 的 err 被忽略）
- 影响：GLOBAL 仍指向 DIRECT 时切入全局模式，全部流量直连，用户以为代理生效（断网/隐私泄漏）。
- 建议：选择失败则拒绝切换或提示；切换后回读 `GLOBAL.now` 验证。
- 测试：GLOBAL=DIRECT 且选择失败时不应切到 global。

#### P2-15 前端进程规则无全局串行化，快速连点会并发写配置

- 位置：`frontend/src/App.tsx:683-712`
- 说明：`onSetProc` 不设置 `busy`，同一进程连点或不同进程同时改会并发调用 `SetProcessRule`。
- 影响：后端配置重写/内核重载并发，可能触发 P1-09/P1-12。
- 建议：按进程使用请求队列或全局 mutation 锁；只允许最后一次意图生效。
- 测试：快速连续切换同一进程策略，最终后端状态等于最后一次点击。

#### P2-16 package.py 打包前不校验内嵌资源

- 位置：`package.py:29-54`
- 说明：`package.py` 直接构建前端、生成资源、编译、压缩，不调用 `fetch-assets.py` 校验；`build.ps1:37-41` 才做校验。全新克隆下 `assets` 为空占位文件时仍可成功产出 zip，但 exe 内嵌 0 字节内核/驱动。
- 影响：发布包首次运行必然失败，属于发布阻断问题。
- 建议：打包脚本强制运行 `fetch-assets.py`（严格模式）并校验哈希；CI 发布流程复用。
- 测试：空 assets 下 package.py 应失败。

#### P2-17 CI 缺少 race、lint、Windows 产物与发布校验

- 位置：`.github/workflows/ci.yml:41-57`；`.golangci.yml:1-13`
- 现状：已有 PR 触发、gofmt、vet、test、前端 typecheck/build；缺 `go test -race`、`golangci-lint`、Windows x64 产物构建、资源完整性、SHA256 发布校验。
- 影响：并发回归、Windows 平台构建问题、资源缺失可能进入发布。
- 建议：按任务书 C6 增加独立 PR 与 release workflow。
- 测试：故意制造 data race / 未格式化 / 缺少资源，确认 CI 失败。

#### P2-18 第三方许可证兼容性未完成法律确认

- 位置：`THIRD_PARTY_NOTICES.md:18-26`
- 现状：项目声明 GPL-3.0，mihomo 为 GPL-3.0，wintun 标注"GPL-2.0 或商业授权"。
- 风险：GPL-2.0-only 与 GPL-3.0 的兼容性、以及"独立进程/独立 DLL"是否构成聚合分发，需要法务确认；仅按许可证名称判断不足。
- 建议：确认 wintun 具体许可证文本（是否 "or later"）、随包提供源码/许可证义务、保留商业授权选项评估。
- 测试：发布前法律与合规检查清单。

---

### P3

#### P3-01 前端 `npm run lint` 实际只是 `tsc --noEmit`

- 位置：`frontend/package.json:15`
- 说明：没有 ESLint/React hooks 规则，无法发现常见的 hooks 依赖与异步清理问题。
- 建议：接入 ESLint + TypeScript 规则，CI 复用。

#### P3-02 隐藏组前缀 `nx-pp-` 与订阅命名空间可能冲突

- 位置：`internal/configgen/gen.go:212-225`、`service.go:550-575`
- 说明：若机场策略组/节点本身以 `nx-pp-` 开头，会与内部隐藏组同名；可能造成内核拒绝重复组名或节点被 UI 隐藏。
- 建议：隐藏组名加入随机/固定盐或校验订阅命名空间，发现冲突时改名并记录提示。

#### P3-03 订阅来源中 `proxy-providers` / `rule-providers` 的本地路径未清洗

- 位置：`internal/configgen/gen.go:142-165`（只剥离监听/脚本/外部 UI 等字段，未限制 provider 的 file 路径与 URL）
- 说明：恶意订阅可让 mihomo 读取本机任意路径或访问内网 URL。当前不构成代码执行，但属于不必要的本地文件/网络暴露面。
- 建议：白名单化 provider 路径到数据目录；禁止订阅自带的 file provider 指向目录外。

#### P3-04 节点/策略组重名未检测

- 位置：`internal/subscription/fetch.go:316-355`（只统计名称，不校验重复）
- 影响：多个 provider 或 proxies 中同名节点时，选择/测速/规则结果依赖内核内部去重策略，行为不确定。
- 建议：解析阶段检测重复并告警，或在 UI 标记 provider 归属。

#### P3-05 托盘快速节点列表只在启动时构建

- 位置：`main.go:352-366`
- 影响：切换订阅或测速后托盘菜单不更新，显示过时节点。
- 建议：菜单打开时动态重建或订阅切换后刷新。

#### P3-06 app-state.json 权限 0644，包含 WinHTTP 备份

- 位置：`service.go:3253-3255`
- 说明：WinHTTP 注册表二进制备份不属于高敏数据，但仍建议 0600；与 proxy.json 保持一致。
- 建议：统一敏感状态文件权限为 0600。

#### P3-07 发布产物没有 SHA256 清单与构建元数据

- 位置：`package.py:96-111`
- 说明：只产出 zip，不生成 checksums、git commit、资源版本清单。
- 建议：发布脚本生成 `.sha256` 与 `build-info.json`，CI 校验。

---

## 5. 专项复现矩阵（第二阶段输入）

| 编号 | 场景 | 当前判定 | 预备测试方式 |
|---|---|---|---|
| T01 | 接管后强杀程序，下次启动恢复 | 风险存在（P1-10/P1-07） | 进程注入/强杀 + 重启，检查 proxy.json/app-state/注册表 |
| T02 | 恢复失败时备份保留 | 不满足（P1-01/P1-02） | 注入 sysproxy 写失败 |
| T03 | TUN 开启失败完整恢复 | 不满足（P1-03） | 让 RestartKernel 失败，断言代理/PAC/Override/WinHTTP |
| T04 | TUN 关闭失败实际状态与 UI 一致 | 不满足（P1-03） | 停内核后 Start 失败，断言 tunOn |
| T05 | Start 并发 10 次最多一个内核 | 不满足（P1-05） | 并发单测 + 进程计数 |
| T06 | Start/Stop 并发无孤儿 | 不满足（P1-05） | 并发 + Job Object 检查 |
| T07 | Restart 并发串行化 | 不满足（P1-05） | 并发重启 + 端口/进程数 |
| T08 | 内核异常退出有限恢复 | 不满足（P1-06） | 假内核启动后退出，统计重启次数 |
| T09 | Job Object 分配失败不留孤儿 | 风险存在（P2-19） | 模拟 Assign 失败，检查进程 |
| T10 | 磁盘写入失败保留有效配置 | 不满足（P1-10/P1-11） | 只读目录/磁盘满/写中断 |
| T11 | 订阅更新期间切换订阅不串写 | 风险存在（P1-12） | 并发刷新 + 切换 |
| T12 | 两个同名 EXE 不误伤 | 不满足（P1-08） | 两个目录同名 exe + 分离规则 |
| T13 | PID 复用不继承旧身份 | 待验证 | 高频进程重启 + 连接采样 |
| T14 | API 端口冲突正确识别 | 不满足（P1-13） | 预占端口后启动 |
| T15 | 休眠唤醒后接管状态可恢复 | 未验证 | 真实睡眠/唤醒 + 注册表/TUN/WinHTTP |
| T16 | 外部改系统代理不误覆盖 | 不满足（P1-04） | 外部写注册表 + 启动/退出 |
| T17 | 订阅重定向不访问未授权目标 | 不满足（P2-11） | 重定向到 localhost/内网 |
| T18 | 更新内核强制退出不损坏二进制 | 风险存在（P2-07） | 替换窗口强杀 + 校验哈希 |

另有 P2-19：`attachToJob` 完全忽略 `CreateJobObjectW` / `SetInformationJobObject` / `AssignProcessToJobObject` 的返回值（`internal/kernel/job_windows.go:59-82`），失败时静默降级，T09 需要覆盖。

---

## 6. 测试覆盖分析

已有测试：

- `internal/configgen/gen_test.go`：规则位置、隐藏组、主组选择。
- `process_targets_test.go`：显式目标、provider 目标、去重、失效目标。
- `internal/subscription/parse_test.go`：Clash/base64 解析与 provider 统计。
- `internal/kernel/update_test.go`：zip 解包与变体选择。
- `internal/kernel/manager_test.go`：仅日志缓冲，**没有 Start/Stop/Restart 并发测试**。
- `internal/autostart/autostart_test.go`：任务 XML 编码。
- `procname_test.go`、`subinfo_test.go`、`healthcheck_test.go`、`datadir_test.go`：局部纯逻辑。

明显空白：

1. 系统代理 / TUN 事务：`internal/sysproxy` 无测试，`CoreService` 无故障注入测试。
2. 内核生命周期：无并发/孤儿/崩溃恢复测试。
3. 订阅并发：无自动更新与切换竞态测试。
4. 持久化：无截断/只读/原子替换测试。
5. 前端：无组件或交互测试；`lint` 仅类型检查。
6. CI：无 `-race`、无 `golangci-lint`、无 Windows 构建与发布校验。

---

## 7. 与整改阶段的对应关系

- **C1 网络安全与恢复**：P1-01、P1-02、P1-03、P1-04、P1-07、P1-10、P2-01、P2-02、P2-04。
- **C2 内核生命周期**：P1-05、P1-06、P1-13、P2-07、P2-08、P2-19。
- **C3 进程分流**：P1-08、P1-09、P2-10、P2-12（规则结构迁移）、P3-02。
- **C4 持久化与安全**：P1-10、P1-11、P1-12、P2-03、P2-06、P2-11、P2-12、P2-16、P3-03。
- **C5 架构治理**：状态模型、CoreService 拆分、前端 mutation 串行化（P2-15）。
- **C6 CI/发布**：P2-17、P2-18、P3-01、P3-07。

---

## 8. 审查结论与暂停点

当前版本功能完整、局部防护（API 仅本机、Secret 随机、订阅 TLS 默认严格、规则名过滤、Job Object、进程目标 provider 方案）设计合理，但**系统接管的事务性、内核生命周期串行化、进程身份模型、持久化原子性**尚未达到生产级要求。

按任务书要求，第一阶段到此暂停，等待确认后再进入 T01-T18 专项复现与 C1 整改。未修改任何业务代码，仅新增本报告。

下一步建议确认事项：

1. 是否接受本报告的问题分级与范围。
2. 第二阶段是否先做 T01-T09（网络与内核），还是全部 T01-T18 一次性建立测试骨架。
3. 真实 Windows 测试环境是否具备管理员权限、可强杀进程、可修改 HKLM WinHTTP、可执行睡眠/唤醒。
