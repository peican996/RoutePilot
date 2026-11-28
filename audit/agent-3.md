# RoutePilot 审计 · 子报告 3：网络安全与订阅安全专项

- **范围**：订阅下载链路（URL → DNS → 拨号 → 重定向 → 响应体）、订阅内容解析、订阅内容转内核配置的出站面、内核更新下载链路。
- **基线**：`39ca02c1aa9303fc14fe8264d2715432cd50f030`（HEAD，工作区干净）
- **性质**：只读审计。本报告未修改仓库任何源码或测试，仅新增本文件。

## 0.1 审阅文件清单

| 文件 | 行数 | 关注点 |
|---|---|---|
| `internal/subscription/guard.go` | 287 | URL/IP 校验、拨号防护、重定向策略 |
| `internal/subscription/fetch.go` | 509 | HTTP client、重定向手工跟随、响应体上限、YAML/Base64 解析 |
| `internal/subscription/guard_test.go` | 281 | SSRF/超时/TLS/重定向上限等用例 |
| `internal/subscription/redirect_test.go` | 250 | 跨站/同站/降级/userinfo/Referer 用例 |
| `internal/subscription/parse_test.go` | 122 | 仅正常格式（Clash / base64 / 明文 URI） |
| `internal/subscription/fetch_test.go` | 47 | userinfo 头解析、TLS 错误识别 |
| `internal/subscription/repro_t17_test.go` | 34 | T17 回归（`//go:build repro`，默认不参与构建） |
| `service.go`（订阅段） | 2760-2812、4089-4105、4205-4296、4440-4520、4523-4693、5488-5620 | ImportConfig / 落盘 / 订阅状态机 / 自动更新 |
| `internal/configgen/gen.go` | 457 | 订阅配置 → 内核配置（字段剥离、规则拼装、deepCopy） |
| `internal/kernel/update.go` | 900+ | 内核下载 URL 白名单、重定向、摘要校验 |

## 0.2 执行过的命令与结果

```
go version                                        -> go1.27.3 windows/amd64
git log --oneline -12 -- internal/subscription/   -> 1f48bbd / bbf912e / 1ee52ec ...（C3 系列加固已合入）
go test -count=1 -run 'TestSSRF|TestCheckPublicIP|TestRedirect|TestSameSite|TestParse|TestDirectSubscription' \
        ./internal/subscription/                  -> ok routepilot/internal/subscription 2.201s
```

只读源码核对：`$(go env GOROOT)/src/net/ip.go`（`ParseIP` → `netip.ParseAddr`，`To4` 对 `::ffff:` 的处理）、`$GOMODCACHE/gopkg.in/yaml.v3@v3.0.1/decode.go`（别名炸弹限制、`d.unmarshal` 递归分发）。
**未对任何非本地地址发起请求**。

## 0.3 结论摘要

| 等级 | 数量 | 说明 |
|---|---|---|
| P0 | 0 | — |
| P1 | 0 | 应用侧 SSRF 防线（C3 系列）经复核**未发现可绕过的具体路径** |
| P2 | 3 | A3-01（订阅内容驱动内核出站，完全绕过 guard.go）、A3-02（明文 HTTP 订阅）、A3-03（YAML 无深度限制的崩溃/持久化 DoS） |
| P3 | 5 | A3-04 ~ A3-08 |

一句话结论：**`Fetch` 这条链路的 SSRF 防护是扎实的**（每跳复检 + 拨号用已校验 IP + 不继承任何凭据头），主要残余风险不在"怎么下载"，而在"下载到的东西能指挥内核干什么"——订阅里的 `proxy-providers` / `rule-providers` 带 `type: http` 时，内核会以管理员权限向任意地址发起请求，`gen.go` 的黑名单式 `sanitize` 完全没管这一类。

---

# 1. 问题清单

## A3-01【P2】订阅内容可指挥内核发起任意出站请求（绕过 guard.go 全部 SSRF 防护）

