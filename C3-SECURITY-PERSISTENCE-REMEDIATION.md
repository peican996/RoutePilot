# C3 安全性与持久化治理整改报告

- 日期：2026-11-09
- 仓库：`peican996/RoutePilot`（本地 `nexuslink_review`）
- 分支：`main`，基线 `040b6b4`
- 范围：仅 C3（订阅 SSRF、内核升级原子性、配置与系统代理持久化）。C4 未执行。
- 依赖：C1（内核生命周期）、C2（进程身份）改动仍在工作区，尚未提交。

---

## 1. 结论摘要

C3 的三块高风险面均已按「入口校验 + 实际连接校验 + 原子写入 + 可回滚」重做：

1. **订阅 SSRF**：新增 URL 协议/主机校验、DNS 解析结果校验、拨号使用已校验 IP、每次重定向复检、响应大小/超时限制；回环、私网、链路本地、CGNAT、文档网段、IPv6 特殊段全部拒绝。系统/环境 HTTP 代理也无法绕过校验（订阅请求不复用代理）。
2. **内核升级**：下载内容必须同时通过大小、sha256、PE 文件头校验；替换前备份旧内核；临时文件 Sync 后原子替换；替换失败旧内核保持可用；新内核启动失败自动回滚并重新拉起旧内核；升级前/后版本、哈希、备份路径与回滚原因写入 `core-upgrade.json`。
3. **持久化**：`config.yaml`、订阅原文/provider、`subs.json`、订阅元信息、进程规则/provider、忽略列表、`app-state.json`、`proxy.json`、流量历史全部改为「临时文件 + Sync + Rename」；系统代理恢复失败时保留备份；看门狗放弃恢复时主动还原系统代理，避免整机指向死端口。

全量、复现、真实内核测试通过；改动未提交，等待人工审查。

---

## 2. 修改文件清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/subscription/guard.go` | 新增 | URL/IP 校验、DNS 防重绑定拨号、重定向策略、响应限制 |
| `internal/subscription/fetch.go` | 修改 | 接入 guarded client；响应体超限报错 |
| `internal/subscription/guard_test.go` | 新增 | SSRF 全场景测试 |
| `internal/subscription/repro_t17_test.go` | 修改 | T17 从「复现」改为「已修复」回归 |
| `internal/kernel/update.go` | 修改 | 升级校验、备份、原子替换、回滚、升级记录、启动时清理临时文件 |
| `internal/kernel/manager.go` | 修改 | 记录升级备份路径 |
| `internal/kernel/upgrade_test.go` | 新增 | PE 校验、备份/回滚、占用失败、记录落盘、失败启动回滚 |
| `internal/configgen/gen.go` | 修改 | `config.yaml` 原子写入 |
| `internal/configgen/gen_test.go` | 修改 | 原子写入回归 |
| `service.go` | 修改 | 升级编排与回滚、系统代理注册表错误处理、看门狗耗尽恢复、全部配置原子写、数据目录迁移失败清理 |
| `c3_persist_test.go` | 新增 | 配置写失败不污染内存、原子写无残留 |

---

## 3. 问题根因

### 3.1 订阅 SSRF

- `do` 只设置 `http.Client` 超时与 TLS 配置，没有限制协议、主机、重定向目标。
- 未检查 DNS 解析结果，`http://internal.example.com` 可解析到 10.x / 127.x。
- 校验与实际连接分离，即使做了字符串校验也存在 DNS Rebinding 时间窗。
- `io.LimitReader(8MB)` 会静默截断超大响应，用户可能拿到不完整配置。
- 重定向默认跟随 10 次且不校验新地址。

### 3.2 内核升级

- `DownloadAndReplace` 使用 `tmp := target + ".new"` 后先 `os.Remove(target)` 再 `Rename`：删除与改名之间如果失败，旧内核已消失、新内核未就位。
- 没有备份、没有回滚；新内核启动失败后用户只能手工处理。
- 摘要为空时直接跳过校验。
- 没有 PE 文件头校验，zip 里取错文件也能写入。
- 没有升级记录，事后无法判断升级前/后版本与回滚原因。

### 3.3 持久化与系统代理

- 多处 `os.WriteFile` 直接覆盖配置文件，写一半崩溃会留下截断 JSON/YAML。
- 内核升级/提取也采用「先删后改名」，存在空窗。
- `reconcileProxy` 在用户关闭系统代理时忽略注册表写错误，仍删除备份文件。
- 看门狗达到重试上限后只发通知，系统代理仍指向已经死亡的内核端口。

---

## 4. 修改前后差异

### 4.1 订阅 SSRF 防护

**入口校验**

