# RoutePilot 统一审计报告（第一阶段 · FINAL-AUDIT）

- 项目：RoutePilot（Go + Wails v3 Windows 桌面代理客户端，内置 mihomo）
- 审计基线：`39ca02c1aa9303fc14fe8264d2715432cd50f030`（`gofmt: 对齐测试表字段`，工作区干净）
- 审计方式：6 个子 Agent 并行只读审查 + 主 Agent 独立复核（逐条回读代码 / 实跑基线命令 / 一次受控外网探测）
- 本阶段**未修改任何源码、测试、配置**；唯一写入为 `audit/*.md`
- 报告日期：2026-11-28

---

## 1. 基线验证（主 Agent 实跑，非引用）

| 命令 | 结果 | 备注 |
|---|---|---|
| `go build ./...` | 通过 | 20s |
| `go vet ./...` | 通过 | 无输出 |
| `go test -count=1 ./...` | 全绿 | routepilot 4.47s / kernel 12.67s / subscription 3.86s |
| `go test -count=1 -tags repro ./...` | 全绿 | kernel 14.16s |
| `go test -race -count=1 ./...` | 全绿 | kernel 13.35s |
| `cd frontend && npm run lint`（= `tsc --noEmit`） | 通过 | 11s |
| `cd frontend && npm run build` | 通过 | `dist/assets/index-CwTU2sY_.js` 300.95 kB |
| `go build -o bin/RoutePilot.exe .` | 通过 | 96.8 MB（**无 `-tags production`**，见 RP-017） |

**基线结论：当前主分支构建可用、测试全绿。但"绿"不等于"对"——本轮发现的问题没有一条是靠现有测试能发现的（见 RP-024/RP-025/RP-027）。**

---

## 2. 结论摘要

| 指标 | 数量 |
|---|---|
| **P0** | **0** |
| **P1** | **2** |
| **P2** | **25** |
| **P3** | **46** |
| **真实缺陷合计** | **73** |
| 完全误报 | 0 |
| 需修正表述 / 降级 | 5（见 §8） |
| 重复条目合并 | 4 组（见 §8） |
| 本轮已修复 | **0**（第一阶段只审查） |
| 未解决 | **73**（全部待第二阶段） |

**一句话结论**：这不是一个"质量很差"的项目——恢复事务、TUN 快照、SSRF 防线、内核子进程测试的完成度明显高于同类个人项目；问题集中在三个地方：**（1）接管路径与恢复路径的事务协议不对称**（恢复是"先落盘后写注册表"，接管却是反的）；**（2）信任链上游**（升级摘要与下载地址同源于第三方镜像）；**（3）"下载到的内容能指挥内核干什么"完全没设防**（订阅 provider 出站）。其余大量条目是"状态标签语义"与"测试/CI 缺口"，不构成发布阻断。

---

## 3. 最优先处理的 5 个问题

| 排序 | 编号 | 一句话 | 为什么优先 |
|---|---|---|---|
| 1 | **RP-001** | 接管系统代理时**先改注册表、后落盘快照**，崩溃即永久失去还原依据 | 唯一能造成"开机断网 + 用户原始配置无法还原 + 无任何本地痕迹"的路径；修复成本低（复用恢复路径已有的 Pending 协议） |
| 2 | **RP-002** | 升级把"第三方镜像返回的 JSON"当信任根，摘要与下载地址同源 | 摘要校验在此路径上只保证传输完整，不保证来源真实；后果是管理员权限下执行任意 PE |
| 3 | **RP-015** | 订阅里的 `proxy-providers` / `rule-providers` / `geox-url` 原样进内核配置 | 应用侧 SSRF 防线做得很扎实，但内核侧完全绕过它；订阅（或 MITM 后的 http 订阅）可指挥内核打内网/云元数据 |
| 4 | **RP-011** | `app-state.json` 整文件覆盖 + 多入口无串行化，存在丢更新窗口 | 丢的恰好是 `winHTTPPending` / TUN 快照这类"机器级恢复依据"，丢失后果等同 RP-001 |
| 5 | **RP-008** | 看门狗已触发的重启无法被用户操作撤销（停止后被拉起、启动后被二次重启） | 用户可感知（连接被无故重置、状态抖动），且是生命周期状态机里唯一"人工意图被覆盖"的洞 |

---

## 4. P1 问题（2 条，详述）

### RP-001 · P1 · 系统代理/WinHTTP 接管是"先改注册表、后落盘快照"，崩溃或落盘失败即永久失去还原依据

