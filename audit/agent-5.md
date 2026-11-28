# RoutePilot 审计报告 · 子 Agent 5：前端、API 与业务逻辑专项

## 1. 头部

### 1.1 范围

按真实架构审计（本项目**无 HTTP 服务端 / 无 REST 路由 / 无 WebSocket**）：

- 「API 越权」→ Wails v3 binding 暴露的 `CoreService` public 方法是否有内部方法外泄、是否缺少状态前置校验
- 「参数校验」→ 每个导出方法的入参边界 / 类型 / 注入校验
- 「WebSocket」→ `Events.On(...)` 订阅的解绑、事件与轮询并存时的一致性

### 1.2 基线

- 仓库：`C:/Users/wpc/WorkBuddy/代理/proxdeck`
- 基线提交：`39ca02c1aa9303fc14fe8264d2715432cd50f030`（`gofmt: 对齐测试表字段`），工作区干净
- 前端产物体积基线：`frontend/dist/assets/index-CwTU2sY_.js` 300,950 B

### 1.3 审阅文件清单（行数为实测）

| 文件 | 行数 |
|---|---|
| frontend/src/App.tsx | 1337 |
| frontend/src/components/SettingsPanel.tsx | 791 |
| frontend/src/NodeMap.tsx | 710 |
| frontend/src/components/ConnectionsPanel.tsx | 462 |
| frontend/src/components/NodesPanel.tsx | 430 |
| frontend/src/TrafficChart.tsx | 245 |
| frontend/src/components/Sidebar.tsx | 160 |
| frontend/src/utils.ts | 136 |
| frontend/src/components/OverviewPanel.tsx | 125 |
| frontend/src/components/LogPanel.tsx | 122 |
| frontend/src/mapData.ts | 114 |
| frontend/src/types.ts | 92 |
| frontend/src/components/ErrorBoundary.tsx | 69 |
| frontend/src/mapLayout.ts | 49 |
| frontend/src/components/Topbar.tsx | 43 |
| frontend/src/constants.ts | 30 |
| frontend/src/main.tsx | 12 |
| frontend/src/api.ts | 9 |
| frontend/src/worldPath.ts | 4 |
| frontend/vite.config.ts | 13 |
| frontend/index.html | 17 |
| frontend/package.json | 29 |
| frontend/bindings/routepilot/coreservice.js | 759 |
| service.go（对外面） | 6054 |
| main.go | 513 |
| internal/kernel/api.go | 289 |
| internal/subscription/guard.go | 287 |
| internal/subscription/fetch.go | 509 |
| internal/procid/procid.go | 258 |
| package.py / build.ps1 / .github/workflows/{ci,release}.yml / build/windows/Taskfile.yml | — |
| frontend/package-lock.json（版本核对） | — |

### 1.4 执行过的命令与结果

```text
git log --oneline -1            -> 39ca02c gofmt: 对齐测试表字段
git status --short              -> 空（工作区干净）
grep -c '^func (s *CoreService) [A-Z]' service.go
                                -> 77（导出方法数）
grep -oE '^export function …' frontend/bindings/routepilot/coreservice.js
                                -> 79 个函数；与 Go 侧对比：Go 有而 JS 无 = IsShuttingDown（1 个）
                                JS 有而 Go 无 = 0（不存在会被前端调用到的不存在方法）
node -e 读取 package-lock.json   -> lockfileVersion 3；vite 8.3.6 / @vitejs/plugin-react 6.2.1
                                （peer vite ^8.0.0 满足）/ react 18.3.1 / typescript 5.9.3
                                / @wailsio/runtime 3.1.0；lock 与 package.json 的版本范围一致
grep -rn DevToolsEnabled $GOMODCACHE/github.com/wailsapp/wails/v3@v3.1.0
                                -> devtools 实现按 build tag 二选一：
                                   windows_devtools.go   build: windows && !server && (!production || devtools)
                                   windows_production.go build: windows && !server && production && !devtools
                                   即：不带 -tags production 时 PutAreDevToolsEnabled(true)
grep -rn "go build" package.py build.ps1 .github/workflows/*.yml
                                -> 三个构建入口均无 -tags production（详见 A5-01）
```

`go build / go vet / go test / go test -race` 由主控验证（本报告不重复）。前端 `npm run lint / build` 由主控验证。

### 1.5 结论摘要

| 等级 | 数量 | 一句话 |
|---|---|---|
| **P0** | **0** | 无远程可利用面、无数据损坏、无权限提升 |
| **P1** | **0** | 桌面单机应用，未发现"非授权方触发且造成实质破坏"的链路 |
| **P2** | **6** | 生产构建开着 devtools、内部生命周期方法被导出、订阅 token 回显、点激活订阅即重启内核、节点切换竞态、网站规则列表 React key 冲突 |
| **P3** | **10** | 无 ESLint、导入无确认/无上限、进程规则并发、托盘选择不持久化、地图/轮询性能组、入参校验缺口、敏感信息落盘、静态资源缓存版本号、bindings 漂移、测速锁与文案误导 |

后端对外面的**硬性防护是扎实的**：域名/进程名/节点名注入面已全部封堵（白名单字符集 + 逗号与控制字符拒绝 + URL `PathEscape`），订阅拉取有完整 SSRF 防护（协议白名单、回环/私网/数字主机拒绝、DNS 钉扎、每一跳重校验），`SetTUN` / `SetSystemProxy` / `SetMode` 都有状态前置校验。本次 P2 集中在**前端交互时序**与**构建配置**，不是后端校验缺失。

---

## 2. 问题清单

### A5-01 · P2 · 正式发布构建未加 `-tags production`，生产版 WebView2 devtools 全程可用

- **文件**：`package.py:44-54`、`build.ps1:64`、`.github/workflows/ci.yml:67`
- **函数/组件**：发布打包 `package.py::main`、本地构建 `build.ps1`、CI 编译步骤
- **代码位置**：
  - `package.py:47-49` → `["go","build","-ldflags","-s -w -H windowsgui", …]`（GitHub Release 实际发布路径，`release.yml:56` 调 `python package.py`）
  - `build.ps1:64` → `go build -ldflags "-s -w -H windowsgui" -o bin\RoutePilot.exe .`（README.md:91 推荐用法）
  - `ci.yml:67` → `go build -o bin/RoutePilot.exe .`
  - 对照：`build/windows/Taskfile.yml:60` 走 `wails3 build` 时有 `-tags production`
- **描述**：Wails v3.1.0 的 devtools 开关由构建标签决定（见 1.4 实测）。三个真实产出二进制的入口都没有 `production` 标签，于是编译进 `webview_window_windows_devtools.go`，启动时执行 `settings.PutAreDevToolsEnabled(true)`。只有 `task/wails3 build` 这条（仓库里没人用来发布）会禁用。
- **触发条件**：用户拿到 Release 包 → 按 F12 或右键 → 「检查」→ 打开 WebView2 DevTools 控制台。
- **实际影响**：
  1. 控制台里可直接发 Wails IPC（`window.chrome.webview.postMessage`），调用任意导出方法，包括 A5-02 列出的 `Shutdown()` / `MarkQuitting()`；
  2. `logErr` 的 `console.warn("[routepilot]", e)`（`App.tsx:206-208`）把 Go 原始 err（含完整本机路径、订阅 URL）打到控制台；
  3. 生产包带调试入口本身违反 Wails 的发布约定，也给"社工诱导用户粘贴一段 JS"提供了落地条件（用户粘贴即等同自己操作，因此定 P2 不升 P1）。
