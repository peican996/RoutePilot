# C2 进程分流身份治理整改报告

- 日期：2026-11-09
- 仓库：`peican996/RoutePilot`（本地 `nexuslink_review`）
- 分支：`main`，基线 `040b6b4`
- 范围：仅 C2（进程分流身份治理）。C3 订阅安全/升级原子性、C4 系统级异常恢复未改动。
- 依赖：本阶段建立在 C1 的内核生命周期改动之上；C1 与 C2 的改动都还在工作区，均未提交。

---

## 1. 结论摘要

C2 的 7 项要求已全部落地，核心变化是：

1. 进程规则的身份从「EXE basename」升级为「规范化完整路径优先、名称降级」。
2. 路径身份生成 `PROCESS-PATH` 规则，同名不同目录的程序可以分别配置出口。
3. 新增进程快照解析器，使用 **PID + 创建时间 + 完整路径 + 文件索引** 做短周期缓存，识别 PID 复用、进程退出与程序升级。
4. 旧版 `process-rules.json` 自动迁移为 `name:` 身份键，不需要用户手工处理。
5. 前端按身份聚合进程，同名程序分开显示；无法获取路径时明确标注「仅名称」。

**真实 mihomo 端到端验证通过**：把 `curl.exe` 复制成两个目录下的同名 `same.exe`，一条路径规则走 DIRECT（HTTP 200），另一条走 REJECT（502），证明同名程序不再串用规则；全大写路径规则也能命中实际大小写路径，确认 Windows 路径匹配不区分大小写。

改动未提交，等待人工审查。

---

## 2. 修改文件清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/procid/procid.go` | 新增 | 进程身份模型、路径/名称规范化、键解析、规则 payload 渲染 |
| `internal/procid/tracker.go` | 新增 | PID/创建时间/路径/文件身份快照缓存、PID 复用与退出清理 |
| `internal/procid/source_windows.go` | 新增 | Toolhelp32 + QueryFullProcessImageName + GetProcessTimes + 文件索引 |
| `service.go` | 修改 | 身份解析接线、规则生成/迁移、按身份断开连接、降级日志 |
| `frontend/src/types.ts` | 修改 | `Conn.processKey` |
| `frontend/src/utils.ts` | 修改 | 身份键展示、父目录区分、仅名称判断 |
| `frontend/src/components/ConnectionsPanel.tsx` | 修改 | 按身份聚合/设置规则、同名程序分开展示、降级标识 |
| `frontend/src/App.tsx` | 修改 | 规则提示与地图进程标签使用身份展示名 |
| `internal/procid/procid_test.go` | 新增 | 路径/名称规范化、同名不同目录、大小写、降级、迁移测试 |
| `internal/procid/tracker_test.go` | 新增 | PID 复用、缓存清理、文件身份变化、TTL 测试 |
| `internal/procid/source_windows_test.go` | 新增 | 真实 Windows 进程快照与文件索引测试 |
| `process_identity_test.go` | 新增 | 规则生成、迁移、payload 渲染、身份匹配测试 |
| `internal/kernel/realkernel_procpath_test.go` | 新增 | 真实 mihomo 同名 EXE 路径区分端到端测试 |
| `repro_t12_test.go` | 修改 | T12 从「复现」改为「已修复」回归测试 |

---

## 3. 问题根因

1. **规则键只有 EXE 名**
   `procRules map[string]string` 直接以 `chrome.exe` 为键，`sanitizeProcName` 还显式拒绝带路径分隔符的输入；`buildProcessTargetPlan` 与 `writeProcRulesYAML` 只生成 `PROCESS-NAME`。两个目录下的同名程序必然共享同一条规则（T12）。