| 字段 | 内容 |
|---|---|
| 文件 | `service.go`；`internal/sysproxy/sysproxy_windows.go` |
| 函数 | `setSystemProxyLocked` / `takeOverWinHTTP` |
| 代码位置 | 接管序列：`captureProxySnapshot`（L1581，仅内存）→ `ops.Enable(addr)`（**L1689 写 HKCU**）→ `ClearAutoConfigURL`（L1696）→ `persistProxyState`（**L1707 才落盘**）；WinHTTP：`WinHTTPRaw`（L1965，仅内存）→ `EnableWinHTTP`（**L1970 改 HKLM**）→ `saveAppState`（L1979，失败仅 `dbg`） |
| 对照 | 恢复事务是正确顺序：先写 `Pending=true` 落盘（L1879-1887）→ 再写注册表（L1890）→ 回读校验（L1894）→ 删备份（L1899）。TUN 前置也正确：`saveAppStateErr`（L2266）成功后才 `Disable()`（L2282） |
| 问题描述 | 系统代理的"接管前原始配置"在被覆盖前只存在于内存；`proxy.json` 在注册表已被改写之后才写。WinHTTP 的原始二进制值同样只在内存，且在 `netsh` 已改 HKLM 之后才随 `app-state.json` 落盘，落盘错误被 `saveAppState`（L5378）吞掉。 |
| 触发条件 | (a) 在 L1689 与 L1707 之间断电 / 进程被杀（毫秒级窗口）；(b) 磁盘满或 `%APPDATA%` 不可写时开启系统代理（管理员场景，`appStateWriter` 失败）。 |
| 实际影响 | (a) 注册表停在 `ProxyEnable=1 / ProxyServer=127.0.0.1:<port>`，`proxy.json` 不存在。下次启动：`recoverPendingProxyRestore`（L1304 `!st.Saved` 直接返回）与 `reconcileProxyLocked`（L2117 同样返回）都不会处理；`restoreProxy` 取自 app-state（仍为 false）→ **浏览器与全部 WinINET 程序在下次启动前完全断网**；端口若在下次启动漂移（`pickPort`），指向的是死端口，断网持续。(b) HKLM `WinHttpSettings` 永久指向死端口，**即使卸载本程序也不会恢复**，破坏 Windows Update / 商店 / 依赖 WinHTTP 的服务，且 `winHTTPPending` 从未置位，后续再无还原依据。 |
| 根因 | 接管路径没有复用恢复路径已验证的"Pending 先落盘"协议；WinHTTP 甚至没有独立的备份文件，只寄生在整文件覆盖的 app-state 里。 |
| 修复建议 | 接管前先以 `Pending=true` 落盘"预备份"，`Enable` 成功后再改写为 `Pending=false`；落盘失败即拒绝接管并返回错误（对齐 TUN 的 `errTUNSnapshotSaveFailed`）。WinHTTP 改为独立文件（或 app-state 内独立 revision），`saveAppStateErr` 失败则**不执行** `netsh`；启动时把"`Pending=true` 且注册表已有我方特征"也纳入恢复判定。 |
| 测试方案 | ① `enableHook` 内断言此时 `proxy.json` 已存在且 `Pending=true`（现状必失败）；② hook 后重建服务实例模拟崩溃，断言能还原用户原值；③ `appStateWriter` 注入 ENOSPC，断言 `EnableWinHTTP` 未被调用且 `SetSystemProxy(true)` 返回错误；④ 成功后断言 `proxy.json` 为 `Saved=true, Pending=false`。 |
| 置信度 | 高（顺序是代码事实；(a) 需命中毫秒级崩溃窗口，(b) 为确定性） |

### RP-002 · P1 · 升级信任根是第三方镜像返回的 JSON：摘要与下载地址同源，摘要无法证明来源真实

| 字段 | 内容 |
|---|---|
| 文件 | `internal/kernel/update.go` |
| 函数 | `repoAPIEndpoints` / `fetchLatestFrom` / `downloadCandidates` / `validateDownloadURL` / `DownloadCore` |
| 代码位置 | 镜像入口 L45；注释中的安全论断 L40-42；白名单含 `gh-proxy.com` L458；`downloadCandidates` 对 `Host == gh-proxy.com` 原样返回 L489-490；`validateDownloadURL` 只看 `u.Hostname()` L472-475；摘要比对 L615-620 |
| 问题描述 | 代码注释明确写"摘要即便来自镜像也不降低安全性……镜像既改不了产物也伪造不出匹配的摘要"。**该论断不成立**：`digest` 与 `browser_download_url` 来自**同一份 JSON**，而这份 JSON 可以由 `https://gh-proxy.com/https://api.github.com/...` 提供。镜像（或其 TLS 中间人）可以同时给出"自己托管在 github.com 的恶意 release 资产 URL + 与之匹配的摘要"，大小/PE/摘要三重校验会全部通过。 |
| 触发条件 | ① `api.github.com` 不可达（国内常见 403/超时）→ `fetchLatest` 落到镜像；② 镜像返回伪造 JSON（`tag_name` 更高、URL 指向攻击者仓库资产、`digest` 匹配）；③ 用户点"升级内核"。 |
| 实际影响 | 以当前进程权限（TUN 场景为管理员）替换并启动任意 PE，且 `.core_custom` 保证下次启动不回退。摘要机制在此路径上只保证"传输未损坏"。 |
| 根因 | 把"摘要"当成了独立于来源的锚点，实际上锚点与来源同源；白名单校验的是 URL 字符串的 host，而不是"内容由谁生产"。 |
| 修复建议 | (a) 把期望摘要内置进二进制或对 release 做签名校验（minisign/cosign），使摘要不依赖运行时 JSON；(b) 镜像只允许用于**已按 github 原始 URL 校验过**的产物下载，不再作为 JSON 入口；(c) `validateDownloadURL` additionally 拒绝 `u.Path` 以 `http://`/`https://` 开头的嵌套形态（纵深防御）。 |
| 测试方案 | 构造 `browser_download_url=https://gh-proxy.com/https://<任意主机>/x.zip` 的 JSON，断言 `validateDownloadURL` 拒绝（当前会通过）；用 `httptest` 返回"坏字节 + 与之匹配的假摘要"，断言安装被拒（需先加 `downloadURLValidator` 之类 seam）。 |
| 置信度 | 代码事实：高；可利用性：中（需镜像被控或被信任 CA 中间人） |
| 主控实测修正 | 我实测 `https://gh-proxy.com/https://example.com/` → **HTTP 404**（返回的是镜像自己的错误页），因此"可下载任意主机"这一子判断**不成立**；且即使转发，请求也由镜像服务器发出，**不构成对用户本机的 SSRF**。本条只保留"信任根与摘要同源"这一实质问题，不再声称本机 SSRF / 任意主机下载。 |

---

## 5. P2 问题（25 条）

> `✔` = 主 Agent 亲自回读代码确认；其余为主 Agent 抽查子 Agent 行号后采信（行号均落在基线版本）。