| 字段 | 内容 |
|---|---|
| 文件 | `internal/configgen/gen.go`；数据来源 `internal/subscription/fetch.go` |
| 函数 | `sanitizeSubscriptionConfig` / `Build`；上游 `Parse` → `parseClashConfig` |
| 代码位置 | `gen.go:150-174`（黑名单 `strippedSubscriptionKeys`）、`gen.go:181-194`（`Build` 只调 `sanitizeSubscriptionConfig`）、`gen.go:298-306`（`rule-providers` 与订阅内容合并）；`fetch.go:284-291`（订阅原样进 `res.Config`） |
| 描述 | 订阅 YAML 里的 `proxy-providers` 与 `rule-providers` 被**原封不动**写进内核配置。mihomo 对 `type: http` 的 provider 会自行按 `interval` 周期拉取 `url`；`geox-url` 同理。这条请求由内核进程发出，**不经过** `guardedDialContext` / `validateSubscriptionURL`，因此回环、内网、云元数据全部可达。 |
| 触发条件 | 用户添加（或已被 MITM 的 http 订阅中携带）形如：`proxy-providers: {p: {type: http, url: "http://169.254.169.254/latest/meta-data/", path: ./p.yaml, interval: 60}}`；或 `rule-providers: {r: {type: http, behavior: classical, url: "http://127.0.0.1:8500/v1/agent/self", path: ./r.yaml}}`；或 `geox-url: {geoip: "http://10.0.0.5/x.dat"}`。 |
| 实际影响 | 内核以管理员权限（TUN 场景）对任意目标发起 GET：探测/触发本机与内网 HTTP 服务、云主机 metadata、路由器管理接口。**盲 SSRF**（响应不回传给攻击者，但请求本身已构成越权访问与内网探测），且配合 `interval` 可周期性重放。这与项目在 C3 中明确建立的"订阅不得访问受限地址"策略直接冲突——策略只在应用侧生效，内核侧完全失效。 |
| 根因 | `sanitizeSubscriptionConfig` 是**黑名单**（12 个键），只想到"开端口 / 装 UI / 跑脚本"，没想到"指挥内核发请求"。而 `gen.go:295-306` 的注释还明确写着要保留订阅自带的 `rule-providers`（为了 RULE-SET 引用可用），等于确认了这条透传路径是有意为之但没做 URL 校验。 |
| 修复建议 | 对 `proxy-providers` / `rule-providers` / `geox-url` 做**结构化清洗**：只允许 `type: file`；或对 `type: http` 的 `url` 复用 `subscription` 包的校验函数（同 scheme/公网 IP 规则），不合法就整条删除并记录告警；`geox-url` 直接剥离（应用已内嵌 `geoip.metadb` / `geosite.dat`，见 `service.go:570-572`）。 |
| 测试方案 | 单测（`internal/configgen/gen_test.go`，本报告不改）：给 `Base` 塞入 `proxy-providers` 带 `url: http://127.0.0.1/`，断言 `Build` 后该 provider 被删或 `url` 被清空。端到端（需靶机）：本地起 `httptest` 监听 127.0.0.1，订阅里指向它，观察内核是否真的发出请求。 |
| 置信度 | 透传：高（代码直读）。内核实际拉取：高（mihomo 既有行为），但"请求是否成功"未实测，属**静态推断，需靶机验证**。 |

## A3-02【P2】允许明文 `http://` 订阅：凭据明文传输 + 响应可被篡改后直达内核

| 字段 | 内容 |
|---|---|
| 文件 | `internal/subscription/guard.go` |
| 函数 | `validateSubscriptionURL` |
| 代码位置 | `guard.go:101-103`（`u.Scheme != "http" && u.Scheme != "https"` 才拒绝）；`fetch.go:79-87`（每跳重建请求，凭据在 URL 里）；`fetch.go:347-350`（响应直接进 `Parse`） |
| 描述 | 订阅地址允许 `http`，且没有任何提示或二次确认。订阅 URL 普遍把 token 放在 path/query（`/api/v1/client/subscribe?token=xxx`），部分面板还用 `user:pass@`。明文链路上：① token 可被同网段/运营商/恶意热点直接抓取（面板账号接管）；② 响应体可被中间人任意替换，替换后的 YAML 会被完整写入 `config.yaml` 交给内核，并可携带 A3-01 的 provider URL，从而把"读流量"升级为"命令内核打内网"。 |
| 触发条件 | 用户填入 `http://` 订阅（自建面板常见）；攻击者处于链路中间（公共 WiFi、被控路由器、运营商劫持页）。 |
| 实际影响 | 凭据泄露 + 配置投毒（任意节点/规则/DNS/hosts 注入），与 A3-01 组合后具备内网 SSRF 能力。 |
| 根因 | 为兼容自建面板而放开 http，但没有"明文风险"的显式确认，也没有把 http 与 https 区别对待。 |
| 修复建议 | 要么默认拒绝 http 并给出"我知道风险，仍然使用"的显式开关（每个订阅单独记忆），要么在 UI 上对该订阅常驻"明文传输"警示条。不建议静默放行。 |
| 测试方案 | 单测：`validateSubscriptionURL("http://example.com/sub")` 当前返回 nil error（可断言现状）；策略变更后断言返回特定 `ErrInsecureScheme`。 |
| 置信度 | 代码行为：高。风险评级：取决于产品是否接受该取舍——若明确接受，可降为 P3/风险接受项。 |

## A3-03【P2】订阅 YAML 无深度限制：8MB 输入可造成栈/内存放大崩溃，且可持久化

