# third_party

客户端要用 Wintun 做虚拟网卡。`internal/client/assets/wintun/` 下那几个 dll 就是从
这里的 zip 里取的，**内容未做任何修改**（Wintun 的许可第 3.a/3.c 条要求不得改动、
不得移除版权声明，而第 3.d 条允许随使用它的软件一起分发）。

## 现在的文件

| 文件 | 说明 |
|---|---|
| `wintun-0.14.1.zip` | wintun.net/builds 的原始发布包，750,540 字节 |
| `wintun-0.14.1/` | 解压后的原始内容（含 LICENSE.txt、README.md、wintun.h） |
| `wintun.sha256` | 提取出来的 dll 的 sha256，用于合规核对 |

zip 的 sha256：

```
07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51  wintun-0.14.1.zip
```

## 怎么重新获取

wintun.net 在国内直连不通（TLS 握手被阻断），本机也没有可用代理。
这次是从一台能出境的服务器上下载后传回来的：

```bash
# 在能出境的机器上
curl -fsSL -o /tmp/wintun.zip https://www.wintun.net/builds/wintun-0.14.1.zip
sha256sum /tmp/wintun.zip     # 应为 07c25618...
```

```powershell
# 拉回本地（ssh 工具的别名按 ~/.ssh/config 来）
# action=download host=<别名> remotePath=/tmp/wintun.zip localPath=...\third_party\wintun-0.14.1.zip

# 解压并把 dll 放进 assets
Expand-Archive .\wintun-0.14.1.zip -DestinationPath .\wintun-0.14.1 -Force
Get-ChildItem .\wintun-0.14.1 -Recurse -Filter wintun.dll | ForEach-Object {
    Copy-Item $_.FullName "..\internal\client\assets\wintun\wintun_$($_.Directory.Name).dll" -Force
}
```

## 为什么要提交进仓库

不是"顺手放个二进制"。`go:embed` 要求文件在编译时就存在，而这些 dll 又拿不到
（国内下不动、CI 上也未必有境外出口）。把它们留在仓库里，任何一台机器 clone 下来
都能直接 `build.ps1`。
