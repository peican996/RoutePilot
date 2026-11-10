# C5 故障矩阵

基线：`1f48bbd75cc25e17f1d416cc29440b21618b0011`
说明：`修复前` 列为基线行为，`修复后` 列为当前实现；`测试` 列为本阶段新增/沿用的确定性用例。

---

## 一、TUN 开启路径

| ID | 故障注入 | 发生时点 | 修复前行为 | 修复后行为 | 测试 |
|---|---|---|---|---|---|
| F-01 | 快照写盘失败 | 关闭系统代理之前 | 无快照概念，继续执行 `Disable()` | 中止操作，注册表不动，返回错误 | `TestC5TunAbortsWhenSnapshotCannotPersist` |
| F-02 | 进程被强制终止 | `Disable()` 之后、内核重启成功之前 | 快照只在内存，永久丢失 | 快照已在 app-state；下次启动 `retryTunProxyRestore` 恢复 | `TestC5TunCrashBeforeCommitRestoresOnBoot` |
| F-03 | 系统断电 | 同上 | 同上 | 同上（app-state 原子写） | 同上 |
| F-04 | `Disable()` 失败 | 关闭系统代理 | 仅回滚注册表 | 回滚注册表；回滚失败则保留快照 + Pending | `errTUNRollbackFailed` 分支（代码路径） |
| F-05 | 内核重启失败、旧内核仍活 | 重启阶段 | 重新接管系统代理 | 同左，且按回滚结果决定快照去留 | 既有 `c1_proxy_test.go` 系列 |
| F-06 | 内核重启失败、内核已死 | 重启阶段 | 应用 `prevBackup` | 同左，并置 `restoreProxy` 意图 | 同上 |
| F-07 | 本程序此前已接管代理 | 开 TUN 前 | 快照记录自己的旧端口 | 快照取接管备份里的用户原始配置 | `TestC5TunSnapshotUsesOriginalFromTakeoverBackup` |
| F-08 | 开启成功 | 提交阶段 | 只保留 `tunPrevProxyOn` | 同时持久化 `tunPrevRegistry` + `tunAfterDisable` | `TestC5TunSnapshotPersistedBeforeDisable` |

## 二、TUN 关闭路径

| ID | 故障注入 | 发生时点 | 修复前行为 | 修复后行为 | 测试 |
|---|---|---|---|---|---|
| F-09 | 恢复注册表失败 | `applyProxySnapshot` | 仅写日志，快照已删 | 报错、保留快照、置 Pending、下次启动重试 | `TestC5TunOffRestoreFailureKeepsPendingThenRetries` |
| F-10 | 写入成功但回读不一致 | `verifyProxySnapshot` | 无回读校验 | 判为失败，保留快照与 Pending | 同上（校验分支） |
| F-11 | 恢复成功但删除快照失败 | `clearTunSnapshot` | 无删除失败概念 | 如实报错，内存快照回滚；下次启动幂等收敛 | `TestC5TunSnapshotDeleteFailureReported` |
| F-12 | PAC-only 配置 | 恢复阶段 | `Server == ""` 直接跳过；`applyProxySnapshot` 还会关掉启用态 | 正常恢复启用态与 PAC，不写 ProxyServer | `TestC5TunRestorePACOnly` |
| F-13 | 用户/外部软件在 TUN 期间改了代理 | 恢复阶段 | 无条件覆盖 | 比对 `afterDisable` 后放弃还原 | `TestC5TunExternalChangeNotOverwritten` |
| F-14 | 关闭成功且原先由本程序接管 | 提交阶段 | 只恢复 `tunPrevProxyOn` | 重新接管；快照清除；接管备份记录用户原值 | `TestC5TunSnapshotUsesOriginalFromTakeoverBackup` |
| F-15 | 关闭 TUN 时内核未运行 | 重启阶段 | 置 `restoreProxy` | 同左；快照清除并交由启动接管 | 代码路径 + 既有测试 |
| F-16 | 关闭过程中崩溃（tunOn 已置 false） | 提交之后 | 快照已删，无恢复 | 快照在盘、启动补做恢复 | `TestC5TunCrashBeforeCommitRestoresOnBoot`（同一恢复入口） |
| F-17 | 启动时 TUN 仍生效 | `Boot` | 无快照，无判定 | 跳过恢复，保留快照避免双代理 | `TestC5TunOnBootKeepsSnapshot` |