| 编号 | 级别 | 位置（文件:行） | 问题 | 实际影响 | 修复要点 | 置信度 |
|---|---|---|---|---|---|---|
| RP-003 | P2 | `service.go:2366,2389,2395,2401` | TUN 切换失败的回滚分支在 `proxyMu` 之外写注册表，且 `tunChanging` 已提前置 false ✔ | 可与用户开关/`Shutdown` 交错：用户刚开的代理被静默抹掉（`proxyOn` 仍 true）；退出时可能把接管值写回，留下死端口 | 整个失败分支包进 `withProxyTx`；回滚落地后再清 `tunChanging`；加 `IsShuttingDown` 判断 | 高 |
| RP-004 | P2 | `service.go:2239-2243,2257-2260` | TUN 前置快照在锁外读取，进入事务后不复核 | 并发开代理时会把"我方端口"记成"用户原始配置"，关 TUN 后凭空打开一个用户从未设过的代理 | 进 `withProxyTx` 后再读 `proxyOn/backup`；`captureProxySnapshot` 结果为自身端口时按"我方接管"处理 | 高 |
| RP-005 | P2 | `service.go:1899-1908,1736-1746` | 恢复成功但删 `proxy.json` 失败时直接返回，内存 `proxyOn` 未回滚 | 用户关不掉开关（`bk.Pending` 拒绝再次开启）；端口漂移时 `updateSystemProxyPort` 会把代理重新写回 | 删失败时置 `proxyOn=false`，端口校正依据改为 `!backup.Pending && proxyOn` | 高 |
| RP-006 | P2 | `service.go:1948-1980,1719`；`sysproxy_windows.go:268-275` | WinHTTP 接管/还原的所有错误只写 `dbg`，主流程仍返回 nil；失败时不置 `winHTTPPending`，无重试依据 | UI 显示"系统代理已开启"，用户以为服务流量也走代理，实际直连；排障无任何可见线索 | `takeOverWinHTTP` 返回状态；失败时 `appendBootErr` + 前端事件；非管理员时明确提示 | 高 |
| RP-007 | P2 | `service.go:1521-1565,2915-2932` | 内核崩溃到看门狗放弃之间，系统代理始终指向无监听的端口 | 全机断网约 2.3 分钟（2+4+8+16+32 退避 + 每次 15s 校验）；若客户端进程被杀且不再启动则无上界 | `mgr.OnExit` 里立即还原系统代理，重启成功后再按 `restoreProxy` 重新接管 | 高 |
| RP-008 | P2 | `internal/kernel/watchdog.go:147-172`；`service.go:1525-1527,1543-1553,1464-1468,1507-1511` | `fire` 通过门禁后即置 `restarting` 并调用 `restart()`；`restartKernel` 只检查 quitting，不看 watchdog 是否已被 Pause | 崩溃后 2s 内点"启动"，启动完成后被 `mgr.Restart` 再杀一次；点"停止"后可能被拉起 | 引入 `intentEpoch`，`restartKernel` 拿到 `lifeMu` 后比对 epoch；或在 `Pause/Stop` 时作废在途重启 | 高 |
| RP-009 | P2 | `service.go:710-718,2940` | `startKernelWatchdog`（及自动更新/采样/健康检查）只在 Boot 成功路径末尾调用；Boot 任一步失败即 return | 该会话内内核崩溃无人自动恢复，`kernel-crashed` 事件也不会发；后台能力整体降级且无提示 | 把后台任务安装提到 `mgr.Start` 之前，或在 `StartKernel` 里惰性安装（幂等） | 高 |
| RP-010 | P2 | `internal/kernel/manager.go:430`；`safego.go:43-51` | `waitProcess` 的 panic 上报传 `nil`，panic 被静默吞掉，且收尾（`close(r.exited)`/`close(r.done)`/清 `run`）没有 defer ✔ | 一次 panic 即把内核管理砖化：后续 Start 永远"已在运行"、Stop 超时、看门狗收不到退出事件、日志无痕迹 | 传真实 reporter；收尾逻辑改 defer；`Start` 区分"僵尸 run" | 高（机制）/低（触发概率） |
| RP-011 | P2 | `service.go:5390-5445`；调用方 8 处 | `app-state.json` 是"取快照 → 序列化 → 原子替换"的整文件覆盖，多入口无串行化；`saveAppState`（L5378）失败只写 `dbg` ✔ | 旧快照后写会覆盖新快照；丢的若是 `winHTTPPending`/TUN 快照，且随后崩溃，则机器级代理不再还原；`ProxyOn` 落盘失败则下次启动擅自重新接管（用户"我关了它自己又开"） | 加 `stateSaveMu` 包住"取快照+落盘"；关键路径改 `saveAppStateErr` 并上报失败 | 高（存在丢更新）/中（丢后即崩） |
| RP-012 | P2 | `service.go:4485-4510` | `ImportConfig` 只导入元数据；哈希订阅文件缺失时回退读 `subscription.yaml`，再用新 URL 落盘 ✔ | 导入后"界面显示订阅 B、内核加载订阅 A 的节点"（隐私与计费错乱）；`activeIdx` 不重置致高亮错位 | 导入时重置 `activeIdx`、校验文件存在并标记"待刷新"；收紧回退条件为"真正旧布局" | 高 |
| RP-013 | P2 | `internal/kernel/update.go:923-939`；`service.go:661,710-713` | 启动期只做 PE 头校验；"PE 合法但起不来"的自定义内核会被直接采用，`failBoot` 后不尝试 `.bak`/内置 | 每次启动都失败，只能手动点"回退内置"；`core-upgrade.json` 里的 `backupPath` 无读者 | Boot 在 `mgr.Start` 失败后做一次有界自愈（回滚 `.bak` → 再失败释放内置） | 高 |
| RP-014 | P2 | `service.go:3483-3506,1525-1550` | 任何 `RestartKernel` 错误都触发回滚，包括"程序正在退出/端口被占/配置写盘失败" | 已安装成功的新内核被静默丢弃；用户看到"升级失败"，原因与内核无关 | 只对可归因于二进制的错误回滚；其余报错但不回滚 | 高 |
| RP-015 | P2 | `internal/configgen/gen.go:156-174,298-306`；`subscription/fetch.go:284-291` | 订阅里的 `proxy-providers`/`rule-providers`/`geox-url` 原样写进内核配置 | 内核以管理员权限向任意 URL 周期发起 GET（回环/内网/云元数据），完全绕过 `guard.go` | 结构化清洗：只允许 `type: file`；`http` provider 的 url 复用订阅校验；`geox-url` 直接剥离 | 高（透传）/中（内核实际请求，需靶机） |
| RP-016 | P2 | `subscription/fetch.go:273-292,367-377`；`configgen/gen.go:131-148` | YAML 解析与 `deepCopy` 都是递归，8MiB 内可达百万层嵌套 | 栈溢出 `fatal error`（不可 recover）→ 进程直接死亡；订阅原文已落盘 → 每次启动重新崩溃（持久化 DoS） | 解析前用 `yaml.Node` 迭代统计深度（阈值 ~64）；`deepCopy` 同样限深；入口加 recover 兜底 | 中（需子进程实测） |
| RP-017 | P2 | `package.py:44-54`；`build.ps1:64`；`ci.yml:67` | 三个产出二进制的入口都没有 `-tags production` ✔；Wails v3.1.0 的 devtools 由该标签决定（已核对模块源码） | Release 包可按 F12 打开 DevTools，可直接调用任意绑定（含 RP-018） | 三处统一 `-tags production -trimpath`；CI 断言标签存在 | 高 |
| RP-018 | P2 | `service.go:608,1320,1327,2703`；`frontend/bindings/routepilot/coreservice.js:29,45,264,557` | `Boot`/`Attach`/`Shutdown`/`MarkQuitting` 作为 public 方法被 Wails 全量导出 ✔ | 在 DevTools 里调用 `MarkQuitting()` → `quitting=true` 但进程不退出，此后所有启停/切换都被拒，只能杀进程 | 改为非导出并由 `main.go` 同包调用；或加内部调用守卫 | 高 |
| RP-019 | P2 | `subscription/fetch.go:89-92,170`；`service.go:4556,5529`；`App.tsx:439-441,1159` | 订阅失败时 `*url.Error` 原文（含 `?token=`）回显到顶部横幅；同时写进 `debug.log`（0644）✔ | 用户截图求助即泄漏订阅凭据；同机其他用户/备份软件可读日志 | 统一 `redactURL`（去 query/userinfo），日志只打 host；日志文件改 0600 | 高 |
| RP-020 | P2 | `NodesPanel.tsx:153-158`；`service.go:4236-4250` | 订阅卡片整块可点且不判断 `active`；后端无 `idx==activeIdx` 短路，最后无条件 `RestartKernel()` ✔ | 误点当前订阅即断全部连接数秒；连点 N 次即重启 N 次 | 后端短路 + 前端 `disabled`/仅"刷新" | 高 |
| RP-021 | P2 | `App.tsx:1208-1211`；`NodesPanel.tsx:378-384`；`service.go:4768-4794` | 节点选择无并发门控（`busy` 没接到卡片/地图）；`rememberSelection` 无"末次赢" | 快速连点后实际生效节点与最后点击不一致；错误选择被持久化，重启后"自己变回去" | 前端 `disabled` + 后端按序串行；确认 `now==name` 后再持久化 | 高 |
| RP-022 | P2 | `SettingsPanel.tsx:424` ✔；`service.go:5695-5702,5088` | 网站规则列表 `key={r.domain}`，而后端允许同域名 exact+suffix 两条并存 | React key 冲突：第二条规则在 UI 中不可见/不可删，但实际参与分流 | `key={`${r.type}|${r.domain}`}` | 高 |
| RP-023 | P2 | `service.go:702,4247,4633` | `writeProcRulesYAML` 的返回在 3 处被 `_ =` 丢弃 ✔（4247 已复核） | 发布失败时内核配置指向过期 generation：用户看到规则已保存，实际走旧规则；Boot 处最糟（启动成功但规则是旧的） | Boot 处 `appendBootErr`；订阅两处并入返回值 | 高 |
| RP-024 | P2 | `.golangci.yml` 全文；`ci.yml` 无 lint 步骤 | 配了 errcheck/staticcheck/unused，但 CI 从未执行 | 与 §6.1 的 13 处吞错互为因果：配置存在造成"有静态检查"的错觉 | CI 加 golangci-lint（先 `--new-from-rev` 增量） | 高 |
| RP-025 | P2 | `ci.yml:59-60,101-102` ✔ | `-tags repro` 不带 `-race`，`-race` 不带 `repro` | 最"脏"的并发用例（`repro_persist_test.go` 24 goroutine 并发写）永远在无 race 模式跑 | race job 加 `-tags repro` 一行 | 高 |
| RP-026 | P2 | `service.go:608-721`；`Boot` 在测试中零出现 | 启动编排（15 步流水线）与 `failBoot` 的 6 个分支完全无测试；`startupRestorePlan` 只测了纯函数，没测"代码是否照做" | 决定"重启电脑后还能不能上网"的路径只能靠真机肉眼验 | 抽 `bootSteps` 注入点；补 3 条 Boot 失败/恢复用例 | 高 |
| RP-027 | P2 | `internal/kernel/update.go:464-477,583-632`；测试侧 0 命中 ✔ | 摘要不符/大小不符/白名单拒绝三条分支**零测试**（`grep` 确认） | 任何人改坏校验，全套测试（含 repro/race）仍全绿；这是"测试有效性"与"风险"差距最大处 | `httptest` 4 条 table + 镜像坏数据回退直连 | 高 |

