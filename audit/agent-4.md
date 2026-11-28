# Agent-4 审计报告：升级、文件系统与数据持久化

## 1. 头部

| 项 | 内容 |
|---|---|
| 审计对象 | RoutePilot（Go 1.23+ / Wails v3，Windows 桌面代理客户端） |
| 仓库 | `C:/Users/wpc/WorkBuddy/代理/proxdeck` |
| 基线 | `39ca02c1aa9303fc14fe8264d2715432cd50f030`（`git rev-parse HEAD` 实测一致，`git status --porcelain` 为空） |
| 审计方式 | 只读静态审查 + 定向单测（不触发任何真实下载/内核替换） |
| 结论摘要 | **P0 = 0，P1 = 1，P2 = 4，P3 = 8** |

### 1.1 审阅文件清单（行数）

| 文件 | 行数 | 关注点 |
|---|---|---|
| `internal/kernel/update.go` | 1005 | 版本检查 / 下载 / 摘要校验 / 解包 / 备份 / 替换 / 回滚 / EnsureBinary |
| `internal/kernel/manager.go` | 771 | 进程生命周期、`lastBackup` 语义、`EnsureAux` 落盘 |
| `internal/kernel/watchdog.go` | 342 | 与升级并发的重启来源 |
| `internal/kernel/update_test.go` | 89 | 解包与资产选择 |
| `internal/kernel/update_fallback_test.go` | 114 | 入口回退（限流） |
| `internal/kernel/upgrade_test.go` | 225 | PE 校验 / 备份回滚 / 替换失败 / 记录读写 |
| `internal/kernel/download_test.go` | 102 | 候选地址 / 进度 |
| `internal/kernel/c5_semver_test.go` | 83 | SemVer 比较 |
| `internal/kernel/c5_1_semver_test.go` | 68 | SemVer 严格性 |
| `service.go` | 6054 | UpdateCore / ResetCore / 数据目录迁移 / app-state / 订阅 / 配置导入导出 |
| `config_files.go` | 104 | 配置损坏识别与保留 |
| `internal/configgen/gen.go` | 457 | config.yaml 生成与落盘 |
| `procgen.go` | 327 | 进程规则 generation + manifest 发布 |
| `main.go` | 513 | 数据目录初始化、单实例 |
| `frontend/src/App.tsx` / `SettingsPanel.tsx` | — | 升级按钮可用性（是否自带互斥） |

### 1.2 执行过的命令与结果

| 命令 | 结果 |
|---|---|
| `git rev-parse HEAD` / `git status --porcelain` | `39ca02c…` / 空（工作区干净） |
| `wc -l` 上述文件 | 见 1.1 |
| `grep -rn "os.WriteFile\|os.OpenFile\|os.Create(" --include=*.go` | 非原子写入仅 5 处（见第 5 节写入点清单） |
| `grep -rn "LastBackupPath\|RollbackBinary\|LastUpgradeRecord"` | 生产代码中 `LastUpgradeRecord` 无调用方；`RollbackBinary` 仅在 `UpdateCore` 内调用 |
| `go test -count=1 -run 'TestInstallBinaryRenameFailureKeepsOld\|TestInstallBinaryBackupAndRollback\|TestRollbackWithoutBackupFails' -v ./internal/kernel/` | 3/3 PASS；`RenameFailureKeepsOld` 耗时 **1.53s**，与 `replaceFileWithRetry` 的 4 次重试（150+300+450+600ms）吻合，确认"文件被占用 → 重试 4 次 → 放弃且旧内核完好" |

> 未重跑全量测试（按纪律），未做任何真实网络请求，未运行会调用 `UpdateCore` 的路径。

### 1.3 与任务书的事实纠正（影响结论口径）

1. **不存在 `-datadir` 命令行参数**。`grep -n "flag\.\|os.Args" main.go service.go` 无命中；唯一覆盖入口是 `setDataDirOverride`（`service.go:118-122`，注释明确"仅用于测试"）。因此"用户可用 `-datadir` 迁移数据目录"这一前提不成立，`migrateDataDir` 的固定路径问题只在测试注入场景有意义（见 A4-16）。
2. 升级链路有**两条**：`DownloadAndReplace`（update.go:571）与生产实际使用的 `DownloadCore + InstallCore`（service.go:3457/3468）。`DownloadAndReplace` 只被测试使用。
3. `core-upgrade.json`（`upgradeRecordFile`）是**只写不读**的诊断文件：`LastUpgradeRecord`（update.go:750）在生产代码中没有任何调用方。这一点是 A4-02 的关键证据。

---

## 2. 问题清单

> 编号规则：A4-01 起。等级门槛遵循任务纪律（P0/P1 必须能给出可复现因果链）。

### A4-01（P1）镜像提供的 release JSON 与摘要同源，域名白名单可被"嵌套 URL"绕过 → 任意二进制以管理员权限执行

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/update.go` |
| 函数 | `repoAPIEndpoints`(L43-46) / `downloadCandidates`(L487-493) / `validateDownloadURL`(L464-477) / `DownloadCore`(L583-632) |
| 代码位置 | L45（镜像 JSON 入口）、L458（白名单含 `gh-proxy.com`）、L489-492（镜像前缀拼接/原样返回）、L599（校验）、L615-620（摘要比对） |
| 描述 | 代码注释（L40-42）声称"摘要即便来自镜像也不降低安全性……镜像既改不了产物也伪造不出匹配的摘要"。**该论断不成立**：摘要 `digest` 与下载地址 `browser_download_url` 来自**同一份 JSON**，而该 JSON 可以由第三方镜像 `https://gh-proxy.com/https://api.github.com/...` 提供。更关键的是，当 JSON 里的 URL 本身是 `https://gh-proxy.com/https://<任意主机>/x.zip` 时，`downloadCandidates` 因 `u.Host == downloadMirrorHost` 直接原样返回（L489-490），`validateDownloadURL` 只看到 `gh-proxy.com`（L472-475）便放行——白名单检查的是"第一跳主机"，而 `gh-proxy.com` 的语义就是"代理任意 URL"，等于把白名单降级为"允许下载任意 https 主机"。 |
| 触发条件 | 攻击者能控制镜像返回的 JSON（镜像运营方；或可对其做 TLS 中间人的企业代理/被信任 CA）。操作序列：① 阻断/使 `api.github.com` 失败（国内本就常见 403/超时）→ ② `fetchLatest` 落到镜像入口（L104-114）→ ③ 镜像返回伪造 JSON：`tag_name=v9.9.9`、`browser_download_url=https://gh-proxy.com/https://attacker.example/mihomo.zip`、`digest=sha256:<攻击者包的摘要>`、`size=<真实大小>`；④ 用户点"升级"→ `DownloadCore` 从 attacker.example 取包（经 gh-proxy 转发）→ ⑤ 摘要/大小/PE 校验**全部通过**（因为摘要也是伪造的）→ ⑥ `installBinary` 写入 `%APPDATA%\RoutePilot\bin\mihomo.exe` 并 `markCustom` → ⑦ `RestartKernel` 以当前进程权限启动（TUN 场景为管理员）。 |
| 实际影响 | 远程任意代码执行（管理员权限下的持久化二进制，且 `.core_custom` 保证下次启动不回退）。摘要机制在此路径上只保证"传输完整"，不保证"来源真实"。 |
| 根因 | ① 信任链的根被放在一个"可任意转发的第三方 HTTPS 代理"上；② 白名单只校验 URL 的 host 字段，未禁止"被代理的绝对 URL"形态（`path` 以 `http(s)://` 开头）；③ 摘要在设计上被当作"独立于来源的锚点"，但实际与来源同源。 |
| 修复建议 | (a) `validateDownloadURL` 增加：`u.Path` 不得以 `http://`/`https://` 开头（拒绝嵌套代理形态）；(b) `downloadCandidates` 只在 host 属于 `github.com`/`api.github.com` 时补镜像前缀，其它形态一律拒绝；(c) 把 `gh-proxy.com` 从"JSON 入口"移除，镜像只允许用于**已按 github 原始 URL 校验过**的产物下载；(d) 更彻底：把期望摘要内置进二进制或在 release 页签名校验（minisign/cosign），使摘要不再依赖运行时 JSON。 |
| 测试方案 | 见 6-T5：构造 `browser_download_url=https://gh-proxy.com/https://127.0.0.1:PORT/x.zip` 的 JSON，断言 `validateDownloadURL` 返回错误（当前会通过）。 |
| 置信度 | 对代码事实：**高**（逐行确认）；对可利用性：**中**（需要控制镜像 JSON 或做 TLS 中间人） |

