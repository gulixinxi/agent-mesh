#Requires -RunAsAdministrator
<#
.SYNOPSIS
安装 Agent Mesh 中央服务端为 Windows 服务。

.DESCRIPTION
完成以下动作：
  1. 建安装目录与数据目录
  2. 复制 server.exe
  3. （可选）生成自签 TLS 证书（可用 -CertHost 追加公网 IP / 域名到 SAN）
  4. 监听端口预检（避免与他人程序抢端口）
  5. 写 agent-mesh.json 配置（含随机生成的集群密钥与控制台口令）
  6. 防火墙放行监听端口
  7. 注册并启动 Windows 服务，并做健康自检

未显式指定 -Secret / -ConsolePass 时会自动生成强随机值，安装完成后打印出来，
请务必保存——配置文件里虽然可读，但那是唯一一次以明文形式呈现给操作者的机会。

跨网部署（云主机 / 反向隧道）时，客户端访问的不是本机网卡 IP，
必须用 -CertHost 显式声明客户端实际访问的 IP 或域名，否则 TLS 握手必然失败：
  .\install-server.ps1 -TLS -CertHost mesh.example.com -CertHost 1.2.3.4

安装时会预检监听端口是否被别的程序占用。端口冲突时 Windows 上 bind 仍会"成功"，
请求被随机分流，表现为服务 Running、日志无异常，但控制台怎么登都登不上。
检测到冲突会中止并报出占用进程名与 PID；确需共用端口时加 -Force 跳过预检。

.PARAMETER KeepLegacyService
检测到旧版服务装在其他目录时不接管，直接报错退出。默认行为是接管：停止旧实例、
注销旧注册项、由本次安装路径接手（旧文件保留），从而保证重启后只有一个实例。

.PARAMETER PublicUrl
服务端对外声明的基址（如 http://192.168.1.2:8099）。跨网段 / 端口映射 / 反向代理时
必须与实际访问地址一致，否则下发到同事电脑上的安装命令会指向一个连不上的地址。

.PARAMETER ClientPack
客户端二进制所在目录，服务端由此提供 /join/<码>/client 下载。
不配置该目录时，同事电脑上的一键安装会卡在"取不到客户端"。

.PARAMETER RegenCert
强制重新签发 TLS 证书。默认复用已有证书——重签会让所有已装好的客户端 CA 校验失败。

.EXAMPLE
.\install-server.ps1 -Addr ":8099" -TLS -CertHost 192.168.1.2 -PublicUrl "http://192.168.1.2:8099" -ClientPack "C:\Program Files\AgentMesh\Pack"

.NOTES
重装会自动沿用既有配置里的集群密钥与控制台口令。若每次都生成新的，
等于把已经装好的所有节点一次性踢下线——而这正是"换端口重装一次"最容易踩的坑。
#>
param(
    [string]$Addr = ":8080",
    [string]$Secret = "",
    [string]$ConsoleUser = "admin",
    [string]$ConsolePass = "",
    [string]$InstallDir = "C:\Program Files\AgentMesh\Server",
    [string]$DataDir = "C:\ProgramData\AgentMesh",
    [string]$ExePath = "",
    [string]$PublicUrl = "",
    [string]$ClientPack = "",
    [string[]]$CertHost = @(),
    [switch]$TLS,
    [switch]$RegenCert,
    [switch]$KeepLegacyService,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

function New-RandomSecret {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return [Convert]::ToBase64String($bytes)
}

# ---------- 旧实例接管 ----------
#
# 服务名在 Windows 上全局唯一。旧版装在别的目录时，注册表里那条依旧指向旧路径：
# 安装全程看着成功，重启后却先把旧程序拉起来，新装的这份永远轮不到自启动，
# 于是两个进程抢同一个端口——而这台机器上一次故障就是这么来的。
# 所以默认处理策略是接管（停旧→注销→改指当前路径），而不是报错让人回去卸载。

function Get-MeshService {
    param([string]$Name)
    return Get-CimInstance Win32_Service -Filter "Name='$Name'" -ErrorAction SilentlyContinue
}

# 从服务登记的命令行里剥出可执行文件路径（可能带引号、可能后接参数）。
function Get-RegisteredExePath {
    param([string]$PathName)
    if ([string]::IsNullOrWhiteSpace($PathName)) { return "" }
    $p = $PathName.Trim()
    if ($p -match '^"([^"]+)"') { return $Matches[1] }
    if ($p -match '^(\S+\.exe)') { return $Matches[1] }
    return ($p -split '\s+')[0]
}

# 停止服务并等到进程真正退出。文件句柄要进程退出才释放，
# 只看 Stop-Service 返回是不算数的（它接受停止指令后就返回了）。
function Stop-MeshService {
    param([string]$Name, [int]$TimeoutSec = 25)
    $svc = Get-MeshService $Name
    if (-not $svc -or $svc.State -eq 'Stopped') { return }
    $pid0 = $svc.ProcessId
    Write-Host "       状态：运行中（PID $pid0），正在停止..." -ForegroundColor DarkYellow
    Stop-Service -Name $Name -Force -ErrorAction SilentlyContinue
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 400
        $cur = Get-MeshService $Name
        if (-not $cur -or $cur.State -eq 'Stopped' -or $cur.ProcessId -eq 0) { return }
    }
    # 优雅停止超时：宁可强杀也不能留着，否则注册项删不干净，重启后照旧起来。
    $cur = Get-MeshService $Name
    if ($cur -and $cur.ProcessId -gt 0) {
        Write-Host "       停止超时（${TimeoutSec}s），强制结束进程 PID $($cur.ProcessId)" -ForegroundColor Yellow
        Stop-Process -Id $cur.ProcessId -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }
}

