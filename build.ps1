# 交叉编译 AntApp Link 的发行二进制。
#
# 客户端把 wintun.dll 用 go:embed 内嵌进 exe，所以 internal/client/assets/wintun/ 下
# 必须有对应架构的 dll；缺了会编译不过（embed 找不到文件）。dll 的来路见 third_party/README.md。
#
#   pwsh -File build.ps1
#   pwsh -File build.ps1 -Version 0.2.0

[CmdletBinding()]
param(
    [string]$Version = '0.2.8',
    [string]$Dist = 'dist'
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

# 版本号会拼进发布文件名（AntAppLink-安装-<版本>.exe）和 dist 目录名，只允许
# SemVer 的安全字符：路径分隔符、盘符和 .. 段一旦进来，发布脚本的递归删除可能
# 越出仓库。和 tools/release.ps1、internal/update 的 Manifest.Validate 同一套规则。
if ($Version -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$') {
    throw "版本号格式不对：$Version（只允许 主.次.修订 和 -/+ 安全后缀，例如 0.4.13）"
}

# go.mod 固定最低安全修订，拒绝用旧版工具链生成发行产物。
# 只约束下界：未来 minor（1.28+）放行，否则每次升级 Go 都要改这个脚本。
$goVersion = & go env GOVERSION
if ($LASTEXITCODE -ne 0) { throw '无法加载 go.mod 要求的安全工具链' }
if ($goVersion -notmatch '^go(\d+\.\d+\.\d+)$') { throw "无法识别正式 Go 工具链：$goVersion" }
$compiler = [version]$Matches[1]
$safeCompiler = $compiler.Major -gt 1 -or (
    $compiler.Major -eq 1 -and (
        $compiler.Minor -gt 27 -or
        ($compiler.Minor -eq 27 -and $compiler.Build -ge 3) -or
        ($compiler.Minor -eq 26 -and $compiler.Build -ge 10)))
if (-not $safeCompiler) { throw "构建需要 Go 1.26.10+（1.27 需 1.27.3+）的安全修订，当前 $goVersion；可设置 GOTOOLCHAIN=go1.26.10" }

$distDir = Join-Path $root $Dist
New-Item -ItemType Directory -Force $distDir | Out-Null
# 构建前隔离旧清单，失败构建也不能留下可被误当成新包的签名清单。
$oldManifests = @('latest.json', 'latest.json.sig') | ForEach-Object { Join-Path $distDir $_ } |
    Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
if ($oldManifests) {
    $archiveDir = Join-Path $distDir ('previous-manifests\' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $archiveDir -Force | Out-Null
    foreach ($file in $oldManifests) { Move-Item -LiteralPath $file -Destination $archiveDir }
}

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
# 版本号必须注入：服务端的自动更新就是拿它跟清单里的 version 比，注入失效会让它
# 永远自认旧版、每小时更新重启一次。
Build-Target -Goos 'linux' -Goarch 'amd64' -Out 'antapp-linkd' -Pkg './cmd/antapp-linkd' `
    -Ldflags "-s -w -X github.com/antapp-cc/antapp-link/internal/version.Version=$Version"

# 客户端：GUI 子系统，双击不弹控制台黑框；wintun.dll 已内嵌
Build-Target -Goos 'windows' -Goarch 'amd64' -Out 'antapp-link.exe' -Pkg './cmd/antapp-link' `
    -Ldflags "-s -w -H windowsgui -X github.com/antapp-cc/antapp-link/internal/version.Version=$Version"

# 代码签名：蚁巢证书（装在本机证书库），客户端和安装器都要签，时间戳保证证书过期后签名仍有效。
#
# 顺序有硬要求：客户端必须【先签名、再嵌进安装器】。反过来的话（先嵌后签），安装器里装出来
# 的是未签名的客户端，而 dist/ 里那份 —— 也就是 latest.json 指向、自动更新会下载的那份 ——
# 是签过名的：同一个版本，两条分发路径给出两个不同的二进制。
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

function Invoke-Sign {
    param([string]$Exe)
    if (-not $signtool) { return }
    & $signtool sign /v /fd SHA256 /sha1 $thumbprint /tr $timestampUrl /td SHA256 (Join-Path $distDir $Exe) | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "$Exe 签名失败" }
    Write-Host "  $Exe 已签名（CN=Antapp）"
}

if ($signtool) { Write-Host '== 代码签名 ==' -ForegroundColor Cyan }
else { Write-Warning '没找到 signtool，本次构建不签名' }

Invoke-Sign 'antapp-link.exe'

# 单文件安装器：客户端 exe 和第三方许可拷进 assets，由 go:embed 内嵌进安装器。
# 必须在客户端签名【之后】拷，理由见上面那段注释。
$setupAssets = Join-Path $root 'cmd\antapp-setup\assets'
New-Item -ItemType Directory -Force $setupAssets | Out-Null
Copy-Item (Join-Path $distDir 'antapp-link.exe') (Join-Path $setupAssets 'antapp-link.exe') -Force
Copy-Item (Join-Path $root 'THIRD-PARTY-NOTICES.md') (Join-Path $setupAssets 'README.md') -Force

# 安装程序：内嵌了客户端，分发只带这一个文件
Build-Target -Goos 'windows' -Goarch 'amd64' -Out 'antapp-setup.exe' -Pkg './cmd/antapp-setup' `
    -Ldflags "-s -w -H windowsgui -X github.com/antapp-cc/antapp-link/internal/version.Version=$Version"

Invoke-Sign 'antapp-setup.exe'

# 合规要求：Wintun 的预编译二进制许可要求随包附上原文
Copy-Item (Join-Path $root 'THIRD-PARTY-NOTICES.md') (Join-Path $distDir 'README.md') -Force

# 给客户的分发物：签名后的单文件安装器，带上版本号命名
$setupDist = Join-Path $distDir "AntAppLink-安装-$Version.exe"
Copy-Item (Join-Path $distDir 'antapp-setup.exe') $setupDist -Force

# 仅 tools/release.ps1 为本次最终产物生成并签名清单。

Write-Host ''
Write-Host '== 产物 ==' -ForegroundColor Green
Get-ChildItem $distDir -File | Sort-Object Name | ForEach-Object {
    '  {0,-32} {1,12:N0} 字节' -f $_.Name, $_.Length
}
Write-Host ''
Write-Host '分发：把 AntAppLink-安装-版本号.exe 一个文件给用户即可，双击安装。' -ForegroundColor DarkGray
Write-Host '节点机不需要装任何驱动或额外程序，客户端 exe 自带 wintun.dll。' -ForegroundColor DarkGray