### A4-02（P2）升级后内核"存在但起不来"时，启动期没有任何回滚/自愈路径；回滚能力只活在同一次进程内

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/update.go`、`service.go` |
| 函数 | `RollbackBinary`(L716-731) / `installBinary`(L702-705) / `EnsureBinary`(L923-939) / `Boot`(service.go:661-664, 710-713) |
| 代码位置 | `lastBackup` 只在 `installBinary` 内赋值（update.go:704），`Manager` 结构体注释自己写明"只在同一次进程内有效"（manager.go:278-280）；`RollbackBinary` 唯一调用点是 `UpdateCore`（service.go:3488）；`Boot` 只调 `EnsureBinary`（service.go:661） |
| 描述 | 恢复链是：`EnsureBinary` 若发现 `.core_custom` 存在且 `validatePE` 通过（update.go:923-930）→ **直接采用**；PE 不通过或文件缺失 → 清标记 + 释放内置内核（L931-938，可自愈）。`validatePE`（L781-793）只校验 MZ 头与 PE 签名，不校验"能否启动"。因此"**PE 合法但无法启动**"的新内核会一路被采用到 `mgr.Start`，`Boot` 失败后 `failBoot` 直接 return（service.go:710-713），既不尝试 `mihomo.exe.bak`，也不再释放内置内核。此时 `core-upgrade.json` 里明明记着 `backupPath`，但该文件在生产代码中没有读者（见 1.3-3），`lastBackup` 也因为进程已重启而丢失。 |
| 触发条件 | 磁盘状态：`bin/mihomo.exe` = PE 合法但无法工作的新内核；`bin/.core_custom` 存在；`bin/mihomo.exe.bak` = 旧内核。到达该状态的两条路径：① `installBinary` 成功后 `RestartKernel` 期间的进程被强杀/断电（替换已完成、标记已写、验证未完成）；② `RestartKernel` 失败后 `RollbackBinary` 也失败（`restoreFromBackup` → `atomicWriteBytes` 的 `Rename` 被占用，update.go:773；`abortStart` 若 5s 内没能收尾进程，句柄仍在，manager.go:540-546）。 |
| 实际影响 | 每次启动都停在"内核启动失败"，用户必须自己进设置点"回退内置"（`ResetCore` 可用，因为它不依赖内核运行）。期间用户完全没有代理能力；非技术用户表现为"软件坏了"。 |
| 根因 | 回滚状态（`lastBackup`）只存内存；启动期只做"PE 是否合法"的最弱校验，没有"上次升级未完成/上次启动失败"的持久化状态机。 |
| 修复建议 | 在 `Boot` 里、`mgr.Start` 失败后增加一次有界自愈：读 `core-upgrade.json` 的 `backupPath` → `restoreFromBackup` → 重试 `Start` 一次 → 仍失败则 `ClearCustomMark` + 释放内置内核。或引入 `core-upgrade.json` 的 `state: "pending-validation"`：`installBinary` 写盘时置位，`RestartKernel` 成功后 `SaveUpgradeRecord` 才改为 `"ok"`；启动时看到 `pending-validation` 就直接回滚。 |
| 测试方案 | 见 6-T9/T10：用磁盘状态（伪造 exe + `.core_custom` + `.bak`）+ 断言"第二次 `EnsureBinary` 应回退到 `.bak` 或内置"。 |
| 置信度 | **高**（路径逐行可确认）。C3 报告 §7.2 已把它列为"尚未实现，属于 C4 断电矩阵"，本次审查确认**仍未实现**。 |

### A4-03（P2）回滚触发条件过宽：任何 `RestartKernel` 错误都被当作"新内核启动失败"，成功安装的新版本被静默回退

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `UpdateCore`(L3407-3514) / `restartKernel`(L1521-1565) |
| 代码位置 | L3483-3506（`if err := s.RestartKernel(); err != nil { rec.RolledBack = true; … RollbackBinary() … }`）、L1525-1527（退出中直接报错）、L1546-1549（`configgen.Write` 失败）、L1550（`mgr.Restart` 失败，含 `ErrPortInUse`/`ErrStartTimeout`/`ErrLifecycleBusy`） |
| 描述 | `RestartKernel` 的失败原因里，只有一部分与"新二进制不可用"有关：`程序正在退出，取消重启`（L1526）、配置文件写盘失败（L1547）、控制端口被占（`classifyStartFailure`，manager.go:521-523）都与二进制无关，但都会触发 `RollbackBinary()`，把刚安装好的新内核换回旧版，并写入 `RolledBack=true` 的升级记录。 |
| 触发条件 | ① 下载耗时数分钟后用户在安装阶段关闭窗口 → `Shutdown` 置 `shuttingDown`（L1332）→ `RestartKernel` 立即返回"程序正在退出"（L1526）→ 回滚。② 用户机器上恰好有别的程序占用 `defaultAPIPort` 附近 60 个端口（`pickPort` 只在 Boot 时选一次，L679-681；若被外部抢占则 `ErrPortInUse`）→ 回滚。 |
| 实际影响 | 新版本被静默丢弃（用户看到"升级失败"，但真正原因与内核无关）；`core-upgrade.json` 里的 `rollbackReason` 指向误导性原因；若旧内核随后也起不来（第 2 条场景下旧内核同样会因端口冲突失败），用户得到"已回滚但旧内核仍无法启动"。 |
| 根因 | `InstallCore` 的事务边界与"启动验证"的事务边界之间没有区分错误类别。 |
| 修复建议 | 只对"可归因于二进制"的错误回滚：`ErrProcessExited`、`ErrAPIUnreachable`、`ErrStartTimeout`（进程起来了但 API 不对）；对 `退出中 / ErrPortInUse / 配置写盘失败 / ErrLifecycleBusy` 只报错不回滚，并把 `RolledBack=false` 的记录写下来，提示"新内核已安装，下次启动生效"。 |
| 测试方案 | 6-T11：注入 `restart` 返回 `ErrPortInUse`，断言 `mihomo.exe` 仍是**新**内核、记录 `RolledBack=false`。 |
| 置信度 | **高**（代码路径确定）；触发概率：中低（依赖用户在中途退出或端口被抢） |

### A4-04（P2）`app-state.json` 是"整文件覆盖 + 多入口无串行化"，存在丢更新窗口；丢掉 `winHTTPPending`/TUN 快照时后果可达"机器级代理不还原"

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `saveAppStateErr`(L5390-5445) / `loadAppState`(L5283-5359) |
| 代码位置 | 快照在 L5390-5436（`s.mu.RLock()` 一次性读出全部字段），落盘在 L5444（`writeFileAtomic`）——**快照与落盘之间隔着 `json.MarshalIndent` + `CreateTemp` + `Write` + `Sync`（几十毫秒级）**；调用方至少 8 处且互不同步：`SaveWindowBounds`(L5448-5453)、`ToggleFavorite`(L3205-3219)、`SetMode`(L3952)、`takeOverWinHTTP`(L1979)、`setSystemProxyLocked`(L1715/1728)、`restoreProxyTransactionLocked`(L1908)、`retryPendingRestoreLocked`(L2186/2192)、`ImportConfig`(L2788)、`recoverPendingProxyRestore`(L1317) |
| 描述 | 每次保存都是"读全量内存 → 序列化 → 原子替换"。若两个调用方交错（A 先取快照但 `Sync` 慢、B 后取快照并先落盘），**A 的旧快照会覆盖 B 的新快照**（rename 顺序即写序）。典型受害者是字段级增量语义：`winHTTPPending=true` + `winHTTPBackup`（`takeOverWinHTTP` 先改注册表再落盘，L1970-1979）、`tunPrevRegistry`/`tunProxyPending`（L5424-5435）。 |
| 触发条件 | 两个入口在同一几十毫秒内并发。最现实的一对：窗口尺寸保存（DWM 回调，`SaveWindowBounds`）与"开启系统代理/开 TUN"（按钮回调）。二者都是用户动作直接触发，且 Wails 绑定各自 goroutine，无共享锁。 |
| 实际影响 | 若丢更新后进程紧接着崩溃/被强杀（后续没有任何一次 `saveAppState` 把内存状态再写回去），重启后 `winHTTPPending` 被视为 false → 机器级 WinHTTP 不再被还原，指向已死端口 → 依赖 WinHTTP 的系统服务（Windows Update、部分企业应用）全部断网；或 TUN 快照丢失 → 用户原始代理设置不还原。若丢更新后进程正常退出（`Shutdown` 路径会再次落盘），则只是瞬时不一致。 |
| 根因 | 缺少"持久化"这一层的串行化与版本号；把"字段级事务"（L5424-5435 的条件写入）建立在"整文件最后写入者获胜"之上。 |
| 修复建议 | 加一个专用 `stateSaveMu sync.Mutex` 包住"取快照 + 落盘"全过程（最省事，代价是串行 IO）；或维护 `revision uint64`，`saveAppStateErr` 在锁内递增并记录，写入前比对，避免旧快照后写。 |
| 测试方案 | 6-T12：注入慢速 `appStateWriter`（`writeFileAtomic` 已有 var 注入点，L5386），让 A 卡在 Sync，再触发 B 写 `winHTTPPending=true` → 断言最终文件 `winHTTPPending == true`（当前会失败）。 |
| 置信度 | 对"存在丢更新"：**高**；对"丢更新后立刻崩溃"：中（时间窗小，但后果严重） |

### A4-05（P2）`ImportConfig` 覆盖订阅列表但不重置 `activeIdx`、不校验订阅文件存在、无备份；引导时用 `subscription.yaml` 的内容冒充新订阅

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `ImportConfig`(L2760-2812) / `loadSavedSubscription`(L4464-4520) |
| 代码位置 | L2770-2779（直接替换 `s.subs`/`s.procRules`/`s.siteRules`/`s.ignored`，**不动 `s.activeIdx`/`s.subURL`**）、L4492-4499（`sub-<hash>.yaml` 读不到就回退读 `subscription.yaml`） |
| 描述 | ① 导入后的 `subs[i].URL` 对应的 `sub-<sha256(URL)>.yaml` 在新机器上并不存在（导入包 `ConfigBundle` 只含元数据，不含订阅原文，L2734-2741）。② 于是下次启动 `loadSavedSubscription` 走到 L4493 的 `err != nil` 分支，**回退读 `subscription.yaml`**——那是上一台机器/上一个订阅的原文。③ 随后 `writeSubscriptionFiles(meta.URL, res)`（L4506）把**旧订阅的原文写到新 URL 的哈希文件名下**，并把 `s.subURL` 设为导入的 URL（L4514）。结果：界面显示订阅 B 的地址与配额，内核加载的却是订阅 A 的节点。④ `activeIdx` 未重置：导入 3 条而当前 `activeIdx=5` 时，`ListSubscriptions` 返回 `active: 5`（L4201），前端高亮错位（启动时 L4473-4475 会纠正，但同一会话内不会）。 |
| 触发条件 | 在 A 机器导出配置 → 在 B 机器导入（B 上已有某个旧订阅或 `subscription.yaml` 残留）；或导入后未逐条刷新订阅就重启。 |
| 实际影响 | 用户以为在用导入的机场，实际走的是另一家机场的节点（合规与隐私风险 + 计费错乱）；`subs.json` 里的条目永远无法自带原文，必须逐条刷新才能修复。 |
| 根因 | 导入把"元数据"当作"完整订阅"；`loadSavedSubscription` 的旧布局兼容分支没有区分"布局迁移"与"文件缺失"。 |
| 修复建议 | (a) `ImportConfig` 内重置 `activeIdx=0` 并对每条 `subs[i]` 检查 `sub-<hash>.yaml` 是否存在，缺失则标记为"待刷新"而不是参与加载；(b) 回退到 `subscription.yaml` 的条件收紧为"`subsFile` 不存在且 `subMetaFile` 存在"（真正的旧布局），而不是"任意哈希文件缺失"；(c) 导入前把现有 `subs.json`/`app-state.json` 备份成 `.bak`。 |
| 测试方案 | 6-T13：`subs.json` 含 URL B（无 `sub-B.yaml`），磁盘有 `subscription.yaml`（URL A 的内容）→ 调 `loadSavedSubscription` → 断言不得写出 `sub-B.yaml`，且 `s.subURL` 不得为 B 而内容为 A。 |
| 置信度 | **高**（两条分支均为无条件执行） |

### A4-06（P3）`ResetCore` 忽略 `StopKernel` 的失败、且"先清标记后替换"，失败时静默丢弃用户自定义内核

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `ResetCore`(L3517-3531) |
| 代码位置 | L3521（`_ = s.StopKernel()`）、L3522（先 `ClearCustomMark`）、L3523（`_ = os.Remove(...)` 忽略错误）、L3527（`EnsureBinary`） |
| 描述 | 三步都忽略/后置了失败点：`StopKernel` 失败时内核仍在运行（`SetSystemProxy(false)` 失败会中止停止流程，L1496-1501），此时删除 exe 会因文件被占用失败（错误被丢弃），接着 `EnsureBinary` 因"无标记 + 哈希不等于内置"去 `Rename` 覆盖一个正在运行的文件 → 4 次重试后失败 → 返回"恢复内置内核失败"。但**标记已经在 L3522 被清掉**。 |
| 触发条件 | `StopKernel` 返回错误（系统代理还原失败）时点"回退内置"。 |
| 实际影响 | 本次会话：界面报"回退失败"，实际内核仍是自定义版；下次启动：无标记 + 哈希不等于内置 → `EnsureBinary` 直接用内置覆盖**自定义内核**，用户从未成功的操作被静默执行，`.bak` 里只剩上一版。 |
| 根因 | 破坏性操作（清标记）先于其前置条件（内核已停、文件可替换）验证。 |
| 修复建议 | `if err := s.StopKernel(); err != nil { return err }`；标记改为在 `EnsureBinary` 成功**之后**清除（或失败时回滚标记）。 |
| 测试方案 | 6-T14：注入 `StopKernel` 失败 → 断言 `IsCustomCore` 仍为 true 且返回错误。 |
| 置信度 | 高（静态确定）；触发概率低（依赖代理还原失败） |

### A4-07（P3）`subs.json` 损坏时"保留原文件"但内存列表为空，随后任何一次 `saveSubs` 都会把空列表写回去

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` / `config_files.go` |
| 函数 | `loadSubs`(L4107-4137) / `reportConfigProblem`(config_files.go:72-99) / `preserveCorruptFile`(config_files.go:52-68) |
| 代码位置 | service.go:4117-4121（非 OK 状态只提示并 `return`，`s.subs` 保持 Boot 时的 nil）、4167-4179（`saveSubs` 无条件用内存 `s.subs` 覆盖写） |
| 描述 | 损坏文件被重命名为 `subs.json.corrupt-<时间戳>`（设计正确，原数据可人工取回），但内存态没有"上一次已知良好列表"，下一次任一触发保存的动作（订阅增删改、`SetSubInsecure`、`ImportConfig`）都会写出 `{"items": []}`。 |
| 触发条件 | `subs.json` 被外部编辑坏 / 磁盘故障 / 手工截断；启动后用户做任一订阅操作。 |
| 实际影响 | 用户所有订阅从活动配置中消失，只能靠手动把 `.corrupt-*` 改名回来；启动横幅会提示"配置文件 subs.json 格式损坏"，所以不是无提示。 |
| 根因 | 只做"文件保留"，没有"内存兜底 + 禁止自动覆盖"的闸门。 |
| 修复建议 | 读取失败时置 `s.subsReadonly = true`，在 `saveSubs` 里拒绝写入并提示用户"订阅列表未加载，已暂停保存，请恢复 backups 后重试"。 |
| 测试方案 | 6-T15：写坏 `subs.json` → `loadSubs` → `saveSubs` → 断言不得产生 `items: []` 的覆盖（当前会）。 |
| 置信度 | 高 |

