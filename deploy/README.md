# 云服部署

## 一次性安装

```bash
# 1. 本机交叉编译并上传（两个文件放同一目录）
pwsh -File build.ps1
#   dist/antapp-linkd        -> 云服
#   deploy/install.sh        -> 云服

# 2. 云服上执行
bash install.sh                    # 默认转发端口段 31410-31419
```

脚本会：装二进制到 `/usr/local/bin/antapp-linkd` → 生成 CA 与服务端证书 → 写 systemd unit 并 enable → 配置 netfilter → 启动隧道。

**幂等**。重复跑只刷新二进制和配置，**不会重建 CA**（重建会让已发出的所有连接码一起失效）。CA 材料不完整时它会明确报错并拒绝继续，而不是悄悄重建。

## 给节点机签发连接码

```bash
/usr/local/bin/antapp-linkd invite pi-node-01 -o /root
```

会输出：

- `/root/antapp-node-pi-node-01.json`（完整连接码文件）
- 一行 `antapp://...`（发给节点机导入，内含私钥，**等同密码**）

节点机那边：复制整行，托盘菜单里选「从剪贴板导入连接码」，或命令行 `antapp-link.exe -c "antapp://..."`。

`--server` 可以指定服务端地址；不指定时脚本会自己探测公网 IP（探测不到就直接报错，绝不生成一个内网地址的连接码）。

## 日常运维

```bash
antapp-linkd status -c /etc/antapp-link/server.json    # 隧道状态、已接入节点、DNAT 规则
journalctl -u antapp-linkd -f                          # 隧道日志
systemctl status antapp-linkd antapp-link-up
antapp-linkd up -c /etc/antapp-link/server.json        # 重配 netfilter（幂等）
antapp-linkd down -c /etc/antapp-link/server.json      # 清理 netfilter
```

## 和现网 OpenVPN + rinetd 并行 / 切换

两套东西**不能共用同一段转发端口**：DNAT 在 `PREROUTING` 里跑，会抢在本地监听之前把包改走，占用方（rinetd）会**静默失效** —— 老节点看起来「突然连不上」却没有任何报错。所以：

| | 现网（OpenVPN + rinetd） | 本方案 |
|---|---|---|
| 隧道端口 | tcp/62231 | tcp/62233 |
| 隧道网段 | `10.9.0.0/24` | `10.10.0.0/24` |
| 转发端口 | 31400-31409 | **先用 31410-31419 验证** |

切换步骤：

```bash
# 1. 验证期：新方案跑在 31410-31419，与老服务互不干扰
bash install.sh

# 2. 验收全过之后，停掉老服务
systemctl disable --now openvpn-server@antnest-tcp
pkill -x rinetd && systemctl disable --now rinetd antnest-rinetd-watch 2>/dev/null

# 3. 把新方案的端口段切回正式的 31400-31409
bash install.sh --forward 31400-31409
```

`install.sh` 换端口段时会**先 `down` 再改配置** —— 反过来的话 `down` 会按新端口段去删规则，旧规则就留在链里了。

## 防火墙

`up` 会自动插 `INPUT` 放行隧道端口。转发端口（31400-31409）**不需要**在 `INPUT` 放行：DNAT 在 `PREROUTING` 完成，包走的是 `FORWARD`。

如果你的云服还有别的防火墙前端（安全组 / ufw），记得放行隧道端口。

## 排障

| 现象 | 先看这里 |
|---|---|
| 节点连不上 | `antapp-linkd status` 看「已接入节点」；`journalctl -u antapp-linkd` 找 TLS 握手失败（多半是客户端证书不是本 CA 签的） |
| 端口从外面连不上 | `antapp-linkd status` 里的 DNAT 规则；再确认节点确实在线 —— 客户端没连上时 DNAT 目标不可达，外部表现为超时 |
| 节点显示已连接但上不了网 | 客户端日志里的「出网自检」结论。自检失败时客户端会自动断开并还原网络，日志里会有原因（多半是云服出口的 DNS 查不通） |
| 改了端口段没生效 | `antapp-linkd down` 再 `up`，或直接重跑 `install.sh --forward <段>` |
