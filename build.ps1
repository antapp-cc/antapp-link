# 交叉编译 AntApp Link 的发行二进制。
#
# 客户端把 wintun.dll 用 go:embed 内嵌进 exe，所以 internal/client/assets/wintun/ 下
# 必须有对应架构的 dll；缺了会编译不过（embed 找不到文件）。dll 的来路见 third_party/README.md。
#
#   pwsh -File build.ps1
#   pwsh -File build.ps1 -Version 0.2.0

[CmdletBinding()]
param(
    [string]$Version = '0.2.0',
    [string]$Dist = 'dist'
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

$distDir = Join-Path $root $Dist
New-Item -ItemType Directory -Force $distDir | Out-Null

function Build-Target {
    param(
        [string]$Goos,
        [string]$Goarch,
        [string]$Out,
        [string]$Pkg,
        [string]$Ldflags
    )
    Write-Host "== $Goos/$Goarch  ->  $Out" -ForegroundColor Cyan
    $env:GOOS = $Goos
    $env:GOARCH = $Goarch
    $env:CGO_ENABLED = '0'
    try {
        go build -trimpath -ldflags $Ldflags -o (Join-Path $distDir $Out) $Pkg
        if ($LASTEXITCODE -ne 0) { throw "$Out 构建失败" }
    }
    finally {
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
}

# 服务端：Linux 单二进制，不依赖任何外部程序
Build-Target -Goos 'linux' -Goarch 'amd64' -Out 'antapp-linkd' -Pkg './cmd/antapp-linkd' `
    -Ldflags '-s -w'

# 客户端：GUI 子系统，双击不弹控制台黑框；wintun.dll 已内嵌
Build-Target -Goos 'windows' -Goarch 'amd64' -Out 'antapp-link.exe' -Pkg './cmd/antapp-link' `
    -Ldflags "-s -w -H windowsgui -X github.com/antapp-cc/antapp-link/internal/client.Version=$Version"

# 安装程序：和客户端 exe 必须放在同一个目录里分发
Build-Target -Goos 'windows' -Goarch 'amd64' -Out 'antapp-setup.exe' -Pkg './cmd/antapp-setup' `
    -Ldflags "-s -w -H windowsgui -X github.com/antapp-cc/antapp-link/internal/setup.Version=$Version"

# 合规要求：Wintun 的预编译二进制许可要求随包附上原文
Copy-Item (Join-Path $root 'THIRD-PARTY-NOTICES.md') $distDir -Force

# 代码签名：蚁巢证书（装在本机证书库），安装器和客户端都签，时间戳保证证书过期后签名仍有效
$thumbprint = '58075F4CBD7592BB7A79B6B3F210A2AE551213D8'
$timestampUrl = 'http://timestamp.digicert.com'
$signtool = @(
    (Get-Command signtool -ErrorAction SilentlyContinue).Source,
    "${env:ProgramFiles(x86)}\Windows Kits\10\App Certification Kit\signtool.exe"
) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $signtool) {
    $kitDir = "${env:ProgramFiles(x86)}\Windows Kits\10\bin"
    if (Test-Path $kitDir) {
        $signtool = Get-ChildItem $kitDir -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName -match 'x64' } |
            Sort-Object FullName -Descending |
            Select-Object -First 1 -ExpandProperty FullName
    }
}
if ($signtool) {
    Write-Host '== 代码签名 ==' -ForegroundColor Cyan
    foreach ($exe in @('antapp-setup.exe', 'antapp-link.exe')) {
        $path = Join-Path $distDir $exe
        & $signtool sign /v /fd SHA256 /sha1 $thumbprint /tr $timestampUrl /td SHA256 $path | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "$exe 签名失败" }
        Write-Host "  $exe 已签名（CN=Antapp）"
    }
} else {
    Write-Warning '没找到 signtool，本次构建不签名'
}

# 打成安装包：setup.exe 会去同目录找 antapp-link.exe
$pkgDir = Join-Path $distDir 'AntAppLink-Setup'
if (Test-Path $pkgDir) { Remove-Item $pkgDir -Recurse -Force }
New-Item -ItemType Directory -Force $pkgDir | Out-Null
foreach ($f in @('antapp-setup.exe', 'antapp-link.exe', 'THIRD-PARTY-NOTICES.md')) {
    Copy-Item (Join-Path $distDir $f) $pkgDir -Force
}
$zip = Join-Path $distDir "AntAppLink-Setup-$Version.zip"
Compress-Archive -Path (Join-Path $pkgDir '*') -DestinationPath $zip -Force

Write-Host ''
Write-Host '== 产物 ==' -ForegroundColor Green
Get-ChildItem $distDir -File | Sort-Object Name | ForEach-Object {
    '  {0,-32} {1,12:N0} 字节' -f $_.Name, $_.Length
}
Write-Host ''
Write-Host '== 安装包 ==' -ForegroundColor Green
Get-ChildItem $pkgDir -File | Sort-Object Name | ForEach-Object {
    '  AntAppLink-Setup/{0,-22} {1,12:N0} 字节' -f $_.Name, $_.Length
}
Write-Host ''
Write-Host '分发：把 AntAppLink-Setup 整个目录（或那个 zip）给用户，运行 antapp-setup.exe。' -ForegroundColor DarkGray
Write-Host '节点机不需要装任何驱动或额外程序，客户端 exe 自带 wintun.dll。' -ForegroundColor DarkGray