# 注销服务注册项，并等它彻底消失后才返回。
# sc delete 返回成功不代表注销完成：还有句柄没放干净时它只是被标记删除，
# 这时立刻 CreateService 会撞上 1078（服务已存在）。
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

# 读取旧安装留下的配置，用于沿用凭据。
# 只看两个位置：服务注册路径所在目录（旧版可能装在别处）与本次安装目录。
function Get-LegacyConfig {
    param([string]$Name, [string]$ThisInstallDir)
    $paths = @()
    $svc = Get-MeshService $Name
    if ($svc) {
        $old = Get-RegisteredExePath $svc.PathName
        if ($old) { $paths += (Join-Path (Split-Path -Parent $old) "agent-mesh.json") }
    }
    $paths += (Join-Path $ThisInstallDir "agent-mesh.json")
    foreach ($p in ($paths | Where-Object { $_ } | Select-Object -Unique)) {
        if (Test-Path $p) {
            try { return (Get-Content $p -Raw -Encoding UTF8 | ConvertFrom-Json) } catch { }
        }
    }
    return $null
}

# 扫描服务之外可能把旧版本重新拉起来的自启动项。
# 服务名唯一能保证"同名不会有两个"，但它管不到别人另投的启动途径：
# 计划任务、注册表 Run 键、启动文件夹——这些残留正是"重装后重启又冒出一个旧版"的来源。
function Get-LegacyAutostart {
    param([string[]]$MatchNames)
    $items = @()
    foreach ($task in (Get-ScheduledTask -ErrorAction SilentlyContinue)) {
        foreach ($act in $task.Actions) {
            $exe = $act.Execute
            foreach ($n in $MatchNames) {
                if ($exe -and $exe -match $n) {
                    $items += [pscustomobject]@{ Kind = "计划任务"; Location = ""; Name = $task.TaskName; Target = "$exe $($act.Arguments)" }
                    break
                }
            }
        }
    }
    $runKeys = @(
        "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run",
        "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\RunOnce",
        "HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run"
    )
    foreach ($k in $runKeys) {
        if (-not (Test-Path $k)) { continue }
        $props = Get-ItemProperty $k -ErrorAction SilentlyContinue
        foreach ($prop in $props.PSObject.Properties) {
            if ($prop.Name -match '^PS') { continue }
            foreach ($n in $MatchNames) {
                if ($prop.Value -match $n) {
                    $items += [pscustomobject]@{ Kind = "注册表 Run"; Location = $k; Name = $prop.Name; Target = "$($prop.Value)" }
                    break
                }
            }
        }
    }
    foreach ($dir in @("$env:ProgramData\Microsoft\Windows\Start Menu\Programs\Startup",
                       "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup")) {
        if (-not (Test-Path $dir)) { continue }
        foreach ($f in (Get-ChildItem $dir -Filter "*.lnk" -ErrorAction SilentlyContinue)) {
            $sh = New-Object -ComObject WScript.Shell
            $target = $sh.CreateShortcut($f.FullName).TargetPath
            foreach ($n in $MatchNames) {
                if ($target -and $target -match $n) {
                    $items += [pscustomobject]@{ Kind = "启动文件夹"; Location = $f.FullName; Name = $f.Name; Target = $target }
                    break
                }
            }
        }
    }
    return $items
}