| 字段 | 内容 |
|---|---|
| 文件 | `internal/subscription/fetch.go`；`internal/configgen/gen.go` |
| 函数 | `Parse` → `parseClashConfig` → `yaml.Unmarshal`；后续 `configgen.deepCopy` |
| 代码位置 | `fetch.go:273-292`（`Parse`，无深度/规模限制、无 `recover`）、`fetch.go:367-377`（`yaml.Unmarshal(body, &map[string]any)`）；`gen.go:131-148`（`deepCopy` 递归） |
| 描述 | 响应体上限 8MiB 限制了**字节数**，但没限制**嵌套深度**。yaml.v3 的解析器是迭代状态机（不会栈溢出），但 **decoder 与 `deepCopy` 都是按层递归**：flow 风格的 `{a:{a:{a:...}}}` 每层仅 4-5 字节，8MiB 可达 150 万~250 万层，`d.unmarshal` / `d.mapping` / `deepCopy` 每层 1~3 个栈帧。 |
| 触发条件 | 恶意订阅返回（或 http 订阅被 MITM 注入）：`proxies: [{name: x, type: ss}]` 保证 `parseClashConfig` 返回 true，随后跟一个百万层嵌套的 flow mapping。 |
| 实际影响 | ① `Parse` 阶段栈增长到 GB 级 → `fatal error: goroutine stack exceeds 1000000000-byte limit`，**进程直接死亡且不可 recover**；② 若侥幸不崩，也让客户端内存瞬时吃掉 1GB 左右；③ 因为订阅原文在 `Parse` 成功后立即落盘（`service.go:4091`），**重启后 `loadSavedSubscription` 会再次解析同一份文件**（`service.go:4501`），形成"删不掉就每次启动崩"的持久化 DoS——用户必须手工清空 dataDir 才能恢复。 |
| 根因 | `Parse` 把"内容不可信"只理解为"格式可能不对"，没考虑"格式对但结构畸形"；`yaml.Unmarshal` 的别名炸弹防护（`decode.go:489` 的 `allowedAliasRatio`）只防 index 级爆炸，不防深度。 |
| 修复建议 | ① `Parse` 入口加 `defer recover()` 兜底（`yaml` 自身 panic 走 `failf` 会被 `handleError` 转成 error，但深层递归的 stack overflow 无法 recover，仍需 ②）；② 在 `yaml.Unmarshal` 前用 `yaml.Node` 迭代遍历（显式栈）统计最大深度，超过阈值（如 64）直接拒绝；③ `configgen.deepCopy` 同样加深度上限。 |
| 测试方案 | 现有 `parse_test.go` 无任何畸形输入用例。建议新增表驱动用例：`{"深嵌套", strings.Repeat("{a:", 1<<20) + "1" + strings.Repeat("}", 1<<20)}`，期望返回 error 而非进程崩溃（需在子进程中跑，否则测试进程会一起死）。本报告未新增测试文件。 |
| 置信度 | 机制：高（decoder 递归已核实 `decode.go:484-524`、`729-800`）。是否在 8MiB 内必崩：**中**，取决于栈帧大小与 1GB 上限，**需实测**。 |

## A3-04【P3】`checkPublicIP` 未覆盖 IPv6 过渡/隧道地址

| 字段 | 内容 |
|---|---|
| 文件 | `internal/subscription/guard.go` |
| 函数 | `checkPublicIP` / `blockedCIDRs` |
| 代码位置 | `guard.go:64-78`（13 段 CIDR）、`guard.go:128-147` |
| 描述 | 已覆盖 v4 私网/回环/链路本地/CGNAT/文档段/保留段、v6 `::1`/`fc00::/7`/`fe80::/10`/`ff00::/8`/`64:ff9b::/96`/`2001:db8::/32`，以及 `::ffff:a.b.c.d`（`To4()` 归一后判定，已核实 `ip.go:217-228`）。**未覆盖**：`2002::/16`（6to4，内嵌 IPv4，如 `2002:0a00:0005::` ≈ 10.0.0.5）、`2001::/32`（Teredo，内嵌客户端 IPv4）、`::/96`（IPv4-compatible，如 `::7f00:1` ≈ 127.0.0.1）。 |
| 触发条件 | `http://[::7f00:1]/`、`http://[2002:7f00:0001::]/`。字符串层 `net.ParseIP` 能解析且 `IsGlobalUnicast()==true`，`blockedCIDRs` 也不含它们 → 放行 → 直接对字面量拨号。 |
| 实际影响 | 若宿主 OS/网络仍支持对应隧道机制，可把连接导向内网/回环。Windows 自 Vista 起已废弃 IPv4-compatible 地址，6to4/Teredo 默认不启用，**实际可达性很低**。 |
| 根因 | 只按 RFC 6890 的"特殊用途网段"表挑选，漏了过渡机制地址。 |
| 修复建议 | `blockedCIDRs` 追加 `2002::/16`、`2001::/32`、`::/96`（`::/96` 与 `::ffff:0:0/96` 需注意不要误伤 `::1`，后者已被 loopback 先拦）。 |
| 测试方案 | 在 `TestCheckPublicIPRanges` 的 blocked 表中加 `"2002:7f00:1::"`、`"2001:0:1::1"`。 |
| 置信度 | 规则缺口：高。可利用性：低，**静态推断，需靶机验证**。 |

## A3-05【P3】`profile-update-interval` 整数溢出 → 自动更新退化为每 10 分钟全量重拉