- **根因**：仓库存在两套构建脚本（`wails3/task` 与自写的 `package.py`/`build.ps1`），自写的那套只对齐了 `-ldflags`，漏了构建标签；发布流水线用的是漏标签的那套。
- **修复建议**：三处统一改为 `go build -tags production -trimpath -ldflags "-s -w -H windowsgui"`；在 `package.py` 里加一条断言，编译后 `strings bin/RoutePilot.exe | grep -c "AreDevToolsEnabled"` 之类的方式不易判，更可靠的做法是把标签写进单一变量并让 CI 校验 `-tags production` 存在。
- **测试方案**：构建后启动，按 F12 应无反应、右键菜单无「检查」；`go version -m bin/RoutePilot.exe` 确认 `-tags` 记录。
- **置信度**：高（构建标签逻辑已在模块缓存源码中逐行确认；三个脚本均无该标签）

---

### A5-02 · P2 · 导出面包含 `Boot` / `Shutdown` / `MarkQuitting` / `Attach` 四个内部生命周期方法，无前置校验、无二次确认

- **文件**：`service.go:608`（Boot）、`service.go:1327`（Shutdown）、`service.go:2703`（MarkQuitting）、`service.go:1320`（Attach）；`frontend/bindings/routepilot/coreservice.js` 同名导出
- **函数/组件**：`CoreService` 上述四方法
- **代码位置**：
  - `Shutdown()` `service.go:1327-1366`：置 `s.shuttingDown=true`、`wd.Stop()`、`invalidateKernelGeneration()`、`mgr.Close()`
  - `MarkQuitting()` `service.go:2703-2714`：置 `s.quitting=true`、`wd.Stop()`、`invalidateKernelGeneration()`
  - 拒绝点：`StartKernel` `service.go:1436`、`restartKernel` `service.go:1525` 都是 `if s.Quitting() || s.IsShuttingDown() { return … }`
- **描述**：这四方法是给 `main.go` 内部用的（`main.go:180` 调 Boot、`main.go:274/278` 调 Shutdown、`main.go:458` 调 MarkQuitting），但因为是 `CoreService` 的 public 方法，Wails 一并生成 binding 暴露给前端。前端正常流程一次都不调用它们，也没有任何确认弹窗。
- **触发条件**：
  - 打开 DevTools 控制台（依赖 A5-01）执行 `MarkQuitting()`：`quitting=true` 且 watchdog 停止，但**进程不退出**。此后 `StartKernel` / `RestartKernel` / `SetTUN` / `SetSystemProxy` 全部返回「程序正在退出，取消…」，界面点任何开关都失败。
  - 执行 `Shutdown()`：内核被 `mgr.Close()` 关掉、系统代理被还原，同样 `shuttingDown=true` 导致无法再启动内核。
  - 两种情况下应用进入**假死态，唯一恢复手段是杀进程重开**（`MarkQuitting` 不会自己退出）。
- **实际影响**：误触或社工诱导后应用不可用且无自恢复路径；用户会认为"软件坏了"。`Attach(app)` 参数是 `*application.App`，前端传值会反序列化失败（实际不可用），但它出现在导出面里说明生成是"全量 public"而非显式白名单。
- **根因**：Wails v3 按"service 的所有 public 方法"生成 binding，没有 `//wails:ignore` / 非导出包装层来划出"仅内部"的边界。
- **修复建议**：把四个生命周期方法改为非导出（`boot`/`shutdown`/`markQuitting`/`attach`）并由 `main.go` 在同包内调用；`Quitting()` / `IsShuttingDown()` 这类只读查询可保留。若必须保持导出，至少加 `if !s.internalCallAllowed` 之类的守卫。
- **测试方案**：`grep '^export function' bindings/routepilot/coreservice.js` 不应出现 Boot/Shutdown/MarkQuitting/Attach；Go 测试里断言这四个名字不出现在生成物中。
- **置信度**：高（方法与拒绝逻辑逐行确认；与 A5-01 组合可复现）

---

### A5-03 · P2 · 订阅拉取失败时把含 token 的完整订阅 URL 回显到界面提示条

- **文件**：`frontend/src/App.tsx:439-441`、`internal/subscription/fetch.go:157-171`、`internal/subscription/fetch.go:89-92`、`service.go:4553-4559`
- **函数/组件**：`App.onSubscribe` / `App.onRefreshSub` 的 `catch`；`subscription.download` / `doWithDeps`；`CoreService.subscribe`
- **代码位置**：
  - `fetch.go:89-92`：`resp, err := c.Do(req); if err != nil { return nil, nil, err }` — Go 的 `http.Client.Do` 返回 `*url.Error`，其 `Error()` 形式为 `Get "https://api.example.com/api/v1/client/subscribe?token=XXXX": dial tcp …`
  - `fetch.go:170`：`fmt.Errorf("拉取失败: %w", err)` — 原样保留
  - `service.go:4556`：`s.subInfo.Error = err.Error()`
  - `App.tsx:439-441`：`catch (e) { setErr(e instanceof Error ? e.message : String(e)) }` → `App.tsx:1159` `banner = err || …` → `App.tsx:1186-1190` 直接渲染
- **描述**：订阅 URL 的 query 里通常带 `?token=…`（`NodesPanel.tsx:302` 的 placeholder 就是这个形状）。任何网络失败（DNS、超时、TLS、代理未启动）都会让完整 URL 进入 `err.message`，然后整段显示在页面顶部红色提示条上。
- **触发条件**：在「节点 → 更换/添加订阅」里填一个错误或不可达的地址点「保存并应用」；或点「刷新」时断网；或机场返回 5xx。
- **实际影响**：用户为求助而截图 / 复制提示文本时，订阅凭据（等价于账号密码）泄漏。这是订阅类客户端最典型的凭据泄漏路径。注：`sub.error` 字段虽也含该字符串，但前端仅在 `NodesPanel.tsx:274` 用 `includes("证书")` 判断，**没有直接渲染**，所以唯一泄漏出口是 `err` → banner。
- **根因**：错误在 `subscription` 层用 `%w` 包裹 `*url.Error`，没有做「剥离 URL 的 userinfo 与 query」的净化；前端对错误文本无脱敏。
- **修复建议**：`fetch.go` 里对 `*url.Error` 做一次 `redactURLError`：把 `u.RawQuery` 替换为 `?…`（或只保留 host+path）；或在 `service.subscribe` 赋值 `subInfo.Error` 前统一净化；前端 `setErr` 前对 `https?://…` 做同样的 query 掩码。
- **测试方案**：单测喂一个 `https://h/sub?token=abc` 的可控失败（closed port），断言 `err.Error()` 不含 `token=abc`；GUI 层用不可达地址触发，检查 banner 文本。
- **置信度**：高（`*url.Error` 的字符串格式是标准库行为；链路逐段确认）

---

### A5-04 · P2 · 点击"当前已激活"的订阅卡片 = 无提示重启内核并断开全部连接；连点多张卡片叠加多次重启