2. **没有路径规范化**
   规则键直接使用 mihomo 上报的字符串或其 basename，没有统一大小写、分隔符、相对路径、环境变量、`\\?\` 前缀；同一程序在不同来源下可能生成不同键。

3. **只有名称，没有进程身份**
   没有任何 PID、创建时间或文件身份概念；无法判断 PID 是否被复用，也无法在进程退出后清理解析缓存。

4. **前端按名称聚合**
   `ConnectionsPanel` 用 `c.process` 聚合，同名程序被合并成一行，用户没有入口为它们设置不同规则。

---

## 4. 修改前后差异

### 4.1 身份模型与规范化（`internal/procid`）

**身份键**

- 路径身份：`path:<小写规范化完整路径>`，例如 `path:c:\apps\chrome\chrome.exe`
- 名称降级：`name:<小写 exe 名>`，例如 `name:chrome.exe`
- 规则 payload：
  - 路径 → `PROCESS-PATH,<路径>`
  - 名称 → `PROCESS-NAME,<exe 名>`

路径身份键统一小写，保证连接、规则表、前端三处永远能对上；`Identity.Path` 保留原始大小写用于展示和生成 payload。

**规范化策略（代码注释中同步说明）**

| 项目 | 策略 |
|---|---|
| 大小写 | 键统一 `ToLower`；Windows 路径匹配实测不区分大小写，真实测试用全大写规则命中了实际混合大小写路径 |
| 相对路径 | `filepath.Abs` 转为绝对路径 |
| 分隔符/冗余段 | `filepath.Clean`，统一为反斜杠 |
| 环境变量 | 展开 `$VAR`、`${VAR}`、`%VAR%`；未定义变量保持原样 |
| 长路径前缀 | 去掉 `\\?\` 与 `\\?\UNC\`，与 mihomo 上报格式对齐 |
| 符号链接/目录联接 | **不解析到目标路径**。mihomo 上报的是启动时使用的路径，解析后反而可能对不上；明确记录为策略而非遗漏 |
| 8.3 短路径 | 不展开，按上报字符串处理 |
| 逗号/换行/制表符/NUL | 无法安全写入逗号分隔规则，路径身份降级为名称身份并记录原因 |

**降级逻辑**

- 有完整路径 → 路径身份。
- 路径为空/非法：
  - 先用进程快照按名称查找；**同名进程唯一**时补齐路径，继续使用路径身份。
  - 同名进程多个（如两个 `chrome.exe`）→ 名称身份，`Degraded=true`，写入 `debug.log`，界面显示「仅名称」。
  - 同名进程不存在或无法读取路径 → 名称身份 + 日志。
- 名称包含逗号、控制字符、路径分隔符等 → 不生成身份，`SetProcessRule` 返回明确错误。

### 4.2 进程快照与 PID 复用（`Tracker`）

`ProcInfo` 包含：`PID`、`Name`、`Path`、`Created`、`FileID`。

- 快照通过 Toolhelp32 枚举，`OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` + `QueryFullProcessImageName` 取完整路径，`GetProcessTimes` 取创建时间。
- 缓存 TTL 5 秒；`LookupName` 只在 mihomo 缺少 `processPath` 时触发，避免拖慢正常连接轮询。
- 同一 PID 创建时间变化 → 判定为 **PID 复用**，替换缓存并写日志；规则键从不包含 PID，因此不会把旧规则错误套到新进程。
- 进程从快照消失 → 立即从 `byPID/byName` 移除，并记录「已清理退出的进程缓存」。
- 进程退出后，mihomo 负责结束它的连接；本项目在规则变更时按身份主动断开旧连接（`closeProcessConnections`），连接视图缓存 500ms 失效，不会长期展示已退出进程的陈旧数据。
- 同一 PID、同一路径但创建时间/文件索引变化 → 记录「进程文件身份变化」，用于识别升级/替换。
- 文件身份（卷序列号 + 文件索引）只在需要时读取并缓存，不参与规则键，避免把「文件内容」误当成稳定身份。

### 4.3 规则生成与持久化

- `buildProcessTargetPlan`：按键解析身份，路径生成 `PROCESS-PATH,<path>,<隐藏组>`，名称生成 `PROCESS-NAME,<name>,<隐藏组>`。
- `renderProcRulesPayload`：direct/proxy 两个 classical rule-provider 同样按身份生成 payload。已用真实 mihomo 验证 `PROCESS-PATH` 在 classical provider 中可正常匹配（HTTP 200）。
- `loadProcRules` / `ImportConfig`：`normalizeProcRules` 自动迁移旧键：
  - `chrome.exe` → `name:chrome.exe`
  - `PATH:C:\...` → `path:c:\...`（前缀大小写不敏感）
  - 仅大小写不同的路径合并为一条
  - 含逗号等无法写入规则的键丢弃
  - 迁移后立即持久化 `process-rules.json`
- `SetProcessRule`：接受 `path:` / `name:` / 旧版名称键；按大小写不敏感查找并删除旧键；名称降级会写限频日志；改名后的旧连接按身份精确断开（路径规则只断同一路径，名称规则断全部同名进程）。

### 4.4 前端

- `Conn.processKey` 由后端下发，前端按它聚合进程；同名不同目录不再合并。
- 进程行显示 `basename · 父目录名`，悬停显示完整路径；名称降级显示「仅名称」标识。
- 规则下拉、策略按钮、过滤、地图进程标签全部使用身份键/展示名。
- 无法获取路径时行为可见，不会悄悄共享规则。

---

## 5. 新增测试用例

### 5.1 身份与规范化（`internal/procid/procid_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestNormalizePathCanonical` | 分隔符、冗余段、`\\?\`、`\\?\UNC\` |
| `TestNormalizePathEnvAndRelative` | `%VAR%`、`$VAR`、`${VAR}`、相对路径转绝对 |
| `TestNormalizePathRejectsRuleBreakingChars` | 逗号/换行/制表符路径拒绝 |
| `TestFromProcessPrefersPath` | 有路径时优先路径身份 |
| `TestSameNameDifferentDirsAreDistinct` | 两个目录同名 EXE 身份不同 |
| `TestPathIdentityIgnoresCase` | 大小写不同视为同一身份 |
| `TestFromProcessFallsBackToName` / `TestMalformedPathFallsBackWithReason` | 路径缺失/非法时降级并带原因 |
| `TestInvalidNameRejected` | 非法名称不生成身份 |
| `TestParseKeyLegacyCompatibility` | 旧名称键、路径键、含逗号路径键 |
| `TestRuleRendering` | `PROCESS-PATH` / `PROCESS-NAME` 渲染 |

### 5.2 进程快照（`internal/procid/tracker_test.go`、`source_windows_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestWindowsSourceFindsSelf` | 真实快照能取到自身 PID/路径/创建时间；文件索引可读 |
| `TestTrackerResolveUniqueAndAmbiguous` | 唯一同名补路径；多实例判歧义；不存在判 missing |
| `TestTrackerPIDReuseUpdatesCache` | 同 PID 创建时间变化后不沿用旧路径，并写日志 |
| `TestTrackerFileIdentityChangeLogged` | 同路径升级/替换后返回新文件索引并写日志 |
| `TestTrackerCleansExitedPIDs` | 进程退出后缓存清理并记录 |
| `TestTrackerFileIDLazyLookup` | 文件身份按需读取且缓存 |
| `TestTrackerTTLRefreshes` | TTL 到期刷新快照 |

### 5.3 规则与迁移（`process_identity_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestProcessTargetPlanPathRules` | 同名不同目录生成两条不同 `PROCESS-PATH` 规则 |
| `TestProcessTargetPlanNameFallback` | 旧名称键保持 `PROCESS-NAME` |
| `TestRenderProcRulesPayloadIdentity` | direct/proxy provider payload 区分路径与名称 |
| `TestNormalizeProcRulesMigration` | 旧键迁移、前缀大小写、非法键丢弃 |
| `TestNormalizeProcRulesCaseDedup` | 大小写重复键合并 |
| `TestIdentityMatches` | 路径规则只命中同一路径；名称规则命中全部同名 |

### 5.4 真实内核端到端（`internal/kernel/realkernel_procpath_test.go`）

把系统 `curl.exe` 复制为 `A\same.exe` 与 `B\same.exe`，配置：

- `PROCESS-PATH,<A 的全大写路径>,DIRECT`
- `PROCESS-PATH,<B 的路径>,REJECT`
- `MATCH,REJECT`

分别通过 mihomo 代理访问本地 HTTP 服务。结果：A 返回 200，B 返回 502/退出码 22，证明同名进程按路径区分且路径匹配大小写不敏感。

---

## 6. 测试命令与结果

```powershell
# 全量测试
go test -count=1 ./...
# ok routepilot
# ok routepilot/internal/autostart
# ok routepilot/internal/configgen
# ok routepilot/internal/kernel
# ok routepilot/internal/procid
# ok routepilot/internal/subscription