function Get-ListenPort {
    param([string]$Address)
    $idx = $Address.LastIndexOf(":")
    if ($idx -ge 0 -and $idx -lt $Address.Length - 1) {
        return $Address.Substring($idx + 1)
    }
    return "8080"
}

# 返回占用该端口的进程列表（自动排除我们自己的服务进程——重装时它正在跑很正常）。
# 注意：$PID 是 PowerShell 自带的"当前进程 ID"，绝不能拿来做循环变量名。
function Get-PortOccupiers {
    param([int]$Port)
    $conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    if (-not $conns) { return @() }

    $selfProc = 0
    $svc = Get-CimInstance Win32_Service -Filter "Name='AgentMeshServer'" -ErrorAction SilentlyContinue
    if ($svc) { $selfProc = $svc.ProcessId }

    $result = @()
    foreach ($c in $conns) {
        $owner = $c.OwningProcess
        if ($owner -eq 0 -or $owner -eq $selfProc) { continue }
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $owner" -ErrorAction SilentlyContinue
        # Win32_Process.ExecutablePath 对 SYSTEM 进程常返回空，用 Get-Process 补一次。
        $exePath = ""
        if ($proc) { $exePath = $proc.ExecutablePath }
        if (-not $exePath) {
            $gp = Get-Process -Id $owner -ErrorAction SilentlyContinue
            if ($gp) { $exePath = $gp.Path }
        }
        $result += [pscustomobject]@{
            ProcId = $owner
            Name   = if ($proc) { $proc.Name } else { "(unknown)" }
            Path   = $exePath
        }
    }
    return $result | Sort-Object ProcId -Unique
}

function Get-FreePortSuggestions {
    param([int]$Around, [int]$Count = 3)
    $found = @()
    $candidate = $Around + 1
    while ($found.Count -lt $Count -and $candidate -lt 65535) {
        $busy = Get-NetTCPConnection -LocalPort $candidate -State Listen -ErrorAction SilentlyContinue
        if (-not $busy) { $found += $candidate }
        $candidate++
    }
    return $found
}

# 健康自检：/healthz 免鉴权且固定返回 {"status":"ok"}，这个响应体就是身份指纹。
# 光看 HTTP 200 不够——端口被别人抢了以后，对方也可能返回 200 或 401。
function Test-ServiceAlive {
    param([string]$Url)
    try {
        if ($Url -match '^https://') {
            [System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
        }
        $req = [System.Net.HttpWebRequest]::Create($Url)
        $req.Timeout = 5000
        $req.AllowAutoRedirect = $false
        # 必须绕过系统代理。HttpWebRequest 默认套用 IE 代理设置，而 .NET 对
        # ProxyOverride 里 "127.*" 这类带通配符的项匹配并不靠谱——实测本机开了
        # clash-verge（ProxyServer=127.0.0.1:7890）时，回环请求照样被送去代理，
        # 于是连一个根本没人监听的端口也能拿到 200，健康自检永远"通过"，
        # 恰好放过了它最该拦住的情况。
        $req.Proxy = $null
        $resp = $req.GetResponse()
        # 状态码必须在 Close 之前取：响应对象释放后 StatusCode 会退化成 0，
        # 曾导致健康自检永远误判失败。
        $code = [int]$resp.StatusCode
        $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
        $body = $reader.ReadToEnd()
        $reader.Close()
        $resp.Close()
        return @{ Code = $code; Body = $body }
    } catch {
        $webEx = $_.Exception.Response
        if ($webEx) {
            $stream = $webEx.GetResponseStream()
            $body = ""
            if ($stream) {
                $r = New-Object System.IO.StreamReader($stream)
                $body = $r.ReadToEnd()
                $r.Close()
            }
            return @{ Code = [int]$webEx.StatusCode; Body = $body }
        }
        # .NET 方法调用异常会被 PowerShell 再包一层，取内层才是真正的网络错误。
        $ex = $_.Exception
        if ($ex.InnerException) { $ex = $ex.InnerException }
        return @{ Code = -1; Body = $ex.Message }
    }
}

# 定位 server.exe：优先用 -ExePath，否则找仓库编译产物
if ([string]::IsNullOrWhiteSpace($ExePath)) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    $candidate = Join-Path $repoRoot "bin\server.exe"
    if (-not (Test-Path $candidate)) {
        throw "找不到 server.exe，请先用 -ExePath 指定，或先编译到 bin\server.exe"
    }
    $ExePath = $candidate
}
if (-not (Test-Path $ExePath)) { throw "server.exe 不存在: $ExePath" }