### A4-08（P3）`traffic-history.json` 解析失败被静默忽略，下一次采样直接覆盖（无 `.bak`、无提示）

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `loadHistory`(L3809-3821) / `saveHistory`(L3824-3832) |
| 代码位置 | L3810-3817（读失败或 `json.Unmarshal` 失败一律 `return`，无日志）、L3847（`startHistorySampler` 每分钟必然调 `saveHistory` 的链路 `sampleTrafficOnce`→`appendSample`+`saveHistory`） |
| 描述 | 与 `config_files.go` 的标准（区分 missing/corrupt、保留原文件、上报）不一致：这里是"静默忽略 → 60 秒后覆盖"。 |
| 触发条件 | 文件损坏；或历史版本写入格式变化。 |
| 实际影响 | 流量历史（最多 1 万条）永久丢失，用户无感知。仅统计展示数据，不影响代理功能。 |
| 根因 | 该文件未纳入 `readConfigJSON` 体系。 |
| 修复建议 | 改用 `readConfigJSON` + `preserveCorruptFile`，损坏时改名保留并 dbg 记录。 |
| 测试方案 | 6-T16。 |
| 置信度 | 高 |

### A4-09（P3）`ensureGeodata` 仅按文件大小判断是否需要重写，同尺寸损坏永不修复（与 `EnsureAux` 的哈希判断自相矛盾）

