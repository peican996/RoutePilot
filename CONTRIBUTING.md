# 贡献指南

## 提交前

```bash
go vet ./...
go test ./...
cd frontend && npm run lint && npm run build
```

四项全绿再开 PR。CI 会跑同样的检查。

## 代码风格

- **注释写「为什么」，不写「是什么」**。这个项目的注释密度偏高是有意的：很多地方（Job Object、系统代理备份、规则注入顺序）不写原因就会被后人改坏。
- Go 侧不引入除 `golang.org/x/sys` 之外的重量级依赖。
- 前端不引 UI 库与图表库，组件与 SVG 手写。加依赖前请先在 Issue 里说明理由。
- 面板组件保持「私有 UI 状态下沉、共享数据走 props」，不要把状态再堆回 `App.tsx`。

## 提交信息

一句中文说清做了什么，需要时补一行原因：

```
修复订阅到期时间单位错配

expire 是 Unix 秒，之前按毫秒解析，全部显示 1970 年。
```

## PR 范围

一个 PR 只做一件事。功能改动与重构分开提，方便回滚。

## 需要改动的敏感区域

以下文件改动会影响代理接管或内核启动，PR 里请说明测试方式：

- `internal/sysproxy/` — 系统代理的启用与还原
- `internal/kernel/` — 进程生命周期、Job Object
- `internal/configgen/` — 配置生成（规则注入顺序错会导致内核拒绝启动）
- `service.go` 里的 `Boot` / `StopKernel` / `SetTUN` / `SetSystemProxy`

## 新增内嵌资源

不要直接提交二进制。把版本、下载地址（`url`，zip 包还要写 `member`）、sha256、来源写进 `assets/manifest.json`，并在 `THIRD_PARTY_NOTICES.md` 里补上许可证信息。

`url` 一律指向带版本号的上游地址，别用 `latest`；确实只能跟 `latest` 的数据文件，把 `sha256_policy` 设为 `warn`，其余保持默认的 `strict`——发布流程靠它拦住被投毒或被替换的二进制。
