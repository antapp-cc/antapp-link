# -trimpath 不保留 ldflags，版本必须读取程序的只显示版本入口。
function Get-ReleaseAssetVersion {
    param([string]$Path, [string]$Platform)
    $absolute = [IO.Path]::GetFullPath($Path)
    if ($Platform -eq 'windows') {
        # GUI 子系统不能依赖 PowerShell 的隐式等待/标准输出捕获。
        $process = [Diagnostics.Process]::new()
        $process.StartInfo = [Diagnostics.ProcessStartInfo]::new($absolute, '-version')
        $process.StartInfo.UseShellExecute = $false
        $process.StartInfo.CreateNoWindow = $true
        $process.StartInfo.RedirectStandardOutput = $true
        $process.StartInfo.RedirectStandardError = $true
        try {
            if (-not $process.Start()) { throw '客户端版本进程启动失败' }
            $stdout = $process.StandardOutput.ReadToEndAsync()
            $stderr = $process.StandardError.ReadToEndAsync()
            if (-not $process.WaitForExit(15000)) { $process.Kill(); throw '客户端版本读取超时' }
            if ($process.ExitCode -ne 0) { throw "客户端版本读取失败：$($stderr.GetAwaiter().GetResult())" }
            return $stdout.GetAwaiter().GetResult().Trim()
        } finally { $process.Dispose() }
    } else {
        if (-not (Get-Command wsl -ErrorAction SilentlyContinue)) { throw '正式发布版本校验需要 WSL 执行 Linux --version' }
        $linuxPath = & wsl --exec wslpath -a -u $absolute 2>$null
        if ($LASTEXITCODE -ne 0 -or -not $linuxPath) { throw '无法转换 Linux 候选包路径' }
        $reported = & wsl --exec $linuxPath.Trim() --version 2>$null
    }
    if ($LASTEXITCODE -ne 0) { throw "读取 $Path 版本失败" }
    return ($reported -join "`n").Trim()
}
# 与发布脚本共用的最终资产核对；签名仍由 antapp-sign 使用内置公钥校验。
function Assert-ReleaseContents {
    param([string]$Directory, [string]$Version, [string]$Repo = 'antapp-cc/antapp-link')
    $manifest = Get-Content -LiteralPath (Join-Path $Directory 'latest.json') -Raw | ConvertFrom-Json
    if ($manifest.version -cne $Version) { throw '最终清单版本与发布版本不一致' }
    $compiler = & go env GOVERSION
    if ($LASTEXITCODE -ne 0) { throw '无法读取构建工具链版本' }
    foreach ($asset in @(@('client', 'antapp-link.exe', 'windows'), @('server', 'antapp-linkd', 'linux'))) {
        $entry = $manifest.($asset[0])
        $path = Join-Path $Directory $asset[1]
        $expectedURL = "https://github.com/$Repo/releases/download/v$Version/$($asset[1])"
        if ($entry.url -cne $expectedURL) { throw "$($asset[1]) 的 URL/tag 与本次发布不一致" }
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "发布文件不存在：$path" }
        if ((Get-Item -LiteralPath $path).Length -ne $entry.size -or
            (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ine $entry.sha256) {
            throw "$($asset[1]) 的大小或 SHA-256 与最终清单不一致"
        }
        $metadata = & go version -m $path
        if ($LASTEXITCODE -ne 0) { throw "无法读取 $($asset[1]) 的 Go 构建信息" }
        $text = $metadata -join "`n"
        if ($text -notmatch (':\s+' + [regex]::Escape($compiler.Trim()) + '(\s|$)') -or
            $text -notmatch ('GOOS=' + $asset[2] + '(\s|$)') -or $text -notmatch 'GOARCH=amd64(\s|$)') {
            throw "$($asset[1]) 的工具链或目标平台不符合本次构建"
        }
        $reported = Get-ReleaseAssetVersion -Path $path -Platform $asset[2]
        $program = if ($asset[0] -eq 'client') { 'antapp-link' } else { 'antapp-linkd' }
        if ($reported -cne "$program $Version") { throw "$($asset[1]) 实际版本与发布版本不一致：$reported" }
    }
}