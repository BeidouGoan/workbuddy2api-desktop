# WorkBuddy2API 桌面版

把一个运行在本机的 OpenAI 兼容网关（[WorkBuddy2API](https://github.com/misakano7545/workbuddy2api-panel)）包装成 Windows 桌面程序：双击即用，常驻托盘，不再需要开命令行窗口。

- 上游项目（服务端与 Web 管理面板）：<https://github.com/misakano7545/workbuddy2api-panel>
- 本仓库只新增 `desktop/` 桌面外壳，`repo/` 为上游源码（保持原样，便于一并构建）

## 特性

- **单文件分发**：`WorkBuddy2API.exe` 内嵌服务端，首次运行自动释放到 `%LOCALAPPDATA%\WorkBuddy2API\`，无需安装、无需命令行
- **桌面控制台**：深色界面展示服务状态、监听端口、地址与 API Key，一键复制、一键启停/重启
- **保留完整管理面板**：账号池、代理、积分、任务中心等原有 Web 面板功能全部可用，顶部「← 控制台」可在两者间切换
- **托盘常驻**：关闭窗口即最小化到托盘，右键菜单可显示/退出、开机自启、打开数据目录
- **稳定托管**：崩溃自动退避重启；退出时通过 Job Object 回收子进程，不留孤儿进程
- **健壮性提示**：端口被占用时自动顺延并提示；缺少 WebView2 运行时给出安装引导而不是静默失败
- **低占用**：隐藏到托盘后回收工作集，适合 8G 内存机器长期后台运行

## 环境要求

- Windows 10 / 11 x64（Windows 11 与较新的 Windows 10 已自带 WebView2；如缺失，程序会提示并提供下载入口）
- 无需预装 Go、Node 或 .NET

## 使用

1. 从 [Releases](../../releases) 下载 `WorkBuddy2API.exe`
2. 双击运行。首次启动会在 `%LOCALAPPDATA%\WorkBuddy2API\` 生成 `config.json`，服务监听 `127.0.0.1:7863`
3. 在控制台里打开 Web 管理面板，添加账号后即可通过 `http://127.0.0.1:7863/v1` 调用

数据目录：`%LOCALAPPDATA%\WorkBuddy2API\`（配置、账号、日志、WebView 缓存均在此处，卸载时直接删除该目录即可）。

## 从源码构建

需要 Go 1.22+ 与 PowerShell 5.1+：

```powershell
powershell -ExecutionPolicy Bypass -File desktop\build.ps1
```

脚本会先编译 `repo/cmd/server`，把它嵌入 `desktop`，产物输出到 `dist\WorkBuddy2API.exe`。

目录说明：

| 路径 | 说明 |
| --- | --- |
| `desktop/` | 桌面外壳（Go + WebView2 + Win32），负责窗口、托盘、自启、进程托管、localhost 控制台 |
| `desktop/ui/console.html` | 内嵌控制台页面 |
| `repo/` | 上游服务端源码（网关、Web 管理面板），构建时嵌入为 `assets/server.exe` |
| `dist/` | 构建产物，不入库 |

## 自动跟踪上游与发布

仓库内置 GitHub Actions 工作流 [.github/workflows/upstream-release.yml](.github/workflows/upstream-release.yml)：

- **定时检查**：每天 UTC 03:17（北京时间 11:17）比对上游 HEAD 与 `.github/upstream-commit`
- **自动同步**：发现新提交时，用上游对应 commit 的源码整体替换 `repo/` 并提交到 `main`
- **自动构建发布**：在 `windows-latest` 上执行 `desktop/build.ps1`，随后创建 Release 并上传 `WorkBuddy2API.exe`
- **手动触发**：Actions 页面选择「上游同步与发布」→ Run workflow；勾选 `force` 可在上游无变化时强制重新构建发布

版本号规则：应用版本取自根目录 `VERSION`（当前 `1.0.0`），Release 标签为 `v<应用版本>-build.<Actions 运行号>`，例如 `v1.0.0-build.7`。构建号单调递增，不覆盖历史产物。发布说明由工作流以 UTF-8 写入，并附上游 commit 与对比链接。

工作流使用仓库自带的 `GITHUB_TOKEN`（`contents: write`），不需要任何个人访问令牌。

## 许可与致谢

本项目遵循 **MIT License**，见 [LICENSE](LICENSE)。

上游项目：

- [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)（直接上游）
- 其上游 fork：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)

MIT 允许再分发，本仓库保留上游版权声明。`repo/` 内容与上游提交 `53fbd2e` 一致，除新增 `desktop/` 外未修改服务端行为。

> 本项目仅供学习与研究使用，请遵守上游服务的相关条款。