| 字段 | 内容 |
|---|---|
| 文件 | `internal/subscription/fetch.go`；`service.go` |
| 函数 | `Fetch`（响应头解析）→ `maybeAutoRefresh` |
| 代码位置 | `fetch.go:357-361`（`strconv.Atoi` 后直接赋给 `int`，无范围校验）、`service.go:5518`（`time.Duration(m.UpdateHrs) * time.Hour`） |
| 描述 | `time.Duration` 是 int64 纳秒，上限约 292 年 ≈ 256 万小时。`UpdateHrs` 取 5e9 时，`5e9 * 3.6e12 ns` 溢出为负数，导致 `time.Since(FetchedAt) < 负值` 恒为 false → 每个 10 分钟 tick 都判定"该更新了"。 |
| 触发条件 | 服务端返回 `profile-update-interval: 5000000000`。 |
| 实际影响 | 客户端每 10 分钟拉取一次全部订阅（含非激活订阅），持续消耗用户带宽并给对方服务器加压；若内容每次都变，还会每 10 分钟重启内核（断连）。非致命。 |
| 根因 | 外部整数未做区间约束（`Atoi` 只保证不溢出 int，乘 `time.Hour` 时才溢出）。 |
| 修复建议 | 解析后 clamp 到 `[1, 24*30]`；或计算处用 `time.Duration` 溢出检查（`if d/time.Hour != time.Duration(hrs) { skip }`）。 |
| 测试方案 | `fetch_test.go` 加 `parseUpdateInterval` 表驱动；`service` 层用可注入时钟验证跳过逻辑。 |
| 置信度 | 高（算术确定；触发只需构造响应头）。 |

## A3-06【P3】订阅 URL（含 token）与错误文本写入明文日志，且日志文件权限 0644

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `dbg`；调用点 `maybeAutoRefresh` |
| 代码位置 | `service.go:5363-5375`（`debug.log`，`os.O_CREATE\|os.O_WRONLY\|os.O_APPEND, 0o644`）、`service.go:5529`（`s.dbg("auto refresh skipped (%s): %v", m.URL, err)`） |
| 描述 | 自动更新失败时把**完整订阅 URL**（往往就是 token 本身）写进 `%LOCALAPPDATA%\...\debug.log`；文件权限 0644，而同目录的订阅原文/元信息用的是 0600（`service.go:4091`）。此外 `s.subInfo.Error` 会把 `url.Error` 文本（含完整 URL，Go 只抹掉 password，不抹 query token）回显到界面。 |
| 触发条件 | 任意一次订阅拉取失败（DNS 失败、超时、被 SSRF 拦下）。 |
| 实际影响 | 同机其他用户/备份软件/日志采集工具可读到订阅 token → 面板账号被他人使用。属于低危信息暴露（Windows 下 ACL 继承后 0644 的实际效果弱于 Unix）。 |
| 根因 | 日志统一走 `dbg`，没有"敏感字段脱敏"这一层。 |
| 修复建议 | 日志里只打 `host`，不打完整 URL（`hostLabel` 这类函数已有先例，见 `update.go:496-501`）；`debug.log` 改 0600。 |
| 测试方案 | 单测：断言 `redactURL("https://example.com/sub?token=abc")` 不含 `abc`。 |
| 置信度 | 高。 |

## A3-07【P3】`writeSubscriptionFiles` 两次写盘非原子，失败时磁盘与内存不一致

| 字段 | 内容 |
|---|---|
| 文件 | `service.go` |
| 函数 | `writeSubscriptionFiles` |
| 代码位置 | `service.go:4089-4105`（先写 `res.Raw`，再写/删 provider 文件，任一步失败即返回错误） |
| 描述 | 两次 `writeFileAtomic` 各自原子，但**整体不是事务**。若 raw 写成功、provider 写失败（磁盘满、杀软锁文件），调用方 `applySubscriptionExpect` 返回错误、内存保持旧订阅，而磁盘上已经是新订阅原文。 |
| 触发条件 | 落盘阶段 provider 文件写入失败。 |
| 实际影响 | 下次启动 `loadSavedSubscription` 会读到这份"从未被激活"的新原文并解析生效——用户看到的配置与实际生效的不一致；若新原文来自被劫持的 http 订阅，等于让一份未被用户确认的配置在重启后生效。 |
| 根因 | 缺少"先写临时文件、两步都成功后再统一 rename"的两阶段提交。 |
| 修复建议 | 两个文件都先写 `.tmp`，成功后再依次 rename；或先写 provider 再写 raw，并在失败时回滚已写文件。 |
| 测试方案 | 用只读目录/注入失败的文件写入器构造第二步失败，断言磁盘上仍是旧内容。 |
| 置信度 | 高（代码路径直读）；触发需要外部故障，概率低。 |

## A3-08【P3】未限制订阅目标端口，可把客户端当作任意公网 `IP:port` 的 HTTP 探测器

| 字段 | 内容 |
|---|---|
| 文件 | `internal/subscription/guard.go` |
| 函数 | `validateSubscriptionURL` / `guardedDialContext` |
| 代码位置 | `guard.go:96-125`（只校验 scheme + 主机）、`guard.go:188-207`（端口原样透传） |
| 描述 | 任意端口都允许（如 `http://1.2.3.4:22/`、`:6379`、`:9200`）。IP 层已挡住内网，因此不构成对内网的 SSRF；但订阅可以让客户端去连任意公网 IP 的任意端口并发 HTTP 请求。 |
| 触发条件 | 订阅 URL 指向公网 IP 的非 HTTP 端口。 |
| 实际影响 | 客户端被当作"公网端口探测器"（响应内容不回流给攻击者，仅成功/失败时序有差异），影响有限；也会给第三方留下"来自该用户 IP 的异常请求"的痕迹。 |
| 根因 | 端口未纳入策略。 |
| 修复建议 | 若产品无特殊需求，限制为 80/443/8080/8443 或"仅 1024 以上"；至少对非标准端口给出提示。 |
| 测试方案 | `validateSubscriptionURL("http://example.com:22/")` 断言现状放行。 |
| 置信度 | 高（行为确定）；风险评级低。 |

