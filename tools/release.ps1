# 打一个可发布的版本，并生成 GitHub Release 需要的文件。
#
#   pwsh -File tools/release.ps1 -Version 0.2.0
#
# 产出在 dist/release-<版本>/：
#   antapp-link.exe        客户端更新包
#   latest.json            更新清单（客户端按它判断版本、校验 sha256）
#   THIRD-PARTY-NOTICES.md
#   上传指引.txt
#
# 然后到 GitHub 建一个 tag 为 v<版本> 的 Release，把前三个文件传上去。
#
# ⚠ 资产名必须固定为 antapp-link.exe / latest.json —— 客户端访问的是
#   releases/latest/download/latest.json 这种固定链接，靠的就是文件名不变。

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$Repo = 'antapp-cc/antapp-link'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

Write-Host "== 构建 $Version ==" -ForegroundColor Cyan
& (Join-Path $root 'build.ps1') -Version $Version
if ($LASTEXITCODE -ne 0) { throw '构建失败' }

$exe = Join-Path $root 'dist\antapp-link.exe'
if (-not (Test-Path $exe)) { throw "找不到 $exe" }

$hash = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
$size = (Get-Item $exe).Length

$out = Join-Path $root "dist\release-$Version"
if (Test-Path $out) { Remove-Item $out -Recurse -Force }
New-Item -ItemType Directory -Force $out | Out-Null

Copy-Item $exe (Join-Path $out 'antapp-link.exe') -Force
Copy-Item (Join-Path $root 'THIRD-PARTY-NOTICES.md') $out -Force

# 更新清单。字段名要和 internal/update 里的结构体对齐。
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
}

# 写无 BOM 的 UTF-8：带 BOM 的话有些 JSON 解析器会直接报错
$json = $manifest | ConvertTo-Json -Depth 6
[System.IO.File]::WriteAllText(
    (Join-Path $out 'latest.json'),
    $json + "`n",
    (New-Object System.Text.UTF8Encoding($false)))

$guide = @"
发布 $Version
============================================================

产物目录：$out

  1. 到 https://github.com/$Repo/releases/new 新建 Release
  2. Tag 填：$tag      （必须和 latest.json 里的 url 一致）
  3. 标题随便写，比如：AntApp Link $Version
  4. 把下面三个文件作为附件传上去（文件名不要改）：
       antapp-link.exe
       latest.json
       THIRD-PARTY-NOTICES.md
  5. 发布。客户端下次检查更新时会读到 latest.json，比对版本后自行下载。

校验值
  antapp-link.exe  sha256 = $hash
  大小                     = $size 字节

注意
  * 一定要勾选 "Set as the latest release"，否则 releases/latest 还是指向旧版。
  * 想撤回某个版本就把它标记成 pre-release，客户端不会取到。
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
Write-Host ''
Write-Host "下一步：按 $out\上传指引.txt 传到 GitHub Release（tag $tag）。" -ForegroundColor Yellow