---

## 6. P3 问题（46 条）

| 编号 | 位置（文件:行） | 问题 | 影响 / 修复要点 |
|---|---|---|---|
| RP-028 | `sysproxy_windows.go:268-275`；`service.go:1718` | `netsh` 用 `exec.Command` 无超时，且在 `proxyMu` 内执行 | 若 netsh 挂起，全部代理操作与 `Shutdown` 排队 → GUI 无法退出；改 `CommandContext` + 5s |
| RP-029 | `service.go:1691,1697,1704,1708` | 接管失败回滚的错误被 `_ =` 丢弃、无日志；该分支零测试 | 可能残留"半接管且无备份无痕迹"；回滚失败至少 `dbg` + 保留 Pending |
| RP-030 | `service.go:1779,1916-1918`；`sysproxy_windows.go:157-168` | `ProxyOverride` 用 string 表达，无法区分"不存在"；`proxyStillOurs` 把 `cur==""` 一律判为我方 | 空值项残留；用户"彻底清空代理"会被写回旧值；备份字段改 `*string`/加 `Present bool` |
| RP-031 | `service.go:2282-2303,1199-1220` | TUN 前置段两个崩溃窗口（Disable 前后）启动时不校验注册表与 TUN 一致性 | 极端情况下启动后 TUN+系统代理双接管；关 TUN 时 `errTunNoBasis` 后代理留在己方端口 |
| RP-032 | `main.go:380,392-403` | 托盘"系统代理"勾选只在初始化时取值，TUN 关闭代理后不刷新 | 托盘显示 ✓ 而实际关闭，再点会自相矛盾；改为订阅状态事件或按需重建 |
| RP-033 | `watchdog.go:310,147-215` | `time.AfterFunc` 回调无 recover；`restarting` 未在 defer 复位 | `restart()` 内 panic 会终止整个进程；panic 后 `restarting` 永真会吞掉后续恢复 |
| RP-034 | `manager.go:585-591,600` | 非主动的 exit code 0 被当作"正常停止"，不告警不恢复 | 内核被优雅结束/自行 `os.Exit(0)` 时状态撒谎为"已停止"，看门狗不介入 |
| RP-035 | `manager.go:620-623` | `Failed + run==nil` 时 `Stop` 直接返回，状态与 `lastErr` 都不动 | UI 长期停在红色错误态；停止后归一为 `Stopped` 并保留历史错误 |
| RP-036 | `manager.go:640-649,579-591` | `Stopping→Failed`（停超时）允许进程仍存活，随后被改写成 `Stopped` 并清空诊断 | "Failed 但进程活着"的矛盾窗口；停止超时原因被抹掉 |
| RP-037 | `manager.go:540-556,360-363` | `abortStart` 清理超时保留 `run`，对外仍报"已在运行" | 用户卡在"启动→已在运行→状态失败"的循环；应返回"清理中"并暴露状态位 |
| RP-038 | `service.go:679-681`；`manager.go:732-743` | 三个端口只在 Boot 选一次，重启不重选；`checkAddrFree` 在对方未 listen 时放行 | 一次瞬时端口抢占 → 本会话内核再也起不来，只能重启应用（需真机验证 mihomo 行为） |
| RP-039 | `service.go:3464-3483,3521-3530` | `UpdateCore`/`ResetCore` 的"停止→换文件→重启"三次独立取 `lifeMu`，不是原子事务 | 插队的 Start 会让替换失败/用已删除的内核启动；建议 `withKernelDown` 事务 |
| RP-040 | `service.go:2940`；`manager.go:593` | `mgr.OnExit` 无锁写 / 持锁读 | 极窄窗口内可能漏掉一次自动恢复；加 `SetOnExit` 在 `mu` 内赋值 |
| RP-041 | `subscription/guard.go:101-103` | 允许明文 `http://` 订阅，无二次确认、无警示 | 凭据明文传输 + 响应可被篡改后直达内核。**属产品兼容性取舍**，建议显式风险开关或常驻警示，而非一律拒绝 |
| RP-042 | `subscription/guard.go:64-78` | `blockedCIDRs` 未覆盖 `2002::/16`（6to4）、`2001::/32`（Teredo）、`::/96` | Windows 默认不启用这些机制，可达性低；追加 CIDR 即可 |
| RP-043 | `subscription/fetch.go:357-361`；`service.go:5518` | `profile-update-interval` 无范围校验，`time.Duration(hrs)*Hour` 溢出为负 | 自动更新退化为每 10 分钟全量重拉（带宽 + 对方服务器压力 + 可能频繁重启内核）；clamp 到 `[1,720]` |
| RP-044 | `service.go:4089-4105` | 订阅 raw 与 provider 两次写盘各自原子但整体非事务 | 第二步失败时磁盘是新原文、内存是旧订阅，下次启动加载未确认内容；两阶段 tmp+rename |
| RP-045 | `subscription/guard.go:96-125,188-207` | 不限端口 | 客户端可被当作公网任意 `IP:port` 探测器（响应不回流，影响有限）；限制 80/443/8080/8443 |
| RP-046 | `service.go:3517-3531,3521-3523` | `ResetCore` `_ = StopKernel()`、先清标记后替换、忽略 `os.Remove` 失败 | 停内核失败时仍删文件 → 报"恢复内置内核失败"，原因与事实不符；标记已清导致下次启动静默覆盖自定义内核 |
| RP-047 | `service.go:4107-4137,4167-4179` | `subs.json` 损坏时原文件被保留（`.corrupt-*`），但内存为空，下一次 `saveSubs` 写空列表 | 活动订阅全部消失，需手工恢复 `.corrupt-*`；加 `subsReadonly` 闸门禁止自动覆盖 |
| RP-048 | `service.go:3809-3821,3824-3832` | `traffic-history.json` 解析失败静默忽略，60s 后被覆盖，无 `.bak` 无日志 | 流量历史永久丢失且无感知；纳入 `readConfigJSON` 体系 |
| RP-049 | `service.go:568-580` | `ensureGeodata` 只按 size 判断、写失败 `_ =` 吞掉（与 `EnsureAux` 的哈希判断自相矛盾） | 等尺寸损坏永不修复；`GEOIP,CN` 分流静默失真；写失败无痕。改哈希比对 + 上报 |
| RP-050 | `internal/kernel/manager.go:337` | `EnsureAux` 用 `os.WriteFile` 非原子写 `wintun.dll` | 断电留下 0 字节 DLL（下次启动可自愈，但二次失败时 TUN 无提示起不来）；换 `atomicWriteBytes` |
| RP-051 | `update.go:797-883`；`service.go:2041-2076`；`gen.go:426-457` | 五处 tmp+Sync+rename 均未对父目录 fsync / `MOVEFILE_WRITE_THROUGH` | 断电后 rename 可能未持久化（最坏回退一版或文件缺失，多可自愈）；内核替换建议写透 |
| RP-052 | `service.go:3407-3514` | `UpdateCore` 无服务端互斥（仅前端按钮 disabled） | 并发升级会破坏 `.bak` 语义（不产生不可启动状态）；加 `coreUpdateMu.TryLock` |
| RP-053 | `service.go:478-483` | `migrateDataDir` 绕过 `dataDir()`/override，直接用 `os.UserConfigDir()` | 未来"override + 调 Boot"的测试会移动开发机真实数据目录；首行加 override 短路 |
| RP-054 | `frontend/package.json:15` | `lint` 实为 `tsc --noEmit`，无 ESLint（无 hooks/exhaustive-deps 规则） | 已存在靠"闭包里只用 ref"侥幸安全的 effect；下次改动易产生 stale closure；引入 eslint-plugin-react-hooks |
| RP-055 | `App.tsx:950-967`；`SettingsPanel.tsx:522`；`service.go:2760-2812` | 导入配置无二次确认、无大小上限、双击弹两次文件框 | 误导入即清空订阅与全部规则并重启内核；加 confirm + `importing` 锁 + 4MB 上限 + 导入前备份 |
| RP-056 | `App.tsx:691-720`；`ConnectionsPanel.tsx:201-212` | `onSetProc` 不走全局 `busy`，策略可并发提交 | 规则最终值正确（`rulesMu` 串行），但同一进程连接被断 2~3 次；乐观守卫 4s 可能先过期 |
| RP-057 | `main.go:437`；`service.go:3331-3356` | `QuickSelectNode` 不写 `rememberSelection`，与 `SelectNode` 语义不一致 | 托盘选 B，重启后回到主窗口选的 A；`pickSelectorGroup` 可能改的是业务组而非主出口 |
| RP-058 | `App.tsx:1196-1212,783-793,827-836,366`；`NodeMap.tsx:407-422,536-542` | 地图常驻且隐藏时仍跑 SMIL 动画；`mousemove` 每次全量重绘；两个 60s 轮询不随隐藏降频；`targetLocs` 无界 | CPU/掉帧、内存缓慢增长；加 paused、rAF 节流、隐藏降频、LRU 上限 |
| RP-059 | `service.go:4768,5448-5453,3063-3070` | `SelectNode` 空值/长度、`SaveWindowBounds` 范围、`SetTrafficAlertLimit` 溢出 | 无安全影响（有 `PathEscape` 与窗口 clamp 兜底），但错误信息与语义不严谨 |
| RP-060 | `service.go:2734-2741` | `ExportConfig` 含完整订阅 URL（token） | 用户"把配置发你看看"即泄漏凭据；导出前提示或提供脱敏导出 |
| RP-061 | `frontend/index.html:10` | `style.css?v=1.1.3` 硬编码，`package.json` 已 1.6.0 | 升级后样式可能被 WebView2 缓存（需真机验证）；改由 Vite 处理 CSS |
| RP-062 | `frontend/bindings/routepilot/coreservice.js` | bindings 未随源码重生成（Go 有 77 方法，JS 79 函数，缺 `IsShuttingDown`） | 当前无运行时炸点；后端删方法后会在运行时报 "no method found"；CI 校验 `git diff --exit-code` |
| RP-063 | `App.tsx:988-999`；`ConnectionsPanel.tsx:164`；`service.go:5942-5944` | 单节点测速用单值锁、其他节点按钮看似可点；内核未运行时"改完即刻生效"文案失真 | 用户以为测速/设置失效；改 `Set<string>` + 文案区分"已保存，启动后生效" |
| RP-064 | `main.go:100-141` | 单实例锁（互斥体 + 端口）零测试 | 双实例会并发写 `app-state.json`/`procgen`，是"配置莫名回滚"的经典成因；抽 syscall 注入点后 3 条用例即可覆盖 |
| RP-065 | `c3_persist_test.go:12` | 该测试用 `NewCoreService()` 且未 `setDataDirOverride` ✔（`publishProcRules` 会 `MkdirAll` 真实 `%APPDATA%\RoutePilot\procgen`） | 当前实际影响 = 在开发机创建空目录；若注入被重构掉会真的发布规则到用户配置。补隔离 + `t.Fatal` 守卫 |
| RP-066 | `service.go`（6054 行 / 178 方法 / 13 把锁） | 上帝对象，是多数测试缺口的结构性根因 | 建议按 `appstate` → `proxytx` → `substore` → `apiclient` 顺序做无行为切分 |
| RP-067 | `repro_persist_test.go:17-44,48-103`；`internal/kernel/repro_concurrency_test.go:161-190` | repro 系列测试空转：T10 断言的是旧 bug 现象（且"原内容不可恢复"表述已过时，`loadSubs` 会保留 `.corrupt-*`）；T11 自己当写入方；T18 不碰产品代码 | 无论修没修都是绿的，还 `t.Logf("FAIL")` 输出误导信息；改写或删除 |
| RP-068 | 多处 | 5 处重复实现：原子写两份、fake kernel 两套、repro 与 lifecycle 重复、"是否仍属我方"三份、进程规则两套写路径 | "三份归属判定"语义不一致（`proxyStillOurs` 与 `tunRestoreUserChanged` 对空串处理不同）；抽公共函数 + 一张 table 锁语义 |
| RP-069 | `internal/geoip`、`internal/elevate`、`internal/kernel/api.go` | 三个包零测试（`api.go` 仅 `Version`/`Configs` 被间接跑到） | `api.do` 的错误包装是可诊断性来源；每个用例 5-15 行，投入产出比最高 |
| RP-070 | `service.go:3471,3521,3523` | `UpdateCore`/`ResetCore` 吞掉 `RestartKernel`/`StopKernel`/`os.Remove` 的失败 | 用户以为在回滚/降级，实际没有内核；错误文案把排障引向错误方向 |
| RP-071 | `App.tsx:206-208` | `console.warn("[routepilot]", e)` 把 Go 原始 err（含完整路径、订阅 URL）打到控制台 | 配合 RP-017 的 DevTools，排障者/旁观者可读到本机路径与凭据 |
| RP-072 | `internal/kernel/lifecycle_test.go:60-71,449-474` 等 | 6 处"靠 sleep 制造先后关系"（`freeAddr` TOCTOU、`TestAPIUnreachableClassified` 150ms sleep） | 高负载 runner 上可能偶发红（错误分类变化），进而让人习惯性重跑；改先 listen 再 Start；CI 加 `-shuffle=on` |
| RP-073 | `.github/workflows/release.yml` 全文 | 打 tag 直接发版不跑测试；无 Authenticode 签名；`-tags realkernel` 的 4 个集成测试永不执行 | 一次误 tag 就能发出未测代码；未签名 exe 会被 SmartScreen 拦；realkernel 测试随内核升级静默腐烂 |