# 身份/迁移用例重复两轮
go test -count=2 ./internal/procid/... .
# ok routepilot/internal/procid
# ok routepilot

# 复现套件中的 T12（已修复）与内核复现
go test -count=1 -tags repro ./internal/kernel .
# ok routepilot/internal/kernel
# ok routepilot

# 真实 mihomo：同名进程区分 + C1 生命周期
go test -tags realkernel -count=1 -run "TestRealKernel" -v ./internal/kernel
# --- PASS: TestRealKernelDistinguishesSameNameProcessPaths (0.31s)
#     同名进程已按路径区分: A=200 (direct), B=502 (REJECT)
# --- PASS: TestRealKernelLifecycle (0.23s)
#     真实内核已就绪: pid=2648 version=v1.19.33

# 真实 mihomo：classical rule-provider 中的 PROCESS-PATH（手工验证）
# HTTP=200

# 静态检查与前端
go vet ./...          # 无输出
gofmt -l .            # 无输出
cd frontend; npm run build
# ✓ built in 97ms
```

**说明**

- Race Detector 仍不可用：`CGO_ENABLED=0`，`go test -race` 需要 cgo。
- 执行完整的 `go test -tags repro ./...` 时，`internal/subscription` 的 `TestReproT17RedirectToLoopback` 仍然失败（SSRF 复现）。这是 **C3** 的订阅安全问题，本次按要求未改动；C2 的复现套件只跑 `./internal/kernel` 与根包。

---

## 7. 尚未解决的问题

1. **符号链接/目录联接不解析**：这是有意选择，避免规则路径与 mihomo 上报路径不一致。若用户通过不同符号链接启动同一程序，会产生多个路径身份；需要在文档或后续版本中提供「合并别名」能力。
2. **路径含逗号无法生成路径规则**：会降级为名称规则并记录日志。同名且路径含逗号的程序仍会共享名称规则——这是规则语法的硬限制。
3. **同名多实例且 mihomo 不提供路径**：只能降级为名称规则；界面有「仅名称」标识，日志有歧义说明，但无法自动区分。
4. **程序换目录升级**：旧路径规则会保留但不再命中；新路径生成新身份。当前不会自动迁移旧规则，也不会提示规则失效。属于可接受的保守行为，后续可加「同名新路径」提示。
5. **文件身份仅用于诊断**：不写入规则键，避免文件替换导致规则失效；升级识别依赖创建时间/文件索引日志。
6. **未做实机 UI/管理员/TUN 验收**：本轮验证了真实 mihomo 数据面，但未在图形界面重复点击同名进程策略、未验证 TUN 场景；属于 C4 范围。
7. **C3/C4 未执行**：订阅 SSRF、升级原子性、系统代理持久化、休眠/断电恢复仍是下一阶段内容。

---

## 8. 兼容性风险

| 变更 | 风险 | 处理 |
|---|---|---|
| `process-rules.json` 键格式变化 | 降级回旧版本时，旧版本会把 `name:` / `path:` 当成非法进程名，规则不生效 | 迁移前可备份 `process-rules.json`；升级后自动迁移并落盘 |
| 路径规则依赖 `find-process-mode: always` | 关闭后 mihomo 不返回进程信息，路径规则无法匹配 | 现有配置一直强制开启，未改动 |
| 规则 payload 路径转为小写 | 若未来 mihomo 改为大小写敏感匹配，可能失效 | 已用 v1.19.33 真实内核验证大小写不敏感；规则中保留 `Identity.Path` 原始大小写用于排查 |
| 名称降级仍会共享规则 | 用户可能误以为已隔离 | UI「仅名称」标识 + 限频日志 + 设置时明确写入 |
| 进程快照枚举系统进程 | 权限不足的进程没有路径/创建时间 | 跳过或以名称降级，不影响用户进程规则 |
| 前端新增 `processKey` 字段 | 旧前端连接数据没有该字段 | `?? ""` 兜底，仅影响聚合行，不影响后端 |

---

## 9. 是否需要人工实机验证

需要：

- 图形界面中同时运行两个同名 EXE（不同目录），确认进程列表分成两行、可分别设置直连/代理/指定节点。
- 用管理员权限 + TUN 重复上述操作，确认路径规则在 TUN 模式下同样生效。
- 打开一个无法获取路径的系统进程，确认界面显示「仅名称」，`debug.log` 有降级原因，且不会刷屏。
- 升级一个已配置规则的程序（同路径替换 / 换目录），确认旧规则行为与预期一致，日志能说明身份变化。
- 观察长时间运行后进程缓存大小与日志，确认退出进程被清理。

---

## 10. Git diff 摘要与提交状态

C2 新增/修改（不含 C1 的内核文件）：

```
 frontend/src/App.tsx                         |   6 +-
 frontend/src/components/ConnectionsPanel.tsx |  59 ++-
 frontend/src/types.ts                        |   1 +
 frontend/src/utils.ts                        |  35 +-
 repro_t12_test.go                            |  35 +-
 service.go                                   | 约 350 行（其中部分是 C1 生命周期接线）
 新增：
 internal/procid/procid.go          239 行
 internal/procid/tracker.go         204 行
 internal/procid/source_windows.go   86 行
 internal/procid/procid_test.go     152 行
 internal/procid/tracker_test.go    166 行
 internal/procid/source_windows_test.go 38 行
 process_identity_test.go           117 行
 internal/kernel/realkernel_procpath_test.go
```

工作区状态：

- 分支 `main`，HEAD `040b6b4`，与 `origin/main` 一致。
- **C1 与 C2 的全部改动均未提交**；C1 报告 `C1-KERNEL-LIFECYCLE-REMEDIATION.md` 也仍在工作区。
- 未执行 `git add` / `git commit` / `git push`。
- 未修改 C3、C4 代码；未改动订阅、内核升级、系统代理事务逻辑。

本阶段到此暂停，等待人工审查后再决定提交与进入 C3。
