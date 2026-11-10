# C4 关键故障恢复矩阵

说明：
- "验证结果"只写实际执行过的内容；未实机验证的场景明确标注为"设计分析/未实机"。
- 自动化测试全部使用 `fakeRegistry` 与临时数据目录，不会改动本机系统设置。

---

## 1. 内核启动过程中程序崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 应用已启动，`mgr.Start()` 正在等待 API 就绪 |
| 故障发生点 | `startLocked()` 的 `awaitReady` 与 `state=Running` 之间 |
| 重启后读取 | `app-state.json`（TUN/代理意图）、`proxy.json`、内核二进制与配置 |
| 恢复行为 | 进程已退出时不再提交 Running；下次启动重新走完整 Start |
| 残留风险 | 无系统代理残留（接管在 afterStart 之后）；可能残留 `bin` 下内核进程，由 Job Object 随父进程结束 |
| 数据丢失 | 无 |
| 验证 | `TestStartRejectsExitBeforeRunningCommit`、`TestStartAfterExitBeforeRunningCanRetry`（确定性 hook） |

---

## 2. 内核运行过程中程序崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | Manager Running，Windows Job Object 已绑定内核 |
| 故障发生点 | 应用进程任意时刻被强杀 |
| 重启后读取 | `app-state.json`、`proxy.json`、`kernel.log` |
| 恢复行为 | Job Object `KILL_ON_JOB_CLOSE` 结束内核；下次启动重新拉起；`product.json` 序号防重复接管 |
| 残留风险 | 上一次的 kernel.log 保留用于复盘 |
| 数据丢失 | 无（进程规则/订阅均为原子写） |
| 验证 | 既有 `TestJobAttached`、真实内核生命周期测试；本机未做强制杀进程实测 |

---

## 3. 系统代理开启后程序崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | `proxyOn=true`，注册表指向 `127.0.0.1:17890`，`proxy.json` 记录原配置 |
| 故障发生点 | 接管成功后的任意时刻 |
| 重启后读取 | `app-state.json` 的 `proxyOn`、`proxy.json` 的 Addr/Server/Override/PAC |
| 恢复行为 | 启动时 `recoverPendingProxyRestore` 处理 Pending；否则 `reconcileProxy` 确认地址仍是我们写入的端口后继续接管；退出时还原 |
| 残留风险 | 若内核启动失败，`failBoot` 主动还原代理 |
| 数据丢失 | 无 |
| 验证 | `TestT01CrashRecoveryRestoresOriginal`、`TestT01CrashRecoveryReconcilesActiveTakeover`、`TestR1BootRetriesPendingRestore` |

---

## 4. TUN 开启后程序崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | `tunOn=true`，系统代理已还原并关闭，虚拟网卡存在 |
| 故障发生点 | TUN 重启成功后的任意时刻 |
| 重启后读取 | `app-state.json` 的 `tun`、`tunPrevProxyOn` |
| 恢复行为 | 管理员启动时按 `tun` 生成含 TUN 的配置；非管理员不会声称 TUN 已开；系统代理保持关闭 |
| 残留风险 | 崩溃瞬间 Windows 路由表可能短暂残留 Meta 网卡，内核重启后重建 |
| 数据丢失 | 无 |
| 验证 | `TestTUNDisableSuccessRestoresProxy`、`TestTUNDisableFailureDoesNotRestoreProxy`；真实内核 TUN 未在自动化中启用（需要管理员） |

---

## 5. 系统代理恢复过程中程序崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 正在把注册表还原为用户原配置，`proxy.json` 已标记 `Pending` |
| 故障发生点 | `restoreProxyTransactionLocked` 写注册表与删除备份之间 |
| 重启后读取 | `proxy.json`（`saved=true, pending=true`） |
| 恢复行为 | 启动阶段优先重试；成功后才删除备份 |
| 残留风险 | 若用户/外部软件在失败后改了代理，`proxyStillOurs` 判定为外部接管并放弃还原，不覆盖 |
| 数据丢失 | 无；备份保留到校验通过 |
| 验证 | `TestT02RestoreFailureKeepsBackupAndRetries`、`TestR1ExternalChangeAfterFailedRestoreIsNotOverwritten`、`TestR1RestoreIsIdempotent` |

---

## 6. 订阅下载完成但配置提交失败

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 旧订阅可用，刷新已拿到新内容 |
| 故障发生点 | `applySubscriptionExpect` 写文件/写元信息失败 |
| 重启后读取 | `subs.json`、`sub-*.yaml`、`subscription.yaml` |
| 恢复行为 | 返回错误，内存与激活订阅不变；旧订阅继续可用 |
| 残留风险 | 已写入的新 `sub-<url>.yaml` 可能留下，但不会被激活 |
| 数据丢失 | 无 |
| 验证 | `TestRefreshDoesNotOverrideConcurrentSwitch`、`TestSubscriptionOpsConcurrentConsistency` |

---

## 7. 多文件配置写入中途崩溃

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 已有一代完整 `procgen` |
| 故障发生点 | 新 generation 文件写到一半，或文件写完但 `current.json` 未发布 |
| 重启后读取 | `procgen/current.json` → `current.json.prev` → 旧版平铺文件 |
| 恢复行为 | 只接受 manifest + 三文件哈希全部匹配的版本；否则回退上一代/旧格式 |
| 残留风险 | 半成品 generation 文件会被清理；旧版平铺文件可能短暂混合，但不被当前版本读取 |
| 数据丢失 | 无 |
| 验证 | `TestProcGenManifestFailureKeepsPrevious`、`TestProcGenTruncatedFileFallsBack`、`TestProcGenCorruptManifestFallsBackPrev`、`TestProcGenCleanupKeepsTwoGenerations`、`TestRealKernelLoadsProcGenProvider` |