---

## 7. 主 Agent 独立复核记录

### 7.1 亲自回读代码确认（不只依赖子 Agent）
- 接管/恢复序列：`service.go:1581/1689/1696/1707`、`1948-1980`、`1856-1910`、`2266/2282/2302` —— 确认 RP-001、RP-003/004/005/006 的行号与因果。
- TUN 前置与失败回滚：`service.go:2222-2430` —— 确认"失败分支在锁外写注册表"这一结构性事实。
- 升级替换与启动：`internal/kernel/update.go:571-712,890-1005`、`service.go:661-720` —— 确认 RP-013/RP-014 的路径。
- 订阅防护：`internal/subscription/guard.go`（全文 287 行）、`fetch.go:30-140` —— 确认"每跳复检 + 拨号用已校验 IP + 不继承凭据头"确实到位，SSRF 主链路**不成立**（未列入问题）。
- 配置透传：`internal/configgen/gen.go:125-325` —— 确认 `proxy-providers`/`rule-providers`/`geox-url` 未清洗（RP-015）。
- 测试隔离：`c3_persist_test.go`（全文 55 行）—— 确认无 `setDataDirOverride`（RP-065）；`procgen.go:77` 会 `MkdirAll`。
- 构建标签：`package.py:44-54`、`build.ps1:64`、`ci.yml:67`，以及 `$GOMODCACHE/.../webview_window_windows_devtools.go` 的 `//go:build windows && !server && (!production || devtools)` —— 确认 RP-017。
- 导出面：`grep 'export function (Boot|Shutdown|MarkQuitting|Attach)' frontend/bindings/routepilot/coreservice.js` → 4 个全中 —— 确认 RP-018。
- CI：`ci.yml:56/60/102` —— 确认 RP-024（无 golangci）与 RP-025（repro 无 race）。
- 前端 key：`SettingsPanel.tsx:424` `key={r.domain}` —— 确认 RP-022。
- 订阅切换：`service.go:4205-4252` —— 确认无 `activeIdx` 短路且末尾 `RestartKernel()`（RP-020）、`4247` 的 `_ = writeProcRuleYAML`（RP-023）。
- 订阅加载回退：`service.go:4485-4510` —— 确认 RP-012。
- `waitProcess` panic 上报：`manager.go:430` + `safego.go:43-51` —— 确认 RP-010。

