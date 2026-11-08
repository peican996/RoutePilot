# NexusLink（proxdeck）代码审查报告

- **审查日期**：2026-11-08
- **审查范围**：`main.go`、`service.go`、`internal/*`（kernel / configgen / subscription / sysproxy / elevate / autostart / geoip）、`frontend/src/*`、打包脚本（package.py / nexuslink-installer.nsi）
- **规模**：Go 后端约 5,800 行，前端约 3,300 行
- **问题统计**：共 25 项 —— 严重 6、中等 12、轻微 7

**总体评价**：架构思路清晰，注释质量高（深拷贝防规则叠加、Job Object 防孤儿进程、系统代理备份还原等都考虑到了），但存在若干直接影响核心功能的真 bug，以及几处安全面偏松的设计。建议按"严重 → 中等 → 轻微"顺序修复。

---

## 一、严重（6 项，建议立即修复）

### S1. TUN 开关在会话内不生效

- **位置**：`service.go:324`（`options()`）、`service.go:640-688`（`SetTUN`）
- **问题**：`options()` 生成内核配置时用 `wantTUN := s.restoreTUN && elevate.IsElevated()`，而 `restoreTUN` 只在 `loadAppState()`（启动时）从磁盘赋值。`SetTUN` 只改 `s.tunOn`，从不改 `s.restoreTUN`。
- **后果**：本次会话内开 TUN → `RestartKernel` 生成的配置里没有 TUN，但界面显示已开启；关 TUN 同理关不掉。只有重启应用（让 `loadAppState` 把 `tunOn` 读回 `restoreTUN`）才生效。UI 状态与实际完全脱节。
- **修复建议**：`options()` 改用当前期望态 `s.tunOn`（或让 `SetTUN` 同时更新 `restoreTUN`）；`restoreTUN` 仅作为"启动时是否需要自动恢复"的标记，配置生成永远以 `s.tunOn` 为准。

### S2. 停内核会误关其他代理软件的系统代理

- **位置**：`service.go:473-489`（`StopKernel`）、`service.go:563-575`（`restoreSystemProxy`）
- **问题**：`StopKernel` 无条件调 `SetSystemProxy(false)` → `restoreSystemProxy`。当 `backup` 为空（即我们从未接管过系统代理）时，仍然执行 `sysproxy.Disable()`。
- **后果**：用户机器上如果开着 Clash Verge 等其他代理，点"停止内核"/升级内核/回退内核时，别人的系统代理被直接关掉。`UpdateCore`、`ResetCore` 也走这条路。
- **修复建议**：`restoreSystemProxy` 开头判断 `if !bk.Saved { return nil }`——没有接管记录就什么都不碰。`StopKernel` 里只在 `s.proxyOn` 为 true 时才还原。

### S3. 内核升级"下载即替换"，无任何来源校验

- **位置**：`internal/kernel/update.go:137-186`（`DownloadAndReplace`）、`service.go:1131-1155`（`UpdateCore`）
- **问题**：`UpdateCore(url)` 接受前端传入的任意 URL，直接 HTTP GET、解 zip、取出 `mihomo.exe` 覆盖执行文件。不限制域名为 GitHub，不比对 release 摘要/签名（前端显示的 sha256 是下载后自己算的，只能展示不能验证）。
- **后果**：前端被注入、或调用方传错/被篡改的 URL，即构成任意代码执行（以用户权限运行的代理程序，TUN 场景下还是管理员权限）。
- **修复建议**：
  1. URL 白名单：只允许 `https://github.com/MetaCubeX/mihomo/...` 与 `https://objects.githubusercontent.com/...`（跟随重定向后再次校验最终 host）；
  2. 下载后校验 GitHub release 返回的 digest（新版 release API 提供 `digest: sha256:...`），不匹配则拒绝替换；
  3. 兜底：至少把 URL 校验收敛到后端（前端只传版本号，URL 由后端 `CheckUpdate` 结果决定）。

### S4. 地图地名子串误匹配，定位系统性错乱

