# 第三方组件声明

本项目的发行包（`antapp-link.exe` / `antapp-linkd`）包含或依赖以下第三方组件。
按各自的许可要求在此声明。

---

## 1. Wintun（Windows 虚拟网卡驱动）

- 来源：<https://www.wintun.net/> 发布页下载的预编译 `wintun.dll`（版本 0.14.1）
- 用途：Windows 客户端的虚拟网卡。本项目**仅通过 `wintun.h` 暴露的 API** 使用它
  （经 Go 绑定 `golang.zx2c4.com/wintun`）
- 著作权：Copyright © WireGuard LLC. All Rights Reserved.

### 为什么适用下面这份许可而不是 GPLv2

Wintun 仓库的 README 写得很明确：**源码**是 GPLv2，而**从 wintun.net 下载的预编译二进制**
"are released under a more permissive license suitable for more forms of software"。
该许可第 3.d 条允许「随其他软件一起分发」，前提是只通过 Permitted API 使用它 ——
本项目的用法正落在这一条内，因此可以随本软件一起商业分发。

对应的两条义务，本项目已经遵守：

1. **不修改、不改名**：发行包里放的是 `wintun.dll` 原始文件，逐字节未动
   （sha256 见 `third_party/wintun.sha256`），文件名也保持原样。
2. **附上许可原文**：即下面的全文。

### Prebuilt Binaries License 全文

