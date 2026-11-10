# C4 测试报告

## 1. 环境

| 项 | 值 |
| --- | --- |
| OS | Windows 10 Pro 2009 / Build 26200（Windows 11） |
| CPU 架构 | amd64 |
| Go | `go1.27.3 windows/amd64` |
| Node | 前端使用仓库现有 node_modules，Vite 8.3.6 |
| 内核资源 | `assets/mihomo.exe`（v1.19.33）、wintun、GeoIP 均在仓库内 |
| CGO 编译器 | **不可用**（`gcc` 不在 PATH） |

---

## 2. 执行结果

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 ./...` | 通过 |
| `go test -count=3 ./internal/kernel .` | 通过（kernel 31.357s / root 4.197s） |
| `go test -count=1 -tags repro ./...` | 通过 |
| `go test -tags realkernel -run TestRealKernel ./internal/kernel` | 通过（3 项） |
| `go test -tags realkernel -run TestRealKernelLoadsProcGenProvider .` | 通过 |
| `go vet ./...` | 通过（无输出） |
| `gofmt -l .` | 通过（无输出） |
| `npm run build`（frontend） | 通过（67 modules，dist 299.14 kB） |
| `GOOS=windows GOARCH=amd64 go build ./...` | 通过（exit 0） |
| `go test -race -count=1 ./...` | **未执行成功**：`-race requires cgo`；`CGO_ENABLED=1` 时报 `cgo: C compiler "gcc" not found` |

原始输出（节选）：

```
ok  	routepilot	2.811s
ok  	routepilot/internal/autostart	0.738s
ok  	routepilot/internal/configgen	0.736s
ok  	routepilot/internal/kernel	11.613s
ok  	routepilot/internal/procid	0.778s
ok  	routepilot/internal/subscription	2.511s
```

```
--- PASS: TestRealKernelDistinguishesSameNameProcessPaths (0.32s)
--- PASS: TestRealKernelSmartRoutingRulesStart (0.22s)
--- PASS: TestRealKernelLifecycle (0.22s)
--- PASS: TestRealKernelLoadsProcGenProvider (0.39s)
```

---

## 3. 未执行项与原因

| 项目 | 原因 | 替代验证 |
| --- | --- | --- |
| 本地 `go test -race` | 本机无 gcc，CGO 不可用 | 已新增 CI race job（windows-latest + `choco install mingw` + `CGO_ENABLED=1`）；本轮未在 CI 实际运行 |
| 真实断电 / 休眠唤醒 | 自动化环境无法安全模拟 | 用 generation 发布点、退出信号、故障注入覆盖同类的"半写/崩溃"路径；实机步骤见矩阵文档 |
| 真实注册表操作 | 避免修改用户电脑 | 全部使用 `fakeRegistry` 故障注入；管理员实机步骤见 `C4-FAILURE-MATRIX.md` 第 13 节 |
| 新构建 exe 的实机运行 | 当前用户正在运行旧版（bin 下 exe 被占用） | 已完成 Windows amd64 编译检查；C4 变更未部署到运行实例，避免中断用户网络 |

> 说明：没有把未执行的测试写成通过。`realkernel` 测试是真实 mihomo 进程，
> 已在本机实际执行。

---

## 4. 新增测试覆盖

### 4.1 TUN 与系统代理（`c4_tun_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestTUNToggleBlocksConcurrentProxyEnable` | TUN 重启阶段并发 `SetSystemProxy(true)` 被拒绝；最终无双接管 |
| `TestTUNEnableFailureRestoresProxyTakeover` | TUN 开启失败回滚到切换前的代理接管状态 |
| `TestTUNEnableRestoreFailureKeepsPendingBackup` | 切 TUN 前代理恢复失败：保留 Pending 备份、不重启内核 |
| `TestTUNDisableFailureDoesNotRestoreProxy` | 关 TUN 失败：不宣称代理已恢复，保留恢复意图 |
| `TestTUNDisableSuccessRestoresProxy` | 关 TUN 成功按记录恢复代理 |
| `TestTUNDisableWithoutKernelPersistsRestoreIntent` | 内核未运行时关 TUN：不写注册表，但把恢复意图写入启动流程 |
| `TestReconcileSkippedDuringTUNToggle` | TUN 切换期间 reconcile 不写注册表 |

### 4.2 内核启动/退出竞态（`start_exit_race_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestStartRejectsExitBeforeRunningCommit` | `before-running` hook 阻塞 → 子进程退出 → 放行 → Start 返回 `ErrProcessExited`，状态不为 Running |
| `TestStartAfterExitBeforeRunningCanRetry` | 退出后 run 被清空，下一次 Start 不误报 `ErrAlreadyRunning` |

### 4.3 订阅安全（`redirect_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestCrossHostRedirectRejectedWithoutLeakingToken` | 带 token 跳转第三方：拒绝，第三方 0 请求 |
| `TestSameHostRedirectSendsNoReferer` | 同主机跳转允许，但无 Referer、无原始查询参数 |
| `TestSameSiteSubdomainRedirectAllowed` | 同站点子域跳转可用 |
| `TestRedirectToPrivateStillBlocked` | 重定向到内网仍被 SSRF 拦截 |
| `TestRedirectDowngradeRejected` | https → http 拒绝 |
| `TestRedirectWithUserInfoRejected` | 目标带 userinfo 拒绝 |
| `TestSameSiteHostBoundary` | `evil-example.com` 不被当成 `example.com` 子域 |
| `TestDirectSubscriptionStillWorks` | 正常订阅拉取不受影响 |

### 4.4 订阅事务（`c4_subscription_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestRefreshDoesNotOverrideConcurrentSwitch` | 下载期间切换 B，刷新 A 返回 `errSubscriptionChanged`，B 保持激活 |
| `TestSubscriptionOpsConcurrentConsistency` | 7 个并发 apply/switch，无死锁，条目不重复、下标与激活一致 |
| `TestRemoveInactiveKeepsActive` | 删除非激活订阅不影响当前配置 |

### 4.5 配置损坏（`c4_config_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestCorruptConfigIsPreservedAndReported` | 5 类配置 × 非法 JSON / 类型错误 / 截断：备份 + bootErr + 状态回退 |
| `TestEmptyConfigIsTreatedAsCorrupt` | 空文件按截断处理 |
| `TestFutureVersionConfigIsRejected` | `version: 99` 不被解释、被保留 |
| `TestMissingConfigIsNormal` | 首次启动不报错、不产生备份 |
| `TestSaveAfterCorruptDoesNotDestroyBackup` | 后续保存只写新文件，备份内容不变 |

### 4.6 generation 发布（`c4_procgen_test.go`）

| 测试 | 覆盖 |
| --- | --- |
| `TestProcGenPublishAndResolve` | 发布后可解析、文件齐全、options 使用 procgen 路径 |
| `TestProcGenManifestFailureKeepsPrevious` | manifest 未发布：旧版本继续可用 |
| `TestProcGenCorruptManifestFallsBackPrev` | current 损坏回退 prev，重启加载同样回退 |
| `TestProcGenTruncatedFileFallsBack` | 最新一代文件截断，哈希校验失败回退 |
| `TestLoadProcRulesFallsBackToLegacy` | 无 generation 时使用旧版平铺文件 |
| `TestProcGenCleanupKeepsTwoGenerations` | 多次发布只保留 current + prev |
| `TestProcGenRejectsPathTraversalManifest` | manifest 中 `../` 路径被拒绝 |

### 4.7 真实内核（`realkernel_procgen_test.go`）

真实 mihomo 从 `./procgen/*.yaml` 加载 classical rule-provider 成功启动，
且 `/providers/rules` 返回 `pp-direct`。