- **位置**：`frontend/src/mapData.ts:58-70`（`resolvePlace`）及 `:17-52` 的地名表
- **问题**：用 `includes` 做子串匹配，2 字母短 key（`US`/`MY`/`NY`/`IN`/`KR`/`LA` 等）会命中无关单词。
- **后果**（实测可复现）：`GERMANY` 含 `NY`/`MY` → 落到纽约；`AUSTRALIA`/`RUSSIA` 含 `US` → 落到美国；`CHINA` 含 `IN` → 落到印度；`UKRAINE` 含 `KR` → 落到韩国；`POLAND`/`IRELAND` 含 `LA` → 落到洛杉矶。地图目标点大面积标错。
- **修复建议**：改为按分隔符切词后做**全等 token 匹配**（`text.split(/[^a-zA-Z]+/)` 后比较），或给短 key 加词边界匹配；等长 key 冲突时优先最长匹配。

### S5. GeoIP 查询竞态导致地图落点永久丢失

- **位置**：`frontend/src/App.tsx:522-538`
- **问题**：`conns` 每 1.2s 变化即重跑 effect；前一次 `ResolveTargets` 未返回时 cleanup 置 `alive=false` 直接丢弃结果，但 IP 已在 `queriedIps` 中标记，永远不再重查；`.catch(() => {})` 失败同样不回退。
- **后果**：地图第二段路径的落点随机、永久缺失。
- **修复建议**：结果被丢弃或请求失败时把该批 IP 从 `queriedIps` 移除；或按请求序号只采纳最后一次结果（丢弃的视为未查过）。

### S6. 流量告警：NaN 写入 + 永不按日重置 + 数据竞争

- **位置**：`service.go:908-948`（`trafficAlertSent`、`checkTrafficAlertToday`）、`frontend/src/App.tsx:1942-1954`
- **问题**：
  1. `trafficAlertSent` 是无锁的包级全局变量，多 goroutine 读写构成数据竞争；
  2. 置 `true` 后没有任何按日期重置的逻辑——整个进程生命周期只告警一次，不是"当日告警一次"；
  3. 前端 `parseInt(el.value || "0", 10)` 对非数字输入得 `NaN`，无校验直接调 `SetTrafficAlertLimit(NaN)`，且 `defaultValue` + 命令式取 DOM 值，保存后不回显。
- **修复建议**：把 `trafficAlertSent` 收进 `CoreService` 并加锁，记录"已告警日期"，跨天自动复位；前端用受控输入 + `Number.isFinite` 校验，非法定为 0。

---

## 二、中等（12 项，建议尽快修复）

### M1. 系统代理备份不保存绕过列表（ProxyOverride）

- **位置**：`internal/sysproxy/sysproxy_windows.go:29-49`
- **问题**：`Enable()` 无条件把 `ProxyOverride` 覆盖为固定 bypass 串；`proxyBackup` 只存 Server/Enabled。还原时用户自定义的代理绕过列表永久丢失。
- **修复建议**：备份结构里加 `Override string`，接管前读出保存，还原时写回。

### M2. `rule-providers` 被整体覆盖

- **位置**：`internal/configgen/gen.go:199`
- **问题**：`cfg["rule-providers"] = providers` 直接替换，订阅自带的 rule-provider 全部丢弃；订阅规则里引用的 `RULE-SET` 指向不存在的 provider，内核可能拒绝启动。
- **修复建议**：合并——先取订阅原有 `rule-providers` map，再写入 `pp-direct`/`pp-proxy`（冲突时用我们的名字空间避免撞名）。

### M3. 策略组选择靠 map 随机取"第一个"

- **位置**：`service.go:1036-1047`（`QuickNodes`）、`service.go:1104-1110`（`QuickSelectNode`）
- **问题**：遍历 `map[string]Proxy` 找第一个 `Selector` 组，Go map 迭代顺序随机；多策略组订阅会选错组或每次不同。
- **修复建议**：复用 `configgen.MainProxyGroup`（按成员数最多的 select 组判定）或按名称确定性排序后取第一个。

### M4. 新程序检测整条功能是死代码

- **位置**：`service.go:2064`（`startProcessWatcher`）
- **问题**：函数定义了但**从未被调用**（`Boot()` 里没有它），`new-process` 事件注册了也没有发送方——"学习式分流"提示框永远弹不出来。
- **修复建议**：在 `Boot()` 末尾调用 `s.startProcessWatcher()`；如果功能已废弃，连同 `detectNewProcs`、事件注册、前端监听一并删除。

### M5. 三个事件未注册，通知可能到不了前端

- **位置**：`main.go:63-67` vs `service.go:849/854/896/946`
- **问题**：只 `RegisterEvent` 了 `new-process` / `sub-auto-updated`；`kernel-crashed`、`network-degraded`、`traffic-alert` 直接 `Emit`。需实测 Wails v3 未注册事件能否被前端 `Events.On` 收到。
- **修复建议**：三个事件补 `application.RegisterEvent[string](...)`；若实测能收到可改为加注释说明，但显式注册更稳妥。