## 三、系统代理通用路径（回归）

| ID | 故障注入 | 修复前 | 修复后 | 测试 |
|---|---|---|---|---|
| F-18 | 还原写入失败 | 保留 Pending 备份 | 不变 | `TestR1RestoreWriteFailuresKeepBackup` |
| F-19 | 回读不一致 | 保留备份 | 不变 | `TestR1ReadbackMismatchKeepsBackup` |
| F-20 | 删除 proxy.json 失败 | 报错并保留内存备份 | 不变 | `TestRestoreSuccessRemoveFailureKeepsBackup` |
| F-21 | 外部接管后重试 | 放弃还原 | 不变 | `proxyStillOurs` 系列 |

## 四、内核升级

| ID | 故障注入 | 修复前 | 修复后 | 测试 |
|---|---|---|---|---|
| F-22 | `1.19.0-alpha.2` vs `1.19.0-alpha.10` | 判为相等 | 正确判小于 | `TestCompareSemver` |
| F-23 | 本地版本高于 latest | 可能提示升级 → 降级 | 不提示 | `TestVersionNewerSemantics` |
| F-24 | build metadata 不同 | 可能被当作版本差异 | 忽略 | 同上 |
| F-25 | 非法版本串 | 按"不等"提示升级 | 保守不提示 | 同上 |

## 五、进程身份

| ID | 故障注入 | 修复前 | 修复后 | 测试 |
|---|---|---|---|---|
| F-26 | PID 被同名新实例复用 | 采用旧快照路径 | `verify` 失败 → 强制刷新 → 新路径 | `TestLookupNamePIDReuseForcesRefresh` |
| F-27 | 实例校验持续失败 | 静默采用 | 降级为名称匹配并留痕 | `TestLookupNameStaleInstanceDowngradesWithLog` |
| F-28 | 同名不同路径 | 歧义 | 不变（歧义 + 不生成路径规则） | `TestLookupNameSameNameDifferentPathIsAmbiguous` |
| F-29 | 快照刷新失败 | 静默沿用 | 记录"进程快照刷新失败" | `TestRefreshFailureIsLogged` |
| F-30 | Toolhelp 名称与 OpenProcess 路径不一致 | 拼成错误身份 | 丢弃路径，仅保留名称 | `queryProcess` 一致性校验（代码路径） |

## 六、开机自启

| ID | 故障注入 | 修复前 | 修复后 | 测试 |
|---|---|---|---|---|
| F-31 | 计划任务查询权限不足 | 判为"不存在"→ 写 Run 项 → 双实例 | 返回错误，不写 Run | `TestSetUnknownTaskStateNeverWritesRunValue` |
| F-32 | 任务不存在（明确） | 写 Run 项 | 不变 | `TestClassifyTaskQuery` + `Set` |
| F-33 | 任务存在 + 提权开启 | 清理 Run | 不变 | `TestSetWithExistingTaskConvergesRunValue` |
| F-34 | 任务存在 + 非提权开启 | 报错 | 不变 | `TestSetNonElevatedWithExistingTaskRejected` |
| F-35 | 升级自启时查询失败 | 早退，不清理 | 返回错误，不改动 Run | `TestUpgradeIfElevatedUnknownStateIsNoop` |

---

## 七、覆盖矩阵小结

| 要求 | 覆盖的故障 ID | 确定性测试 |
|---|---|---|
| 快照先持久化 | F-01, F-02, F-03, F-08 | 3 |
| 四字段完整 | F-08 | 1 |
| 崩溃/断电不丢依据 | F-02, F-03, F-16 | 2 |
| 先恢复再回读验证 | F-09, F-10 | 1 |
| 验证通过才删快照 | F-11 | 1 |
| 失败保留 Pending 并启动重试 | F-09, F-16 | 2 |
| PAC-only 可恢复 | F-12 | 1 |
| 不覆盖外部新设置 | F-13 | 1 |
| SemVer 边界 | F-22 ~ F-25 | 2 |
| 进程身份降级留痕 | F-26 ~ F-30 | 4 |
| 自启三态与双实例 | F-31 ~ F-35 | 5 |

所有用例均不使用 `sleep` 作为并发同步手段；涉及时序的断言通过 `enableHook` / `disableHook` / 注入函数 / 磁盘现场构造完成。