- **文件**：`frontend/src/components/NodesPanel.tsx:153-158`、`frontend/src/App.tsx:460-472`、`service.go:4205-4252`
- **函数/组件**：`NodesPanel` 订阅条目 `onClick`；`App.onSwitchSub`；`CoreService.SwitchSubscription`
- **代码位置**：
  - `NodesPanel.tsx:153-158`：整个 `<div className="sub-item">` 上 `onClick={() => onSwitchSub(it.url)}`，**没有 `if (it.active) return`，也没有 `disabled`**
  - `App.tsx:460-472`：`onSwitchSub` 只 `setBusy(true)`，但该 `busy` 没有作用到这个 div 上
  - `service.go:4236-4250`：无论 `idx` 是否等于 `s.activeIdx`，最后都 `if s.mgr != nil && s.mgr.IsRunning() { return s.RestartKernel() }`
- **描述**：订阅卡片是"整块可点"的，`title` 只有 URL，没有任何"点这里会重启内核"的提示。点到已经激活的那张卡片，后端照走一遍 `SwitchSubscription` → 重新解析本地配置 → `RestartKernel()`。`RestartKernel` 是完整 `Stop + Start` 事务（`service.go:1543-1550`），所有连接归零。
- **触发条件**：用户在订阅页误点当前订阅（很常见——卡片看起来就是个可选项）；或在多订阅时快速连点 A、B 两张卡片。
- **实际影响**：
  1. 单次误点：全部 TCP 连接被重置，正在下载/视频/游戏中断几秒，界面没有"正在重启内核"的提示（`busy` 只让侧栏按钮变灰）；
  2. 连点：`RestartKernel` 由 `lifeMu` 串行（`service.go:1522`），两次点击会**依次完整重启两次**，断连两次，运维日志里出现两条 `RestartKernel <- …`；
  3. 后端 `SwitchSubscription` 无 `idx == activeIdx` 短路，属于"幂等操作却有重副作用"。
- **根因**：把"切换订阅（本地读文件）"和"重启内核（重副作用）"耦合在一个调用里，前端又没有区分"已激活"与"未激活"。
- **修复建议**：`SwitchSubscription` 开头加 `if idx == s.activeIdx { return nil }`（并让前端把 `it.active` 的卡片设为不可点或仅做"刷新"）；前端给非激活卡片加 `disabled={busy}`，并在点击前 `if (it.active) return`；把"切换订阅"与"重载内核"拆成两个方法，让 UI 至少能显示"正在重载"。
- **测试方案**：单测 `SwitchSubscription(activeIdx)` 后断言 mock manager 的 `Restart` 调用次数为 0；GUI 层连点两张卡片，检查 debug.log 中 `RestartKernel` 记录条数应为 1。
- **置信度**：高（代码路径无短路；`RestartKernel` 的 Stop+Start 语义已确认）

---

### A5-05 · P2 · 节点切换无并发门控：生效节点可能与最后一次点击不一致，且错误选择被持久化

- **文件**：`frontend/src/App.tsx:1208-1211`、`frontend/src/components/NodesPanel.tsx:378-384`、`frontend/src/App.tsx:1263-1269`、`service.go:4768-4794`、`internal/kernel/api.go:148-151`
- **函数/组件**：`NodeMap onPick` / `NodesPanel` 节点卡片 `onClick` / 右键菜单「设为当前出口」；`CoreService.SelectNode` / `rememberSelection`；`kernel.Client.Select`
- **代码位置**：
  - `App.tsx:1208-1211`：`onPick={(name) => activeGroupName && run(() => svc.SelectNode(activeGroupName, name))}` — `run` 会 `setBusy(true)`，但 `NodeMap` 的点击不读 `busy`
  - `NodesPanel.tsx:381-384`：`onClick={() => activeGroupName && run(() => svc.SelectNode(activeGroupName, n.name))}` — 卡片没有 `disabled={busy}`
  - `service.go:4768-4779`：`SelectNode` 无锁、无去重，成功即 `rememberSelection(group, name)`
  - `service.go:4783-4794`：`rememberSelection` 直接 `s.selections[group] = name` 并 `saveAppState()`
- **描述**：每次点击都发一个独立的 Wails 调用（各自 goroutine），后端 `api.Select` 只是一次 `PUT /proxies/{group}`，mihomo 侧对同一 Selector 的并发更新以到达顺序为准。快速连点 A→B→C 时，最终落到哪个节点取决于三次网络往返的顺序，而不是点击顺序；`rememberSelection` 的写入顺序同样不确定。
- **触发条件**：节点列表/地图上快速连续点选多个节点（机场节点几十个，用户通常会来回试）。
- **实际影响**：
  1. 界面 1.2 s 后轮询到真实 `now`，会显示"停在 B"而用户最后点的是 C — 用户以为点错了，或没注意就走错出口；
  2. 更严重的是 `selections[group]` 被写成 B，`restoreSelections`（`service.go:4798-4816`）在**下次启动内核/重启内核**时把 B 写回去 —— 错误选择被持久化，用户重启应用后发现节点"自己变回去了"。
- **根因**：前端没有"选择中"的门（`busy` 没接到节点卡片/地图上），后端也没有串行化 + 末次赢（last-write-wins by sequence）。
- **修复建议**：前端对节点卡片与地图 `onPick` 加 `disabled={busy}`；`SelectNode` 后端加一把 `selectMu` 让同 group 的选择串行，或在前端用「请求序号 + 只接受最后一次」的方式丢弃过期结果；`rememberSelection` 只在"后端确认 `now == name`"后写入。
- **测试方案**：并发 10 次 `SelectNode` 不同 name，最终 `s.selections[group]` 应等于最后一次调用（需要一个"调用序号"字段来定义"最后"），且 `Nodes()` 返回的 `now` 与之相同。
- **置信度**：高（时序与持久化链路确认，竞态结果需真机复现，标注「静态推断，需真机验证」表现）

---

### A5-06 · P2 · 网站分流列表用 `domain` 作 React key，同域名的「精确」+「后缀」两条规则必然 key 冲突

- **文件**：`frontend/src/components/SettingsPanel.tsx:420-468`（关键 `:424`）、`service.go:5675-5720`、`service.go:5069-5096`
- **函数/组件**：`SettingsPanel` 的 `siteRules.map`；`CoreService.SetSiteRuleWithType` / `normalizeSiteRuleList`
- **代码位置**：
  - `SettingsPanel.tsx:422-425`：`{siteRules.map((r) => (<div key={r.domain} …>))}`
  - `service.go:5695-5702`：upsert 判据是 `next[i].Type == rt && strings.EqualFold(next[i].Domain, d)` —— **type 不同就是两条独立记录**
  - `service.go:5088`：`normalizeSiteRuleList` 的去重键是 `ruleType + "|" + domain` —— 同样允许 `(example.com, suffix)` 与 `(example.com, exact)` 并存
- **描述**：后端明确支持同一域名两种匹配类型各存一条（`buildSiteRulePlan` 还专门让 exact 排在 suffix 前面，说明这是设计内用法），但前端列表 key 只用了 `domain`。两条记录的 key 完全相同。
- **触发条件**：设置页 → 添加 `example.com`（默认「域名后缀」）→ 再添加 `example.com` 并选「精确域名」。
- **实际影响**：React 报 "Encountered two children with the same key"，两条中的一条在列表里被覆盖/丢失（具体是"少渲染一条"还是"渲染位置串位"取决于 diff，需真机确认）。后果是用户**在 UI 里看不到也删不掉第二条规则**，而它实际参与分流 —— 排障时会出现"我没设过这个域名却走了直连"。注意 `onUpdateSitePolicy` 的 `siteRowBusy` key（`SettingsPanel.tsx:242`）用的是 `domain|type`，是对的，问题只在 JSX key。
- **根因**：JSX key 用了"半个主键"。
- **修复建议**：`key={`${r.type}|${r.domain}`}`。
- **测试方案**：添加同域名 exact+suffix 两条规则，断言列表渲染 2 条且各自可独立改出口/删除；React 控制台无 duplicate key 警告。
- **置信度**：高（key 冲突由代码直接决定；具体表现标注「静态推断，需真机验证」）