| 字段 | 内容 |
|---|---|
| 文件 | `service.go`、`internal/kernel/manager.go` |
| 函数 | `ensureGeodata`(L568-580) / `EnsureAux`(manager.go:326-342) |
| 代码位置 | service.go:575（`st.Size() == int64(len(data))` 即跳过）、578（`_ = writeFileAtomic(...)`，**错误被丢弃**）；对照 manager.go:332-336 的注释："与 EnsureBinary 一致按哈希比对：等尺寸的损坏/错版文件必须重写，否则 TUN 场景会莫名起不来"。 |
| 描述 | 同一类问题（等尺寸损坏）在 wintun.dll 上已经修好，`geoip.metadb`/`geosite.dat`（十几 MB）仍然只看 size；且写入失败连日志都没有。 |
| 触发条件 | 断电/磁盘错误导致 geo 库内容损坏但长度不变；或用户替换了同尺寸的错版库。 |
| 实际影响 | 地图上的地理位置、`GEOIP,CN` 分流依据静默失真；不报错、不自愈。 |
| 根因 | 未复用 `EnsureAux` 的哈希比对实现。 |
| 修复建议 | 与 `EnsureAux` 统一（`sha256` 比对），并在失败时 `s.dbg`。 |
| 测试方案 | 6-T17。 |
| 置信度 | 高 |

### A4-10（P3）`wintun.dll` 用 `os.WriteFile` 非原子写入

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/manager.go` |
| 函数 | `EnsureAux`(L326-342) |
| 代码位置 | L337（`os.WriteFile(p, data, 0o644)`，OpenFile 的 O_TRUNC 语义：先截断后写） |
| 描述 | 全仓唯一一处对"会被内核加载的二进制"做非原子写。截断与写完之间崩溃 → 磁盘上是 0 字节或半截 DLL。 |
| 触发条件 | `EnsureAux` 执行期间断电/强杀（Boot 早期，L666）。 |
| 实际影响 | 下次启动的哈希比对会重写（L334），所以**能自愈**；但若下次启动时 `EnsureAux` 再次失败（磁盘满/只读），TUN 会在毫无提示的情况下起不来（仅 `m.Logs.Add`）。影响面小，列为 P3。 |
| 根因 | 未使用项目已有的 `writeFileAtomic`。 |
| 修复建议 | 换成 `writeFileAtomic`（`manager` 包内已有 `atomicWriteBytes`，改 perm 复用即可）。 |
| 测试方案 | 6-T18（静态可验证：断言不再出现 `os.WriteFile`）。 |
| 置信度 | 高 |

### A4-11（P3）替换与备份都用 `os.Rename`，但没有目录 fsync / `MOVEFILE_WRITE_THROUGH`，断电后 rename 可能未持久化

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/update.go`、`service.go`、`internal/configgen/gen.go` |
| 函数 | `replaceFileWithRetry`(L797-806) / `copyFileAtomic`(L809-846) / `atomicWriteBytes`(L849-883) / `atomicWriteFile`(service.go:2041-2076) / `configgen.writeFileAtomic`(gen.go:426-457) |
| 代码位置 | 五处都是 `tmp.Sync()` → `tmp.Close()` → `os.Rename()`，**没有任何一处对父目录做 fsync**；Go 的 `os.Rename` 在 Windows 走 `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`，不带 `MOVEFILE_WRITE_THROUGH`。 |
| 描述 | 文件内容已 fsync，但"目录项改名"这一步的元数据不一定已落盘。断电后可能出现：目标文件仍是旧内容（可接受）、或目标目录项丢失（`mihomo.exe` 不存在）。 |
| 触发条件 | rename 成功后极短时间内断电。 |
| 实际影响 | 内核：文件缺失 → `EnsureBinary` 走 L933-938 清标记 + 释放内置 → **可自愈**；`.bak` 丢失 → 本次事务失去回滚能力（但内置内核仍在）。JSON 配置：可能回到上一版本内容（原子写的预期语义），不产生截断。综合影响低，P3。 |
| 根因 | Windows 上缺少"写透"语义的等价实现（可用 `MoveFileEx` + `MOVEFILE_WRITE_THROUGH`，或对目录句柄 `FlushFileBuffers`）。 |
| 修复建议 | 至少在 `installBinary`（内核替换）与 `installBinary` 的 `.bak` 上用带 `MOVEFILE_WRITE_THROUGH` 的替换；其它 JSON 可维持现状并在文档中说明"最坏回退一版"。 |
| 测试方案 | 无法在单测中验证；列入真机断电矩阵（7-T2）。 |
| 置信度 | 静态推断（Windows 语义），需真机验证 |

### A4-12（P3）`UpdateCore` 无服务端互斥，并发调用会互相覆盖备份与目标文件

