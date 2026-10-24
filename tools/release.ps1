# 打一个可发布的版本，并生成 GitHub Release 需要的文件。
#
#   pwsh -File tools/release.ps1 -Version 0.2.0
#
# 产出在 dist/release-<版本>/：
#   antapp-link.exe        客户端更新包
#   antapp-linkd           服务端更新包（Linux amd64；服务端按 latest.json 自动升级）
#   latest.json            更新清单（客户端和服务端都按它判断版本、校验 sha256）
#   README.md
#   上传指引.txt
#
# 然后到 GitHub 建一个 tag 为 v<版本> 的 Release，把前四个文件传上去。
#
# ⚠ 资产名必须固定为 antapp-link.exe / antapp-linkd / latest.json —— 两端访问的都是
#   releases/latest/download/latest.json 这种固定链接，靠的就是文件名不变。

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$Repo = 'antapp-cc/antapp-link'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

# 版本号格式：和 internal/update 的 versionPattern 同一规则。格式错了签名发布
# 照样能过，但所有节点会把它比较成「没有更新」——必须在构建前拦下来。
if ($Version -notmatch '^\d+\.\d+\.\d+([-+].*)?$') {
    throw "版本号格式不对：$Version（需要 主.次.修订，例如 0.4.12）"
}

Write-Host "== 构建 $Version ==" -ForegroundColor Cyan
& (Join-Path $root 'build.ps1') -Version $Version
if ($LASTEXITCODE -ne 0) { throw '构建失败' }

$exe = Join-Path $root 'dist\antapp-link.exe'
if (-not (Test-Path $exe)) { throw "找不到 $exe" }
$linkd = Join-Path $root 'dist\antapp-linkd'
if (-not (Test-Path $linkd)) { throw "找不到 $linkd" }

$hash = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
$size = (Get-Item $exe).Length
$linkdHash = (Get-FileHash $linkd -Algorithm SHA256).Hash.ToLower()
$linkdSize = (Get-Item $linkd).Length

$out = Join-Path $root "dist\release-$Version"
if (Test-Path $out) { Remove-Item $out -Recurse -Force }
New-Item -ItemType Directory -Force $out | Out-Null

Copy-Item $exe (Join-Path $out 'antapp-link.exe') -Force
Copy-Item $linkd (Join-Path $out 'antapp-linkd') -Force
Copy-Item (Join-Path $root 'README.md') $out -Force

# 更新清单。字段名要和 internal/update 里的结构体对齐。
# server 段是服务端自动更新（antapp-linkd update）读的：没有它服务端会跳过这个版本。
$tag = "v$Version"
$manifest = [ordered]@{
    version      = $Version
    notes        = "AntApp Link $Version"
    published_at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    client       = [ordered]@{
        url    = "https://github.com/$Repo/releases/download/$tag/antapp-link.exe"
        sha256 = $hash
        size   = $size
    }
    server       = [ordered]@{
        url    = "https://github.com/$Repo/releases/download/$tag/antapp-linkd"
        sha256 = $linkdHash
        size   = $linkdSize
    }
}

# 写无 BOM 的 UTF-8：带 BOM 的话有些 JSON 解析器会直接报错
$json = $manifest | ConvertTo-Json -Depth 6
$manifestPath = Join-Path $out 'latest.json'
[System.IO.File]::WriteAllText($manifestPath, $json + "`n",
    (New-Object System.Text.UTF8Encoding($false)))

# 签这份**最终**清单。
#
# 必须在这里签，不能指望 build.ps1 那一步：它签的是 dist/latest.json，而发布产物
# 在 dist/release-<版本>/ 下，是另一个路径、也是后来才生成的。漏了这一步的后果不是
# 「签名缺失」这么好听 —— 更新器要求清单旁必须有 latest.json.sig，缺了直接拒绝更新，
# 发出去的版本所有已部署节点都收不到，只能手动重装。
$releaseKey = Join-Path $env:USERPROFILE '.antapp-release\release.key'
if (-not (Test-Path $releaseKey)) {
    throw "缺发布私钥 $releaseKey —— 没有它签不出节点会接受的清单。"
}
Write-Host '== 更新清单签名 ==' -ForegroundColor Cyan
& go run ./cmd/antapp-sign -key $releaseKey -in $manifestPath
if ($LASTEXITCODE -ne 0) { throw '清单签名失败' }
if (-not (Test-Path "$manifestPath.sig")) { throw "签名没有生成：$manifestPath.sig 不存在" }

# 用编译期内置的公钥验一遍 —— 就是节点端会做的那件事。签错了本地就发现，不用等发布。
Write-Host '== 更新清单验签（模拟节点端）==' -ForegroundColor Cyan
& go run ./cmd/antapp-sign -verify -in $manifestPath
if ($LASTEXITCODE -ne 0) { throw '清单验签失败 —— 发布后节点会拒绝更新' }

$guide = @"
发布 $Version
============================================================

产物目录：$out

  1. 到 https://github.com/$Repo/releases/new 新建 Release
  2. Tag 填：$tag      （必须和 latest.json 里的 url 一致）
  3. 标题必须写成：AntApp Link $Version
     Release 列表直接显示这个标题，历史版本全是 "AntApp Link X.Y.Z" 的格式；
     只写版本号会在列表里显得和别的格格不入（0.4.9 / 0.4.10 就这样翻过一次车）。
     gh CLI 用法：gh release create $tag --title "AntApp Link $Version" ...
  4. 把下面五个文件作为附件传上去（文件名不要改）：
       antapp-link.exe
       antapp-linkd
       latest.json
       latest.json.sig        ← 少传这个，两端都会拒绝自动更新
       README.md
  5. 发布。客户端和服务端下次检查更新时会读到 latest.json，比对版本后各自下载。

校验值
  antapp-link.exe  sha256 = $hash
  大小                     = $size 字节
  antapp-linkd     sha256 = $linkdHash
  大小                     = $linkdSize 字节

注意
  * 一定要勾选 "Set as the latest release"，否则 releases/latest 还是指向旧版。
  * 想撤回某个版本就把它标记成 pre-release，两端都不会取到。
  * 改了协议版本号（internal/proto 的 protoVersion）时，必须等客户端先更新到位再
    更新服务端，否则老客户端会因为版本不符连不上新服务端。
  * 只上传本目录（$out）里的文件。dist 下还有别的同名文件（联调用的假程序），
    拿错了会让客户端「发现新版本但下载失败」—— 因为清单里的地址指向本机端口。
"@
[System.IO.File]::WriteAllText((Join-Path $out '上传指引.txt'), $guide,
    (New-Object System.Text.UTF8Encoding($false)))

Write-Host ''
Write-Host '== 发布产物 ==' -ForegroundColor Green
Get-ChildItem $out | Sort-Object Name | ForEach-Object {
    '  {0,-26} {1,12:N0} 字节' -f $_.Name, $_.Length
}
Write-Host ''
Write-Host "  client.sha256 = $hash"
Write-Host "  server.sha256 = $linkdHash"
Write-Host ''
Write-Host "下一步：按 $out\上传指引.txt 传到 GitHub Release（tag $tag）。" -ForegroundColor Yellow
