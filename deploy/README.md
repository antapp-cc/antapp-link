# 云服部署

## 一次性安装

**方式一（推荐）：云服上一条命令，脚本自动从 GitHub Release 下载二进制**

```bash
wget -qO install.sh "https://raw.githubusercontent.com/antapp-cc/antapp-link/main/deploy/install.sh" && bash install.sh
```

**方式二：离线部署**（云服访问 GitHub 不便时）

```bash
# 1. 本机交叉编译并上传（两个文件放同一目录）
pwsh -File build.ps1
#   dist/antapp-linkd        -> 云服
#   deploy/install.sh        -> 云服

# 2. 云服上执行
bash install.sh                    # 默认转发端口段 31400-31409
```

脚本会：装二进制到 `/usr/local/bin/antapp-linkd` → 生成 CA 与服务端证书 → 写 systemd unit 并 enable → 安装并配置 dnsmasq（隧道 DNS 中继）→ 配置 netfilter → 启动隧道。

**每台 VPS 的 PKI 密钥在首次安装时现场随机生成**，互不相同、互不通用——一台泄露不影响其他台。

**幂等**。重复跑只刷新二进制和配置，**不会重建 CA**（重建会让已发出的所有连接码一起失效）。CA 材料不完整时它会明确报错并拒绝继续，而不是悄悄重建。

## 给节点机签发连接码

```bash
/usr/local/bin/antapp-linkd invite pi-node-01 -o /root
```

会输出：

- `/root/pinode.antapp`（完整连接码文件，双击即可导入客户端；放进节点机 config\ 时建议按服务器改名，如 `pinode-82.antapp`）
- 一行 `antapp://...`（发给节点机导入，内含私钥，**等同密码**）

节点机那边：把 `.antapp` 文件拷过去双击，或者复制整行后在托盘菜单里选「导入连接码」，也可以命令行 `antapp-link.exe -c "antapp://..."`。

`--server` 可以指定服务端地址；不指定时脚本会自己探测公网 IP（探测不到就直接报错，绝不生成一个内网地址的连接码）。

## 日常运维

```bash
antapp-linkd status -c /etc/antapp-link/server.json    # 隧道状态、已接入节点、端口转发
journalctl -u antapp-linkd -f                          # 隧道日志
systemctl status antapp-linkd antapp-link-up
antapp-linkd up -c /etc/antapp-link/server.json        # 重配 netfilter（幂等）
antapp-linkd down -c /etc/antapp-link/server.json      # 清理 netfilter
```

## 服务端自动更新

0.2.9 起服务端会自己升级。`antapp-linkd install` 会装一个 `antapp-link-update.timer`，每小时跑一次 `antapp-linkd update`：读 GitHub Release 的 `latest.json`，发现版本比当前新就下载 `server` 段指向的 `antapp-linkd`，校验 sha256 → 备份 → 原子替换 → `systemctl restart` → 确认新版本真的起来了才算成功。

**起不来会自动回滚**到 `$BIN.bak`，并把这一版记进 `/run/antapp-link/update-failed`，之后不再反复重试（否则会变成每小时重启一次服务、把所有节点反复打断）。回滚的判据是 systemd 的 `NRestarts` —— 光看进程 active 或状态文件时间戳都会被"启动→写状态→崩溃→重启→再写"这种循环骗过去。

**升级只替换二进制这一个文件**：不调用 `init`、不碰 `/etc/antapp-link/pki/`、不改 `server.json`、不动已签发的连接码。CA 不变，所以老连接码继续有效。

### 清单签名（发布时必须做）

`latest.json` 必须带签名一起发布，否则**新版本会拒绝更新、自动更新直接停摆**。

原因：`latest.json` 里的下载 URL 和 sha256 出自同一份文件，只校验 sha256 挡不住能改 Release 的人 —— 他可以把两者一起换掉。而更新链路以 **root** 运行，一次投毒就是所有节点上的任意代码执行。

```powershell
# 只在第一次做：生成密钥对，私钥离线保管（默认 ~/.antapp-release/release.key）
go run ./cmd/antapp-sign -genkey -key $env:USERPROFILE\.antapp-release\release.key
# 把打印出来的公钥填进 internal/update/pubkey.go 的 releasePubKey，然后重新构建两端

# 每次发布：给 dist/latest.json 生成 dist/latest.json.sig（build.ps1 已自动做这一步）
go run ./cmd/antapp-sign -key $env:USERPROFILE\.antapp-release\release.key -in dist/latest.json
```

发布资产要包含 **`latest.json` 和 `latest.json.sig` 两个文件**（连同 `antapp-link.exe`、`antapp-linkd`）。

> ⚠ **别弄丢私钥，也别轻易换密钥对。** 私钥丢了就再也签不出节点会接受的清单（只能全部重装）；
> 换密钥对意味着所有已部署节点要先升级一次 —— 它们内置的是旧公钥。

> ⚠ **引导步骤：已经装好的机器必须再跑一次安装，自动更新才会启动。**
> timer 是 `install` 子命令写进去的，老二进制里既没有 `update` 子命令也没有 timer：
>
> ```bash
> bash install.sh --yes
> # 或等价地：
> /usr/local/bin/antapp-linkd install -c /etc/antapp-link/server.json
> ```
>
> 不跑这一步的话，二进制换新了但永远不会有定时任务去升级它。