| 字段 | 内容 |
|---|---|
| 文件 | `service.go`、`internal/kernel/update.go` |
| 函数 | `UpdateCore`(L3407-3514) / `installBinary`(L641-712) |
| 代码位置 | `UpdateCore` 全程不取 `lifeMu`（只在内部 `StopKernel`/`RestartKernel` 各自取），也不存在 `updateMu`；`installBinary` 的备份（L650-657）与替换（L686）之间没有任何跨进程/跨协程保护 |
| 描述 | 两次并发 `UpdateCore`：A 备份旧内核 → A 替换为 new1 → B 备份（此时备份的是 **new1**）→ B 替换为 new2。最终 `.bak` = new1 而非原始内核；若 A 的 `RestartKernel` 因 `ErrAlreadyRunning` 失败（L1446-1448 返回错误），A 会以"新内核启动失败"回滚到 new1，并把记录写成 `RolledBack=true`；实际磁盘上是"new2 被回滚成 new1"，语义混乱。文件本身不会损坏（`Rename` 原子 + `CreateTemp` 唯一），也不会出现 0 字节。 |
| 触发条件 | 两个调用方并发。当前前端按钮在升级期间 `disabled`（SettingsPanel.tsx:642），`grep` 未发现托盘/自动升级入口，所以**正常 UI 无法触发**；风险来自任何能调用绑定的 JS（XSS/调试面板/多窗口）或未来的自动升级。 |
| 实际影响 | 备份语义被破坏（`.bak` 不再是"升级前"版本）、升级记录与实际不符；不产生不可启动状态（内置内核始终可用）。 |
| 根因 | 缺少数秒级的长事务锁（`lifeMu` 只在子步骤内持有）。 |
| 修复建议 | `UpdateCore`/`ResetCore` 共用一个 `coreUpdateMu.TryLock()`，第二次直接返回"正在升级中"。 |
| 测试方案 | 6-T19：并发调用 `InstallCore` 两次，断言 `.bak` 始终是"替换前"的内容。 |
| 置信度 | 高（静态）；可达性：低（需非 UI 调用方） |

### A4-13（P3）`migrateDataDir` 绕过 `dataDir()`/`dataDirOverride`，测试注入目录时迁移不可控

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `migrateDataDir`(L478-513) / `dataDir`(L465-474) / `setDataDirOverride`(L118-122) |
| 代码位置 | L479-483（直接用 `os.UserConfigDir()` + 常量，未走 `dataDir()`） |
| 描述 | 只要 `CoreService.Boot` 被测试调用（当前无测试这么做，见 `grep "\.Boot()" --include=*_test.go` 无命中），迁移就会作用在**真实用户目录**（`%APPDATA%\NexusLink` → `%APPDATA%\RoutePilot`），与 override 无关。另外生产环境没有 `-datadir`，所以当前无用户可见影响。 |
| 触发条件 | 未来任何"用 override 隔离数据目录 + 调 Boot"的测试。 |
| 实际影响 | 测试可能移动/删除开发者本机真实数据目录；行为与 `dataDir()` 的抽象不一致。 |
| 根因 | 迁移逻辑早于 override 抽象，未回填。 |
| 修复建议 | `migrateDataDir` 首行 `if getDataDirOverride() != "" { return }`（或让 dataDir 的 base 统一由一个函数给出）。 |
| 测试方案 | 6-T20。 |
| 置信度 | 高 |

---

## 3.1 升级状态机表（按代码真实阶段，逐格）

阶段定义与代码位置：
S0 空闲 / S1 检查版本（`CheckUpdate` service.go:3412）/ S2 下载（`DownloadCore` update.go:583，**纯内存**，无任何落盘）/ S3 校验解包（update.go:608-629）/ S4 停内核（service.go:3464 → `StopKernel`，先还原系统代理）/ S5 备份（update.go:650-657）/ S6 暂存新内核（update.go:660-683）/ S7 替换（update.go:686-689）/ S8 写标记（update.go:692-700）/ S9 重启验证（service.go:3483 → `RestartKernel`，含 API 就绪探测 manager.go:435-439）/ S10 写记录（service.go:3508）/ S11 结束。

| 阶段 | 崩溃瞬间磁盘实际状态 | 下次启动的恢复行为 | 能否自愈 | 证据 |
|---|---|---|---|---|
| S1 检查 | `bin/mihomo.exe`、`.core_custom`、`.bak` 均不变；无临时文件；系统代理仍开 | 正常启动；`app-state.ProxyOn=true` → `afterStart` 重新接管 | 是 | update.go:100-149 无磁盘写 |
| S2 下载 | 同上（**没有任何临时 zip 落盘**） | 正常启动 | 是 | fetchCoreZip 只写 `bytes.Buffer`（L539） |
| S3 校验 | 同上（摘要/PE 都在内存） | 正常启动 | 是 | L608-629 |
| S4 停内核（崩溃在 `SetSystemProxy(false)` 落盘之后） | 文件不变；`proxy.json` 已删除、`app-state.ProxyOn=false` | 内核正常启动，但**系统代理保持关闭**（用户需手动再开） | 部分（功能自愈，用户意图丢失） | service.go:1496、1908、5378 |
| S4 停内核（崩溃在 `proxy.json` 删除之前） | `proxy.json` 带 `Pending=true` | `recoverPendingProxyRestore` 重试还原，再按 `ProxyOn` 恢复 | 是 | service.go:1295-1318 |
| S5 备份（复制中） | `.mihomo.exe.bak.tmp-XXXX` 孤儿；`mihomo.exe` 完整旧版；`.bak` 是上一版 | `EnsureBinary` 先 `cleanupUpgradeTemps` 删孤儿（L919,989-1005），再用旧版 | 是 | update.go:817-828 |
| S5 备份（已 rename 完成） | `.bak` = 完整旧版；`mihomo.exe` = 旧版 | 同 S0 | 是 | L841 |
| S6 暂存（写/同步中） | `.mihomo-download-XXXX` 孤儿（可能半截）；目标为旧版 | 清理孤儿；用旧版 | 是 | L660-671、L993-996 |
| S7 替换（rename 前后，首次升级，无标记） | 原子：要么全旧，要么全新。若为全新：`mihomo.exe`=新、`.bak`=旧、**无 `.core_custom`** | `EnsureBinary` 无标记 → 哈希与内置不同 → **用内置覆盖**（L949-980） | 是，但**升级被静默丢弃** | L686-689、L941-948 |
| S7 替换（第二次及以后升级，已存在标记） | `mihomo.exe`=新、`.bak`=旧、`.core_custom` 存在 | 保留新内核，直接生效（只是缺升级记录） | 是 | L923-930 |
| S8 写标记（rename 前崩溃） | 同 S7（首次/非首次分别如上），可能残留 `..core_custom.tmp-*` | 同 S7；孤儿由 L995 清理 | 是 | L849-883、L995 |
| S9 重启验证（新内核 PE 合法但起不来） | `mihomo.exe`=坏新内核、`.core_custom` 存在、`.bak`=旧 | `EnsureBinary` 只做 PE 校验 → **采用坏内核** → `mgr.Start` 失败 → `failBoot`，**不尝试 `.bak`，不重放内置** | **否**（仅能手动"回退内置"） | service.go:661、710-713；update.go:781-793 → **A4-02** |
| S9 重启验证（新内核正常，崩溃在 `afterStart` 前） | 新内核 + 标记 + `.bak` | 采用新内核，正常运行 | 是 | service.go:1550-1563 |
| S10 写记录 | 新内核 + 标记 + `.bak`，`core-upgrade.json` 可能缺失或半写（原子写，只会是全旧或全新） | 无影响（该文件无读者） | 是 | update.go:741-747、1.3-3 |
| S11 结束 | 新内核 + 标记 + `.bak`（永久保留） | 正常 | — | — |
| 失败回滚（S9 失败 → `RollbackBinary` 成功） | `.bak` → `mihomo.exe`，标记仍在，`.bak` 保留 | 采用旧内核 | 是 | L3488、L765-777 |
| 失败回滚（S9 失败 → `RollbackBinary` 失败） | 坏新内核 + 标记 + `.bak` | **同 S9 崩溃格：无自愈** | **否** | L3499-3501 → **A4-02** |

