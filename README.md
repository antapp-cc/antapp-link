# AntApp Link

给 Pi 节点用的虚拟专线。**服务端与客户端全部自研**，不依赖 OpenVPN、不依赖 rinetd、不需要 easy-rsa。

它取代现网这两套东西：

| 被取代 | 原来怎么做的 | 现在 |
|---|---|---|
| OpenVPN server + 客户端 | 装 `OpenVPN.msi`（境外几百 MB），`push redirect-gateway` 全流量走云服 | 两端自己的协议，客户端是**单个 exe**（wintun.dll 已内嵌） |
| `rinetd` + 看门狗 | 用户态代理 `31400-31409`，**只转发 TCP**，每 5 秒解析 OpenVPN status 日志反查虚拟 IP | 内核 DNAT，**TCP + UDP 都转**，不需要轮询、不需要看门狗 |

端口转发交给内核是关键取舍：两端都只做「虚拟网卡 ↔ 隧道」的搬运，TCP/IP 栈、NAT、DNAT 全归操作系统，所以协议里只剩 IP 包和心跳两种数据帧。

## 快速开始

```bash
# 云服（Linux，root）
pwsh -File build.ps1          # 本机交叉编译
# 把 dist/antapp-linkd 和 deploy/install.sh 传到云服，然后：
bash install.sh
/usr/local/bin/antapp-linkd invite pi-node-01 -o /root
```

```powershell
# 节点机（Windows，管理员）
.\antapp-link.exe -c "antapp://..."      # 导入连接码
# 之后双击即可，都在托盘里操作；可勾选开机自启
```

详细步骤见 [deploy/README.md](deploy/README.md)。

## Windows 客户端

### 安装

把 `AntAppLink-Setup` 目录（或那个 zip）给用户，运行 `antapp-setup.exe`：

- 释放到 `C:\Program Files\AntApp Link\`
- 建桌面与开始菜单快捷方式（可勾掉）
- 登记到「应用和功能」；**安装时会把卸载程序自己也复制进安装目录**（`uninstall.exe`），所以用户删掉当初那个安装包之后，仍然能从「应用和功能」或客户端托盘菜单卸载
- 装完自动启动；**首次使用会引导导入连接码**

**连接码、日志、状态文件都在 `C:\Program Files\AntApp Link\data\`** —— 跟程序放在一起，翻安装目录就能看到，不用去 `ProgramData` 里找。该目录写不进去时（比如程序被放在只读位置）才退回到 `%ProgramData%\AntAppLink\`。

安装目录长这样：

```
C:\Program Files\AntApp Link\
  antapp-link.exe
  wintun.dll           首次运行时释放
  data\
    node.conf          连接码（内含私钥）
    antapp.ico
    state.json         网络现场快照，崩溃自愈用
    logs\client.log
