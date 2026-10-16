## v0.5.6 · 2026-10-16 · 每日自动签到

### 下载哪个文件？

| 你的系统 | 下载 |
|----------|------|
| **Windows 64 位** | `codebuddy-proxy-windows-x64.exe` |
| Linux x64 | `codebuddy-proxy-linux-amd64` |
| macOS Apple 芯片 | `codebuddy-proxy-darwin-arm64` |
| macOS Intel | `codebuddy-proxy-darwin-amd64` |

校验：`SHA256SUMS.txt`

---

### 改了什么

- 新增每日自动签到：管理台勾选开启并设置时间（`HH:MM`），到点后自动为全部账号签到 —— 国内站与国际站分别跑满各自号池，不局限于当前激活号池。
- 配置持久化在 `proxy-autocheckin.json`（与账号文件同目录），重启保留；当天目标时间已过且当天尚未跑过时会自动补跑一次。
- 新增管理台接口：`autocheckin/config`（读 / 写）、`autocheckin/status`（含国内 / 国际分站最近结果）、`autocheckin/run`（立即执行一次）。
- 可用环境变量无界面引导（仅在没有配置文件时）：`CODEBUDDY_AUTO_CHECKIN_ENABLED`、`CODEBUDDY_AUTO_CHECKIN_TIME`。

### 升级

覆盖旧二进制后重启。自动签到默认关闭，需在管理台勾选并设置时间。账号池不用迁移。

完整说明见 [`CHANGELOG.md`](https://github.com/wnddd839/buddy-proxy/blob/main/CHANGELOG.md)。