- 只允许 `http` / `https`。
- 拒绝 `localhost` / `localhost.` / `*.localhost`。
- 主机是 IP 字面量时立即做特殊地址校验；纯数字、`0x` 形式主机直接拒绝（防整数 IP 绕过）。
- 带 IPv6 zone（`%eth0`）的地址拒绝。

**IP 分类**

拒绝以下地址（IPv4-mapped IPv6 先转 IPv4）：

- `IsLoopback`、`IsPrivate`、`IsUnspecified`、`IsLinkLocalUnicast/Multicast`、`IsMulticast`、`IsInterfaceLocalMulticast`
- 额外网段：`0.0.0.0/8`、`100.64.0.0/10`、`192.0.0.0/24`、`192.0.2.0/24`、`198.18.0.0/15`、`198.51.100.0/24`、`203.0.113.0/24`、`240.0.0.0/4`、`255.255.255.255/32`
- IPv6：`64:ff9b::/96`、`100::/64`、`2001:2::/48`、`2001:db8::/32`，以及 `fc00::/7`、`fe80::/10`、`ff00::/8`（由标准库判断）

**防 DNS Rebinding**

- `guardedDialContext` 在每次建连时重新解析主机，校验**全部** IP；任何一个落到受限网段即整体拒绝（混合解析也不放行）。
- 连接使用校验过的 IP（`net.JoinHostPort(ip, port)`），不再把域名交给系统二次解析。
- 每次重定向都重新做 URL 校验，并重新走上述解析/校验。

**资源限制**

- 重定向最多 5 次；超过返回 `ErrTooManyRedirects`。
- `Content-Length` 与实际读取都限制在 8MB；超过返回 `ErrResponseTooLarge`，不再静默截断。
- 客户端总超时 45s、响应头 20s、TLS 握手 10s、拨号 10s。
- 非 200 响应直接报错。

**TLS**

- 保持用户针对单个订阅显式开启的 `InsecureSkipVerify`，但不自动降级。
- 默认校验证书；测试覆盖「默认失败、显式 insecure 成功」。

### 4.2 内核升级原子性

1. **下载校验**：要求 release 提供 sha256 与 size；下载后校验长度、sha256，再校验 PE（MZ + PE 签名）。
2. **备份**：替换前把当前 `mihomo.exe` 复制为 `mihomo.exe.bak`（临时文件 + Sync + Rename）。
3. **原子替换**：新内核写入同目录 `.mihomo-download-*` 临时文件，`Sync` 后 `Rename` 覆盖；失败重试 4 次（应对 Windows 句柄延迟释放），旧文件不会被预先删除。
4. **标记失败回滚**：写 `.core_custom` 失败时视为升级失败，立即用备份恢复。
5. **启动失败回滚**：`UpdateCore` 在 `RestartKernel` 失败后调用 `RollbackBinary` 恢复备份，再启动旧内核；返回错误中区分「回滚失败」与「旧内核也无法启动」。
6. **记录**：`core-upgrade.json` 保存 `beforeVersion / targetVersion / hash / backupPath / replacedAt / rolledBack / rollbackReason / rolledBackAt`；同时写 `debug.log`。
7. **启动恢复**：`EnsureBinary` 校验自定义内核 PE 是否有效；无效或缺失时清除标记并回退到内置版本；启动时清理中断留下的 `.mihomo-*` 临时文件。

### 4.3 配置与系统代理持久化

- 新增/统一 `writeFileAtomic`：同目录 `CreateTemp` → `Chmod` → `Write` → `Sync` → `Close` → `Rename`；失败自动清理临时文件。
- 覆盖范围：
  - `config.yaml`（`configgen.Write`，含 API secret，0600）
  - 订阅原文、base64 provider 文件、`subscription.yaml`、`subscription.json`、`subs.json`
  - `process-rules.json`、`process-direct.yaml`、`process-proxy.yaml`、`process-ignored.json`
  - `app-state.json`、`proxy.json`、`traffic-history.json`
  - 数据目录迁移与 geo 数据释放
- 进程规则改为「先构造候选表 → 三个文件全部写成功 → 再替换内存 map」；写失败时内存不变，避免界面显示已修改但重启丢失。
- 系统代理：`proxy.json` 先标记 `Pending` 再恢复；恢复后回读注册表校验，成功才删除备份；失败保留备份，下次启动重试。
- `reconcileProxy` 在用户关闭系统代理时收集注册表写错误；任一失败则保留备份并记录日志，不再当作恢复成功删除。
- 看门狗达到上限时主动 `restoreSystemProxy`；失败保留 `proxy.json`，下次启动继续恢复。

---

## 5. 新增测试用例