---

# 2. 攻击面矩阵

| # | 入口 | 数据流 | 防护函数（精确位置） | 覆盖 | 缺口 |
|---|---|---|---|---|---|
| 1 | 订阅 URL（首跳） | 用户输入 → `Fetch` → `doWithDeps` | `validateSubscriptionURL` (guard.go:96)、`guardedDialContext` (181) | ✅ scheme/主机/IP 字面量/数字形式 + 拨号前解析校验 | 允许 http 明文（A3-02）；不限端口（A3-08） |
| 2 | 重定向目标（每跳） | `Location` → `current.Parse` → 复检 | `validateSubscriptionURL` (fetch.go:111)、`redirectAllowed` (114, guard.go:244)、拨号校验 (193) | ✅ 三次校验叠加，跨站/降级/userinfo 全拒 | 同站判定只看主机名、忽略端口；A3-04 的 IPv6 过渡地址 |
| 3 | 订阅内容 → 内核配置 | `res.Config` → `configgen.Build` → `config.yaml` → mihomo | `sanitizeSubscriptionConfig` (gen.go:170) 黑名单 12 键 | ❌ **未覆盖出站类字段** | `proxy-providers`/`rule-providers`/`geox-url`/`dns.nameserver`/`hosts`（A3-01） |
| 4 | 订阅内容 → 解析 | `Parse` → yaml/base64/URI | 8MiB 上限 (guard.go:18, fetch.go:128) | ⚠️ 仅限制字节数 | 无深度上限、无 `recover`（A3-03） |
| 5 | 订阅内容 → 展示层 | `res.Name`/组名/节点名 → JSON → React | React 默认转义（`frontend/src` 无 `innerHTML`）；组名含 `,\n\r\t\x00` 时 `MainProxyGroup` 跳过 (gen.go:388)、`buildSiteRulePlan` 跳过 (service.go:907) | ✅ | 无 |
| 6 | `ImportConfig` | 用户 JSON → `s.subs` | 不发网络请求；后续 `RefreshSubscription` 走 #1 | ✅ | `Insecure` 可直接由导入文件置 true（需用户主动导入） |
| 7 | 内核更新 URL | `browser_download_url` → `fetchCoreZip` | `validateDownloadURL` (update.go:464) + `CheckRedirect` 复检 (509-514) + sha256 (617) + 大小上限 (529/546) | ✅ | 无 IP 层校验，但 https + 证书校验使 DNS 劫持不可利用 |
| 8 | 本地已落盘订阅文件 | `loadSavedSubscription` / `SwitchSubscription` → `Parse` | 同 #4 | ⚠️ | 同 A3-03；本地写权限已是同一信任边界 |
| 9 | 订阅响应头 | `subscription-userinfo` / `content-disposition` / `profile-update-interval` / `profile-web-page-url` | 无校验 | ⚠️ | A3-05；`webPage` 仅存不用（前端 `src` 无引用） |

---

# 3. 安全测试用例（全部离线，只打 `httptest` 回环，未实际新增/修改测试文件）

| # | 构造输入 | 期望结果 | 现有代码实际行为（读码/已验证） | 落点 |
|---|---|---|---|---|
| T1 | `http://2130706433/`、`http://0x7f000001/`、`http://0177.0.0.1/` | 拒绝 | 拒绝（`isNumericHost` 判全数字/`0x` 前缀，guard.go:119-123、271-287）。第 3 条 Go 1.27 的 `net.ParseIP` 已拒绝前导零，即便漏过也会被 `isNumericHost` 全数字规则拦下 | `guard_test.go:TestSSRFRejectsBlockedLiterals`（前两条已覆盖） |
| T2 | `http://[::ffff:127.0.0.1]:80/`、`http://[::ffff:169.254.169.254]/` | 拒绝 | 拒绝（`ParseIP` → `To4()` 归一 → `IsLoopback`/`IsLinkLocalUnicast`，ip.go:217-228 + guard.go:132-138） | **无覆盖**（建议加入 `TestCheckPublicIPRanges`） |
| T3 | `http://[::7f00:1]/`、`http://[2002:7f00:1::]/` | 拒绝 | **放行**（A3-04） | 无覆盖 |
| T4 | 首跳公网 → 302 `http://127.0.0.1:port/` | 拒绝，且本机服务收不到请求 | 拒绝（`validateSubscriptionURL(next)` 拦下，fetch.go:111） | `TestSSRFBlocksRedirectToLoopback`（已通过） |
| T5 | 首跳公网 → 302 `http://evil.test/?token=X` | 拒绝，第三方 0 次命中 | 拒绝（`sameSiteHost` false，fetch.go:114） | `TestCrossHostRedirectRejectedWithoutLeakingToken`（已通过） |
| T6 | 同主机 302，检查 `Referer` 与 query 继承 | 无 Referer、无 token 继承 | 符合（每跳重建 Header，fetch.go:79-87） | `TestSameHostRedirectSendsNoReferer`（已通过） |
| T7 | `https://` → 302 `http://` | 拒绝 | 拒绝（guard.go:248-250） | `TestRedirectDowngradeRejected`（已通过） |
| T8 | 302 → `https://user:pass@same.host/` | 拒绝 | 拒绝（guard.go:245-247） | `TestRedirectWithUserInfoRejected`（已通过） |
| T9 | 循环 302（自指） | 5 跳后报错 | `ErrTooManyRedirects`（fetch.go:102-104） | `TestSSRFRedirectLimit`（已通过） |
| T10 | 8MiB+1 响应（含 `Content-Length` 与实际超限两种） | 报错，不静默截断 | `ErrResponseTooLarge`（fetch.go:125-134） | `TestSSRFLargeResponseRejected`（已通过） |
| T11 | gzip 压缩炸弹（压缩后 1KB → 解压 500MB） | 8MiB 处报错，内存有界 | 有界：`Transport` 自动解压后 `LimitReader` 作用于**解压流**，最多读 8MiB+1 即停（fetch.go:128-134）。**无覆盖**（值得补一条） | 无 |
| T12 | `profile-update-interval: 5000000000` | 忽略该头或 clamp | 写入 `UpdateHrs`，后台 `time.Duration*Hour` 溢出 → 每 10 分钟重拉（A3-05） | 无 |
| T13 | 订阅含 `proxy-providers: {p: {type: http, url: "http://127.0.0.1:PORT/"}}` | provider 被剥离/URL 被清空 | **原样写进内核配置**（A3-01），guard.go 完全不参与 | 无（`TestBuildStripsSubscriptionControlledFields` 未含此键） |
| T14 | 8MiB 深嵌套 flow mapping（含顶层 `proxies`） | 返回 error | 预期栈溢出 fatal（A3-03，需实测） | 无 |
| T15 | 重复 key 的 YAML（`a: 1\na: 2`） | 容错或明确报错 | yaml.v3 `uniqueKeys` 报错 → `parseClashConfig` 返 false → 整份订阅被判"无法识别"（fail-closed，不写坏旧配置） | 无 |