### 7.2 受控外网探测（唯一一次）
- `curl 'https://gh-proxy.com/https://example.com/'` → **HTTP 404**（返回镜像自己的错误页，非目标内容）。
- 结论：**"白名单可用嵌套 URL 下载任意主机"不成立**；且即便转发，请求源是镜像服务器而非用户本机，**不构成对本机的 SSRF**。据此把 RP-002 的表述从"任意主机下载/RCE 直连"修正为"信任根与摘要同源"。

### 7.3 未能验证（保留为静态推断）
- 真实断电/强杀下的注册表与磁盘状态（RP-001/011/016/041/051）。
- `netsh winhttp` 的真实失败模式与耗时（RP-028/006）。
- mihomo 在 `external-controller` 端口冲突时是退出还是顺延（RP-038）。
- 8MiB 深嵌套 YAML 是否必然栈溢出（RP-016）—— 需子进程实测。
- 前端交互类（RP-022 的实际渲染表现、RP-021 的竞态结果、RP-058 的帧率）。

---

## 8. 误报与降级清单（去误报结果）

| 条目 | 子 Agent 结论 | 主控判定 | 理由 |
|---|---|---|---|
| A4-01 的"嵌套 URL 可下载任意主机 / 本机 SSRF" | P1，含"任意主机"论断 | **保留 P1，但删去该子论断** | 实测 404；且请求由镜像发出，不触及用户本机 |
| A3-02 明文 http 订阅 | P2 | **降 P3（风险接受项）** | 为兼容自建面板的显式取舍，已在 UI 暴露 `insecure` 开关语义；建议加警示而非阻断 |
| A1-08 `netsh` 无超时 | P2 | **降 P3** | 需"netsh 挂起"这一未验证前提；修复成本极低，但不足以与确定性缺陷同级 |
| A6-03 单实例锁零测试 | P2 | **降 P3** | 无证据表明当前实现有错，属测试缺口 |
| A6-08 测试写真实数据目录 | P2 | **降 P3** | 当前实际副作用仅为 `MkdirAll` 一个空目录；"会写真实规则"需先移除注入才成立 |
| A2-03 与 A6-04 | 两条独立 P2/P2 | **合并为 RP-010** | 同一行代码（`manager.go:430`）的同一根因 |
| A1-05 与 A4-04 | 两条（P2/P2） | **合并为 RP-011** | 同为 `app-state.json` 持久化健壮性：一个是"整文件覆盖丢更新"，一个是"落盘失败被吞" |
| A3-06 与 A5-03 | 两条（P3/P2） | **合并为 RP-019** | 同一凭据泄漏根因（`*url.Error` 原文未脱敏），一个流向日志、一个流向横幅 |
| A4-09 与 A6-15 | 两条 P3 | **合并为 RP-049** | 同一函数（`ensureGeodata`）的两个表现 |
| A4-06 与 A6-14 | 两条 P3 | **合并为 RP-046/RP-070 同族**（RP-046 覆盖 ResetCore 顺序，RP-070 覆盖吞错） | 同函数不同侧面，保留两条但交叉引用，不重复计数 |