### M6. 订阅拉取静默降级跳过证书校验

- **位置**：`internal/subscription/fetch.go:76-88`
- **问题**：正常 TLS 失败后自动用 `InsecureSkipVerify` 重试并继续使用返回内容。订阅含节点凭据，中间人可篡改规则与节点（虽有 `Insecure` 标记展示，但默认行为偏不安全）。
- **修复建议**：改为"失败后弹出确认，用户同意才用忽略证书的方式重试"；或至少在 UI 上把 Insecure 状态做醒目警告，且自动更新路径（`maybeAutoRefresh`）不允许静默降级。

### M7. 订阅管理用数组下标做标识（前后端都有竞态）

- **位置**：`frontend/src/App.tsx:619-649`、`service.go:1684-1739`（`RemoveSubscription`）
- **问题**：前端删除/切换传 `idx`，列表由 3s 轮询刷新，点击瞬间若列表换序会作用到错误的订阅；后端 `RemoveSubscription` 也有"校验下标后解锁、再加锁操作"的竞态窗口。
- **修复建议**：API 改为按 URL（或给 `subMeta` 加稳定 ID）传递；后端在同一个锁内完成校验与删除。

### M8. 导入配置后规则不生效

- **位置**：`service.go:751-772`（`ImportConfig`）
- **问题**：只写盘、`saveSubs`、`saveAppState`，不调 `writeProcRulesYAML`、不重启内核。导入的进程分流规则要等下次别的操作才落进 provider 文件。
- **修复建议**：导入成功后调用 `s.writeProcRulesYAML()`，并在内核运行时 `RestartKernel()`（或热刷新 provider）。

### M9. 前端轮询乱序覆盖 + 全局快捷键误触

- **位置**：`frontend/src/App.tsx:429-475`（tick）、`:953-962`（onKey）
- **问题**：轮询无请求序号，慢响应晚到会覆盖新数据；`Ctrl+Shift+P/T` 等全局快捷键不排除 `input/textarea` 焦点，打字时会误触发代理切换/全量测速。
- **修复建议**：每次请求带自增序号，只采纳最新一次结果；`onKey` 开头判断 `e.target` 是否为可编辑元素，是则 return。

### M10. 大量静默吞错与 unhandled rejection

- **位置**：`frontend/src/App.tsx` 多处（486、868-869、882、944、1584-1597、1818-1829、2216-2228 等）
- **问题**：`.catch(() => {})` 让 AutoStart / 流量历史 / 延迟历史 / 收藏 / GeoIP 失败完全无感知；`ToggleFavorite`（两处）、导出日志等裸 Promise 无 catch，拒绝时 unhandled rejection，收藏状态可能与后端不一致。
- **修复建议**：统一走一个 `safeCall` 包装（失败设 `err` 提示）；至少给所有 Promise 补 catch 并提示。

### M11. 单实例锁可被任意程序抢占，且失败无提示

- **位置**：`main.go:53-80`
- **问题**：用绑定 `127.0.0.1:47914` 实现单实例。任何本地进程先占住该端口，NexusLink 就永远起不来（重试 4.5s 后静默退出，只写 log）。
- **修复建议**：加一层命名互斥体（正确姿势：创建后立刻 `GetLastError`，或用 `CreateMutex` 返回值判断），端口方案保留为补充；启动失败时弹窗告知而不是静默退出。

### M12. 其他中等问题（打包条目）

| 问题 | 位置 | 修复建议 |
|---|---|---|
| API 密钥生成失败时回退常量 `"proxdeck"`，本机程序可预测 | `service.go:227-233` | 失败应直接报错拒绝启动，或用更充足的熵源重试 |
| 版本更新用 `lat != cur` 字符串不等判断，当前版本读不到时永远提示有更新 | `internal/kernel/update.go:107-115` | `cur == ""` 时不报 `HasUpdate`；改用语义化版本比较 |
| 流量图时间窗冻结在 `useMemo`（依赖仅 `data`），不随时间滚动；`preserveAspectRatio="none"` 拉伸刻度 | `frontend/src/TrafficChart.tsx:34-36, 125` | 加一个 30s 的 `now` state 进依赖；去掉 `preserveAspectRatio="none"` 或改为等比 |
| 全局 `busy` 单布尔无法表达并发操作，第一个 `finally` 就解锁 | `frontend/src/App.tsx:540-550` | 改为按操作 key 的计数/集合 |