手动操作：

```bash
antapp-linkd update -c /etc/antapp-link/server.json --check   # 只查，不下载不重启
antapp-linkd update -c /etc/antapp-link/server.json           # 真更新（timer 干的就是这个）
systemctl list-timers antapp-link-update.timer                # 下次什么时候跑
journalctl -u antapp-link-update -n 50                        # 上一次更新做了什么
cat /run/antapp-link/update-failed                            # 被锁住的失败版本（删掉即可重试）
ls -la /usr/local/bin/antapp-linkd.bak                        # 回滚副本；手动回滚：install -m755 它 /usr/local/bin/antapp-linkd
```

**两种版本不要走自动更新**（自动更新不会破坏现场，但也解决不了）：

- **改了 `protoVersion` 的版本** —— 必须等客户端先更新到位，否则老客户端会因版本不符连不上新服务端；
- **需要改 PKI 或配置格式的版本** —— 得人工跑 `install.sh`。

## 和现网 OpenVPN + rinetd 并行 / 切换

两套东西**不能共用同一段转发端口**：DNAT 在 `PREROUTING` 里跑，会抢在本地监听之前把包改走，占用方（rinetd）会**静默失效** —— 老节点看起来「突然连不上」却没有任何报错。

| | 现网（OpenVPN + rinetd） | 本方案 |
|---|---|---|
| 隧道端口 | tcp/62231 | tcp/62233 |
| 隧道网段 | `10.9.0.0/24` | `10.10.0.0/24` |
| 转发端口 | 31400-31409 | **31400-31409（同一段）** |

**装到全新 VPS 上时直接用默认值就行**，`31400-31409` 就是正式端口段。

只有一种情况需要换段：跟这台机器上已有的服务撞了（比如老 rinetd 还在跑）。那时：

```bash
# 1. 挑一段没被占用的，先跑起来
bash install.sh --forward 31410-31419

# 2. 验收全过之后停掉占用方
systemctl disable --now openvpn-server@antnest-tcp
pkill -x rinetd && systemctl disable --now rinetd antnest-rinetd-watch 2>/dev/null

# 3. 切回正式端口段
bash install.sh --forward 31400-31409
```

`install.sh` 换端口段时会**先 `down` 再改配置** —— 反过来的话 `down` 会按新端口段去删规则，旧规则就留在链里了。

## 防火墙

`up` 会自动插 `INPUT` 放行隧道端口和转发端口段（31400-31409）。转发采用**服务端应答式转发器**：antapp-linkd 在本机监听这些端口，收到连接立即应答、再经隧道转给节点机同端口（替代内核 DNAT——DNAT 的握手要走完到节点机的全程，外部检查的延迟能翻倍）。转发只做 **TCP**，没有 UDP 规则；节点机看到的连接来源是服务端隧道地址（10.10.0.1）。

如果你的云服还有别的防火墙前端（安全组 / ufw），记得放行隧道端口。

## 隧道 DNS 中继（dnsmasq + filter-AAAA）

`install.sh` 会安装 dnsmasq，`antapp-linkd install` 写入 `/etc/dnsmasq.d/antapp.conf`：dnsmasq 监听隧道网关（默认 `10.10.0.1`），上游 8.8.8.8 / 1.1.1.1，并开启 **`filter-AAAA`**。签发的连接码里 DNS 就是网关地址——隧道客户端的全部解析都走这条中继。

**为什么必须过滤 AAAA**：隧道只接管 IPv4。把 AAAA（v6 地址）发给客户端，浏览器内核会拿它直连（完全绕开隧道），而 v6 直连多数情况是死路——实测会造成 Chromium 内核应用（如 Pi Desktop）整批内嵌页面白屏。

排障：`dig @10.10.0.1 <域名> A`（应有答案）；`dig @10.10.0.1 <域名> AAAA`（应为空）；`systemctl status dnsmasq`。dnsmasq 用 `bind-dynamic` 绑定，开机时 antapp0 还没建起来也不会启动失败。

> 坑：Debian/Ubuntu 上 `apt-get install dnsmasq` 会**立即自启**——那时 antapp.conf 还没写入，服务就带着空配置跑起来了。`install.sh` 已处理（写入配置后强制 `restart`），手动安装时也要注意这个顺序。

## 排障

| 现象 | 先看这里 |
|---|---|
| 节点连不上 | `antapp-linkd status` 看「已接入节点」；`journalctl -u antapp-linkd` 找 TLS 握手失败（多半是客户端证书不是本 CA 签的） |
| 端口从外面连不上 | `ss -tlnp | grep antapp-linkd` 确认转发器在监听 31400-31409；再确认节点确实在线 —— 客户端没连上时隧道转发不可达，外部表现为超时 |
| 节点显示已连接但上不了网 | 客户端日志里的「出网自检」结论。自检失败时客户端会自动断开并还原网络，日志里会有原因（多半是云服出口的 DNS 查不通） |
| 改了端口段没生效 | `antapp-linkd down` 再 `up`，或直接重跑 `install.sh --forward <段>` |