**完全误报：0 条。** 所有条目在回读代码后均对应真实代码位置；差异只在严重度与表述边界。

---

## 9. 第二阶段整改的冲突预警（提前给分派用）

| 热点文件 | 涉及条目 | 冲突风险 |
|---|---|---|
| `service.go`（6054 行） | RP-001/003/004/005/006/007/009/011/012/013/014/020/021/023/038/039/046/047/048/049/052/053/059/070 | **最高**。必须串行化或按函数区间切分，禁止两个 Agent 同时改 |
| `internal/kernel/update.go` | RP-002/013/014/027/051 | 与 `service.go` 的 `UpdateCore`（RP-014）联动，需同一人 |
| `internal/kernel/manager.go` + `watchdog.go` | RP-008/010/033/034/035/036/037/040 | 状态机改动会影响 lifecycle 全部测试，建议一人完成 |
| `internal/subscription/*` + `gen.go` | RP-015/016/041/042/043/044/045 | 安全策略与配置生成耦合，需同一人 |
| `frontend/src/*` | RP-017/018/019/020/021/022/054/055/056/058/061/063/070 | 与 Go 侧同时改 `NodesPanel`/`App.tsx` 的调用契约时需协调 |
| `internal/sysproxy/*` | RP-001/006/028/030 | 与 `service.go` 的代理事务强耦合 |
| CI / 测试 | RP-024/025/026/027/064/065/067/072/073 | 只改测试与 workflow，不碰业务实现（符合原任务分工） |