### 5.1 SSRF（`internal/subscription/guard_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestSSRFRejectsBlockedLiterals` | 127.0.0.1、localhost、localhost.、::1、10/172.16/192.168、169.254、100.64、0.0.0.0、整数 IP、0x IP、fe80::1、2001:db8::1 |
| `TestSSRFRejectsNonHTTPScheme` | file/ftp/gopher |
| `TestSSRFDialRejectsPrivateDNS` | 域名解析到私有 IP，连接前拒绝 |
| `TestSSRFRejectsMixedDNS` | 公网 + 私网混合解析整体拒绝 |
| `TestSSRFAllowsPublicAddress` | 合法公网域名正常拉取 |
| `TestSSRFBlocksRedirectToLoopback` | 外网地址 302 到 127.0.0.1 |
| `TestSSRFBlocksRedirectToPrivateIP` | 302 到 10.0.0.5 |
| `TestSSRFBlocksRedirectToPrivateDNS` | 302 到解析为 192.168 的域名 |
| `TestSSRFRedirectLimit` | 多级重定向超过上限 |
| `TestSSRFLargeResponseRejected` | 8MB+1 响应 |
| `TestSSRFTLSInsecureExplicitOnly` | 默认校验证书失败；显式 insecure 成功 |
| `TestSSRFTimeout` | 服务端超时响应 |
| `TestCheckPublicIPRanges` | 地址分类边界 |

### 5.2 内核升级（`internal/kernel/upgrade_test.go`）

| 用例 | 验证点 |
|---|---|
| `TestValidatePEFile` | 合法 MZ/PE、缺失、偏移越界 |
| `TestInstallBinaryBackupAndRollback` | 备份内容、目标替换、标记、回滚、备份保留 |
| `TestInstallBinaryRenameFailureKeepsOld` | 目标被独占占用时替换失败，旧内核与备份均完好 |
| `TestRollbackWithoutBackupFails` | 无备份回滚报错 |
| `TestDownloadRejectsMissingDigest` | 缺摘要拒绝升级 |
| `TestUpgradeRecordRoundTrip` | 升级记录落盘/读取 |
| `TestFailedNewKernelRollsBackToOld` | 新内核无法启动 → 回滚 → 旧内核启动成功 |

### 5.3 持久化

| 用例 | 验证点 |
|---|---|
| `TestWriteFileAtomicLeavesNoTemp` | 原子写内容完整、无临时文件 |
| `TestSetProcessRuleWriteFailureLeavesMemoryUnchanged` | 写失败时内存规则不提前变更 |
| `TestWriteConfigAtomic` | `config.yaml` 原子写、无 `.tmp` 残留 |
| 既有 `TestT01CrashRecovery*`、`TestT02RestoreFailureKeepsBackupAndRetries`、`TestT10ProxyStateAtomicWriteFailureKeepsOldFile`、`TestT16ExternalProxyChangeIsNotOverwritten` | 系统代理崩溃恢复、恢复失败保留备份、原子写失败保留旧文件、外部修改不覆盖 |
| `TestReproT17RedirectToLoopback` | 订阅入口/重定向到本机被拒绝（已从复现转为回归） |

---

## 6. 测试执行命令与结果

```powershell
# 全量测试
go test -count=1 ./...
# ok routepilot
# ok routepilot/internal/autostart
# ok routepilot/internal/configgen
# ok routepilot/internal/kernel      10.3s
# ok routepilot/internal/procid
# ok routepilot/internal/subscription 3.0s

# 全部复现用例（含 T17 SSRF，现已通过）
go test -count=1 -tags repro ./...
# ok routepilot
# ok routepilot/internal/autostart
# ok routepilot/internal/configgen
# ok routepilot/internal/kernel
# ok routepilot/internal/procid
# ok routepilot/internal/subscription

# 真实 mihomo（C1 生命周期 + C2 同名进程路径）
go test -tags realkernel -count=1 -run "TestRealKernel" -v ./internal/kernel
# --- PASS: TestRealKernelDistinguishesSameNameProcessPaths (0.30s)
# --- PASS: TestRealKernelLifecycle (0.21s)
#     真实内核已就绪: pid=4064 version=v1.19.33

# 静态与前端
go vet ./...        # 无输出
gofmt -l .          # 无输出
cd frontend; npm run build
# ✓ built in 101ms
```

**Race Detector**：仍不可用（`CGO_ENABLED=0`，`-race requires cgo`）。

---

## 7. 尚未解决的问题

