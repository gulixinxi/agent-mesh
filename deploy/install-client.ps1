#Requires -RunAsAdministrator
<#
.SYNOPSIS
安装 Agent Mesh 节点客户端为 Windows 服务。

.DESCRIPTION
完成以下动作：
  1. 建安装目录与数据目录
  2. 复制 client.exe
  3. （可选）从服务端 CA 路径复制 ca.pem
  4. 写 agent-mesh.json 配置
  5. 注册并启动 Windows 服务

客户端是被动连接方，不需要放行入站端口。

安装前会预检服务端地址是否可达。连不上时不中止安装，但会明确报警——
否则装完表现为「服务 Running、一切正常」，实际只是在日志里无限重试连不上。

.PARAMETER KeepLegacyService
检测到旧版服务装在其他目录时不接管，直接报错退出。默认接管：
停止旧实例、注销旧注册项、由本次安装路径接手，保证重启后只有一个常驻实例。
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$ServerURL,
    [string]$Secret = "",
    [string]$ClientID = "",
    [string]$CaPath = "",
    [string]$InstallDir = "C:\Program Files\AgentMesh\Client",
    [string]$DataDir = "C:\ProgramData\AgentMesh",
    [string]$ExePath = "",
    [int]$P2PPort = 6001,
    [switch]$KeepLegacyService
)

$ErrorActionPreference = "Stop"

# ---------- 旧实例接管 ----------
# 服务名在 Windows 上全局唯一。旧版装在别的目录时，注册表里那条依旧指向旧路径：
# 安装全程看着成功，重启后却先把旧程序拉起来，新装的这份永远排不上自启动。
# 默认接管（停旧 → 注销 → 改指当前路径），而不是报错让人回去卸载。
function Get-MeshService {
    param([string]$Name)
    return Get-CimInstance Win32_Service -Filter "Name='$Name'" -ErrorAction SilentlyContinue
}

function Get-RegisteredExePath {
    param([string]$PathName)
    if ([string]::IsNullOrWhiteSpace($PathName)) { return "" }
    $p = $PathName.Trim()
    if ($p -match '^"([^"]+)"') { return $Matches[1] }
    if ($p -match '^(\S+\.exe)') { return $Matches[1] }
    return ($p -split '\s+')[0]
}

# 停服并等到进程真正退出：文件句柄要进程退出才释放，
# Stop-Service 本身在"接受停止指令"后就返回了，不算数。
function Stop-MeshService {
    param([string]$Name, [int]$TimeoutSec = 25)
    $svc = Get-MeshService $Name
    if (-not $svc -or $svc.State -eq 'Stopped') { return }
    Write-Host "       状态：运行中（PID $($svc.ProcessId)），正在停止..." -ForegroundColor DarkYellow
    Stop-Service -Name $Name -Force -ErrorAction SilentlyContinue
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 400
        $cur = Get-MeshService $Name
        if (-not $cur -or $cur.State -eq 'Stopped' -or $cur.ProcessId -eq 0) { return }
    }
    $cur = Get-MeshService $Name
    if ($cur -and $cur.ProcessId -gt 0) {
        Write-Host "       停止超时（${TimeoutSec}s），强制结束 PID $($cur.ProcessId)" -ForegroundColor Yellow
        Stop-Process -Id $cur.ProcessId -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }
}

# 注销注册项并等它彻底消失。sc delete 返回成功不代表注册项已移除，
# 残留期间重建同名服务会撞上 1078。
function Remove-MeshServiceRegistration {
    param([string]$Name)
    sc.exe delete $Name | Out-Null
    $deadline = (Get-Date).AddSeconds(20)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 300
        if (-not (Get-MeshService $Name)) { return $true }
    }
    return $false
}

function Test-TcpReachable {
    param([string]$Target, [int]$TargetPort, [int]$TimeoutMs = 3000)
    $tcp = New-Object System.Net.Sockets.TcpClient
    try {
        $async = $tcp.BeginConnect($Target, $TargetPort, $null, $null)
        $ok = $async.AsyncWaitHandle.WaitOne($TimeoutMs, $false)
        return ($ok -and $tcp.Connected)
    } catch {
        return $false
    } finally {
        $tcp.Close()
    }
}

function Get-ServerProbeTarget {
    param([string]$Url)
    try {
        $u = [System.Uri]$Url
        $p = $u.Port
        if ($p -le 0) { $p = if ($u.Scheme -eq 'https') { 443 } else { 80 } }
        return @{ Host = $u.Host; Port = $p }
    } catch {
        return $null
    }
}