---

# 4. 防护规则清单（guard.go 逐条）

| # | 规则 | 位置 | 拒绝对象 | 对应测试 |
|---|---|---|---|---|
| 1 | scheme 白名单 `http`/`https` | guard.go:101-103 | file/ftp/gopher/自定义 scheme | `TestSSRFRejectsNonHTTPScheme` |
| 2 | 必须有 host | :104-106 | `http:///path`、opaque URL | 无（建议补） |
| 3 | `localhost` / `*.localhost`（大小写、尾点归一） | :107-110 | `localhost`、`localhost.`、`a.localhost` | `TestSSRFRejectsBlockedLiterals` |
| 4 | 主机含 `%` → 视为 IPv6 zone | :111-114 | `[fe80::1%25eth0]` | 间接（`[fe80::1]`） |
| 5 | IP 字面量 → `checkPublicIP` | :115-118 | 见 #7/#8 | `TestSSRFRejectsBlockedLiterals` |
| 6 | 数字形式主机（全数字/点、`0x` 前缀） | :119-123、271-287 | `2130706433`、`0x7f000001`、`0177.0.0.1`、`127.1` | 同上（前两条） |
| 7 | `To4()` 归一后要求 `IsGlobalUnicast` 且非 loopback/private/unspecified/link-local/multicast | :128-140 | 127/8、10/8、172.16/12、192.168/16、169.254/16、fc00::/7、fe80::/10、::1、0.0.0.0、::、ff00::/8、`::ffff:1.2.3.4` | `TestCheckPublicIPRanges` |
| 8 | 补充 CIDR 13 段 | :64-78 | 0.0.0.0/8、100.64/10（含阿里云 100.100.100.200）、192.0.0/24、192.0.2/24、198.18/15、198.51.100/24、203.0.113/24、240/4、255.255.255.255、64:ff9b::/96、100::/64、2001:2::/48、2001:db8::/32 | `TestCheckPublicIPRanges`（部分） |
| 9 | 重定向目标不得带 userinfo | :245-247 | `https://u:p@host/` | `TestRedirectWithUserInfoRejected` |
| 10 | 禁止 https → http 降级 | :248-250 | 降级跳转 | `TestRedirectDowngradeRejected` |
| 11 | 只允许同站跳转（相等或父子域，按标签边界比较） | :251-253、259-269 | `evil-example.com`、`example.com.evil.test` | `TestCrossHostRedirectRejected…`、`TestSameSiteSubdomainRedirectAllowed`、`TestSameSiteHostBoundary` |
| 12 | 每跳重跑 #1~#8 | fetch.go:105-113 | 重定向到受限目标 | `TestSSRFBlocksRedirectToLoopback`、`…ToPrivateIP`、`…ToPrivateDNS` |
| 13 | 每跳重建 Header，删除 Referer/Authorization/Cookie | fetch.go:79-87 | 凭据随跳转外泄 | `TestSameHostRedirectSendsNoReferer` |
| 14 | 最大 5 跳 | guard.go:20、fetch.go:102-104 | 重定向链 | `TestSSRFRedirectLimit` |
| 15 | 拨号前解析 + **全部** IP 校验 + 用校验后 IP 连接 | :152-208 | DNS 指向内网、混合解析 | `TestSSRFDialRejectsPrivateDNS`、`TestSSRFRejectsMixedDNS` |
| 16 | 声明长度 > 8MiB 拒绝 | fetch.go:125-127 | 超大响应 | `TestSSRFLargeResponseRejected` |
| 17 | 实际读取 > 8MiB 报错（非截断） | :128-134 | 无 Content-Length 的超大响应 | 同上 |
| 18 | 空响应拒绝 | :135-137 | 空 body | 无 |
| 19 | 总超时 45s（覆盖全部跳）+ 响应头 20s + TLS 握手 10s | guard.go:44-59、213-225、fetch.go:72 | 慢速/挂死服务端 | `TestSSRFTimeout` |
| 20 | TLS 最低 1.2；`InsecureSkipVerify` 仅用户显式开启 | guard.go:220、fetch.go:157-171 | 自签证书静默降级 | `TestSSRFTLSInsecureExplicitOnly` |
| 21 | `Proxy: nil`（不走系统/环境代理） | guard.go:218 | 借本机代理绕过校验 | 无（C3 文档说明为取舍） |