**结论**：整个状态机只有一条"不可自愈"的通道，即"`mihomo.exe` 是 PE 合法但无法启动的内核 + `.core_custom` 存在"，它由 S9 崩溃 / 回滚失败两条路进入，且没有启动期兜底（A4-02）。其余所有断电点要么完全无损，要么退化为"用内置内核覆盖"（功能性自愈，但会静默丢弃已下载的升级）。

---

## 3.2 落盘写入点清单

| # | 路径（相对数据目录） | 写入函数（位置） | 原子 | `.bak` | 损坏后行为 |
|---|---|---|---|---|---|
| 1 | `bin/mihomo.exe` | `installBinary`(update.go:675-689) / `EnsureBinary`(L965-980) / `restoreFromBackup`(L773) | 是（tmp+Sync+rename） | 配对 `.bak`（升级时） | `EnsureBinary` 做 PE 校验；无效/缺失 → 清标记 + 释放内置（可自愈）；PE 合法但不可启动 → **无自愈**（A4-02） |
| 2 | `bin/mihomo.exe.bak` | `copyFileAtomic`(update.go:809-846) | 是 | — | `restoreFromBackup` 先 `validatePE`，坏则拒绝回滚并保留文件（L770-772） |
| 3 | `bin/.core_custom` | `markCustom`→`atomicWriteBytes`(L438-441) | 是 | 无 | 缺失 = 视为内置版 → 升级被覆盖（S7）；存在但 exe 不可用 → failBoot |
| 4 | `core-upgrade.json` | `SaveUpgradeRecord`(L741-747) | 是 | 无 | 原子写；**无读者**，损坏不影响运行 |
| 5 | `config.yaml` | `configgen.Write`(gen.go:407-422) | 是 | 无 | 损坏 → 内核拒绝启动 → `failBoot`；下次任一次 `StartKernel` 会用内存 options 重生成（可自愈） |
| 6 | `app-state.json` | `saveAppStateErr`(service.go:5444) | 是 | `configCorrupt` 时**重命名**为 `app-state.json.corrupt-<ts>`（config_files.go:52-68） | 原文件保留 + 启动横幅提示；内存回落默认值，**后续保存会用默认值覆盖活动状态**（TUN/WinHTTP 恢复线索失效，但 `.corrupt-*` 可人工恢复）→ A4-04/A4-07 同族 |
| 7 | `proxy.json` | `persistProxyState`(L2031-2037) / `removeProxyState`(L1899) | 是 | 无（但协议是 Pending 标记而非覆盖） | `readProxyState` 损坏 → 保留原文件 + `setBootErr` + `restoreProxy=false`（L1296-1302），**不覆盖** |
| 8 | `subs.json` | `saveSubs`(L4167-4179) | 是 | 损坏时重命名为 `.corrupt-*` | 原文件保留，但内存为空 → 下一次保存写空列表（A4-07） |
| 9 | `subscription.yaml`（旧布局/当前订阅镜像） | `applySubscriptionExpect`(L4590) | 是 | 无 | 解析失败 → 不加载订阅（内核起空配置），刷新可修 |
| 10 | `sub-<sha256(URL)[:8]>.yaml` | `writeSubscriptionFiles`(L4091) | 是 | 无 | 损坏 → `SwitchSubscription` 报"订阅配置损坏"；刷新可修 |
| 11 | `sub-<hash>.provider.yaml` | `writeSubscriptionFiles`(L4097) / 删除(L4102) | 是 | 无 | 同上 |
| 12 | `subscription.json`（meta） | `persistSubMeta`(L5482) | 是 | 损坏时重命名 | 仅元信息，丢失后可重新拉取 |
| 13 | `process-rules.json` / `process-proxy.yaml` / `process-direct.yaml`（legacy） | `writeLegacyProcSnapshot`(procgen.go:139-151) | 是 | 无 | 仅用于降级兼容；主路径不读 |
| 14 | `procgen/{proxy,direct,rules}-<gen>.*` + `current.json`(+`.prev`) | `publishProcRules`(procgen.go:65-136) | 是（manifest 最后提交） | `.prev` manifest | **三级回退**：current 哈希校验 → prev → legacy 平铺文件；manifest 路径穿越已校验（procgen.go:225-229）。全仓最健壮的一处 |
| 15 | `traffic-history.json` | `saveHistory`(L3824-3832) | 是 | 无 | 解析失败静默忽略 → 60s 后被覆盖（A4-08） |
| 16 | `site-rules.json` | `persistSiteRules`(L5114-5120) | 是 | 损坏时重命名 | 保留 + 提示；规则回落为空（用户需重配，`.corrupt-*` 可手工恢复） |
| 17 | `process-ignored.json` | `IgnoreProcess`(L5161) / `ImportConfig`(L2798) | 是 | 损坏时重命名 | 保留 + 提示 |
| 18 | `wintun.dll`（数据目录 + `bin/`） | `EnsureAux`(manager.go:337) | **否**（`os.WriteFile`） | 无 | 下次启动哈希比对重写（可自愈）→ A4-10 |
| 19 | `geoip.metadb` / `geosite.dat` | `ensureGeodata`(L578) | 是 | 无 | **只比 size**，等尺寸损坏不修复；写失败被丢弃 → A4-09 |
| 20 | `debug.log` / `kernel.log`(+`.1`) | `dbg`(L5365) / `openKernelLog`(manager.go:174) / `rotateLocked`(L218-234) | 追加写 | 轮转 `.1` | 只增日志，损坏不影响功能 |
| 21 | `*.corrupt-<ts>` | `preserveCorruptFile`(config_files.go:52-68) | rename（首选）/ 复制（兜底） | — | 恢复入口，不自动清理 |

---

## 3.3 并发写同一文件的可能路径（谁和谁会撞）

| 文件 | 写者 A | 写者 B | 撞的机制 | 后果 | 是否已有防护 |
|---|---|---|---|---|---|
| `app-state.json` | `SaveWindowBounds`(L5448) / `ToggleFavorite`(L3206) | `takeOverWinHTTP`(L1979) / `setSystemProxyLocked`(L1715) / `retryPendingRestoreLocked`(L2186) | 整文件覆盖，快照→落盘之间有 IO 窗口 | 丢更新（`winHTTPPending`/`tunPrevRegistry` 被旧快照覆盖）→ A4-04 | 无 |
| `subs.json` | `Subscribe`→`applySubscriptionExpect`(subsMu 内) | `SetSubInsecure`(L4293) / `ImportConfig`(L2787) / 自动更新 | 同为整文件覆盖；`SetSubInsecure` 不取 `subsMu` | 新增订阅在磁盘上消失（内存有、磁盘无） | 部分（`saveSubs` 在锁内拷贝，但写盘无序） |
| `bin/mihomo.exe` / `.bak` | `UpdateCore` A | `UpdateCore` B / `ResetCore` | 无长事务锁 | `.bak` 语义错乱、回滚到错误版本 → A4-12 / A4-06 | 无（仅 UI disabled） |
| `bin/mihomo.exe`（读-用） | 替换（rename） | 内核进程（`exec.Command(exe)`） | Windows 文件句柄 | rename 失败 → 4 次重试共 1.5s → 报错且旧内核保留（实测 1.53s） | 有（`replaceFileWithRetry`） |
| `config.yaml` | `StartKernel`(L1456) / `restartKernel`(L1546) | `applyGeneratedConfig`(L5806) / `ImportConfig` | 原子替换，只丢内容一致性 | 内核可能读到 A 的内容而 A/B 端口不同（端口仅 Boot 时变，风险低） | 原子写 |
| `procgen/*` | `publishProcRules` | 同上 | `procGenMu` 串行 + manifest 提交点 | 无 | 有 |
| `traffic-history.json` | 采样协程（每分钟） | — | 单写者 | 无 | 单写者 + `histMu` |
| `proxy.json` | `setSystemProxyLocked`（持 `proxyMu`） | `restoreProxyTransactionLocked`（持 `proxyMu`） | `proxyMu` 串行 | 无 | 有 |
| `debug.log` | 全局 `dbg` | 全局 `dbg` | O_APPEND 单行写 | 行可能交错（<2MB 的 Truncate 会与 append 竞争，L5370-5373） | 弱（可接受） |