---

## 三、轻微（7 项，体验与工程卫生）

### L1. 品牌/路径三处不一致，文档写错

- **位置**：`service.go:203-208`（数据目录 `%APPDATA%\ProxDeck`）、`internal/autostart`（注册表名 `ProxDeck`）、`package.py:73`（说明书）
- **问题**：产品叫 NexusLink，但数据目录、自启注册表名还叫 ProxDeck；`package.py` 生成的使用说明写 `%LOCALAPPDATA%\proxdeck\`——实际是 `%APPDATA%\ProxDeck`（Roaming，不是 Local，大小写也不同）。
- **修复建议**：统一命名（注意迁移：首次启动检测旧 `ProxDeck` 目录并搬迁）；至少先改对说明文档；NSIS 卸载器的 `$APPDATA\ProxDeck` 是对的，改名后要同步。

### L2. 仓库里堆着约 750MB 发布物

- **位置**：项目根目录
- **问题**：22 个 `NexusLink-v*-win64-portable.zip`（各约 34MB）全在根目录；`.gitignore` 不含 `*.zip`、`dist-release`、`*.syso`；`bin/` 下还有 `ProxDeck.exe~`、`NexusLink.exe~` 等备份残留。
- **修复建议**：zip 输出到 `release/` 或 `dist-release/` 并加进 `.gitignore`；历史 zip 移出仓库；清理 `*.exe~`。

### L3. App.tsx 2265 行单文件巨石

- **位置**：`frontend/src/App.tsx`
- **问题**：约 1900 行的单函数组件，订阅卡、右键菜单、规则列表、设置页全部内联；`fmtBytes` 在 `App.tsx:188` 与 `TrafficChart.tsx:13` 各写一份且口径不一致（`0` vs `"0 B"`）；`connDuration` 等工具函数散落组件体；日志/规则行用 `key={i}`。
- **修复建议**：按面板拆组件（订阅 / 连接 / 节点 / 设置），工具函数抽到 `utils.ts`，列表 key 改用稳定标识。

### L4. 日志全量高频轮询

- **位置**：`frontend/src/App.tsx:437`、`:1845`
- **问题**：`Logs()` 每 1.2s 全量拉整个日志数组再全量渲染，日志一多是明显的 IPC 与渲染开销；且 `:477-482` 强制滚底会打断用户回看。
- **修复建议**：后端给 `Logs(sinceSeq)` 增量接口或截断到最近 N 条；前端仅在"已接近底部"时自动跟随。

### L5. 测试与工程化几乎空白

- **位置**：`procname_test.go`（全项目唯一测试，34 行）、`frontend/package.json`
- **问题**：无 lint、无 CI；`package.json` 的 `name` 还是脚手架残留 `react-ts-latest`，未声明 Node 版本（vite 8 要求 Node ≥ 20.19）。
- **修复建议**：至少给 `configgen`、`sanitizeProcName`、`parseUserInfo` 这类纯函数补表驱动测试；加 `golangci-lint` + `tsc --noEmit`；修正 name、补 `engines`。

### L6. 地图本机位置硬编码东莞

- **位置**：`frontend/src/mapData.ts:73`（`HOME = {lon: 113.75, lat: 23.02}`）
- **问题**：非该地区用户的地图起点/连线全部偏移，无配置入口。
- **修复建议**：改为可配置项，或按 GeoIP 推断本机公网位置。

### L7. 其他前端小项

| 问题 | 位置 | 修复建议 |
|---|---|---|
| 提示 `window.setTimeout` 不清理、互相截断，卸载后仍 setState | `App.tsx:492-519` 等 | 保存句柄，新提示先 clear 旧的，卸载时清理 |
| 日志强制滚底打断用户回看 | `App.tsx:477-482` | 判断 `scrollHeight - scrollTop - clientHeight < 40` 才跟随 |
| `status/traffic/sub` 状态缺空值防御，`null` 会导致白屏 | `App.tsx:440-441` | 与 `conns/logs` 一样加 `Array.isArray`/兜底 |
| 右键菜单窄窗口定位到负坐标 | `App.tsx:2186-2187` | `Math.max(0, Math.min(x, innerWidth-210))` |
| 中文标签宽度按拉丁字母系数估算，避让失效 | `mapLayout.ts:23` | 按 CJK 字符宽度 ~fontSize 单独计算 |
| NodeMap 激活节点被合并进簇时高亮丢失、`dragRef` 渲染期读取 | `NodeMap.tsx:386-387, 285` | 簇内含 active 成员时也高亮；cursor 改用 state |
| 双重类型断言绕过编译器（`as unknown as`），手写重复类型 | `App.tsx:31-94, 132-186` | 直接使用 `bindings/proxdeck` 导出的模型类型 |
| 魔法数散落（轮询 1200/3000/10000/30000、合并半径 15 等） | 多处 | 提常量命名 |

---

## 四、已核查、未发现问题的点

- **XSS/注入面**：前端全部走 JSX 插值，无 `dangerouslySetInnerHTML` / `innerHTML` / `eval`；进程名写规则前有 `sanitizeProcName` 拦截逗号/换行/路径分隔符（`service.go:2264`），规则注入面是干净的。
- **配置生成防污染**：`configgen.deepCopy` 避免了往订阅原配置重复插 RULE-SET（注释与实现一致）。
- **子进程管理**：Job Object 挂接（`job_windows.go`）、`stopping` 标志区分主动停止与崩溃，重启看门狗带次数上限，逻辑正确。
- **前端清理**：`Events.On` 三个事件均保存并调用反注册；wheel / interval / visibilitychange 监听清理完整。
- **权限边界**：自启写 HKCU（不弹 UAC）、TUN 校验管理员、配置文件 0600 权限收紧，均正确。

---

## 五、修复优先级建议

1. **第一批（核心功能正确性）**：S1、S2、S4、S5、S6 —— 直接影响"TUN 能不能用、网会不会断、地图/告警对不对"。
2. **第二批（安全）**：S3、M6、M11 相关的密钥兜底。
3. **第三批（一致性/健壮性）**：M1、M2、M3、M4、M5、M7、M8、M9、M10。
4. **第四批（工程卫生）**：L1、L2、L5 优先于代码风格类的 L3/L4/L7。

---
---

# 第二轮复审（2026-11-08 15:30，针对修改后代码）

**验证手段**：`go build`/`go vet`/`go test` 全绿，`tsc --noEmit` 无类型错误；25 项历史问题逐项复核（18 项修复到位、6 项部分修复、M4 采用整体删除方案）。

**本轮新发现问题（按优先级）：**

## 严重（2 项，前后端契约错配）

### R1. 订阅到期时间单位错配：秒被当毫秒，全部显示"已过期（1970-xx-xx）"

- **位置**：`frontend/src/utils.ts:68-77`（`fmtExpire` 按毫秒解析）↔ `internal/subscription/fetch.go:43`（`Expire` 是 Unix **秒**，注释写明）；消费点 `frontend/src/components/NodesPanel.tsx:252`
- **问题**：整条链路（fetch → service → 前端）无 ×1000，`fmtExpire` 却按毫秒算 `new Date(ms)` 和天数。所有带 `expire` 头的订阅都显示 1970 年已过期、剩余天数为巨大负数。utils.ts:67 注释"接口给的是毫秒时间戳"本身是错的。
- **修法**：`fmtExpire` 入口做单位归一（`if (ms > 0 && ms < 1e12) ms *= 1000`）或后端统一转毫秒；两处选一处，同步修正注释。

### R2. ListSubscriptions 不返回 `insecure`，"恢复证书校验"按钮永不出现

- **位置**：`service.go:1897-1914`（items 缺 `insecure` 字段）↔ `frontend/src/components/NodesPanel.tsx:266-281`（`{it.insecure && …恢复按钮}`）
- **问题**：`subs.items` 来自轮询（3s 必非空），`it.insecure` 恒 undefined，恢复按钮仅启动头几秒走 `[sub]` 兜底时可见。用户点过"忽略证书校验并重试"后**永久跳过 TLS 校验且 UI 无法关闭**（安全设置单向锁死）。`App.tsx:154` 的 `items: any[]` 让类型系统全程沉默。
- **修法**：`ListSubscriptions` 的 map 补 `"insecure": m.Insecure`；把 `subs.items` 从 `any[]` 收紧为真实结构体。

## 中等（5 项）

### R3. copyDirAll 吞错后 RemoveAll 旧目录 —— 数据目录迁移可能丢数据

- **位置**：`service.go:234-287`（`migrateDataDir` / `copyDirAll`）
- **问题**：`copyDirAll` 对每个文件的 `ReadFile`/`WriteFile` 失败都是 `continue`/`_ =`，永远返回 nil；随后 `migrateDataDir` 无条件 `os.RemoveAll(oldDir)`。被占用（如旧实例未退干净时的 mihomo.exe）或读失败的文件**没复制成功就被删掉**。
- **修法**：`copyDirAll` 统计失败数，任何失败返回 error（或返回失败清单），`migrateDataDir` 只在零失败时才删旧目录；否则保留旧目录并告警。

### R4. 清空日志与在途轮询竞态：旧日志闪回，清空窗口内新日志永久丢失

- **位置**：`frontend/src/App.tsx:854-861`（`onClearLogs` 归零 `logSeqRef`）↔ `App.tsx:229-242`（tick 应用增量）
- **问题**：清空时若有一轮 `LogsSince(旧游标)` 在途，晚到响应会拼回旧行并把游标写回大序号；下一轮 reset 用空数组整段替换，其间内核新写入的日志被丢弃且不再补拉。`alive/seq` 守卫不防"清空"这种外部操作。
- **修法**：加 `logGenRef` 代数计数器，`onClearLogs` 时 `++`；tick 应用结果前比对代数，不一致整批丢弃。

### R5. S1 残留（上轮已报，确认仍在）：非管理员启动时 tunOn 不复位

- **位置**：`service.go` loadAppState（`s.tunOn = st.TUN` 无条件赋值）+ afterStart（只会置 true）
- **问题**：上次 TUN 开着、这次普通权限启动 → `tunOn=true` 但配置里没 TUN：UI 显示 TUN 已开（实际没开），且系统代理开关被"请先关闭 TUN"误拦。
- **修法**：`loadAppState` 或 `afterStart` 加 `if !elevate.IsElevated() { s.tunOn = false }`。

### R6. NodeMap 悬停浮层翻转无下限钳制

- **位置**：`frontend/src/NodeMap.tsx:492`（`tip.x - 314` / `tip.y - 124` 可为负）
- **问题**：选点菜单（:535）已修 `Math.max(0,…)`，悬停浮层同类问题漏网；贴左/上边缘时浮层出屏。
- **修法**：与 :535 一致钳制 left/top。

### R7. onTestAll 无并发守卫 + 快捷键缺 e.repeat 防抖

- **位置**：`frontend/src/App.tsx:806-818`（右键菜单可绕过按钮 disabled 再次触发全量测速，先完成者提前复位 `testing`）；`App.tsx:687-710`（长按 Ctrl+Shift+P 连发翻转系统代理）
- **修法**：`onTestAll` 开头 `if (testing) return;`；keydown handler 首行 `if (e.repeat) return;`。

## 轻微（4 项）

| # | 问题 | 位置 | 修法 |
|---|---|---|---|
| R8 | 模式切换/进程策略的乐观更新被 1.2s~3s 轮询整对象替换打回，高亮/策略闪回；`onSetProc` 并发时回滚基线互踩 | App.tsx:377-391 / 559-587 / 226 / 267 | pending 标记挡轮询覆盖，或成功回调后强制拉一次校准 |
| R9 | 日志行/规则行仍用 `key={i}` | LogPanel.tsx:107、SettingsPanel.tsx:264 | 换稳定标识（日志可用行内容+序号） |
| R10 | 右键菜单 ToggleFavorite 裸 Promise 无 catch | App.tsx:1061 | 套 `run()`/`reportErr`，与 NodesPanel 同款 |
| R11 | `LogBuffer.Since` 在游标超出保留窗口时静默截断、无 gap 标记 | internal/kernel/manager.go:61-74 | 可选：返回 `truncated` 标志让前端提示"更早日志已丢弃" |

## 本轮确认已修好的上轮遗留

NodeMap 选点菜单坐标钳制（NodeMap.tsx:535）、`api.ts` 的 `as unknown as` 消除（改为直接导出 bindings 的 CoreService）、constants.ts 死常量清理、TrafficChart 宽度测量补偿、`e.repeat` 之外的快捷键焦点排除、订阅 ByURL 全链路、TLS 需显式勾选（`Fetch(ctx, url, allowInsecure)` 链路正确）。

## 复审结论

重构质量高：组件拆分边界清晰、事件/定时器清理零遗漏、乐观更新带回滚、空值防御到位。**发布前必须修 R1、R2（用户可见的契约 bug）和 R3（数据丢失风险）**，R4/R5 强烈建议一并处理；其余可下一批。
