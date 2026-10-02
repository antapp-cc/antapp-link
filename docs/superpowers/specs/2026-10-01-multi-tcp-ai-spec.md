# antapp-link 多条 TCP 并发——AI 实现规格书（交接文档）

> **本文档写给 AI 编码助手。** 目标：你在不了解本项目历史的情况下，仅凭本文档
> 就能独立、正确地实现"多条 TCP 并发数据面"。动手前先完整读完本文档。
> 配套文档（同目录）：`2026-10-01-multi-tcp-concurrency.md` 是设计论证与验收细节，
> 有冲突时以本文档的契约为准。

---

## 0. 项目背景（事实速览，实现前先核对代码）

- 项目：Go 自研 L3 隧道（搬运 IP 包）。服务端 Linux（`internal/server`），
  客户端 Windows（`internal/client`），两端共用 `internal/proto`。零第三方运行时依赖。
- 当前传输：单条 TLS 1.3 over TCP（mTLS），内层帧协议见 `internal/proto/frame.go`。
- **实现前必须通读的文件**（先读再写，不要凭猜测）：
  - `internal/proto/frame.go` —— 帧格式：1 字节 type + 3 字节大端 length + 载荷；
    `MaxPayload=2048`；`WriteFrame` 内部是**两次 Write**（帧头一次、载荷一次）；
    未知帧类型必须容忍（前向兼容契约）。
  - `internal/client/tunnel.go` —— `session()` 每次调用 = 一次逻辑会话；
    `pumpFromDevice` / `pumpFromTunnel` / `heartbeat` 三个 goroutine；
    `Backoff()` 指数退避 1s→30s。
  - `internal/server/server.go` —— 单客户端模型：`attach()` 新连接踢旧连接；
    `pumpTun` 把 TUN 读到的包发给当前会话；`serveSession` 读循环。
  - `internal/pki/invite.go` —— 连接码 `Invite` 结构，已有 `Mode` 字段（tcp/udp）。
  - `internal/server/config.go` —— `Config` 与默认值 `Default()`。
  - `internal/server/udprelay.go` —— UDP 数据通道，**已写好但未接线，本任务不动它**。
- 平台桩体系：`*_windows.go` / `*_linux.go` / `*_other.go` 三件套。
  `go vet ./... && go test ./...` 必须在任何平台全绿（这是每一步的完成标准）。

## 1. 目标与硬性约束

**目标**：一个逻辑会话 = N 条并行 TLS 连接（N 可配 1–4，默认 4），内层 IP 帧按
流哈希分发到各连接。

**硬性约束（违反任何一条 = 实现错误）**：

1. N=1 时行为与现状**逐字节一致**（它是显式可选的单连接降级路径）。
2. 同一内层流（规范化五元组）的所有包**永远走同一条外层连接**——内层 TCP 绝不能
   看到乱序。这是方案的正确性基石。
3. 双向兼容：老客户端↔新服务端、新客户端↔老服务端都自动退化为单连接模式。
4. 不引入任何第三方依赖；不改帧头格式；新帧类型只能加在"未知类型被忽略"的兼容位上。
5. 心跳语义不变：PING/PONG 仅在控制连接；任何连接的任何帧都刷新 lastSeen。

## 2. 协议契约（精确，不得偏离）

### 2.1 HELLO 载荷（JSON）

现状：`{"version":1,"client":"<name>"}`。新增三个**可选**字段：

```json
{ "version": 1, "client": "pinode-01", "sid": "a3f9…32位hex", "members": 4, "member": 0 }
```

| 字段 | 含义 | 缺省（老客户端） |
|---|---|---|
| `sid` | 逻辑会话 ID：16 字节 crypto/rand → 32 hex 字符；客户端**每次 `session()` 重新生成** | `""` |
| `members` | 客户端请求的连接总数 | 0 → 按 1 处理 |
| `member` | 本连接槽位号：0=控制连接（主），>0=成员连接 | 0 |

### 2.2 HELLO_ACK 载荷

现有字段全部不变，新增一个可选字段：`"members": N`。
N = `min(客户端请求, 服务端 max_members)`。缺省或 0 → 客户端按单连接运行。
（老服务端不回这个字段 → 新客户端自动单连接，兼容成立。）

### 2.3 新帧类型

```go
TypeMemberAck Type = 0x08 // 服务端→客户端，成员连接握手确认；载荷空或 {"member":k}
```

### 2.4 服务端配置（internal/server/config.go）

`TunnelConfig` 新增：`MaxMembers int \`json:"max_members,omitempty"\``。
`Default()` 里设 4；`Validate()` 校验 1–4。
`invite` 不传 `--members` 时写入服务端上限（默认即 4），显式传 1 才签单连接。

### 2.5 连接码（internal/pki/invite.go）