---

## 3.4 必须补的故障注入测试清单

> "断电"统一用"在注入点返回/panic 模拟进程立即退出（不做任何 defer 清理）"的等价做法；对文件系统层面则直接构造"崩溃后应有的磁盘状态"，再跑启动恢复路径。
> 当前 `DownloadCore` 的 URL 白名单（https + 固定域名，update.go:464-477）**不可注入**，这是"摘要不匹配/大小不符"两条分支完全没有测试的根本原因——建议先加 `var downloadURLValidator = validateDownloadURL` 这类 seam，测试才可写。

| 编号 | 注入点 | 期望 | 断言要点 |
|---|---|---|---|
| T1 | `fetchCoreZip` 返回合法 zip，但 `wantDigest` 故意写错 | 不替换、不写标记、不产生 `.bak` | `mihomo.exe` 字节不变；`IsCustomCore(dir)==false`；无 `bin/.mihomo-download-*` 残留；错误串含"摘要不匹配" |
| T2 | `wantSize` 与响应长度差 1 | 同上 | 错误串含"大小不符"；磁盘零改动（需要 T5 的 seam 才能走 `DownloadCore`，否则直接测 `fetchCoreZip`+断言逻辑） |
| T3 | `markCustom` 注入失败（新增 `var markCustomFn`） | 已替换的内核被回滚 | `mihomo.exe` == 旧字节；返回错误含"已回滚"；`.bak` 仍存在；`IsCustomCore==false` |
| T4 | `restoreFromBackup` 的目标被独占句柄占用（复用 `windows.CreateFile` 手法，upgrade_test.go:118-132） | 回滚失败但 `.bak` 保留 | 返回错误含"回滚失败"；`.bak` 内容 == 旧内核；不出现 0 字节 `mihomo.exe` |
| T5 | 新增 `downloadURLValidator` seam，喂 `https://gh-proxy.com/https://127.0.0.1:1/x.zip` | **当前应通过**（证明 A4-01），修复后应被拒 | 修复后 `validateDownloadURL` 返回错误；`u.Path` 以 `http` 开头即拒绝 |
| T6 | `fetchCoreZip` 的重定向目标为 `http://github.com/...` / `https://evil.example/x` | 拒绝 | 错误来自 `CheckRedirect`（update.go:509-514） |
| T7 | `replaceFileWithRetry` 的目标被占用 | 4 次重试后失败、旧内核完好 | 已有 `TestInstallBinaryRenameFailureKeepsOld` 覆盖（实测 1.53s）；补断言 `.mihomo-download-*` 无残留 |
| T8 | 手工构造 5 类孤儿（`.mihomo-download-*`、`.mihomo-embed-*`、`.mihomo.exe.bak.tmp-*`、`.mihomo.exe.tmp-*`、`..core_custom.tmp-*`）+ 正式文件 + `.bak`，调 `EnsureBinary` | 只删孤儿 | 正式文件与 `.bak` 字节不变；孤儿计数为 0 |
| T9 | 断电等价：`installBinary` 在 rename 成功、`markCustom` 之前"直接退出"（注入点）→ 新进程调 `EnsureBinary(embedded)` | 采用内置（升级丢失），**不 panic、不空窗** | `mihomo.exe` == embedded；`IsCustomCore==false`；无 `mihomo.exe.bak` 被误删 |
| T10 | 断电等价：`markCustom` 完成后退出 → 新进程 `EnsureBinary(embedded)` | 保留自定义内核 | `mihomo.exe` == 新内核；`IsCustomCore==true` |
| T11 | 注入 `restart` 返回 `ErrPortInUse`（A4-03） | **不回滚**，记录 `RolledBack=false` | `mihomo.exe` == 新内核；`core-upgrade.json.RolledBack==false`；错误信息提示"下次启动生效" |
| T12 | `appStateWriter` 注入慢写（先 `time.Sleep` 再真正写），并发 `SaveWindowBounds` + `takeOverWinHTTP` | 最终文件包含 `winHTTPPending=true` 与 `winHTTPBackup` | 反序列化后两字段非零；无 `app-state.json.tmp-*` 残留 |
| T13 | `subs.json` 含 URL-B（无 `sub-B.yaml`），`subscription.yaml` 是 URL-A 的内容 | 不得用 A 的内容冒充 B | `sub-<hash(B)>.yaml` 不存在；`s.subURL != B` 或返回"订阅不可用"；`ListSubscriptions().active` 在范围内 |
| T14 | `StopKernel` 注入失败 → `ResetCore` | 中止，不清标记 | `IsCustomCore==true`；`mihomo.exe` 不变 |
| T15 | 写坏 `subs.json` → `loadSubs` → `saveSubs` | 不得写空列表 | 磁盘仍是 `.corrupt-*` + 原文件不存在（或修复后：拒写并提示） |
| T16 | 写坏 `traffic-history.json` → `loadHistory` → `saveHistory` | 修复后：保留 `.corrupt-*` 并提示 | 文件存在 `.corrupt-*`；`debug.log` 有记录 |
| T17 | 把 `geoip.metadb` 写成同尺寸垃圾 → `ensureGeodata` | 修复后：按哈希重写 | 修复后文件 sha256 == 内置数据 |
| T18 | 静态断言 | `EnsureAux` 不再使用 `os.WriteFile` | `grep` 级检查 + 目标被占用时的行为不变 |
| T19 | 两个 goroutine 并发 `InstallCore`（不经过网络） | `.bak` 始终是"替换前"的版本；目标为完整 PE | 无 0 字节文件；最终 `mihomo.exe` 长度 == 其中一个输入 |
| T20 | 设置 `dataDirOverride` 后调用 `migrateDataDir` | 不做任何迁移 | 真实 `%APPDATA%\RoutePilot` 未被创建/未被移动（需在修复后断言；当前会迁移） |

---

## 4. 已排除的疑点（逐个说明依据）