```
Prebuilt Binaries License
-------------------------

1. DEFINITIONS. "Software" means the precise contents of the "wintun.dll"
   files that are included in the .zip file that contains this document as
   downloaded from wintun.net/builds.

2. LICENSE GRANT. WireGuard LLC grants to you a non-exclusive and
   non-transferable right to use Software for lawful purposes under certain
   obligations and limited rights as set forth in this agreement.

3. RESTRICTIONS. Software is owned and copyrighted by WireGuard LLC. It is
   licensed, not sold. Title to Software and all associated intellectual
   property rights are retained by WireGuard. You must not:
   a. reverse engineer, decompile, disassemble, extract from, or otherwise
      modify the Software;
   b. modify or create derivative work based upon Software in whole or in
      parts, except insofar as only the API interfaces of the "wintun.h" file
      distributed alongside the Software (the "Permitted API") are used;
   c. remove any proprietary notices, labels, or copyrights from the Software;
   d. resell, redistribute, lease, rent, transfer, sublicense, or otherwise
      transfer rights of the Software without the prior written consent of
      WireGuard LLC, except insofar as the Software is distributed alongside
      other software that uses the Software only via the Permitted API;
   e. use the name of WireGuard LLC, the WireGuard project, the Wintun
      project, or the names of its contributors to endorse or promote products
      derived from the Software without specific prior written consent.

4. LIMITED WARRANTY. THE SOFTWARE IS PROVIDED "AS IS" AND WITHOUT WARRANTY OF
   ANY KIND. WIREGUARD LLC HEREBY EXCLUDES AND DISCLAIMS ALL IMPLIED OR
   STATUTORY WARRANTIES, INCLUDING ANY WARRANTIES OF MERCHANTABILITY, FITNESS
   FOR A PARTICULAR PURPOSE, QUALITY, NON-INFRINGEMENT, TITLE, RESULTS,
   EFFORTS, OR QUIET ENJOYMENT. THERE IS NO WARRANTY THAT THE PRODUCT WILL BE
   ERROR-FREE OR WILL FUNCTION WITHOUT INTERRUPTION. YOU ASSUME THE ENTIRE
   RISK FOR THE RESULTS OBTAINED USING THE PRODUCT. TO THE EXTENT THAT
   WIREGUARD LLC MAY NOT DISCLAIM ANY WARRANTY AS A MATTER OF APPLICABLE LAW,
   THE SCOPE AND DURATION OF SUCH WARRANTY WILL BE THE MINIMUM PERMITTED UNDER
   SUCH LAW. ALL EXPRESS OR IMPLIED CONDITIONS, REPRESENTATIONS AND
   WARRANTIES, INCLUDING ANY IMPLIED WARRANTY OF MERCHANTABILITY, FITNESS FOR
   A PARTICULAR PURPOSE OR NON-INFRINGEMENT ARE DISCLAIMED, EXCEPT TO THE
   EXTENT THAT THESE DISCLAIMERS ARE HELD TO BE LEGALLY INVALID.

5. LIMITATION OF LIABILITY. To the extent not prohibited by law, in no event
   WireGuard LLC or any third-party-developer will be liable for any lost
   revenue, profit or data or for special, indirect, consequential, incidental
   or punitive damages, however caused regardless of the theory of liability,
   arising out of or related to the use of or inability to use Software, even
   if WireGuard LLC has been advised of the possibility of such damages.
   Solely you are responsible for determining the appropriateness of using
   Software and accept full responsibility for all risks associated with its
   exercise of rights under this agreement, including but not limited to the
   risks and costs of program errors, compliance with applicable laws, damage
   to or loss of data, programs or equipment, and unavailability or
   interruption of operations. The foregoing limitations will apply even if
   the above stated warranty fails of its essential purpose. You acknowledge,
   that it is in the nature of software that software is complex and not
   completely free of errors. In no event shall WireGuard LLC or any
   third-party-developer be liable to you under any theory for any damages
   suffered by you or any user of Software or for any special, incidental,
   indirect, consequential or similar damages (including without limitation
   damages for loss of business profits, business interruption, loss of
   business information or any other pecuniary loss) arising out of the use or
   inability to use Software, even if WireGuard LLC has been advised of the
   possibility of such damages and regardless of the legal or quitable theory
   (contract, tort, or otherwise) upon which the claim is based.

6. TERMINATION. This agreement is affected until terminated. You may
   terminate this agreement at any time. This agreement will terminate
   immediately without notice from WireGuard LLC if you fail to comply with
   the terms and conditions of this agreement. Upon termination, you must
   delete Software and all copies of Software and cease all forms of
   distribution of Software.

7. SEVERABILITY. If any provision of this agreement is held to be
   unenforceable, this agreement will remain in effect with the provision
   omitted, unless omission would frustrate the intent of the parties, in
   which case this agreement will immediately terminate.

8. RESERVATION OF RIGHTS. All rights not expressly granted in this agreement
   are reserved by WireGuard LLC. For example, WireGuard LLC reserves the
   right at any time to cease development of Software, to alter distribution
   details, features, specifications, capabilities, functions, licensing
   terms, release dates, APIs, ABI, general availability, or other
   characteristics of the Software.
```

---

## 2. Go 依赖

以下组件以源码形式静态链接进二进制，均在各自许可下允许商业分发。
完整许可文本随源码仓库与 Go module 缓存分发。

| 组件 | 版本 | 许可 | 项目地址 |
|---|---|---|---|
| `fyne.io/systray` | v1.12.2 | Apache-2.0 | <https://github.com/fyne-io/systray> |
| `github.com/godbus/dbus/v5` | v5.1.0 | BSD-2-Clause | <https://github.com/godbus/dbus> |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | <https://cs.opensource.google/go/x/sys> |
| `golang.zx2c4.com/wintun` | v0.0.0-20230126152724 | MIT | <https://git.zx2c4.com/wintun/> |

`golang.zx2c4.com/wintun` 只是 `wintun.dll` 的 Go 语言绑定（动态加载，不含驱动本身），
驱动本体见上面第 1 节。

Go 标准库的许可见 <https://go.dev/copyright>。

---

## 3. 声明边界

本项目的服务端（`antapp-linkd`）不包含任何第三方二进制，只用 Go 标准库与 `golang.org/x/sys`。
上面第 1 节的 Wintun 只出现在 Windows 客户端里。
