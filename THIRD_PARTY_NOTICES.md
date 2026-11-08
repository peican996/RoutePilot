# 第三方组件声明

本项目以 GPL-3.0 发布。以下组件随二进制一起分发，各自的许可证与版权归属如下。
**升级任何版本时，请同步更新本文件与 `assets/manifest.json`。**

## 随包分发的二进制

### mihomo

- 版本：v1.19.33
- 许可证：**GPL-3.0**
- 源码：https://github.com/MetaCubeX/mihomo
- 用途：代理内核，以**独立进程**方式启动，通过外部控制 API 通信
- 文件：`assets/mihomo.exe`

`LICENSE` 中已包含 GPL-3.0 全文。获取对应源码请访问上述仓库，或使用本项目记录的具体版本标签。

### wintun

- 版本：0.14.1
- 许可证：**GPL-2.0** 或 WireGuard LLC 商业授权（二选一）
- 主页：https://www.wintun.net/
- 用途：TUN 模式的虚拟网卡驱动
- 文件：`assets/wintun.dll`

本项目按 GPL-2.0 使用。若需闭源分发，请自行向 WireGuard LLC 申请商业授权。

### GeoIP / GeoSite 数据

- 来源：https://github.com/MetaCubeX/meta-rules-dat
- 许可证：以该仓库声明为准（copyleft 系，随上游更新可能有变化）
- 用途：离线 GeoIP 查询与域名分流规则
- 文件：`assets/geo/geoip.metadb`、`assets/geo/geosite.dat`

## 构建期依赖（不随包分发）

Go 与 npm 依赖均通过 `go.mod` / `frontend/package-lock.json` 锁定版本。
主要直接依赖的许可证类型：

| 依赖 | 许可证 |
|---|---|
| Wails v3 | MIT |
| React / React DOM | MIT |
| Vite | MIT |
| golang.org/x/sys | BSD-3-Clause |
| gopkg.in/yaml.v3 | MIT / Apache-2.0 |

完整的传递依赖清单可由以下命令生成：

```bash
go list -m -json all | jq -r '.Path + " " + (.Version // "-")'
cd frontend && npx license-checker --summary
```

## 字体与图标

界面内嵌以下字体（`frontend/public/fonts`），均为 **SIL Open Font License 1.1**：

| 字体 | 来源 |
|---|---|
| Orbitron | https://github.com/theleagueof/orbitron |
| Rajdhani | https://github.com/itfoundry/rajdhani |
| JetBrains Mono | https://github.com/JetBrains/JetBrainsMono |

应用图标为项目自制，随本项目以 GPL-3.0 发布。字体许可证全文见各上游仓库的 `OFL.txt`。