---

## 10. Windows 真机验证缺口

完整矩阵见 `audit/WINDOWS-TEST-MATRIX.md`。当前**必须**在真机（管理员 + 真实网卡）执行、无法自动化的 12 项：

1. 接管中强杀 / 断电后，注册表与 `proxy.json` 的一致性（RP-001）
2. WinHTTP `netsh` 接管→还原后，`WinHttpSettings` 字节级一致（RP-006）
3. 非管理员启动但上次开过 TUN → 回落系统代理且提示正确（RP-007 相邻）
4. TUN 开启：wintun 加载、路由表 0.0.0.0/0、DNS 劫持、关闭后复原
5. TUN 与系统代理互斥（真实注册表）
6. 内核崩溃 → 看门狗重启期间的断网时长实测（RP-007）
7. 父进程被强杀后 Job Object 是否带走 mihomo（RP-010 相邻）
8. 新内核"PE 合法但起不来" → 是否只能手动回退（RP-013）
9. 杀软/索引器占用 `mihomo.exe` 时升级 → 旧内核仍可用（RP-014 相邻）
10. 双击启动两次 → 弹"已在运行"而非"创建锁失败"（RP-064）
11. Release 包按 F12 是否打开 DevTools（RP-017，修复后应打不开）
12. 订阅 provider 指向 `httptest` 回环 → 内核是否真的发起请求（RP-015）

---

## 11. 本阶段交付物

```
audit/
  agent-1.md              系统代理 / WinHTTP 专项（458 行，含时序图与调用链全集）
  agent-2.md              内核生命周期 / 并发专项（281 行，含状态机矩阵）
  agent-3.md              网络安全 / 订阅专项（287 行，含攻击面矩阵与防护规则清单）
  agent-4.md              升级 / 文件系统 / 持久化专项（412 行，含升级崩溃状态机逐格表）
  agent-5.md              前端 / API / 业务逻辑专项（509 行，含导出面清单与轮询时序）
  agent-6.md              测试体系 / 架构 / CI 专项（591 行，含测试有效性抽检 10 例）
  FINAL-AUDIT.md          本文件（统一编号、去重、误报判定、优先级）
  WINDOWS-TEST-MATRIX.md  Windows 真机验证矩阵
```

`FINAL-FIX-REPORT.md` 按任务书属第三/四阶段产物，待整改与独立复审完成后生成。

---

## 12. 发布条件判断（当前版本）

**不具备发布条件。** 阻断项是 2 条 P1：

- RP-001：接管事务的顺序缺陷，能把"用户原始代理配置"永久丢掉，且下一次启动表现为整机断网；修复面小、风险低，应在发版前修掉。
- RP-002：升级链路的信任根问题。若短期不引入签名，至少应把镜像从"JSON 入口"降级为"产物下载通道"，并在 UI 明确"升级信任第三方镜像"。

P2 中建议同批修掉的 3 条（成本低、用户可感知）：RP-017（构建标签，一行）、RP-023（吞掉的规则发布错误）、RP-022（React key）。

其余 P2/P3 可在发布后按批次推进，不构成阻断。
