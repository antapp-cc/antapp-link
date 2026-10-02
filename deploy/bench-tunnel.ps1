# 隧道吞吐对照：单流 vs 4 并发流，各跑多轮取中位数。
#
#   pwsh -File deploy/bench-tunnel.ps1
#   pwsh -File deploy/bench-tunnel.ps1 -Bytes 20000000 -Rounds 5
#
# 每条 curl 都是独立的 TCP 流，按流哈希落到各自的外层连接上：
# members=4 时 4 条流有机会走 4 条连接，members=1 时只能挤一条。

param(
    [int]$Bytes = 10000000,
    [int]$Rounds = 3
)

$ErrorActionPreference = 'Stop'
$url = "https://speed.cloudflare.com/__down?bytes=$Bytes"

function Get-Median {
    param([double[]]$Values)
    $sorted = $Values | Sort-Object
    return $sorted[[int]($sorted.Count / 2)]
}

$single = @()
$multi = @()

for ($i = 1; $i -le $Rounds; $i++) {
    $one = [double](curl.exe -s -o NUL --max-time 120 -w "%{speed_download}" $url)
    $single += $one

    $lines = curl.exe -s -Z --max-time 120 -o NUL -o NUL -o NUL -o NUL -w "%{speed_download}`n" $url $url $url $url
    $sum = ($lines | Where-Object { $_ -match '^\d' } | ForEach-Object { [double]$_ } | Measure-Object -Sum).Sum
    $multi += $sum

    Write-Host ("第 {0} 轮：单流 {1,12:N0} B/s   4 流合计 {2,12:N0} B/s" -f $i, $one, $sum)
}

$sm = Get-Median $single
$mm = Get-Median $multi

Write-Host ''
Write-Host ("单流中位数      ：{0,12:N0} B/s" -f $sm)
Write-Host ("4 流合计中位数  ：{0,12:N0} B/s" -f $mm)
Write-Host ("聚合倍数        ：{0,12:N2} x" -f ($mm / $sm))
Write-Host ''
Write-Host ("单流原始值：{0}" -f (($single | ForEach-Object { [int]$_ }) -join ', '))
Write-Host ("4 流原始值：{0}" -f (($multi | ForEach-Object { [int]$_ }) -join ', '))
