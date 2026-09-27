# 重新生成 Windows 资源（manifest + 图标）
#
# 为什么需要这一步：walk 的图标加载走 LoadIconWithScaleDown，这个 API 只存在于
# Common-Controls v6；不嵌 manifest 的话程序加载的是 v5，会在 NewIconFromFile 上直接失败，
# 而且控件外观会退回 Win95 风格。manifest 同时声明了 requireAdministrator，
# 让 UAC 在双击时就弹出来，而不是等用户点「连接」才发现没权限。
#
# 生成的 .syso 已经提交进仓库，所以**平时构建不需要跑这个脚本**。
# 只有改了 app.manifest 或换了图标之后才需要重新生成。
#
#   pwsh -File tools/mkres.ps1

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

$rsrc = Join-Path (go env GOPATH) 'bin\rsrc.exe'
if (-not (Test-Path $rsrc)) {
    Write-Host '正在安装 rsrc ...' -ForegroundColor Cyan
    go install github.com/akavel/rsrc@latest
    if ($LASTEXITCODE -ne 0) { throw 'rsrc 安装失败' }
}
if (-not (Test-Path $rsrc)) { throw "找不到 rsrc：$rsrc" }

$targets = @(
    @{ Dir = 'cmd\antapp-link'; Ico = 'internal\client\assets\antapp.ico' },
    @{ Dir = 'cmd\antapp-setup'; Ico = 'cmd\antapp-setup\assets\antapp.ico' }
)

foreach ($t in $targets) {
    $manifest = Join-Path $t.Dir 'app.manifest'
    $out = Join-Path $t.Dir 'rsrc_windows_amd64.syso'
    Write-Host "== $($t.Dir)" -ForegroundColor Cyan
    & $rsrc -manifest $manifest -ico $t.Ico -o $out
    if ($LASTEXITCODE -ne 0) { throw "$($t.Dir) 资源生成失败" }
    Get-Item $out | ForEach-Object { '   {0}  {1:N0} 字节' -f $_.Name, $_.Length }
}

Write-Host ''
Write-Host '完成。记得把新的 .syso 一起提交。' -ForegroundColor Green