`Invite` 新增 `Members int \`json:"members,omitempty"\``（0=单连接）；
服务端 `invite` 子命令加 `--members` 参数写入。

## 3. 流哈希算法（必须逐字实现，两端一致）

**新文件 `internal/proto/flowhash.go`**（两端共用，无依赖）：

```go
// Slot 返回该 IP 包应走的槽位号。畸形/非 IPv4 一律 0（控制连接）。
func Slot(payload []byte, n int) int
```

算法（FNV-1a 32 位：offset basis 2166136261，prime 16777619）：

1. 长度 < 20 或 `payload[0]>>4 != 4` → 返回 0。
2. 读 protocol（字节 9）、src IP（12..16）、dst IP（16..20）。
3. **分片特判**：字节 6..7 的 flags/offset 中 frag_off≠0 或 MF 置位 →
   哈希输入改为 `(srcIP, dstIP, protocol, ip_id)`（ip_id 在字节 4..6），
   保证同一 IP 报文的各分片同槽。
4. 否则若 protocol 是 TCP(6) 或 UDP(17) 且传输层头完整（≥4 字节）：
   端点对 = `[(srcIP, sport), (dstIP, dport)]`（sport/dport 为传输层头前 4 字节，
   各 2 字节）。**规范化**：两端点按字节序比较，小的在前——得到方向无关的
   定长输入：`srcIP(4) + sport(2) + dstIP(4) + dport(2)`，追加 protocol 字节。
5. 其他协议：输入 = srcIP + dstIP + protocol。
6. FNV-1a 遍历输入字节 → `uint32 % n`。

**必测用例**：src/dst 互换后同槽（方向对称性）；分片各片同槽；n=1 恒返 0；
畸形包不 panic 且返 0；分布均匀性（大样本卡方）。

## 4. 服务端改动（约 350–450 行）

### 4.1 会话模型（server.go）

- 现 `session` 升级为逻辑会话：新增 `sid string`、`maxN int`、
  `slots []*connEntry`（长度 = 生效 N）。`connEntry` 含 conn 与独立 writeMu。
- `attach` 拆两条路径：
  - **控制连接**（member==0）：sid ≠ current.sid → 踢旧接新（沿用现状逻辑与
    BYE 文案风格）；sid 相同 → 拒绝（同会话不允许第二条控制连接）。
  - **成员连接**（member>0）：要求 current 存在、证书 CN 相同、sid 相同、槽 k 空闲
    → 填槽，回 `TypeMemberAck`；任一不满足 → 回 `TypeBye` 带原因，**绝不影响现有会话**。
- `pumpTun` 改走 `sess.writePkt(pkt)`：`proto.Slot(pkt, N)` 选槽 → 该槽连接写出；
  槽位死亡 → 落到控制连接（槽 0）。配合 §6 写合并批量写出。
- 每条连接各自的读循环把 IP 帧写 TUN：`/dev/net/tun` 的 `write()` 按包原子、
  可并发；每流单写者保序。可加一把 mutex 保险，非必须。
- **熔断**：单会话 JOIN 失败计数超阈值（如 60 秒内 20 次）→ 拒绝该 sid 的后续
  JOIN 并打告警日志（防客户端 bug 造成拨接风暴）。

### 4.2 心跳与状态

- PING/PONG 仅控制连接收发（现状不动）。
- `status.go` 增加：members、每槽收发字节、JOIN 熔断计数。
- `cli.go`：`invite` 加 `--members`；`status` 展示成员数。

### 4.3 sysctl（deploy/install.sh）

在现有 BBR 段**之后**追加，完全复用 BBR 段的风格（`sysctl -w` 逐项直写 →
回读校验 → 不一致 die；记录原值备份供回滚）：

```
net.ipv4.tcp_slow_start_after_idle=0
net.ipv4.tcp_rmem      第三值 → 16777216
net.ipv4.tcp_wmem      第三值 → 16777216
net.core.rmem_max=16777216
net.core.wmem_max=16777216
net.ipv4.tcp_mtu_probing=1
net.ipv4.tcp_fastopen=3   # Go 标准库不发起 TFO，仅占位；不写任何验收项
```

## 5. 客户端改动（约 250–350 行）

### 5.1 会话（tunnel.go）

- `session()` 开始时生成 `sid`（crypto/rand 16 字节 hex）。
- 控制连接握手成功且 `ACK.members > 1` → 启动**成员维护 goroutine**：
  - 并发拨 `member = 1..N-1`，每条：TLS 握手 → HELLO`{sid, members, member:k}`
    → 等 `TypeMemberAck` → 进该连接读循环。
  - 失败对同一 k 指数退避重试（复用 `Backoff()`，1s 起步 30s 封顶）。
    **JOIN 失败绝不结束会话**。