---

# 5. 已排除的疑点（看着像漏洞，实际被代码兜住）

| 疑点 | 结论与依据 |
|---|---|
| **重定向绕过 SSRF**（T17 历史问题） | 已修复且修复到位：`CheckRedirect` 返回 `ErrUseLastResponse`（guard.go:231-233）完全关闭自动跟随，改为手工循环，每跳**先** `validateSubscriptionURL`（fetch.go:111）**再** `redirectAllowed`（114），落地前还要过拨号校验（193）。三重叠加，`repro_t17_test.go` 回归为"已修复"。 |
| **DNS rebinding** | 不存在时间窗：`guardedDialContext` 自己解析、自己校验、然后**用校验过的 IP** 建连（guard.go:193-205），域名不会再被系统解析一次。校验对象是"全部解析结果"而非主机名，混合解析整体拒绝（:170-174）。 |
| **云元数据 `169.254.169.254` / `100.100.100.200` / `metadata.google.internal`** | 全覆盖：前者命中 `IsLinkLocalUnicast` + `169.254/16`；中者在 `100.64.0.0/10` 内；后者解析后同样落到 169.254.169.254 被拨号校验拦下。AWS IPv6 元数据 `fd00:ec2::254` 命中 `IsPrivate`（fc00::/7）。 |
| **跨域重定向泄露 Authorization/Cookie** | 不存在：请求从不复用 `via` 里的 Header，每跳 `NewRequest` 后显式 `Del("Referer"/"Authorization"/"Cookie")`（fetch.go:85-87）；且跨站跳转本身就被拒。 |
| **HTTPS→HTTP 降级** | 明确拒绝（guard.go:248-250），且有单测。 |
| **超大响应打满内存** | 双保险：先看 `ContentLength`，再用 `LimitReader(max+1)` + 超限**报错**（不静默截断），所以不会出现"截断的 YAML 被当成功解析"。 |
| **压缩炸弹** | 有界。`Transport` 自动 `Accept-Encoding: gzip` 并对 body 透明解压，`LimitReader` 作用在**解压后的流**上，最多产出 8MiB+1 字节；`br`/`deflate` 不会被自动请求。 |
| **无限重定向** | 5 跳硬上限（`maxRedirects`），且整体受 45s ctx 与 `ResponseHeaderTimeout` 约束，循环跳转无法持续消耗资源。 |
| **`user@host` 解析差异绕过**（如 `http://evil.com\@127.0.0.1/`） | 不成立：Go 的 `url.Parse` 以**最后一个 `@`** 切分 userinfo/host，且校验与拨号用的是同一个 `url.URL` 对象，不存在"校验看 A、连接连 B"的解析器差分。测试已覆盖 `127.1.2.3`、`localhost.` 等形态。 |
| **`localhost.` / 大写 / 尾点** | `strings.ToLower` + `TrimSuffix(".")`（guard.go:107、260-261），已测。 |
| **IDN / Unicode 域名** | 不构成绕过：`net/http` 与 `net` 解析器不做 IDNA 转换，非 ASCII 主机名在 DNS 阶段通常直接失败；即便某个 resolver 认了，连接前仍要过 IP 白名单，最终仍落在 #15 的拨号校验上。 |
| **`userinfo` 首跳携带凭据** | 允许，但只发给用户自己填的那台主机（用户在 URL 里写的），不是泄漏。**副作用**：`fetch.go:86` 的 `Del("Authorization")` 会把 Go 由 userinfo 自动生成的 Basic 头删掉，导致"用 `user:pass@` 做鉴权的订阅"永远 401——这是可用性回归，不是安全问题。 |
| **订阅字段把内核打穿（开监听口、装外部 UI、跑脚本）** | 已由 `strippedSubscriptionKeys` 覆盖 12 个键（gen.go:156-174），并有 `TestBuildStripsSubscriptionControlledFields` 守护；`mixed-port`/`allow-lan`/`bind-address`/`external-controller`/`secret`/`tun`/`dns.listen` 一律被应用覆盖。**残余**只剩"出站类字段"，见 A3-01。 |
| **订阅组名做规则注入**（`MATCH,<组名>`） | 已兜住：`MainProxyGroup` 跳过含 `,\n\r\t\x00` 的组名（gen.go:388）；`buildSiteRulePlan` 同样跳过（service.go:907）；进程规则用哈希组名 `nx-pp-<hash>`（service.go:778-780）而不拼原名字，目标名只作为 YAML 列表元素出现，不会破坏规则语法。 |
| **订阅字段 → 前端 XSS** | `frontend/src` 无 `dangerouslySetInnerHTML`/`innerHTML`，React 默认转义；`profile-web-page-url` 只在 `types.ts`/`App.tsx` 声明字段，**没有任何打开链接的调用**，`javascript:`/`file:` 注入无处落地。 |
| **`content-disposition` 文件名做路径穿越** | `res.Name` 只用于展示与 `subs.json`，落盘路径统一走 `RawFileName`（sha256 前 8 字节，fetch.go:258-261），不含用户/服务端可控字符。 |
| **内核更新下载被投毒** | 较强：域名白名单（update.go:450-459）+ 仅 https + **每跳重定向复检**（509-514）+ sha256 摘要强制（571-586、617）+ 大小上限 200MiB（55、529、546）+ PE 头校验（626）。摘要缺失直接拒绝升级，是正确的 fail-closed。缺 IP 层校验，但 https 证书绑定主机名，DNS 劫持只会导致证书校验失败。 |
| **`ImportConfig` 直接注入任意订阅 URL 并触发请求** | 不成立：`ImportConfig`（service.go:2760-2812）只写内存与磁盘，不发网络请求；后续任何拉取都走 `subscription.Fetch`，仍过全套校验。 |
| **解析失败写坏旧配置** | 不成立（除 A3-07 的极端落盘失败）：`subscribe` 在 fetch/parse 出错时直接返回，不进入 `applySubscriptionExpect`（service.go:4553-4561）；`SwitchSubscription` 在 Parse 失败时先返回（4223-4227）；`loadSavedSubscription` 失败只 `dbg` 后 return，不动内存状态（4501-4505）。 |
| **yaml 别名炸弹（billion laughs）** | 已由库层拦截：`decode.go:489` 的 `aliasCount/decodeCount` 比值检查会返回 `document contains excessive aliasing`，`Parse` 走 fallback 后返回错误，不会 OOM。 |
| **yaml 解析 panic 逃逸** | 不会：yaml.v3 内部 `failf` 的 panic 由 `unmarshal()` 的 defer 转成 error（`yaml.go` 的 `handleErr`），`Parse` 全路径无裸 panic。 |