| 疑点 | 结论与依据 |
|---|---|
| 下载内容是否"先落盘再用"、校验对象是否落盘文件 | **无窗口**。`DownloadCore` 全程在内存：`fetchCoreZip` 写 `bytes.Buffer`（update.go:539-561），摘要对 `data` 计算（L615-616），随后 `extractMihomo(data)`（L622）解包同一份字节，再到 `installBinary` 才落盘。不存在"校验 A 文件、使用 B 文件"。 |
| 摘要校验失败是否删除临时文件 | **无需删除**：下载阶段没有临时文件。安装阶段任何失败都由 `defer` 删除 `tmpPath`（update.go:666-671），成功后 `replaced=true` 不再删（此时文件已被 rename 走）。 |
| 是否可绕过 https/域名限制（`file://`、http、userinfo 欺骗、后缀欺骗） | 单条 URL 层面**已封堵**：scheme 必须 `https`（L469），`u.Hostname()` 精确匹配白名单（L472-475），`https://github.com@evil.com` 解析出 `evil.com` 被拒，`github.com.evil.com` 不被 map 命中，空 host 不被命中（fail-closed）。重定向经 `CheckRedirect` 再校验（L509-514）。**唯一绕过是"嵌套中转 URL"**，已单列为 A4-01。 |
| 是否存在"新 exe 已写、旧 exe 已删"的半替换状态 | **不可能**。C3 已修：替换前不再 `os.Remove(target)`，直接 `Rename` 覆盖（update.go:686；注释 L974-975 说明原因）。崩溃只可能落在"全旧"或"全新"。 |
| 备份是否在替换前完成并 fsync | **是**。顺序为 `copyFileAtomic`（tmp → `Sync` L835 → `Close` → `Rename` L841）在前（L650-657），新内核写入在后（L660-683）。目录未 fsync 已单列 A4-11。 |
| 临时文件名是否唯一 | **唯一**。全部使用 `os.CreateTemp`（update.go:660/817/854，service.go:2046，gen.go:428），内核名冲突在文件系统层面不可能；`writeProcGenManifestLocked` 的固定名 `current.json.next` 由 `procGenMu` 串行保护（procgen.go:66,161）。 |
| 订阅文件名是否可路径穿越 | **不可**。`RawFileName` = `sub-` + `sha256(rawURL)[:8]` 十六进制（subscription/fetch.go:258-261），与 URL 内容完全无关；`ProviderFileName` 同理（L264-266）。 |
| procgen manifest 是否可指向目录外 | **不可**。`readProcGenManifest` 对三个文件名做 `filepath.Base(name) == name` 且拒绝 `/\` 与 `.`（procgen.go:225-229）。 |
| 配置 JSON 损坏是否会被直接覆盖成默认值 | **多数不会**。`readConfigJSON` 区分 missing/empty/corrupt/unsupported（config_files.go:31-47），`reportConfigProblem` 先把文件**重命名**为 `.corrupt-<ts>` 再继续（L86），因此原数据可人工恢复；`app-state`/`subs`/`site-rules`/`process-ignored`/`subscription.json` 都走这条路径。例外：`traffic-history.json`（A4-08）与"损坏后内存为空、下一次保存写空/默认值"（A4-04/A4-07 的次级效应）。 |
| `proxy.json` 损坏是否会被覆盖 | **不会**。`readProxyState` 返回错误时上层只记 `setBootErr` 并放弃恢复（service.go:1296-1302），文件保留。 |
| `ResetCore` 是否清空范围过大 | **范围正确**：只删 `bin/mihomo.exe` 与 `.core_custom`（L3522-3523），不动 `config.yaml`、订阅、规则、`.bak`。问题只在顺序与错误处理（A4-06）。 |
| 数据目录迁移是否会覆盖已有新目录 | **不会**。`os.Stat(newDir)` 成功即返回（service.go:485-487）；复制失败会 `RemoveAll(newDir)` 并保留旧目录（L503-509）。`copyDirAll` 失败会返回 error（L554-562，C4 已修）。 |
| 并发两次升级是否会产生 0 字节/损坏内核 | **不会**。`Rename` 原子 + 唯一临时名；最坏结果是 `.bak` 语义错乱（A4-12），不产生不可启动状态。 |
| SemVer 版本比较是否仍会误判降级 | **已修且覆盖充分**。`parseSemver`/`compareSemver` 严格实现（update.go:241-423），`CheckUpdate` 用 `cmp > 0` 且解析失败时保守不提示（L208-214），`c5_semver_test.go` / `c5_1_semver_test.go` 共 40+ 用例覆盖预发布、build metadata、前导零、溢出。本报告不重复提出。 |
| 升级期间是否会让用户断网 | **设计正确**：先下载（不断网）再停内核（service.go:3431-3466），失败会 `RestartKernel` 拉回（L3471）。 |
| watchdog 是否会在替换期间把旧 exe 拉起来 | 不会主动升级冲突：`StopKernel` 末尾 `pauseWatchdog`（L1511），`RestartKernel` 成功后 `resumeWatchdog`（L1561）。若外部（用户点"连接"）在 `installBinary` 期间启动内核，`Rename` 会失败但旧内核完好（实测 1.53s 后报错），不产生空窗。 |

---

## 5. 未覆盖部分 + 需真机验证项

### 5.1 本次静态审查无法覆盖

1. **真实 Windows 文件占用语义**：`os.Rename` 覆盖"正在被运行的 exe"、被 Defender/索引器/备份软件短暂持有时的行为，只能靠 `TestInstallBinaryRenameFailureKeepsOld`（1.53s 失败）间接印证；杀软锁文件的时长分布无法在 CI 复现。
2. **真实断电**：`MOVEFILE_WRITE_THROUGH`/目录 fsync 的实际影响，需真机断电矩阵（A4-11）。
3. **真实 GitHub release 的 digest 字段**：`fetchLatestFrom` 依赖 `assets[].digest` 存在（L65），若上游改字段名则升级全部失败（fail-closed，不会不安全，但功能会静默失效）。本次禁止外网请求，未验证。
4. **`gh-proxy.com` 的实际行为**：它是否会把 `https://gh-proxy.com/https://<host>` 转发到任意 host（A4-01 的前提）。按该服务的公开语义推断为是，未实测。
5. **UI 全流程**：升级中的进度显示、失败提示、`bootErr` 横幅在"内核起不来"时的可见性（决定 A4-02 的用户可自救程度）。

### 5.2 需真机验证清单

| 编号 | 场景 | 验证方法 | 预期 |
|---|---|---|---|
| V1 | 升级过程中强杀进程（任务管理器结束 RoutePilot） | 分别在"下载中/替换中/重启验证中"三个时点结束，检查 `%APPDATA%\RoutePilot\bin` 内容与下次启动 | 无 0 字节 `mihomo.exe`；能启动 |
| V2 | 断电（或 `powercfg` 模拟）后 `bin` 目录一致性 | 替换后立即断电，重启检查 `mihomo.exe`/`.bak`/`.core_custom` | 三者中至少 `mihomo.exe` 与 `mihomo.exe.bak` 之一可用 |
| V3 | 杀软锁定 `mihomo.exe` 时升级 | 用 Defender 实时扫描大文件制造占用 | 升级失败但旧内核仍可启动，错误可读 |
| V4 | 磁盘写满（<100MB 可用）时升级 | 限制分区剩余空间 | 明确报错，不破坏现有内核（当前会在 `CreateTemp`/`Write` 失败并保留旧版） |
| V5 | 导入他到导出配置后首次启动 | 干净机器导入含 2 条订阅的 bundle | 不得出现"地址是 B、节点是 A"（当前会，A4-05） |
| V6 | 内核起不来时的自救路径 | 手工放一个 PE 头合法但内容为随机的 `mihomo.exe` + `.core_custom` + 正确 `.bak` | 当前：永久 failBoot；修复后：自动回滚（A4-02） |

---

## 6. 附：等级统计

| 等级 | 条数 | 编号 |
|---|---|---|
| P0 | 0 | — |
| P1 | 1 | A4-01 |
| P2 | 4 | A4-02、A4-03、A4-04、A4-05 |
| P3 | 8 | A4-06、A4-07、A4-08、A4-09、A4-10、A4-11、A4-12、A4-13 |

**一句话结论**：升级事务的"替换"环节（备份→临时文件→Sync→Rename→标记）已经做到原子且历史缺陷（先删后改名）确已修复；真正的剩余风险集中在**信任链的上游（镜像 JSON 与摘要同源）**和**启动期缺少"上次升级未完成"的持久化状态机**——前者可导致任意代码执行，后者会让"PE 合法但起不来"的内核变成只能手工解救的僵局。