if ([string]::IsNullOrWhiteSpace($ExePath)) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    $candidate = Join-Path $repoRoot "bin\client.exe"
    if (-not (Test-Path $candidate)) {
        throw "找不到 client.exe，请先用 -ExePath 指定，或先编译到 bin\client.exe"
    }
    $ExePath = $candidate
}
if (-not (Test-Path $ExePath)) { throw "client.exe 不存在: $ExePath" }

Write-Host "== 安装 Agent Mesh 客户端 ==" -ForegroundColor Cyan

# ---- 系统前置检查 ----
# 客户端是 64 位程序，且要求 Windows 10 / Server 2016 及以上：
# Go 1.21 起官方不再支持 Windows 7/8/8.1，二进制在这些系统上运行时会直接失效
# （进程起不来，日志里什么都留不下，现场极难判断）。所以拦在最前面。
$osVer = [Environment]::OSVersion.Version
$arch  = "$env:PROCESSOR_ARCHITECTURE"
if ($env:PROCESSOR_ARCHITEW6432) { $arch = "$env:PROCESSOR_ARCHITEW6432" }
if ($osVer.Major -lt 10) {
    Write-Host "[中止] 本机系统版本过低：$([Environment]::OSVersion.VersionString)" -ForegroundColor Red
    Write-Host "       客户端要求 Windows 10 / Server 2016 及以上（第 $($osVer.Major).$($osVer.Minor) 代不支持）。" -ForegroundColor Red
    throw "系统版本过低，无法安装 Agent Mesh 客户端。请改用 Win10+ 的机器，或先升级系统。"
}
if ($arch -ne 'AMD64' -and $arch -ne 'ARM64') {
    Write-Host "[中止] 本机是 32 位系统（$arch），客户端只有 64 位版本。" -ForegroundColor Red
    throw "32 位系统不支持。请改用 64 位 Windows 10+ 的机器。"
}
Write-Host "[检查] 系统 $([Environment]::OSVersion.VersionString) / $arch" -ForegroundColor Green

$svcName   = "AgentMeshClient"
$targetExe = Join-Path $InstallDir "client.exe"

# ---- 旧实例检测与凭据沿用 ----
$legacySvc = Get-MeshService $svcName
$legacyExe = ""
if ($legacySvc) { $legacyExe = Get-RegisteredExePath $legacySvc.PathName }