---

### A5-07 · P3 · 没有 ESLint：`npm run lint` 实为 `tsc --noEmit`，`react-hooks/exhaustive-deps` 与 hooks 规则完全无人拦截

- **文件**：`frontend/package.json:15`、`frontend/tsconfig.json:17-22`
- **代码位置**：`"lint": "tsc --noEmit"`；tsconfig `noUnusedLocals: true`、`noImplicitAny: false`
- **描述**：`lint` 只做类型检查。`react-hooks/exhaustive-deps`、`react-hooks/rules-of-hooks`、`no-floating-promises`、`jsx-a11y` 等全部缺失。`noImplicitAny: false` 让 `App.tsx:311`（`payload: any`）、`NodesPanel.tsx:148`（`(it: any, i: number)`）、`App.tsx:166`（`Record<string, any>`）这类 `any` 静默通过。
- **触发条件**：任何后续改动。
- **实际影响**：这不是"未来可能"，而是已经在掩盖真实问题：`App.tsx:228-269` 的轮询 effect 依赖只有 `[hidden]`，却在内部调用每次 render 重建的 `applyPolledStatus`（`App.tsx:1040`）与 `applyPolledProcRules`（`App.tsx:1050`）；`App.tsx:840-865` 的快捷键 effect 依赖 `[busy, testing, status.proxyOn]`，却调用每次 render 重建的 `onTestAll`。目前靠"闭包里只用了 ref / useState setter"侥幸安全，但 `exhaustive-deps` 缺失意味着**下一次有人在这两个 effect 里引用一个新的 state，就会静默产生 stale closure**（表现为"设置页改了阈值但快捷键仍按旧值判断"这类难查 bug）。
- **根因**：项目从未引入 ESLint，用 tsc 顶替。
- **修复建议**：加 `eslint` + `eslint-plugin-react-hooks` + `@typescript-eslint`，`lint` 改成 `eslint . && tsc --noEmit`；逐步把 `noImplicitAny` 打开到 `true`（先对 `src/` 开）。
- **测试方案**：CI 加 `npm run lint`，故意写一个缺依赖的 `useEffect` 应报错。
- **置信度**：高

---

### A5-08 · P3 · 导入配置无二次确认、无大小上限、双击会弹两次文件框

- **文件**：`frontend/src/App.tsx:950-967`、`frontend/src/components/SettingsPanel.tsx:522-524`、`service.go:2760-2812`
- **函数/组件**：`App.onImportConfig`；`SettingsPanel` 的「导入配置」按钮；`CoreService.ImportConfig`
- **代码位置**：
  - `SettingsPanel.tsx:522-524`：`<button className="btn" onClick={onImportConfig}>导入配置</button>` — 无 `disabled`、无 loading
  - `App.tsx:950-967`：每次点击 `document.createElement("input")` + `input.click()`，`onchange` 里 `await svc.ImportConfig(text)`
  - `service.go:2761-2767`：只校验 `json.Unmarshal` 成功与 `Version == 1`，**没有长度上限**
  - `service.go:2770-2810`：`s.subs = bundle.Subs` 直接替换、覆盖 `procRules`/`siteRules`/`ignored`，然后 `RestartKernel()`
- **描述**：导入是"整体覆盖 + 重启内核"的重操作，但按钮没有任何确认步骤；双击会创建两个 `<input>` 并各自弹一次系统文件框（第二个可能因为焦点被抢而无感），若两次都选中文件则会连续两次覆盖 + 两次重启内核。`file.text()` 把整个文件读进内存，后端再 `json.Unmarshal` 一份，几十 MB 的畸形文件会造成明显卡顿/OOM。
- **触发条件**：双击「导入配置」；或误选一个不是自己配置的 JSON（版本号同为 1，字段名碰巧对一些，就会清空订阅）。
- **实际影响**：订阅被覆盖后必须重新填 URL（token 需重新找），进程规则/网站规则全部丢失；连续两次导入会让内核重启两次。用户主动选文件降低了严重度，故定 P3。
- **根因**：缺少"破坏性操作确认"与输入尺寸边界。
- **修复建议**：`onImportConfig` 加 `window.confirm("导入会覆盖当前订阅与全部规则，确定？")`，并用一个 `importing` state 禁用按钮；`ImportConfig` 开头加 `if len(jsonStr) > 4<<20 { return fmt.Errorf("配置文件过大") }`；导入前把当前配置 `ExportConfig()` 备份到 `config-backup-<ts>.json`。
- **测试方案**：单测传 8 MB 字符串断言被拒；GUI 层双击只弹一次框。
- **置信度**：高

---

### A5-09 · P3 · `onSetProc` 不走 `busy`，进程策略可并发提交，同一进程的连接会被断两次

- **文件**：`frontend/src/App.tsx:691-720`、`frontend/src/components/ConnectionsPanel.tsx:201-212`、`service.go:5867-5975`
- **代码位置**：`ConnectionsPanel.tsx:206-207` `onClick={() => onSetProc(key, p)} disabled={busy}`；`App.tsx:702` `void (async () => { … await svc.SetProcessRule(proc, policy) … })()` — **全程没有 `setBusy`**
- **描述**：按钮的 `disabled={busy}` 依赖的是 `run()` 维护的全局计数，而 `onSetProc` 自己不用 `run`，所以 `busy` 恒为它进入前的值，快速连点「代理 / 直连 / 跟随规则」会把多个 `SetProcessRule` 并发发出。
- **触发条件**：在连接页对同一程序快速点不同策略。
- **实际影响**：后端 `rulesMu` 串行（`service.go:5902`），所以规则最终值是最后完成的那次（通常等于最后点击），**数据不会错**；但每次 target 相关变更都会 `closeProcessConnections`（`service.go:5962`），该程序的连接被断 2~3 次，且中间可能闪一下。`procGuardRef` 4 s 守卫在 `target:` 变更（要 reload config，可能 >4 s）时可能先过期，导致界面短暂回弹到旧值，再等下一轮 3 s 轮询纠正。
- **根因**：乐观更新路径与全局 busy 计数是两套机制，没有交汇。
- **修复建议**：`onSetProc` 也 `setBusy(true/false)`（或给该行单独加 `procRowBusy`），并把 `procGuardRef` 的 `until` 从 4 s 改为"直到请求返回"。
- **测试方案**：GUI 连点两次不同策略，debug.log 中该进程的 `closeProcessConnections` 只应出现 1 次。
- **置信度**：中高（并发与断连路径确认；"断两次"的观感需真机）

---

### A5-10 · P3 · 托盘切换节点不写 `rememberSelection`，与主窗口 `SelectNode` 的持久化语义不一致

