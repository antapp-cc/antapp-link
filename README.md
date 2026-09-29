# AntApp Link

Pi 节点虚拟专线：**服务端（Linux 单二进制）+ 客户端（Windows 单 exe）全自研**。TLS 1.3 双向认证隧道 + Wintun 虚拟网卡 + 内核端口转发。

## 架构

```
Pi 节点机 (Windows)                          云服 (Linux)
┌──────────────────────────┐            ┌───────────────────────────────┐
│ Pi Node / Pi Desktop      │            │ antapp-linkd（systemd 常驻）   │
│        ↕ 内核 TCP/IP 栈    │            │  ├ antapp0  10.10.0.1/24      │
│ Wintun「AntApp Link」      │◄─TLS 1.3──►│  ├ :62233  mTLS 隧道           │
│   10.10.0.2  （单 exe）    │            │  └ dnsmasq 隧道 DNS（无 AAAA） │
│ antapp-link.exe（托盘）    │            │ 端口转发 31400-31409（应答式）  │
└──────────────────────────┘            └───────────────────────────────┘
```

端口转发采用服务端应答式转发器：云服监听 31400-31409，收到连接立即应答、再经隧道转给节点机同端口（节点机无需任何端口软件）。

## 功能

**客户端（Windows，托盘应用）**

- **秒级连接**：网络接管全程进程内系统调用（WireGuard winipcfg），不拉起任何外部命令
- **国内分流**：809 条国内网段直连，其余走隧道——服务端故障也不影响国内上网
- **出口迁移**：换 Wi-Fi / 插拔网线 / 睡眠唤醒自动跟随，绕行路由、DNS、分流无缝迁移，隧道秒级自愈
- **DNS 防污染**：解析走隧道中继（服务端 dnsmasq `filter-AAAA`，v4-only 隧道不发 AAAA），`minepi.com` 一类被污染域名经隧道出口解析
- **多配置文件管理**（OpenVPN 式）：`config\*.antapp` 每个文件一台服务器，托盘菜单直接列出、点谁连谁；一个文件自动连，多个让用户挑；文件名即身份，不改名不复制
- **安全自愈**：连接前先探测服务端（不通则不动网络）；出网自检失败自动断开还原；崩溃 / 强杀后启动自愈
- **在线更新**：启动 8 秒后 + 每 1 小时自动检查；发现新版本**自动下载、校验、应用重启**，全程无交互（界面同时保留手动更新按钮）；启动时自动清理上次更新的 .old 备份
- 托盘 + 主窗口（状态 / 日志 / 延迟 / 流量 / 当前配置文件）、连接码双击导入与托盘热切换、开机自启（计划任务，无 UAC 弹窗）、单实例

**服务端（Linux）**

- 单二进制 + systemd 双单元（netfilter 与隧道分离），`install.sh` 一键安装（依赖自动装，含 dnsmasq）
- mTLS 双向证书认证；连接码内含证书与私钥，等同密码
- 端口转发在云服应答后经隧道转给节点机，节点机零端口配置
- BBR 拥塞控制；CA 幂等生成，重装永不重建（已签发连接码不失效）

## 快速开始

### 服务端（云服，root）

```bash
pwsh -File build.ps1        # 本机交叉编译，发布到 GitHub Release
# 云服（root）一条命令自动部署——自动下载二进制、装依赖、配 DNS 中继：
wget -qO install.sh "https://raw.githubusercontent.com/antapp-cc/antapp-link/main/deploy/install.sh" && bash install.sh
/usr/local/bin/antapp-linkd invite pi-node-01 -o /root   # 产出连接码文件（pinode.antapp）+ 单行码
```

> 每台 VPS 的 PKI 密钥在首次安装时现场随机生成，互不相同、互不通用——一台泄露不影响其他台。多台服务器的连接码放进同一台节点机时建议按服务器改名（如 `pinode-145.antapp`），方便在菜单里区分。

### 客户端（节点机，管理员）

运行 `AntAppLink-Setup` 目录里的 `antapp-setup.exe` 安装（自动建快捷方式、注册 `.antapp` 关联、可勾选开机自启），然后**双击连接码文件**或在托盘菜单「导入连接码」粘贴单行码，连接即用。

详细的服务端运维、迁移与排障见 [deploy/README.md](deploy/README.md)。

## 命令行

服务端子命令：

```
antapp-linkd init      生成 CA 与默认配置
antapp-linkd invite    签发客户端连接码
antapp-linkd install   安装 systemd 与 DNS 中继
antapp-linkd up/down   配置 / 清理 netfilter
antapp-linkd run       前台运行隧道
antapp-linkd status    隧道与转发状态
```

客户端参数：

```
antapp-link.exe -c "antapp://…"   导入连接码并连接
antapp-link.exe -once             前台连接（不显示界面，Ctrl+C 退出）
antapp-link.exe -no-netcfg        联调模式：只建隧道，不动路由与 DNS
antapp-link.exe -version          显示版本
```

## 项目结构

```
cmd/antapp-linkd/        服务端入口（Linux，单二进制）
cmd/antapp-link/         客户端入口（Windows，单 exe + 托盘）
internal/proto/          帧编解码（两端共用）
internal/pki/            自签 CA 与连接码
internal/server/         TUN、隧道循环、netfilter、CLI、systemd 安装
internal/client/         Wintun 网卡、原生网络接管、出口迁移 watcher、界面与托盘
internal/setup/          安装与卸载（释放文件、快捷方式、注册表）
internal/update/         在线更新：检查、下载校验、替换自身
deploy/                  云服安装脚本、DNS 中继说明与排障
tools/                   netwatch 路由探针、资源生成、发版打包、更新链路自测
docs/superpowers/        设计文档与实现计划
third_party/             Wintun 出处与哈希（dll 已提取进 internal/client/assets）
```

## 开发

```powershell
go test ./...        # 全部测试，Windows 上可直接跑
go vet ./...
pwsh -File build.ps1 # 产出 dist/ 下的服务端、客户端与安装包
```

非 Windows / 非 Linux 平台都有桩实现，`go test ./...` 在任何开发机上全绿；平台相关代码由 `*_windows.go` / `*_linux.go` / `*_other.go` 三件套隔开。

## 许可

本仓库代码为私有项目。发行包内含 Wintun 的预编译二进制，其许可与义务见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。