```

### 卸载

三个入口，走的是同一个程序：客户端托盘菜单的「卸载 AntApp Link」、「应用和功能」里的卸载按钮、直接跑卸载器。走界面会弹出卸载向导，问要不要连 `data` 一起删；静默模式（`--quiet`）默认全清。

**卸载器住在 `C:\ProgramData\AntApp Link\`，刻意不放在安装目录里。** 因为卸载器要删掉整个安装目录，而 Windows 不允许删除正在运行的 exe —— 早先放在安装目录时，卸载完总会剩下一个 `uninstall.exe` 删不掉。试过四种绕法（`CREATE_NO_WINDOW`、`DETACHED_PROCESS`、`CREATE_BREAKAWAY_FROM_JOB`、改用任务计划/WMI 启动）都没用，因为矛盾在「它住在哪」，不在怎么启动它。

现在的做法：卸载器启动后先把自己复制到 `%TEMP%`、用副本重跑一遍，原进程立刻退出。副本再去删 `%ProgramData%` 和安装目录，**两边都能删干净**（实测残留 0 条）。

客户端托盘里的卸载入口用 `ShellExecute` 启动卸载器而不是 `exec.Command` —— 卸载器是 GUI 程序，走 shell 才会像用户双击那样正常拿到桌面会话和 UAC 处理。

安装包是自写的（本机没有 Inno Setup / NSIS），所以整个流程没有外部依赖。

### 界面与托盘

主窗口布局照着 Pi 节点机上现有的那个 OpenVPN 客户端来，用户不用重新学：

```
当前状态: 已连接（延迟 38 ms）
┌──────────────────────────────────────────────┐
│ 09-27 21:22:48  信息  已连接 server=…         │
│ 09-27 21:22:48  信息  隧道已建立 tunnel_ip=…  │
└──────────────────────────────────────────────┘
分配 IP: 10.10.0.2            AntApp Link 0.1.0
[断开连接]  [重新连接]                    [隐藏]
```

- 状态行会带上延迟；出错时第三行左边改成显示上次的错误原因
- 日志区等宽字体、实时滚动。界面显示的是「时间 级别 消息」的短格式，文件里仍保留 `time=… level=… msg=…` 的结构化原文供排查
- 按钮跟着状态走：没有连接码时主按钮是「导入连接码」，未连接时是「连接」，已连接时是「断开连接」；「重新连接」只在已连接时可用

**关窗口只是收进托盘**（否则隧道会跟着断）；从托盘菜单退出才会断开并还原网络。托盘提示文字随状态变化（未配置 / 未连接 / 已连接 10.10.0.2 · 38 ms），双击回主窗口，右键菜单里有导入连接码、打开数据目录、开机自启、退出。

导入连接码走剪贴板：界面里做不了「粘贴一大段文本」的体验，而连接码本来就是从聊天窗口复制来的。

### 资源与 manifest

`cmd/*/rsrc_windows_amd64.syso` 里嵌了 manifest 和图标，**已经提交进仓库，平时构建不需要额外工具**。

改动 `app.manifest` 或换图标之后，跑一次：

```powershell
pwsh -File tools/mkres.ps1     # 会用 go install 装 rsrc
```

manifest 干三件事：请求 Common-Controls v6（walk 的图标加载依赖它，不然启动即失败）、声明高 DPI、声明 `requireAdministrator`（建虚拟网卡和改路由都要提权，让 UAC 在双击时就弹而不是点「连接」才失败）。

### 在线更新

启动 8 秒后静默查一次更新源；发现新版时状态行显示「有新版本 vX.Y.Z」，按钮区出现「立即更新」。托盘菜单里也有「检查更新」可手动触发。

流程是：下载 → 校验 sha256 → **先断开隧道、还原网络** → 把当前 exe 改名成 `.old` → 写入新版 → 拉起新进程 → 自己退出。新版启动时顺手删掉 `.old`。

顺序不能反：旧进程要是带着「接管中」的网络直接消失，用户就卡在断网状态，而新进程还没起来。拷贝失败会回滚到 `.old` —— 不能让用户手上既没有旧版也没有新版。

**更新源是「一串 URL 按顺序试」**，内置主源：

```
https://github.com/antapp-cc/antapp-link/releases/latest/download/latest.json
```

用的是 `releases/latest/download/<文件名>` 这种固定链接而不是 API —— 未认证的 GitHub API 每 IP 每小时只有 60 次，而且 assets 结构解析起来脆。加自建备源只需往 [update.go](antapp-link/internal/update/update.go) 的 `DefaultSources` 里补一条。环境变量 `ANTAPP_UPDATE_SOURCES`（分号分隔）可临时覆盖，内网部署和本地联调都用它。

**发布一个版本**：

```powershell
pwsh -File tools/release.ps1 -Version 0.2.0
```

它会构建、算 sha256、生成 `latest.json`，并写一份上传指引到输出目录。到 GitHub 建 tag 为 `v0.2.0` 的 Release，把 `antapp-link.exe` 和 `latest.json` 传上去即可 —— **资产名不能改**（客户端靠固定文件名取），记得勾上 "Set as the latest release"。

⚠️ **只上传 `dist/release-<版本>/` 里的文件**。`dist/` 下还有别的同名文件（更新链路自测用的假程序，放在 `dist/_fixtures/`），拿错了会让客户端「发现新版本但下载失败」—— 那份清单里的下载地址指向本机端口。分辨方法很简单：真客户端内嵌了 wintun.dll，**有 11 MB 左右**，假的那个只有 1.6 MB。

**关于校验强度，说实话**：现在只校 sha256，能挡住传输损坏和下载截断，但**挡不住更新源本身被替换** —— 谁能改 `latest.json`，就能往所有节点机推任意程序。清单结构里已经预留了 `sig` 字段，将来要加 Ed25519 签名不用改格式，老客户端也不会因为多了字段解析失败。

本地验整条链路（不需要真发 Release）：

```powershell
# 见 dist/updatetest 的用法：一个假「新版」+ 一个待更新的旧版 + 本地 http 源
go run ./tools/updatetest -sources http://127.0.0.1:8899/latest.json -apply
```

## 项目结构

```
cmd/antapp-linkd/        服务端入口（Linux，单二进制）
cmd/antapp-link/         客户端入口（Windows，单 exe + 托盘）
internal/proto/          帧编解码（两端共用）
internal/pki/            自签 CA 与连接码
internal/server/         TUN、隧道循环、netfilter、CLI、systemd 安装
internal/client/         Wintun 网卡、隧道、路由与 DNS 接管、界面与托盘、自启
internal/setup/          安装与卸载（释放文件、快捷方式、注册表）
internal/update/         在线更新：检查、下载校验、替换自身
deploy/                  云服安装脚本与迁移说明
tools/                   构建期脚本：资源生成、发版打包、更新链路自测
docs/superpowers/        设计文档与实现计划
third_party/             Wintun 出处与哈希（dll 已提取进 internal/client/assets）
```

## 开发

```powershell
go test ./...        # 57 个测试，Windows 上可直接跑
go vet ./...
pwsh -File build.ps1 # 产出 dist/antapp-linkd 与 dist/antapp-link.exe
```

非 Windows / 非 Linux 平台都有桩实现，所以 `go test ./...` 在任何开发机上都能全绿；平台相关的行为由 `*_windows.go` / `*_linux.go` / `*_other.go` 三件套隔开。

## 两个容易踩的坑

这两条都是实测踩出来的，改动前请先读 [设计文档 §8](docs/superpowers/specs/2026-09-27-antapp-link-design.md)：

1. **接管流量必须用两条 `/1` 路由（`0.0.0.0/1` + `128.0.0.0/1`），不能用默认路由。** 默认路由的胜负还要跟跃点数较劲，本地网卡那条是 metric 0，新加的抢不过，表现是「显示已连接但网页打不开」。`/1` 更具体，按最长前缀匹配直接胜出。
2. **`route add` 必须带 `if <隧道接口索引>`。** 不带的话 Windows 会把隧道网关挂到别的网卡上（实测挂到 WLAN），流量根本没进隧道。

另外连接成功后会做一次**出网自检**（隧道内连通性 + 下发的 DNS 能否真的解析），失败就自动断开并还原网络 —— 不允许出现「连上了却什么都打不开、还没有任何提示」的状态。

## 许可

本仓库代码为私有项目。发行包内含 Wintun 的预编译二进制，其许可与义务见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。
