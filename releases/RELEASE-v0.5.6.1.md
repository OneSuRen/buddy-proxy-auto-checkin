## v0.5.6.1 · 2026-10-16 · 概览积分余额改为号池汇总

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

- 管理台「概览与监控」的余额指标由「Credits 余额」改为「积分余额汇总」：把当前激活号池内所有**已启用且已登录**账号的积分余额（`remaining`）求和后展示；任一账号不限量时显示「不限量」。
- 首屏会静默拉取当前号池全部已登录账号的余额（原先只拉主账号），逐个返回即时刷新汇总值；全部查询失败时显示「查询失败」。
- 管理台 UI 版本戳更新为 `2026.10.16-credits-sum`。

### 升级

覆盖旧二进制后重启。账号池不用迁移。

完整说明见 [`CHANGELOG.md`](https://github.com/wnddd839/buddy-proxy/blob/main/CHANGELOG.md)。