---

## 8. 配置文件损坏

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 用户已有订阅/规则/窗口状态 |
| 故障发生点 | 断电导致 JSON 截断、磁盘位翻转、手工编辑错误 |
| 重启后读取 | 各 JSON 文件 |
| 恢复行为 | 区分缺失/空/损坏/版本不兼容；损坏文件改名为 `.corrupt-<时间>`；界面显示启动错误；后续保存写入新文件 |
| 残留风险 | 用户需要手工从备份恢复；程序不会静默覆盖 |
| 数据丢失 | 不再发生不可逆丢失（原文件保留） |
| 验证 | `TestCorruptConfigIsPreservedAndReported`（7 子用例）、`TestEmptyConfigIsTreatedAsCorrupt`、`TestFutureVersionConfigIsRejected`、`TestSaveAfterCorruptDoesNotDestroyBackup` |

---

## 9. 内核升级过程中断电

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 旧内核可运行，升级包已下载 |
| 故障发生点 | 临时文件写入 / rename / `.bak` 生成任意阶段 |
| 重启后读取 | `mihomo.exe`、`mihomo.exe.bak`、`core-upgrade.json` |
| 恢复行为 | C3 已实现"临时文件 + Sync + rename + 备份 + 启动失败回滚"；C4 未改动该路径 |
| 残留风险 | 极端情况下 `.bak` 与当前文件都不可用，需要重新下载内核 |
| 数据丢失 | 用户配置不受影响 |
| 验证 | 既有 `internal/kernel/update_test.go`、`upgrade_test.go`；本轮未重复执行断电模拟 |

---

## 10. 系统休眠和唤醒

| 项 | 内容 |
| --- | --- |
| 故障前状态 | TUN 或系统代理接管中 |
| 故障发生点 | 休眠/唤醒导致网卡与路由重建 |
| 重启后读取 | 不重启程序；Status 轮询与 Watchdog 继续工作 |
| 恢复行为 | TUN 由 mihomo 的 auto-route 恢复；系统代理是注册表状态，不随休眠变化 |
| 残留风险 | 少数网卡驱动唤醒后 TUN 路由丢失，需要重启内核；当前没有专门的网络变更监听 |
| 数据丢失 | 无 |
| 验证 | **未实机验证**（自动化无法安全触发休眠）。列为 P3 已知限制 |

---

## 11. 网络断开和恢复

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 内核运行，订阅与节点可用 |
| 故障发生点 | 断网导致订阅拉取失败 / 节点不可用 |
| 重启后读取 | `subs.json` 与本地 `sub-*.yaml` |
| 恢复行为 | 拉取失败只记录 `subInfo.Error`，旧订阅与本地配置继续可用；网络健康检查每 2 分钟探测 |
| 残留风险 | 自动更新在断网期间跳过，网络恢复后下一轮补齐 |
| 数据丢失 | 无 |
| 验证 | `TestLegalSubscriptionUpdateStillWorks`（连通性）；断网切换未实机演练 |

---

## 12. Watchdog 连续重启失败

| 项 | 内容 |
| --- | --- |
| 故障前状态 | 内核异常退出，看门狗开始退避 |
| 故障发生点 | 重启持续失败（端口占用、二进制损坏） |
| 重启后读取 | `app-state.json`、`proxy.json` |
| 恢复行为 | 滑动窗口 + 指数退避 + 上限 5 次；熔断时还原系统代理与 WinHTTP 并通知 UI；`restoreSystemProxy` 失败保留备份 |
| 残留风险 | 熔断后需要用户手动处理（释放端口/更新内核） |
| 数据丢失 | 无 |
| 验证 | `TestWatchdogExhaustsAfterMaxRestarts`、`TestWatchdogRestartFailureCountsTowardLimit`、`TestWatchdogR1RestartFailureAndOnExitDeduplicated`、`TestR1WatchdogExhaustedRestoreSerializedWithUser`、`TestWatchdogR1StopDuringRestart` |

---

## 13. 管理员实机验证步骤

普通测试不会改本机设置。需要真实验证时：

```powershell
# 备份原值
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v ProxyEnable
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v ProxyServer
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v ProxyOverride
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v AutoConfigURL
```

1. 管理员启动，确认内核 Running、TUN 关闭。
2. 开系统代理 → 确认注册表指向 `127.0.0.1:17890`、PAC 为空。
3. 开 TUN，并**立即**在托盘反复点"系统代理"：应提示"TUN 正在切换"或被 TUN 拒绝；
   最终 `ProxyEnable=0` 且 Meta 网卡 Up。
4. 关 TUN → 确认系统代理按原状态恢复。
5. 强杀进程 → 重启：确认代理/TUN 状态与 `app-state.json` 一致，无死端口。
6. 手工把 `app-state.json` 改坏 → 重启：界面应显示配置损坏提示，目录出现
   `app-state.json.corrupt-*`，原内容仍在。
7. 检查数据目录 `procgen/`：`current.json` 与 3 个文件存在；手工截断其中一个 →
   重启应回退上一代或旧版平铺文件，内核仍能启动。
8. 订阅跨站跳转：用一个会 302 到其他域名的订阅地址，确认更新被拒绝且日志说明原因。