- **文件**：`main.go:437`、`service.go:3331-3356`、`service.go:4768-4794`
- **代码位置**：`main.go:437` `svc.QuickSelectNode(n)`；`service.go:3355` 结尾直接 `return s.api.Select(ctx2, group, name)` —— **没有 `rememberSelection`**；对照 `service.go:4778` `SelectNode` 成功后会记
- **描述**：同一个"切节点"动作有两个入口，其中一个不落盘。
- **触发条件**：托盘右键 → 切换节点 → 选 B；然后退出并重开（或点停止/启动内核）。
- **实际影响**：`restoreSelections`（`service.go:4798`）把上次主窗口选的 A 写回去，用户发现"我刚在托盘选过 B，重启后变回 A"。另外 `pickSelectorGroup(proxies, name)` 选的是"包含该节点的成员最多的 Selector"，不一定是主策略组 —— 若该节点只存在于机场的业务组（如 "Netflix"），托盘切换改的是业务组而不是主出口，用户会以为"切了没反应"。
- **根因**：两处切节点实现各写一份，未复用。
- **修复建议**：`QuickSelectNode` 内部改调 `SelectNode(group, name)`（复用持久化与校验）；托盘菜单对"非主组节点"做提示或过滤。
- **测试方案**：单测 `QuickSelectNode` 后断言 `s.selections[group] == name`；重启后 `Nodes()` 的 `now` 不变。
- **置信度**：高

---

### A5-11 · P3 · 地图/轮询性能组：NodeMap 常驻且隐藏时仍跑 SMIL 动画、`onMouseMove` 每次全量重绘、两个 60 s 轮询不随隐藏降频、`targetLocs` 无界增长

- **文件**：`frontend/src/App.tsx:1196-1212`、`frontend/src/NodeMap.tsx:407-422`、`frontend/src/NodeMap.tsx:536-539`、`frontend/src/App.tsx:783-793`、`frontend/src/App.tsx:827-836`、`frontend/src/App.tsx:366`
- **代码位置与说明**：
  1. **常驻动画**：`App.tsx:1196` 用 `style={{ display: tab === "overview" ? "block" : "none" }}` 让 `NodeMap` 永不卸载（为了保住缩放状态）；`NodeMap.tsx:408-421` 的 `<animateMotion repeatCount="indefinite">` 在 `display:none` 下仍由浏览器继续推进。节点多、出口多时，切到「日志」「设置」页后仍有持续的 SVG 合成开销。
  2. **mousemove 全量重绘**：`NodeMap.tsx:536-542` 的 `onMouseMove={(e) => setTip((cur) => cur ? {...cur, x, y} : cur)}` 每个 mousemove 事件都 `setState`，导致整个 710 行组件（含 `placed` 的几十个 `<g>`、`labels`）重新执行与 diff。指针在地图上滑动时帧率明显下降（节点上百时尤甚）。
  3. **60 s 轮询不降频**：`App.tsx:791` 与 `App.tsx:834` 的 `setInterval(load, 60_000)` 在 `hidden` 时照跑（4 个 IPC/分钟，可接受但不符合 `POLL_*_HIDDEN_MS` 的设计意图）。
  4. **`targetLocs` 无界**：`App.tsx:366` `setTargetLocs((prev) => ({ ...prev, ...r }))` 只增不减，键是历史出现过的所有目标 IP；`queriedIps` 有 `GEOIP_CACHE_MAX=2000` 上限，`targetLocs` 没有。长跑数天（尤其 P2P/大流量场景）会持续增长到数千项，每轮还要整表拷贝。
- **触发条件**：长时间驻留、地图页悬停、切到非概览页。
- **实际影响**：CPU 占用与掉帧；内存缓慢增长（不构成泄漏，量级几十 KB~几 MB）。
- **根因**：为了保状态而 un-mount 规避，没有做"可见性暂停"与"节流"。
- **修复建议**：`display:none` 时给 `NodeMap` 传 `paused` 并把 `animateMotion` 的条件 `count` 置 0（或直接 `visibility:hidden` + 暂停 SMIL）；`onMouseMove` 用 `requestAnimationFrame` 合并到每帧一次；两个 60 s 轮询改为 `hidden ? 300000 : 60000`；`targetLocs` 加 LRU 上限（如 2000）。
- **测试方案**：DevTools Performance 录制地图悬停 3 s，检查脚本时间；内存快照对比 `targetLocs` 条目数。
- **置信度**：中（静态推断，帧率与 CPU 需真机验证）

---

### A5-12 · P3 · 入参校验缺口组：`SelectNode` 空值/长度、`SaveWindowBounds` 无范围、`SetTrafficAlertLimit` int64 溢出

- **文件**：`service.go:4768`、`service.go:5448-5453`、`service.go:3063-3070`
- **代码位置与说明**：
  1. `SelectNode(group, name)` 不做任何 trim/长度/白名单校验，直接 `api.Select(ctx, group, name)`。`kernel/api.go:149` 用了 `url.PathEscape`，**因此没有路径注入**，但 `group == ""` 会打成 `PUT /proxies/` 得到 404（错误信息回显给用户），超长 name 会构造超长 URL。属"缺校验但无实质危害"。
  2. `SaveWindowBounds(x,y,w,h int)` 无范围校验，直接落盘。`main.go:233` 恢复时有 `w>0 && h>0` 与 `clampToVirtualScreen` 兜底，故实际不可利用。
  3. `SetTrafficAlertLimit(mb int64)` 只挡了负数，`mb * 1024 * 1024` 在 `mb > 8.7e12` 时溢出成负数 → `alertLimit <= 0` → 等价于关闭告警（无害，但语义错误）。
- **触发条件**：仅 DevTools/注入可绕过前端输入限制（`SettingsPanel.tsx:493-494` 已把非法值归一为 0）。
- **实际影响**：无安全影响；`SelectNode("")` 会把一条 404 文本显示给用户（"内核 API 返回 404: …"）。
- **根因**：内部方法按"前端已经校验过"假设编写。
- **修复建议**：`SelectNode` 加 `if strings.TrimSpace(group)=="" || strings.TrimSpace(name)=="" { return fmt.Errorf("策略组或节点名为空") }` 与长度上限；`SetTrafficAlertLimit` 用 `if mb > 1<<20 { mb = 1<<20 }`（1 TB）夹紧。
- **测试方案**：单测空参数返回明确错误；`SetTrafficAlertLimit(math.MaxInt64)` 后 `AlertLimit()` 应为正数。
- **置信度**：高

---

### A5-13 · P3 · 敏感信息落盘：`ExportConfig` 含订阅 token、`debug.log` 记录订阅 URL、`banner` 显示本机路径

- **文件**：`service.go:2734-2741`、`service.go:5529`、`frontend/src/App.tsx:1159`、`service.go:1394`
- **代码位置与说明**：
  1. `ExportConfig` 的 `ConfigBundle.Subs` 是 `[]subMeta`，含 `URL`（带 token）。导出的 JSON 一旦被分享（"把我的配置发你看看"），订阅凭据即泄漏。属功能设计，但缺少提示。
  2. `service.go:5529` `s.dbg("auto refresh skipped (%s): %v", m.URL, err)` 把含 token 的 URL 写进 `dataDir/debug.log`（该文件不在 `ExportLogs` 范围内，但「打开数据目录」可直接看到）。
  3. `App.tsx:1159` `banner = err || status.bootErr || status.error`，`status.error` 来自 `ks.Error`（内核启动失败原文，含配置文件绝对路径、`dataDir`），会整段显示在页面顶部。