- 成员连接死亡（读循环出错）：从槽表摘除（该槽流量自动落控制连接）→ 触发该槽补拨。
- 控制连接死亡：整会话结束，走现有重连逻辑（保持现状）。

### 5.2 发送路径

`pumpFromDevice`：`proto.Slot(buf[:n], N)` 选槽 → 写该槽连接（持该槽 writeMu）。

### 5.3 TLS（internal/pki 的 ClientTLSConfig）

加一行 `ClientSessionCache: tls.NewLRUClientSessionCache(8)`。
**禁止开启 EarlyData**（Go 无客户端 0-RTT 支持且有重放风险）。

## 6. 写合并（独立小任务，可最先做）

**现状缺陷**：`proto.WriteFrame` 是两次 Write（帧头、载荷）→ Go `tls.Conn` 每次
Write 独立成一个 TLS 记录 → **每个内层 IP 包以外层 2 个 TCP 段离开**，外层包率
≈ 内层 2 倍。

**改法**：

- `proto` 新增 `func AppendHeader(dst []byte, t Type, n int) []byte`（向 dst 追加
  4 字节帧头）。**不得改动现有 `WriteFrame` 的签名与行为**（其他调用方在用）。
- 客户端 `pumpFromDevice` / 服务端 `pumpTun`：读到一包后，**非阻塞地**继续收割
  内核缓冲里现成的包（读不到就停，**不等待**），拼
  `[hdr|payload][hdr|payload]…` 单缓冲一次 Write；批总量 ≤ 16384（单条 TLS 记录，
  约 11 帧 @ MTU 1400）。
- 接收端**零改动**（TCP 字节流，`ReadFrame` 本就支持帧背靠背）。
- 打字/ping 这类单包场景行为不变（无现成包就单包单写）。

## 7. 实施顺序（每步独立可提交，完成标准 = vet+test 全绿）

| 步骤 | 内容 | 自验 |
|---|---|---|
| M1 | `flowhash.go` + `AppendHeader` + `TypeMemberAck` + 全部单测 | 纯函数，任何平台可测 |
| M2 | 服务端槽表/join/kick + config/status/cli + install.sh sysctl | 服务端单测；老客户端行为回归 |
| M3 | 客户端多拨/槽表/维护 goroutine + ClientSessionCache | 本地桩联调 |
| M4 | 写合并接入两端 pump + 端到端验收 | §8 全过 |

## 8. 验收清单

1. 限速线路实测：members=4 聚合吞吐 ≥ 单连接基线 × 2
2. 掐断一条成员连接（iptables DROP 单条）：会话不掉、ping 掉包 ≤ 5 个、该槽 ≤ 30s 补齐
3. 掐断控制连接：按现状退避重连，连上后自动恢复 N 条
4. **内层有序性**：隧道内 TCP 下载，云服 `tcpdump -i antapp0` 抓包，脚本断言每条
   内层流 seq 严格单调——最重要的一条
5. 兼容性：老客户端二进制连新服务端回归全过；新客户端连老服务端单连接正常
6. 资源：members=4 稳态，服务端增量 < 10 MiB 内存、4 fd
7. 反向验证：按用户总量限速的线路上聚合吞吐 ≈ 单连接 → 如实记录，该线路 members 回退 1
8. sysctl：重跑 install.sh 回读全对；空闲 5 分钟后突发，首 2 秒速率对照记录
9. 写合并：外层 pps : 内层 pps 从 ~2:1 降到 ~1.1:1
10. 会话恢复：重连日志出现 `DidResume=true`（只验收路径被走到，不承诺 RTT 改善）

## 9. 禁止事项（历史决策，不要重新发明）

1. **禁止逐包轮询分发**——会打散内层流包序，已论证否决；只用流哈希。
2. **禁止对存活连接数取模**——任一成员抖动会引发全体流洗牌；必须对配置 N 取模
   + 槽表 + 死槽落控制连接。
3. **禁止让 JOIN 失败结束会话**；禁止成员连接参与 PING/PONG。
4. **禁止改 `WriteFrame` 签名/行为**；写合并只用新函数。
5. **禁止**：TLS EarlyData、QUIC、MPTCP（Windows 客户端不支持）、任何新第三方依赖。
6. **禁止在 N=1 路径引入任何额外开销或行为差异**（单连接是降级路径，必须与多连接等价）。
7. **不动 `internal/server/udprelay.go`**（UDP 通道未接线，是另一个任务；
   但槽位抽象要为它留路：逻辑会话 = 多条数据通道 + 哈希路由，UDP 将来是其中一个通道）。
8. 每步完成必须 `go vet ./... && go test ./...` 全绿（含跨平台桩编译）。

## 10. 预期工作量

约 1500 行（含测试），单人 4–6 个工作日。M1 是纯函数可当天完成；
正确性风险集中在流哈希实现与槽表并发，单测务必先写。
