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
- 登记到「应用和功能」，从那里卸载，或用 `antapp-setup.exe --uninstall`
- 装完自动启动；**首次使用会引导导入连接码**

安装包是自写的（本机没有 Inno Setup / NSIS），所以整个流程没有外部依赖。

### 界面与托盘

主窗口显示连接状态、隧道地址、延迟、上下行流量和实时日志，按钮有「连接 / 断开」「从剪贴板导入连接码」「打开数据目录」，另有开机自启开关。

关窗口只是**收进托盘**（否则隧道会跟着断）；从托盘菜单退出才会断开并还原网络。托盘图标会随状态变化提示文字（未配置 / 未连接 / 已连接 10.10.0.2 · 38 ms），双击回到主窗口。

导入连接码走剪贴板：界面里做不了「粘贴一大段文本」的体验，而连接码本来就是从聊天窗口复制来的。

### 资源与 manifest

`cmd/*/rsrc_windows_amd64.syso` 里嵌了 manifest 和图标，**已经提交进仓库，平时构建不需要额外工具**。

改动 `app.manifest` 或换图标之后，跑一次：

```powershell
pwsh -File tools/mkres.ps1     # 会用 go install 装 rsrc
```

manifest 干三件事：请求 Common-Controls v6（walk 的图标加载依赖它，不然启动即失败）、声明高 DPI、声明 `requireAdministrator`（建虚拟网卡和改路由都要提权，让 UAC 在双击时就弹而不是点「连接」才失败）。

## 项目结构

```
cmd/antapp-linkd/        服务端入口（Linux，单二进制）
cmd/antapp-link/         客户端入口（Windows，单 exe + 托盘）
internal/proto/          帧编解码（两端共用）
internal/pki/            自签 CA 与连接码
internal/server/         TUN、隧道循环、netfilter、CLI、systemd 安装
internal/client/         Wintun 网卡、隧道、路由与 DNS 接管、托盘、自启
deploy/                  云服安装脚本与迁移说明
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