1. **未执行真实 GitHub 内核升级**：下载、摘要、大小校验路径已单测覆盖，但未在本机实际下载 20MB 包并替换运行版内核；建议审查后手工验证一次。
2. **断电中断的自动回滚**：替换与标记之间崩溃时，目标要么是完整旧版、要么是完整新版；备份始终保留。但「下次启动自动发现新内核不可用并回滚」尚未实现（属于 C4 的断电矩阵）。
3. **升级下载仍在内存中解包**：zip 最大 200MB，先读入内存再解出 exe；已限制大小，但不是流式落盘。极端大包会占用较多内存。
4. **订阅不再走系统/环境代理**：为避免本机代理劫持与循环，guarded client 设置 `Proxy: nil`；企业内网必须经 HTTP 代理访问订阅的用户会失败。这是安全取舍，未提供例外开关。
5. **注册表读取错误仍按「未设置」处理**：`sysproxy.Current/Override/AutoConfigURL` 读取失败时返回空值，无法区分「值为空」与「读取失败」；写入路径已全部检查错误，读取路径未改接口。
6. **未做图形界面/管理员/TUN 全流程验收**：SSRF、升级、持久化均未在 UI 中完整走一遍。
7. **C4 未执行**：休眠唤醒、网络断开、连续崩溃矩阵、长时间稳定性仍未覆盖。

---

## 8. 兼容性风险

| 变更 | 风险 | 处理 |
|---|---|---|
| 私网/回环订阅地址被拒绝 | 局域网自建订阅、Docker 内网地址将不可用 | 安全要求明确拒绝；错误信息会说明受限地址 |
| 缺少 sha256/size 时拒绝升级 | 旧 release 或镜像源无法升级 | GitHub 当前 release 提供 digest/size；错误信息明确 |
| 旧内核备份常驻 `mihomo.exe.bak` | 额外约 60MB 磁盘 | 保留用于回滚，未自动删除 |
| 升级替换使用 `Rename` 覆盖 | 被杀毒软件/索引器占用时可能失败 | 重试 4 次；失败保留旧内核并报错 |
| 配置全部改为原子写 | 临时文件需要同目录写权限 | 数据目录本来就需要写权限；失败会返回明确错误 |
| 系统代理恢复失败保留 `proxy.json` | 下次启动会再次尝试恢复 | 已按设计保留 Pending/备份，避免丢还原点 |
| 看门狗耗尽后关闭系统代理 | 用户可能感知网络短暂切换 | 内核已不可用，继续指向死端口更糟；事件会通知 |
| `EnsureBinary` 对无效自定义内核回退内置版 | 用户手动放入的损坏内核会被忽略 | 写 kernel 日志并清除 `.core_custom` |

---

## 9. 是否需要人工实机验证

需要：

- 使用真实公网订阅地址拉取；再尝试 `http://127.0.0.1`、`http://10.x`、302 到内网的测试服务，确认均被拒绝。
- 自签证书订阅：默认失败；勾选「忽略证书校验」后成功，且不自动降级。
- 管理员权限 + TUN 下开启系统代理，强杀客户端/内核，确认下次启动能恢复原始代理设置或保留备份。
- 实际执行一次内核升级（建议先备份 `%APPDATA%\RoutePilot`），确认新版本启动、升级记录写入；再用一个故意损坏的内核文件验证回滚。
- 在写入配置时强制结束进程，重启后确认旧配置完整可读（原子写）。
- C4 的休眠/唤醒、网络断开恢复、连续崩溃矩阵仍需另行验收。

---

## 10. Git diff 摘要与提交状态

C3 主要改动：

```
 internal/subscription/guard.go          新增 240 行
 internal/subscription/guard_test.go     新增 253 行
 internal/subscription/fetch.go          +24 / -约 20
 internal/subscription/repro_t17_test.go 修改
 internal/kernel/update.go               +399 / 大量重写
 internal/kernel/manager.go              新增 lastBackup 字段
 internal/kernel/upgrade_test.go         新增 204 行
 internal/configgen/gen.go               +37
 internal/configgen/gen_test.go          +约 30
 service.go                              C3 部分约 350 行（与 C1/C2 共用文件）
 c3_persist_test.go                      新增 50 行
```

整体工作区（含 C1/C2）：

```
15 个已跟踪文件修改，+1593 / -378；
新增：C1/C2/C3 报告、procid 包、kernel 生命周期/Watchdog/真实内核测试、
subscription guard、upgrade/persist 测试等。
```

提交状态：

- 分支 `main`，HEAD `040b6b4`，与 `origin/main` 一致。
- **C1、C2、C3 的全部改动均未提交**，三份报告（C1/C2/C3）也都在工作区。
- 未执行 `git add` / `git commit` / `git push`。
- C4 未执行。

本阶段到此暂停，等待人工审查后再决定提交与进入 C4。
