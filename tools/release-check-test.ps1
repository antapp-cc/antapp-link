# 只在临时目录验证发布门禁；不调用构建/签名发布脚本。
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'release-check.ps1')
$folder = Join-Path ([IO.Path]::GetTempPath()) ('antapp-release-check-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $folder | Out-Null
$version = '1.2.3'
$compiler = & go env GOVERSION
function Get-ReleaseAssetVersion { param([string]$Path,[string]$Platform) $program=if($Platform -eq 'windows'){'antapp-link'}else{'antapp-linkd'}; return "$program $script:reportedVersion" }
$script:reportedVersion=$version
function go {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    $global:LASTEXITCODE = 0
    if ($Arguments[0] -eq 'env') { return $script:compiler }
    $os = if ($Arguments[-1].EndsWith('.exe')) { 'windows' } else { 'linux' }
    @("$($Arguments[-1]): $script:compiler", "build GOOS=$os", 'build GOARCH=amd64',
        "build -ldflags=-X github.com/antapp-cc/antapp-link/internal/version.Version=$script:version")
}
try {
    [IO.File]::WriteAllText((Join-Path $folder 'antapp-link.exe'), 'client')
    [IO.File]::WriteAllText((Join-Path $folder 'antapp-linkd'), 'server')
    $manifest = @{version=$version}
    foreach ($spec in @(@('client','antapp-link.exe'), @('server','antapp-linkd'))) {
        $path=Join-Path $folder $spec[1]
        $manifest[$spec[0]]=@{url="https://github.com/antapp-cc/antapp-link/releases/download/v$version/$($spec[1])";size=(Get-Item $path).Length;sha256=(Get-FileHash $path -Algorithm SHA256).Hash.ToLower()}
    }
    function Save-Manifest { $manifest | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $folder 'latest.json') -Encoding utf8 }
    function Expect-Rejected([string]$Scenario) {
        $rejected=$false
        try { Assert-ReleaseContents -Directory $folder -Version '1.2.3' } catch { $rejected=$true }
        if (-not $rejected) { throw "未拒绝：$Scenario" }
        Write-Output "PASS $Scenario"
    }
    Save-Manifest
    Assert-ReleaseContents -Directory $folder -Version $version
    Write-Output 'PASS valid pair'
    $manifest.version='1.2.2'; Save-Manifest; Expect-Rejected 'wrong version'
    $manifest.version=$version; $saved=$manifest.server.url; $manifest.server.url=$manifest.client.url; Save-Manifest; Expect-Rejected 'wrong URL/tag'
    $manifest.server.url=$saved; $saved=$manifest.server.size; $manifest.server.size=1; Save-Manifest; Expect-Rejected 'wrong size'
    $manifest.server.size=$saved; Save-Manifest
    [IO.File]::WriteAllText((Join-Path $folder 'antapp-linkd'), 'Server'); Expect-Rejected 'wrong hash'
    [IO.File]::WriteAllText((Join-Path $folder 'antapp-linkd'), 'server')
    $script:reportedVersion='0.0.0'; Expect-Rejected 'wrong embedded version'; $script:reportedVersion=$version
    $script:compiler='go1.26.5'; $expectedCompiler='go1.26.10'
    # 模拟读取安全工具链、但资产来自旧工具链。
    function go {
        param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
        $global:LASTEXITCODE=0
        if ($Arguments[0] -eq 'env') { return $script:expectedCompiler }
        @("$($Arguments[-1]): go1.26.5", 'build GOOS=windows', 'build GOARCH=amd64', "build -ldflags=-X github.com/antapp-cc/antapp-link/internal/version.Version=$script:version")
    }
    Expect-Rejected 'old compiler'
    $distDir=$folder
    [IO.File]::WriteAllText((Join-Path $folder 'latest.json.sig'), 'old signature')
    $beforeManifest=Get-Content -LiteralPath (Join-Path $folder 'latest.json') -Raw
    $build=Get-Content -LiteralPath (Join-Path (Split-Path $PSScriptRoot -Parent) 'build.ps1') -Raw
    $start=$build.IndexOf('$oldManifests =')
    $end=$build.IndexOf('function Build-Target', $start)
    & ([scriptblock]::Create($build.Substring($start,$end-$start)))
    if ((Test-Path -LiteralPath (Join-Path $folder 'latest.json')) -or (Test-Path -LiteralPath (Join-Path $folder 'latest.json.sig'))) { throw '旧清单未隔离' }
    $copies=@(Get-ChildItem -LiteralPath (Join-Path $folder 'previous-manifests') -Recurse -File)
    if ($copies.Count -ne 2) { throw '旧清单备份不完整' }
    $copiedManifest=$copies | Where-Object Name -eq 'latest.json'
    if ((Get-Content -LiteralPath $copiedManifest.FullName -Raw) -cne $beforeManifest) { throw '归档改变了旧清单内容' }
    Write-Output 'PASS archives old manifest pair'
} finally {
    Remove-Item Function:\go
    # $folder 由临时目录与本轮 GUID 组成，删除前验证绝对目录边界。
    $target=[IO.Path]::GetFullPath($folder)
    $tmp=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if (-not $target.StartsWith($tmp,[StringComparison]::OrdinalIgnoreCase)) { throw '临时目录越界' }
    Remove-Item -LiteralPath $target -Recurse -Force
}