- **触发条件**：用户导出配置分享；用户查看 debug.log；内核启动失败。
- **实际影响**：本机路径暴露（低）、订阅 token 随备份文件外流（中，但需用户主动分享）。
- **根因**：导出即全量、日志不打码。
- **修复建议**：`ExportConfig` 前在 UI 提示"备份包含订阅完整链接，请妥善保管"，或提供"脱敏导出"选项；`dbg` 里对 URL 做 `?…` 掩码。
- **测试方案**：导出文件 grep 不到 `token=`（脱敏模式）；`debug.log` 断言不含 query。
- **置信度**：高

---

### A5-14 · P3 · `style.css?v=1.1.3` 硬编码，与 `package.json` 的 1.6.0 脱节，改样式可能被 WebView2 缓存

- **文件**：`frontend/index.html:10`、`frontend/dist/index.html:10`、`main.go:321-331`
- **代码位置**：`<link rel="stylesheet" href="/style.css?v=1.1.3" />`；`noCacheHTML` 只对 `""`/`/`/`*.html` 下 `Cache-Control: no-cache`，静态资源仍走正常缓存
- **描述**：JS 由 Vite 生成带 hash 的文件名（`index-CwTU2sY_.js`），天然不受缓存影响；CSS 放在 `public/` 下原样拷贝，靠手动维护的 `?v=` 破缓存，而版本号停在 1.1.3（`package.json` 已是 1.6.0），说明升级时忘了改。
- **触发条件**：发布新版本且 `style.css` 内容变化、`?v=` 未变，且 WebView2 走了强缓存（有 `Last-Modified` 时通常会用条件请求，故不一定复现）。
- **实际影响**：升级后界面样式保持旧版（错位/缺样式），用户需清缓存。需真机验证是否真的命中缓存。
- **根因**：手动版本号破缓存，与自动化的 JS hash 机制不统一。
- **修复建议**：把 CSS 移到 `src/` 由 Vite 处理（`import "./style.css"`），或构建时用脚本把 `?v=` 替换为 `package.json` 的 version。
- **测试方案**：改一处 CSS → 重新打包 → 覆盖安装 → 检查界面是否更新。
- **置信度**：中（缓存命中与否标注「静态推断，需真机验证」）

---

### A5-15 · P3 · `bindings/routepilot/coreservice.js` 未随源码重新生成（缺 `IsShuttingDown`）

- **文件**：`frontend/bindings/routepilot/coreservice.js`（759 行）、`service.go:2697`
- **代码位置**：Go 侧 77 个导出方法，JS 侧 79 个函数；差集 = `IsShuttingDown`（Go 有、JS 无），反向差集为空
- **描述**：生成物与源码存在漂移。当前不会出错（前端没人调 `IsShuttingDown`，且 JS 里没有 Go 侧不存在的方法，所以**不存在"调用 undefined"的运行时炸点**），但说明 bindings 不是每次改动后重生成的，`Attach`/`Boot` 这类方法即使从 Go 侧删掉，JS 里仍会留着 ID 调用 → 运行时 `$Call.ByID` 返回 "no method found" 而不是编译期报错。
- **触发条件**：未来有人在前端调 `IsShuttingDown`，或后端删改方法后未重生成。
- **实际影响**：目前无；是流程风险。
- **修复建议**：把 `wails3 generate bindings` 加进构建脚本，并在 CI 里检查 `git diff --exit-code frontend/bindings`。
- **测试方案**：CI 生成后工作区应无 diff。
- **置信度**：高

---

### A5-16 · P3 · 单节点测速用单值锁，测速期间点其他节点静默无效；内核未运行时"改完即刻生效"文案误导

- **文件**：`frontend/src/App.tsx:988-999`、`frontend/src/NodeMap.tsx:674-701`、`frontend/src/components/ConnectionsPanel.tsx:164`、`service.go:5942-5944`
- **代码位置与说明**：
  1. `App.tsx:989` `if (testingNode) return;` —— `testingNode` 是单个字符串，测 A（最长 15 s，`service.go:4823`）期间点 B 的测速按钮直接 return，**没有任何提示**；`NodeMap.tsx:674` 的 `disabled={testing || !onTestNode}` 里 `testing = testingNode === n`，所以 B 的按钮看起来是可点的。
  2. `ConnectionsPanel.tsx:164` 的徽标写着「· 改完即刻生效」，但 `service.go:5942-5944` 在内核未运行时 `if s.api == nil || s.mgr == nil || !s.mgr.IsRunning() { return 0, nil }` —— 规则只落盘，不生效。同理 `SetSiteRuleWithType` → `applyGeneratedConfig` 在未运行时 `return nil`（`service.go:5803-5805`），前端提示却是"已按…设置网站出口"。
- **触发条件**：地图浮层正在测 A 时去点 B；内核停止状态下改进程/网站规则。
- **实际影响**：用户以为测速/设置失效。属误导性反馈，不致数据损坏。
- **修复建议**：`testingNode` 改成 `Set<string>` 支持多节点并发；测速按钮 `disabled` 与 `testingNode` 判定统一；未运行内核时把文案改成"已保存，启动内核后生效"。
- **测试方案**：GUI 依次点两个节点，两个都出现转圈；停内核后改规则，提示文案应含"启动后生效"。
- **置信度**：高

---

## 3. 导出面清单（Wails 可调用方法 × 风险 × 前置校验）

共 77 个 public 方法。下表按风险分组；「前置校验」指方法内部自身做的状态/入参检查。

### 3.1 高危 / 不该暴露（2 个 + 4 个生命周期）

| 方法 | 位置 | 风险 | 前置校验 | 备注 |
|---|---|---|---|---|
| `Shutdown()` | service.go:1327 | **高** | 无 | 关内核 + 还原代理 + `shuttingDown=true` → 之后无法再启动内核（A5-02） |
| `MarkQuitting()` | service.go:2703 | **高** | 无 | `quitting=true` → 所有生命周期操作被拒，进程却不退出（A5-02） |
| `Boot()` | service.go:608 | 中 | 无 | 重复 Boot 会重复初始化（前端从不调用） |
| `Attach(app)` | service.go:1320 | 中 | 无 | 参数是 `*application.App`，前端传值会反序列化失败，实际不可用但暴露在面上 |
| `Quitting()` / `IsShuttingDown()` | service.go:2690/2697 | 低 | — | 只读，可保留 |

### 3.2 重副作用但**有**前置校验（安全）