---

# 6. 未覆盖部分与需靶机/真机验证项

**测试覆盖空白（现存测试"根本没测"的面）**

1. `Parse` 没有任何畸形/恶意输入用例——超深嵌套、超长单行、重复 key、超大节点数、非 map 的 `proxies`、二进制内容，全部无覆盖（`parse_test.go` 只有 4 个 happy path）。
2. `checkPublicIP` 未覆盖 `::ffff:` 映射形态、`0.0.0.0/8` 边界、CGNAT 边界、6to4/Teredo。
3. 没有任何 gzip 压缩炸弹用例（`ddos` 面已兜住但无回归）。
4. `sanitizeSubscriptionConfig` 的用例不含 `proxy-providers` / `rule-providers` / `geox-url` / `dns` / `hosts`，所以 A3-01 这类缺口不会被 CI 发现。
5. 无"重定向到同主机不同端口"用例、无"链式同站跳转"用例、无"响应头畸形（`profile-update-interval` 溢出、负数、超大）"用例。
6. `repro_t17_test.go` 带 `//go:build repro`，默认 `go test ./...` **不会**跑它，只在 `-tags repro` 时生效。

**需靶机/真机验证（静态推断，未实测）**

1. A3-01：本地起 `httptest`（127.0.0.1）作为目标，在订阅里放 `proxy-providers.type: http` 指向它，启动真实内核，观察内核是否真的发起请求（可用内核 `log-level: debug` 或本地监听计数确认）。
2. A3-03：在**子进程**中用 8MiB 深嵌套 YAML 实测是否触发 `goroutine stack exceeds`；同时测 1MiB / 2MiB 的临界点。
3. A3-04：在 Windows 上实测 `connect()` 到 `[::7f00:1]`、`[2002:7f00:1::]` 的返回码，确认是否 `WSAEADDRNOTAVAIL`/不可达。
4. 真实 DNS rebinding：需要一个先答公网 IP、再答内网 IP 的权威 DNS。按代码结构（拨号用已校验 IP、无二次解析）理论上不可利用，但值得用 `dnschef` 这类工具做一次实证。
5. 真实云元数据：在允许出网的云主机上，用 A3-01 的构造验证 `169.254.169.254` 是否真的被内核访问到。

**范围外的相邻观察（不计入本报告问题数，供主控分派）**

- 内核更换 `external-controller` 仅在 `Build` 里覆盖，订阅若给出 `external-controller-tls` / `external-controller-cors` 等键未被剥离，可能让应用自己的 API 客户端连不上（可用性，非安全）。
- `ImportConfig` 对 `bundle.Subs[].Insecure` 不做确认，导入一份他人配置即可把某个订阅置为"跳过证书校验"。