Write-Host "== 安装 Agent Mesh 服务端 ==" -ForegroundColor Cyan

$svcName   = "AgentMeshServer"
$targetExe = Join-Path $InstallDir "server.exe"
$certDir   = Join-Path $DataDir "certs"

# ---- 旧实例检测：装过旧版、且不在当前目录 ----
$legacySvc = Get-MeshService $svcName
$legacyExe = ""
if ($legacySvc) { $legacyExe = Get-RegisteredExePath $legacySvc.PathName }

$legacyIsElsewhere = $false
if ($legacySvc -and $legacyExe) {
    $oldFull = [System.IO.Path]::GetFullPath($legacyExe).TrimEnd('\').ToLower()
    $newFull = [System.IO.Path]::GetFullPath($targetExe).TrimEnd('\').ToLower()
    $legacyIsElsewhere = ($oldFull -ne $newFull)
}

if ($legacyIsElsewhere) {
    if ($KeepLegacyService) {
        throw "服务 $svcName 已存在且指向 $legacyExe`n本次目标 $targetExe`n已指定 -KeepLegacyService，安装中止。"
    }
    Write-Host "[接管] 检测到旧版服务装在其他目录，将由本次安装接管：" -ForegroundColor Cyan
    Write-Host "       旧路径：$legacyExe"
    Write-Host "       新路径：$targetExe"
    Write-Host "       做法：停止旧实例 → 注销旧注册项 → 改指新路径（旧文件保留）"
}

# ---- 沿用既有凭据 ----
# 每次重装都换新密钥的后果是：所有已装好的节点一次性全部鉴权失败。
# 而"换个端口重装一次"恰恰是最常见的操作，所以默认值必须是沿用。
$legacy = Get-LegacyConfig -Name $svcName -ThisInstallDir $InstallDir
$reusedSecret = $false
if ($legacy) {
    if ([string]::IsNullOrWhiteSpace($Secret) -and $legacy.secret) {
        $Secret = $legacy.secret
        $reusedSecret = $true
    }
    if (-not $PSBoundParameters.ContainsKey('ConsoleUser') -and $legacy.console_user) {
        $ConsoleUser = $legacy.console_user
    }
    if ([string]::IsNullOrWhiteSpace($ConsolePass) -and $legacy.console_pass) {
        $ConsolePass = $legacy.console_pass
        $reusedSecret = $true
    }
    if ([string]::IsNullOrWhiteSpace($PublicUrl) -and $legacy.public_url) { $PublicUrl = $legacy.public_url }
    if ([string]::IsNullOrWhiteSpace($ClientPack) -and $legacy.client_pack -and (Test-Path $legacy.client_pack)) {
        $ClientPack = $legacy.client_pack
    }
}
if ($reusedSecret) {
    Write-Host "[沿用] 已复用既有集群密钥与控制台口令 —— 已接入的节点不会掉线" -ForegroundColor Green
}

if ([string]::IsNullOrWhiteSpace($Secret))      { $Secret = New-RandomSecret }
if ([string]::IsNullOrWhiteSpace($ConsolePass)) { $ConsolePass = New-RandomSecret }

# ---- 清理服务之外的自启动残留 ----
# 服务名唯一能保证"同名不会有两个"，但管不到别人另投的启动途径。
# 这些残留正是"重装完看着正常，重启后又冒出一个旧版"的来源。
$leftovers = @(Get-LegacyAutostart -MatchNames @('AgentMesh', 'agent-mesh'))
foreach ($item in $leftovers) {
    Write-Host "[清理] 移除旧自启动项 $($item.Kind)：$($item.Name)" -ForegroundColor DarkYellow
    Write-Host "       $($item.Target)"
    switch ($item.Kind) {
        "计划任务"   { Unregister-ScheduledTask -TaskName $item.Name -Confirm:$false -ErrorAction SilentlyContinue }
        "注册表 Run" { Remove-ItemProperty -Path $item.Location -Name $item.Name -ErrorAction SilentlyContinue }
        "启动文件夹" { Remove-Item -Path $item.Location -Force -ErrorAction SilentlyContinue }
    }
}

$port = Get-ListenPort $Addr

# 端口预检必须在动任何东西之前：别人已经在 LISTEN 同一端口时，
# Windows 上 Go 程序的 bind 依然会"成功"（SO_REUSEADDR），请求会被随机分流，
# 结果是服务看着 Running、日志干净，但控制台怎么登都登不进去，且完全无报错。
$occupiers = @(Get-PortOccupiers -Port $port)
if ($occupiers.Count -gt 0 -and -not $Force) {
    Write-Host ""
    Write-Host "[冲突] 端口 $port 已被以下进程监听：" -ForegroundColor Red
    foreach ($o in $occupiers) {
        Write-Host ("       PID {0}  {1}" -f $o.ProcId, $o.Name) -ForegroundColor Red
        if ($o.Path) { Write-Host ("              {0}" -f $o.Path) -ForegroundColor DarkGray }
    }
    Write-Host ""
    Write-Host "       常见的撞端口程序：Everything（内置 HTTP 服务默认 8080）、" -ForegroundColor Yellow
    Write-Host "       Tomcat / Jenkins / 各类 NAS web 面板 / nginx / php 开发服务器。" -ForegroundColor Yellow
    $alts = Get-FreePortSuggestions -Around $port
    if ($alts.Count -gt 0) {
        Write-Host ""
        Write-Host "       建议换一个端口重装，当前空闲： $($alts -join ', ')" -ForegroundColor Green
        Write-Host "       例： .\install-server.ps1 -Addr \":$($alts[0])\" -ExePath `"$ExePath`"" -ForegroundColor Green
    }
    Write-Host ""
    Write-Host "       确认要让两个程序共用此端口（不推荐，请求会被随机分流），加 -Force 继续。" -ForegroundColor Yellow
    throw "端口 $port 冲突，安装中止"
}
if ($occupiers.Count -gt 0) {
    Write-Host "[警告] 端口 $port 已被占用（-Force 强制执行），请求可能被分流：" -ForegroundColor Yellow
    foreach ($o in $occupiers) { Write-Host ("       PID {0}  {1}" -f $o.ProcId, $o.Name) -ForegroundColor Yellow }
}

$logDir = Join-Path $DataDir "logs"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

# 程序文件替换前必须让旧实例真正退出：Windows 上正在运行的 exe 是被锁住的，
# 直接覆盖报 "The process cannot access the file"，而这话完全看不出跟服务有关。
if ($legacySvc) {
    Stop-MeshService -Name $svcName
    # 路径不同光停服不够：注册项留着，重启后 SCM 还会把旧路径那个拉起来。
    if ($legacyIsElsewhere) {
        if (-not (Remove-MeshServiceRegistration -Name $svcName)) {
            throw "无法注销旧服务注册项，安装中止。可手动执行 sc.exe delete $svcName 后重试"
        }
        Write-Host "[接管] 旧注册项已注销，将在本步注册到 $targetExe" -ForegroundColor DarkYellow
    }
}

try {
    Copy-Item -Path $ExePath -Destination (Join-Path $InstallDir "server.exe") -Force
} catch {
    Write-Host ""
    Write-Host "[失败] 无法替换 server.exe——文件仍被占用。" -ForegroundColor Red
    $hogs = @(Get-Process -Name server -ErrorAction SilentlyContinue)
    if ($hogs.Count -gt 0) {
        Write-Host ("       占用进程 PID：" + (($hogs.Id) -join ", ")) -ForegroundColor Yellow
    }
    Write-Host "       先执行 Stop-Service AgentMeshServer -Force（或 uninstall.ps1 -Role server）后重试。" -ForegroundColor Yellow
    throw
}
Write-Host "[1/7] 已安装程序到 $InstallDir"

$tlsCert = ""
$tlsKey  = ""
$certFile = Join-Path $certDir "server.pem"
$keyFile  = Join-Path $certDir "server-key.pem"
if ($TLS) {
    # 已有证书默认复用：重签会让所有已装好的客户端 CA 校验全部失败，
    # 而"换端口重装"这种操作根本不需要换证书。
    if ((Test-Path $certFile) -and (Test-Path $keyFile) -and -not $RegenCert) {
        $tlsCert = $certFile
        $tlsKey  = $keyFile
        Write-Host "[2/7] 复用既有 TLS 证书：$certFile" -ForegroundColor DarkYellow
        Write-Host "       （要重签请加 -RegenCert，重签后所有客户端须重新入网拿新 CA）"
    } else {
        New-Item -ItemType Directory -Force -Path $certDir | Out-Null
        $gencertArgs = @("gencert", "-out", $certDir)
        foreach ($h in $CertHost) {
            if (-not [string]::IsNullOrWhiteSpace($h)) { $gencertArgs += @("-host", $h) }
        }
        if ($CertHost.Count -eq 0) {
            Write-Host "[提示] 未指定 -CertHost：证书 SAN 只含本机网卡 IP/主机名。" -ForegroundColor Yellow
            Write-Host "       若客户端将用别的 IP 或域名访问（跨网段端口映射/隧道/异地），请加 -CertHost。" -ForegroundColor Yellow
        }
        & (Join-Path $InstallDir "server.exe") @gencertArgs | Out-Host
        if ($LASTEXITCODE -ne 0) { throw "证书生成失败" }
        $tlsCert = $certFile
        $tlsKey  = $keyFile
        Write-Host "[2/7] 已生成自签 TLS 证书"
    }
} else {
    Write-Host "[2/7] 跳过 TLS（未加 -TLS，链路为明文 HTTP）" -ForegroundColor Yellow
}

if ($occupiers.Count -eq 0) {
    Write-Host "[3/7] 端口预检通过：$port 无其他程序占用"
}

$config = [ordered]@{
    addr         = $Addr
    db           = (Join-Path $DataDir "agent_mesh_center.db")
    secret       = $Secret
    console_user = $ConsoleUser
    console_pass = $ConsolePass
    log_dir      = $logDir
    retention    = "720h"
    task_timeout = "5m"
}
if ($TLS) {
    $config.tls_cert = $tlsCert
    $config.tls_key  = $tlsKey
}
if ($PublicUrl)   { $config.public_url  = $PublicUrl }
if ($ClientPack)  { $config.client_pack = $ClientPack }
$configPath = Join-Path $InstallDir "agent-mesh.json"
$config | ConvertTo-Json -Depth 4 | Set-Content -Path $configPath -Encoding UTF8
Write-Host "[4/7] 已写入配置 $configPath"

$ruleName = "Agent Mesh Server ($port)"

# 换过端口就会留下针对旧端口的规则，留着既是暴露面也是误导。
Get-NetFirewallRule -DisplayName "Agent Mesh Server (*)" -ErrorAction SilentlyContinue |
    Where-Object { $_.DisplayName -ne $ruleName } |
    ForEach-Object {
        Write-Host "[提示] 移除旧端口防火墙规则：$($_.DisplayName)" -ForegroundColor DarkYellow
        Remove-NetFirewallRule -DisplayName $_.DisplayName -ErrorAction SilentlyContinue
    }

if (-not (Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Protocol TCP `
        -LocalPort $port -Action Allow -Profile Any | Out-Null
    Write-Host "[5/7] 已放行防火墙端口 $port"
} else {
    Write-Host "[5/7] 防火墙规则已存在，跳过"
}

Push-Location $InstallDir
try {
    & (Join-Path $InstallDir "server.exe") install
    if ($LASTEXITCODE -ne 0) { throw "服务注册失败" }
} finally {
    Pop-Location
}
Write-Host "[6/7] 服务已注册并启动"

# 健康自检。服务状态 Running 只说明进程没崩，不代表端口真的由它在响应——
# 端口被抢时两者都会显示正常，必须实际发一个请求、且校验响应体。
$scheme  = if ($TLS) { "https" } else { "http" }
$probeUrl = "$($scheme)://127.0.0.1:$port/healthz"
$live = $false
for ($i = 1; $i -le 5; $i++) {
    Start-Sleep -Seconds 2
    $probe = Test-ServiceAlive -Url $probeUrl
    if ($probe.Code -eq 200 -and $probe.Body -match '"status"\s*:\s*"ok"') { $live = $true; break }
}
Write-Host ""
if ($live) {
    Write-Host "[7/7] 健康自检通过：端口 $port 确实由本服务响应" -ForegroundColor Green
} else {
    Write-Host "[7/7] 健康自检未通过：$probeUrl" -ForegroundColor Red
    Write-Host ("       HTTP={0}  响应={1}" -f $probe.Code, ($probe.Body -replace '\s+', ' ').Substring(0, [Math]::Min(160, $probe.Body.Length))) -ForegroundColor Red
    Write-Host ""
    if ($occupiers.Count -gt 0) {
        Write-Host "       端口 $port 上还有别的服务在监听，你的请求很可能被它截走。" -ForegroundColor Yellow
    } else {
        Write-Host "       服务可能启动失败。请查看日志尾部：" -ForegroundColor Yellow
        Write-Host "       Get-Content '$(Join-Path $logDir 'agent-mesh-server.log')' -Tail 40" -ForegroundColor Yellow
    }
    Write-Host "       注意：凭据仍然有效，修好端口问题重启即可，不必重新生成。" -ForegroundColor Yellow
}

# 最后一道校验：注册表里那条到底指向谁。
# 这一步才回答"重启后会不会跑成旧的"——健康自检只能证明现在有人在响应，
# 证明不了响应的是本次装的这个程序。
$final = Get-MeshService $svcName
$finalPath = if ($final) { Get-RegisteredExePath $final.PathName } else { "" }
if (-not $finalPath) {
    Write-Host "[校验] 未查到服务注册项，重启后不会自动运行，请检查上面第 6 步" -ForegroundColor Red
} elseif ([System.IO.Path]::GetFullPath($finalPath).TrimEnd('\').ToLower() -eq
          [System.IO.Path]::GetFullPath($targetExe).TrimEnd('\').ToLower()) {
    Write-Host "[校验] 自启动注册项指向本次安装的程序：$finalPath" -ForegroundColor Green
} else {
    Write-Host "[校验] 注册项仍指向别处，重启后可能起来的是旧版本：" -ForegroundColor Red
    Write-Host "       $finalPath"
    Write-Host "       处理：sc.exe delete $svcName 后重新执行本脚本" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "===== 请保存以下凭据（仅此一次明文显示）=====" -ForegroundColor Yellow
Write-Host "集群共享密钥 (客户端 secret) : $Secret"
Write-Host "控制台用户名                 : $ConsoleUser"
Write-Host "控制台口令                   : $ConsolePass"
if ($TLS) {
    Write-Host "CA 证书（分发给客户端 tls_ca）: $(Join-Path $certDir 'ca.pem')"
}
Write-Host "=============================================" -ForegroundColor Yellow
Write-Host ""
Write-Host "本机自测: $($scheme)://127.0.0.1:$port/console"
if ($PublicUrl) { Write-Host "对外基址: $PublicUrl" }
Write-Host "日志文件: $(Join-Path $logDir 'agent-mesh-server.log')"
Write-Host ""
if ($PublicUrl -and $ClientPack) {
    Write-Host "给其他电脑装客户端：控制台 → 接入与设备 → 签发邀请，把命令发给对方即可。" -ForegroundColor Cyan
} else {
    Write-Host "要让其他电脑一键装客户端，还需补两项（重跑本脚本即可，配置会自动沿用）：" -ForegroundColor Yellow
    if (-not $ClientPack) {
        Write-Host "  -ClientPack <客户端目录>   把编译好的 client-windows-amd64.exe 放进去" -ForegroundColor Yellow
    }
    if (-not $PublicUrl) {
        Write-Host "  -PublicUrl  <对方能访问的地址>   如 http://192.168.1.2:$port（跨网段时填映射后的地址）" -ForegroundColor Yellow
    }
}