| 方法 | 位置 | 前置校验 |
|---|---|---|
| `SetTUN(on)` | service.go:2208 | `admin()` + `kernelRunning()` + `tunChanging` 互斥 + 完整回滚事务 |
| `SetSystemProxy(on)` | service.go:1628 | 退出态拒绝 / 内核未运行拒绝 / TUN 占用拒绝 / TUN 切换中拒绝 / 备份 Pending 拒绝 |
| `SetMode(mode)` | service.go:3914 | `api==nil` 拒绝 + 枚举白名单；进 global 前兜底 GLOBAL 出口 |
| `UpdateCore(url)` | service.go:3407 | 必须等于官方 release URL + digest/size 非空 + 有更新才执行；失败自动回滚 |
| `ResetCore()` | service.go:3517 | 重置后 `EnsureBinary` 补回内置内核再重启 |
| `RestartKernel()` / `StartKernel()` | 1515/1432 | `lifeMu` 串行 + 退出态拒绝 + 状态机检查（AlreadyRunning/Busy） |
| `SetSiteRuleWithType` | 5675 | 域名白名单字符集 + 规则类型枚举 + policy 枚举 + 目标节点存在性 |
| `DeleteSiteRuleWithType` | 5728 | 类型枚举 + 域名规范化 |
| `SetProcessRule(proc,policy)` | 5867 | `procid.ParseKey` + policy 枚举 + target 存在性 + 禁用内部组前缀 |
| `SetSmartRouting(on)` | 5638 | 失败回滚开关 + 落盘 |
| `Subscribe(url)` | 4523 | scheme 白名单 + localhost/回环/私网/数字主机拒绝 + DNS 钉扎 + 每跳重校验 |
| `RemoveSubscriptionByURL` | 4322 | URL 定位 + 存在性 |
| `SetHomeLocation` | 3167 | 经纬度范围 |
| `SetAutoStart` | 3547 | — |

### 3.3 有重副作用但**无**二次确认（前端侧）

| 前端动作 | 位置 | 后端后果 | 确认 |
|---|---|---|---|
| 点已激活的订阅卡片 | NodesPanel.tsx:156 | RestartKernel，断全部连接 | **无**（A5-04） |
| 「导入配置」 | SettingsPanel.tsx:522 | 覆盖订阅+规则+RestartKernel | **无**（A5-08） |
| 「以管理员重启」 | App.tsx:423 / SettingsPanel.tsx:344 | 当前进程退出、新进程提权 | 无（用户主动点，可接受） |
| 「回退内置」 | App.tsx:917 | 删内核文件 + 重启 | 有 `window.confirm` |
| 「升级内核」 | App.tsx:886 | 停内核 + 换文件 + 重启 | 有 `window.confirm` |
| 「删除订阅」 | App.tsx:474 | 删本地配置 | 有 `window.confirm` |
| 「全部断开」 | App.tsx:738 | 关闭所有连接 | 有 `window.confirm` |

### 3.4 只读 / 低风险（其余约 50 个）

`Status` `Traffic` `TrafficHistory` `Connections` `Nodes` `Subscription` `ListSubscriptions` `ProcessRules` `SiteRules` `SmartRouting` `HomeLocation` `FavoriteNodes` `IsFavorite` `ToggleFavorite` `NodeDelayHistory` `ListRules` `TrafficAlertLimit` `AutoStart` `IsAdmin` `Mode` `CoreCustomized` `CheckCoreUpdate` `QuickNodes` `SystemProxyState` `TUNState` `ProxyEnabled` `WindowBounds` `ExportLogs` `Logs` `LogsSince` `TestNode` `TestAllNodes` `ResolveTargets` `IgnoredProcesses` `ExportConfig`（读，但含 token，见 A5-13）
写操作：`ClearLogs` `ClearSubscription` `CloseConnection` `CloseAllConnections` `CopySubscriptionURL` `SaveWindowBounds` `SetTrafficAlertLimit` `SetSubInsecure` `SwitchSubscription(ByURL)` `RefreshSubscription` `IgnoreProcess` `OpenDataDir` `RelaunchElevated`、`SelectNode`/`QuickSelectNode`（A5-05/A5-10）

> `CloseConnection(id)` 无 id 校验，但 `kernel/api.go:231` 用 `url.PathEscape(id)`，注入面已封堵。
> `OpenDataDir()` 用 `exec.Command("explorer", dir)`（`service.go:3994`），参数分离不经过 shell，`dir` 来自固定数据目录，**无命令注入**。

---

## 4. 轮询 / 事件时序表

### 4.1 定时器

| 来源 | 位置 | 周期（可见 / 隐藏） | 拉取内容 | 清理 |
|---|---|---|---|---|
| 主轮询 | App.tsx:228-269 | 1200 ms / 10000 ms | `Status` `Traffic` `Connections` `LogsSince(seq)`（4 个 IPC，并发） | `clearInterval` + `alive` 标志 |
| 列表轮询 | App.tsx:272-295 | 3000 ms / 30000 ms | `Nodes` `Subscription` `ProcessRules` `ListSubscriptions` | 同上 |
| 流量历史 | App.tsx:783-793 | 60 s **不随隐藏降频** | `TrafficHistory` | `clearInterval` |
| 延迟历史 | App.tsx:827-836 | 60 s **不随隐藏降频** | `NodeDelayHistory` | `clearInterval` |
| 图表时钟 | TrafficChart.tsx:31-34 | 30 s（`CHART_TICK_MS`） | 无（本地 tick） | `clearInterval` |
| 后端订阅自更新 | service.go:5491-5504 | 10 min（ticker） | 拉取到期订阅 | 退出时 return |
| 后端托盘状态 | main.go:469-512 | 10 s | 本地 `Status()` / 节点签名 | 退出时 return |

### 4.2 事件订阅（App.tsx:310-343）

| 事件 | 后端 Emit | 前端处理 | 解绑 |
|---|---|---|---|
| `sub-auto-updated` | service.go:5555 | toast + 系统通知 | `offSub()` ✅ |
| `kernel-crashed` | kernel manager | `setErr` + toast（8 s） | `offCrash()` ✅ |
| `core-update-progress` | service.go:3454（节流 250 ms） | `setCoreProgress` | `offCoreProgress()` ✅ |
| `network-degraded` | watchdog | `setErr` + toast | `offNet()` ✅ |
| `traffic-alert` | service.go:3118 | **前端未订阅**（`main.go:92` 已 RegisterEvent，但没有 `Events.On`） | — |

**结论**：
- 事件全部正确 `off` 解绑，`StrictMode`（main.tsx:7）下开发模式双挂载/卸载不会残留重复监听；生产模式不双跑。
- `traffic-alert` 注册了但前端不用 —— 后端 Emit 的内容只出现在内核日志里，**"流量超阈值"用户看不到系统通知**。这是一个功能缺口（未列入问题清单，因影响仅为提醒缺失，可归入 A5-13 一类的"功能未闭环"，建议后续补 `Events.On("traffic-alert")` + `notify()`）。

### 4.3 事件与轮询的时序关系

- `kernel-crashed` → `setErr(msg)`（8 s 自动清除）与下一轮 `Status()`（1.2 s 内）的 `st.Error` 会**同时存在**：`banner = err || bootErr || error` 中 `err` 优先，所以用户看到事件原文；8 s 后若内核仍未恢复，`banner` 切到 `status.error`（kernel manager 的错误）。二者不打架，只是文案会切换一次。
- `core-update-progress` 只影响设置页按钮文案（`SettingsPanel.tsx:644`），与 `Status().version` 轮询互不干扰；`UpdateCore` 返回后 `finally` 清空进度（`App.tsx:912-913`）。
- 隐藏窗口期间（10 s 轮询）用户通过托盘改节点/模式，界面最多滞后 10 s 更新；无"旧状态当新状态"的持久错位，因为每轮是整表覆盖。

### 4.4 乐观更新守卫

| 守卫 | 位置 | 生效条件 | 失效风险 |
|---|---|---|---|
| `modeGuardRef` | App.tsx:1037-1048 | 4 s 内或后端确认后解除 | 无（`SetMode` 是 PATCH，通常 <100 ms） |
| `procGuardRef` | App.tsx:1038, 1050-1067 | 4 s 内或后端确认后解除 | `target:` 变更要走 reload config（可能 >4 s）→ 界面短暂回弹（A5-09） |