$legacyIsElsewhere = $false
if ($legacySvc -and $legacyExe) {
    $oldFull = [System.IO.Path]::GetFullPath($legacyExe).TrimEnd('\').ToLower()
    $newFull = [System.IO.Path]::GetFullPath($targetExe).TrimEnd('\').ToLower()
    $legacyIsElsewhere = ($oldFull -ne $newFull)
    if ($legacyIsElsewhere) {
        if ($KeepLegacyService) {
            throw "服务 $svcName 已存在且指向 $legacyExe`n本次目标 $targetExe`n已指定 -KeepLegacyService，安装中止。"
        }
        Write-Host "[接管] 检测到旧版服务装在其他目录，将由本次安装接管：" -ForegroundColor Cyan
        Write-Host "       旧路径：$legacyExe"
        Write-Host "       新路径：$targetExe"
    }
}

# 沿用旧配置里的节点 ID 与密钥：
# ID 变了会在控制台多出一堆同名不同 ID 的幽灵设备，密钥变了则直接下线。
$legacyCfgPath = ""
if ($legacyExe) { $legacyCfgPath = Join-Path (Split-Path -Parent $legacyExe) "agent-mesh.json" }
if (-not (Test-Path $legacyCfgPath)) { $legacyCfgPath = Join-Path $InstallDir "agent-mesh.json" }
$legacy = $null
if (Test-Path $legacyCfgPath) {
    try { $legacy = Get-Content $legacyCfgPath -Raw -Encoding UTF8 | ConvertFrom-Json } catch { $legacy = $null }
}
if ($legacy) {
    $reused = @()
    if ([string]::IsNullOrWhiteSpace($Secret) -and $legacy.secret) { $Secret = $legacy.secret; $reused += "集群密钥" }
    if ([string]::IsNullOrWhiteSpace($ClientID) -and $legacy.id) { $ClientID = $legacy.id; $reused += "节点 ID" }
    if ([string]::IsNullOrWhiteSpace($CaPath) -and $legacy.tls_ca -and (Test-Path $legacy.tls_ca)) {
        $CaPath = $legacy.tls_ca
        $reused += "CA 证书"
    }
    if ($reused.Count -gt 0) {
        Write-Host "[沿用] 已复用既有配置：$($reused -join '、')（来源 $legacyCfgPath）" -ForegroundColor Green
    }
}

if ([string]::IsNullOrWhiteSpace($ClientID)) { $ClientID = $env:COMPUTERNAME }

# 服务端是 HTTPS 时，必须信任它的自签 CA，否则 TLS 握手直接失败。
$tlsCA = ""
if ($ServerURL -match '^https://') {
    if ([string]::IsNullOrWhiteSpace($CaPath)) {
        throw "服务端为 HTTPS，请用 -CaPath 指定服务端生成的 ca.pem"
    }
    if (-not (Test-Path $CaPath)) { throw "CA 证书不存在: $CaPath" }
}

# 预检一：服务端地址可达。
# 跨网段 / NAT / 目标端口改过 / 服务端没起，表现完全一样——都只是连不上。
# 不预检的话，装完看到的是"服务 Running"，而节点在日志里无限重试。
$probeTarget = Get-ServerProbeTarget -Url $ServerURL
if ($probeTarget) {
    if (Test-TcpReachable -Target $probeTarget.Host -TargetPort $probeTarget.Port) {
        Write-Host "[预检] 服务端 $($probeTarget.Host):$($probeTarget.Port) 可达" -ForegroundColor Green
    } else {
        Write-Host ""
        Write-Host "[警告] 连不上服务端 $($probeTarget.Host):$($probeTarget.Port)（TCP 3 秒超时）" -ForegroundColor Red
        Write-Host ""
        Write-Host "       按顺序排查：" -ForegroundColor Yellow
        Write-Host "       1) 服务端装了吗、服务处于 Running 吗" -ForegroundColor Yellow
        Write-Host "       2) 端口写对了吗——用默认 8080 很容易撞上 Everything，确认服务端实际监听端口" -ForegroundColor Yellow
        Write-Host "       3) 服务端防火墙放行了入站吗（安装脚本会自动建规则，手动改过端口要重建）" -ForegroundColor Yellow
        Write-Host "       4) 双方是否跨了路由器 / NAT：中间隔着 NAT 时只能单向连通，" -ForegroundColor Yellow
        Write-Host "          节点能连中枢才行，反向不必通；真要跨网段请先把两边接到同一个路由器下" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "       安装继续，但此节点上线后会一直重试连接，控制台里不会出现它。" -ForegroundColor Yellow
        Write-Host "       修好后不必重装，重启服务即可： Restart-Service AgentMeshClient" -ForegroundColor Yellow
        Write-Host ""
    }
}

# 预检二：P2P 端口。冲突不致命（文件传输还能走服务端中转），所以只警告。
if ($P2PPort -gt 0) {
    $selfProc = 0
    $selfSvc = Get-CimInstance Win32_Service -Filter "Name='AgentMeshClient'" -ErrorAction SilentlyContinue
    if ($selfSvc) { $selfProc = $selfSvc.ProcessId }
    $hogPids = @()
    foreach ($c in (Get-NetTCPConnection -LocalPort $P2PPort -State Listen -ErrorAction SilentlyContinue)) {
        if ($c.OwningProcess -gt 0 -and $c.OwningProcess -ne $selfProc) { $hogPids += $c.OwningProcess }
    }
    if ($hogPids.Count -gt 0) {
        foreach ($hp in ($hogPids | Sort-Object -Unique)) {
            $p = Get-Process -Id $hp -ErrorAction SilentlyContinue
            Write-Host "[警告] P2P 端口 $P2PPort 已被 PID $hp $(if($p){$p.ProcessName}) 占用，直连传文件可能不可用（可走服务端中转）" -ForegroundColor Yellow
        }
        Write-Host "       可换端口重装：-P2PPort 6101" -ForegroundColor Yellow
    }
}
$logDir    = Join-Path $DataDir "logs"
$downloads = Join-Path $DataDir "downloads"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
New-Item -ItemType Directory -Force -Path $downloads | Out-Null

# 替换程序文件前必须让旧实例真正退出（Windows 会锁住运行中的 exe）。
# 路径不同时还要额外注销旧注册项——留着它，重启后 SCM 照旧把旧路径那个拉起来。
if ($legacySvc) {
    Stop-MeshService -Name $svcName
    if ($legacyIsElsewhere) {
        if (-not (Remove-MeshServiceRegistration -Name $svcName)) {
            throw "无法注销旧服务注册项，安装中止。可手动执行 sc.exe delete $svcName 后重试"
        }
        Write-Host "[接管] 旧注册项已注销，将在本步注册到 $targetExe" -ForegroundColor DarkYellow
    }
}

try {
    Copy-Item -Path $ExePath -Destination (Join-Path $InstallDir "client.exe") -Force
} catch {
    Write-Host ""
    Write-Host "[失败] 无法替换 client.exe——文件仍被占用。" -ForegroundColor Red
    $hogs = @(Get-Process -Name client -ErrorAction SilentlyContinue)
    if ($hogs.Count -gt 0) {
        Write-Host ("       占用进程 PID：" + (($hogs.Id) -join ", ")) -ForegroundColor Yellow
    }
    Write-Host "       先执行 Stop-Service AgentMeshClient -Force（或 uninstall.ps1 -Role client）后重试。" -ForegroundColor Yellow
    throw
}
Write-Host "[1/4] 已安装程序到 $InstallDir"

if ($CaPath -ne "") {
    $dest = Join-Path $InstallDir "ca.pem"
    Copy-Item -Path $CaPath -Destination $dest -Force
    $tlsCA = $dest
    Write-Host "[2/4] 已装入 CA 证书 $dest"
} else {
    Write-Host "[2/4] 未配置 TLS CA（服务端为 HTTP 时无需配置）"
}

$config = [ordered]@{
    server       = $ServerURL
    id           = $ClientID
    secret       = $Secret
    p2p_port     = $P2PPort
    download_dir = $downloads
    doubao_db    = ""
    log_dir      = $logDir
}
if ($tlsCA -ne "") { $config.tls_ca = $tlsCA }

$configPath = Join-Path $InstallDir "agent-mesh.json"
$config | ConvertTo-Json -Depth 4 | Set-Content -Path $configPath -Encoding UTF8
Write-Host "[3/4] 已写入配置 $configPath"

Push-Location $InstallDir
try {
    & (Join-Path $InstallDir "client.exe") install
    if ($LASTEXITCODE -ne 0) { throw "服务注册失败" }
} finally {
    Pop-Location
}
Write-Host "[4/4] 服务已注册并启动" -ForegroundColor Green

# 校验自启动注册项指向谁：这是"重启后会不会跑成旧的"的唯一答案。
# 健康自检只能证明现在有人在连，证明不了连的是本次装的这个程序。
$final = Get-MeshService $svcName
$finalPath = if ($final) { Get-RegisteredExePath $final.PathName } else { "" }
if (-not $finalPath) {
    Write-Host "[校验] 未查到服务注册项，此节点重启后不会自动运行" -ForegroundColor Red
} elseif ([System.IO.Path]::GetFullPath($finalPath).TrimEnd('\').ToLower() -eq
          [System.IO.Path]::GetFullPath($targetExe).TrimEnd('\').ToLower()) {
    Write-Host "[校验] 自启动注册项指向本次安装的程序：$finalPath" -ForegroundColor Green
} else {
    Write-Host "[校验] 注册项仍指向别处，重启后起来的可能还是旧版本：" -ForegroundColor Red
    Write-Host "       $finalPath"
    Write-Host "       处理：sc.exe delete $svcName 后重新执行本脚本" -ForegroundColor Yellow
}

# 启动后自检：服务 Running 只代表进程活着，不代表它连得上中枢。
# 直接看日志里有没有连接类报错——这是唯一诚实的信号。
Start-Sleep -Seconds 6
$cliLog = Join-Path $logDir "agent-mesh-client.log"
if (Test-Path $cliLog) {
    $tail = Get-Content $cliLog -Tail 80 -ErrorAction SilentlyContinue
    $bad = @($tail | Where-Object { $_ -match 'connection refused|no such host|i/o timeout|dial tcp|TLS handshake|x509|certificate' })
    if ($bad.Count -gt 0) {
        Write-Host ""
        Write-Host "[自检] 日志里发现连接类报错，此节点可能没能上线：" -ForegroundColor Red
        $bad | Select-Object -Last 3 | ForEach-Object { Write-Host ("       " + $_) -ForegroundColor Red }
        Write-Host "       修好后执行： Restart-Service AgentMeshClient" -ForegroundColor Yellow
    } else {
        Write-Host "[自检] 日志无连接报错，节点应已正常上报" -ForegroundColor Green
    }
} else {
    Write-Host "[自检] 尚未生成日志文件，等 10 秒后检查： $cliLog" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "节点 ID   : $ClientID"
Write-Host "上报目标  : $ServerURL"
Write-Host "日志文件  : $(Join-Path $logDir 'agent-mesh-client.log')"
Write-Host "P2P 端口  : $P2PPort（0 表示关闭 P2P）"
