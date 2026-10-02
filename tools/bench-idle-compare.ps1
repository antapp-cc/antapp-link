# 空闲突发对照：交替切换 net.ipv4.tcp_slow_start_after_idle 并复用同一条 TCP 连接，
# 比较"空闲一段时间后突发"的首 2 秒吞吐。
#
#   pwsh -File tools/bench-idle-compare.ps1                 # 空闲 60s，各 3 轮
#   pwsh -File tools/bench-idle-compare.ps1 -Idle 300 -Rounds 2
#
# 切换内核参数走直连 SSH（不经隧道），所以不会打破隧道那边的空闲窗口。

param(
    [int]$Idle = 60,
    [int]$Rounds = 3,
    [string]$Remote = 'antapp-云服'
)

$ErrorActionPreference = 'Stop'
$script = Join-Path $PSScriptRoot 'bench-idle.py'

$off = @()   # slow_start_after_idle = 0
$on = @()    # slow_start_after_idle = 1（内核默认）

for ($r = 1; $r -le $Rounds; $r++) {
    foreach ($cond in @(0, 1)) {
        ssh $Remote "sysctl -w net.ipv4.tcp_slow_start_after_idle=$cond" | Out-Null
        Start-Sleep -Seconds 2

        $out = (python $script $Idle 2.0 2>&1 | Out-String)
        $rate = 0.0
        if ($out -match '=\s*([\d,]+)\s*B/s') {
            $rate = [double]($matches[1] -replace ',', '')
        } else {
            Write-Host "  解析失败，原始输出：$out"
        }

        if ($cond -eq 0) {
            $off += $rate
            Write-Host ("第 {0} 轮  slow_start_after_idle=0（本方案） : {1,12:N0} B/s" -f $r, $rate)
        } else {
            $on += $rate
            Write-Host ("第 {0} 轮  slow_start_after_idle=1（内核默认）: {1,12:N0} B/s" -f $r, $rate)
        }
    }
}

function Get-Med {
    param([double[]]$V)
    $s = $V | Sort-Object
    return $s[[int]($s.Count / 2)]
}

$mOff = Get-Med $off
$mOn = Get-Med $on

Write-Host ''
Write-Host ("空闲 {0}s 后前 2 秒吞吐中位数：" -f $Idle)
Write-Host ("  关闭慢启动重来（=0）：{0,12:N0} B/s" -f $mOff)
Write-Host ("  内核默认（=1）      ：{0,12:N0} B/s" -f $mOn)
if ($mOn -gt 0) {
    Write-Host ("  提升                ：{0,12:N1} %" -f (($mOff / $mOn - 1) * 100))
}
Write-Host ("  原始值 off：{0}" -f (($off | ForEach-Object { [int]$_ }) -join ', '))
Write-Host ("  原始值 on ：{0}" -f (($on | ForEach-Object { [int]$_ }) -join ', '))