---

## 5. 已排除的疑点（逐条给依据）

| 疑点 | 结论 | 依据 |
|---|---|---|
| XSS（日志/节点名/订阅备注渲染） | **不成立** | 全 `src/` 无 `dangerouslySetInnerHTML` / `innerHTML` / `eval` / `new Function` / `document.write`（全量 grep 0 命中）；`LogPanel.tsx:114-117` 用 `{line}` 文本插值，React 自动转义；`NodeMap.tsx:574-587` 的 `<text>` 同样；无 markdown 渲染、无 URL 化链接 |
| 命令注入（netsh/taskkill/explorer） | **不成立** | `exec.Command("explorer", dir)`（service.go:3994）参数分离不经 shell；`dir` 来自固定 `dataDir()`；无 `cmd /c` 拼接；Go 侧未发现 `exec.Command("cmd"` / `sh -c` |
| YAML / 配置注入（域名、进程名、节点名） | **不成立** | `normalizeSiteDomain`（service.go:969-1003）只允许 `[a-z0-9-]` 与 `.`，显式拒绝 `, \n \r \t \x00` 与 `/ \ :`；`procid.validateNamePayload`/`validatePathPayload`（procid.go:222-243）拒绝逗号与控制字符；`normalizeSitePolicy`（service.go:5789）与 `validateProcessTarget`（service.go:5983）拒绝 `,\n\r\t\x00` 与内部组前缀 |
| 内核 API 路径注入 | **不成立** | `internal/kernel/api.go` 全部使用 `url.PathEscape` / `url.QueryEscape`（L149、L158-159、L231、L243、L280-281） |
| SSRF（订阅 URL） | **已修，不成立** | `subscription/guard.go`：协议白名单 http/https（L101）、localhost/IPv6 zone/数字主机拒绝（L108-123）、`checkPublicIP` 拒绝回环/私网/链路本地/组播/CGNAT/文档段（L128-147）、`resolvePublicIPs` 任一结果内网即整体拒绝（L152-176）、拨号用已校验 IP 防 DNS rebinding（L181-208）、每一跳重定向重新完整校验（L109-113） |
| 事件未解绑导致重复注册 | **不成立** | App.tsx:337-342 四个 `off*()` 齐全 |
| `setInterval` 泄漏 | **不成立** | 6 处 setInterval 全部在 cleanup 里 `clearInterval`（App.tsx:267/293/792/835、TrafficChart.tsx:33、NodeMap 无 interval） |
| 流量历史 / 日志无界增长 | **不成立** | 后端 `historyKeep = 10080`（service.go:3787）裁剪；`LogBuffer.max = 400`（manager.go:100-118）裁剪；前端 `LOG_UI_MAX = 500`（App.tsx:27）；`delayHistoryKeep = 100`（service.go:2823） |
| 非管理员开 TUN | **不成立** | `setTUN` 首行 `if on && !s.admin() { return 错误 }`（service.go:2221-2223） |
| 未启动内核就改进程规则「静默成功」 | **部分成立** | 返回 `(0, nil)`（service.go:5942-5944）确实静默，但规则已落盘，且 UI 文案误导（已记 A5-16）；不是"丢了操作" |
| 前端可调 `SaveWindowBounds` 造成窗口跑到屏幕外 | **不成立** | `main.go:233-237` 恢复时 `w>0 && h>0` + `clampToVirtualScreen` 双重兜底 |
| `sub.error` 把 token 渲染到界面 | **不成立** | `sub.error` 只在 NodesPanel.tsx:274 做 `includes("证书")` 判断，未渲染；真实泄漏出口是 A5-03 的 `err`→banner |
| props drilling 导致不必要重渲染 | **不成立** | `nodesProps`/`settingsProps` 每次 render 新建对象会让 `NodesPanel`/`SettingsPanel` 每次都重渲染，但它们内部全是 useMemo 与廉价计算，1.2 s 一次可接受 |
| `npm ci` 无法复现 / lock 漂移 | **不成立** | lockfileVersion 3，`packages[""]` 的 deps/devDeps 与 `package.json` 完全一致；vite 8.3.6 与 `@vitejs/plugin-react` 6.2.1 的 peer `vite: ^8.0.0` 满足；Node `engines >=20.19` 与 vite 8 要求（^20.19 / >=22.12）一致 |
| React 18 / vite 8 / plugin-react 6 / @wailsio/runtime 3.1.0 版本不兼容 | **不成立** | 上述 peer 检查通过；`@wailsio/runtime` 3.1.0 与后端 wails v3.1.0 同版本 |
| `main.go` 的 `AssetServer` 暴露本地文件 | **不成立** | `AssetFileServerFS(assets)`（main.go:195）只服务 `go:embed all:frontend/dist` 的只读 FS |
| 窗口关闭导致应用退出（代理断掉） | **不成立** | `DisableQuitOnLastWindowClosed: true`（main.go:201）+ `WndProcInterceptor` 拦 WM_CLOSE（main.go:205-213）+ `WindowClosing` 双保险（main.go:259-266） |

---

## 6. 未覆盖部分 + 需真机验证项

### 6.1 未覆盖

1. **`node_modules` 内的实际代码**（只核对了 lock 版本与 peer 关系，未审第三方实现）。
2. **`service.go` 的 6000 行中与本次专题无关的部分**：TUN 注册表事务、WinHTTP 备份/恢复、看门狗、`procgen` 生成器内部、`configgen` 的 YAML 生成细节（这些属其他子 Agent 的后端/生命周期专项）。本报告只审到"从这些入口进入时的前置校验与错误文本"。
3. **`frontend/dist` 产物与源码的一致性**（dist 未纳入 git，靠构建时生成；主控的 `npm run build` 会是最终判据）。
4. **`assets/` 内嵌二进制**（mihomo.exe / wintun.dll / geo 库）的来源与完整性（由 `fetch-assets.py` 的 manifest 哈希负责，未验）。
5. **msix / 安装器（`routepilot-installer.nsi`）** 的提权与升级路径。

### 6.2 需真机验证（本环境无 GUI）

| 编号 | 待验证内容 | 验证方法 |
|---|---|---|
| V1 | 隐藏窗口（`mainWindow.Hide()`）后 `document.visibilityState` 是否变 `"hidden"`，降频是否真的生效 | 打开 DevTools（需先修 A5-01）观察 `hidden` state 与请求间隔 |
| V2 | A5-06 的 key 冲突实际表现（少渲染一条 / 串位 / 删错） | 添加同域名 exact+suffix 两条规则，看列表与删除行为 |
| V3 | A5-05 竞态的可复现程度（连点 3 个节点，最终停在哪个） | 在节点页快速连点，观察 `mainGroup.now` 与 `selections`（重启后） |
| V4 | A5-14 的 CSS 缓存是否真的命中（覆盖安装后样式是否旧） | 改一处颜色重新打包覆盖安装 |
| V5 | A5-11 的地图悬停帧率与隐藏页 CPU | DevTools Performance / 任务管理器 |
| V6 | A5-01 修复前按 F12 是否真能打开 DevTools（Release 包） | 用 `package.py` 产物实测 |
| V7 | `traffic-alert` 未订阅导致的"流量告警无提示" | 设 1 MB 阈值后刷流量，观察是否有系统通知 |
