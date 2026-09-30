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
#>
param(
    [string]$Addr = ":8080",
    [string]$Secret = "",
    [string]$ConsoleUser = "admin",
    [string]$ConsolePass = "",
    [string]$InstallDir = "C:\Program Files\AgentMesh\Server",
    [string]$DataDir = "C:\ProgramData\AgentMesh",
    [string]$ExePath = "",
    [string[]]$CertHost = @(),
    [switch]$TLS,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

function New-RandomSecret {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return [Convert]::ToBase64String($bytes)
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

if ([string]::IsNullOrWhiteSpace($Secret))     { $Secret = New-RandomSecret }
if ([string]::IsNullOrWhiteSpace($ConsolePass)) { $ConsolePass = New-RandomSecret }

Write-Host "== 安装 Agent Mesh 服务端 ==" -ForegroundColor Cyan

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

# 重装时必须先停服：Windows 上正在运行的可执行文件是被锁住的，
# 直接覆盖会报 "The process cannot access the file"，而这话完全看不出跟服务有关。
if (Get-Service -Name "AgentMeshServer" -ErrorAction SilentlyContinue) {
    Write-Host "[提示] 已存在服务 AgentMeshServer，先停止以替换程序文件" -ForegroundColor DarkYellow
    Stop-Service -Name "AgentMeshServer" -Force -ErrorAction SilentlyContinue
    $deadline = (Get-Date).AddSeconds(20)
    while ((Get-Date) -lt $deadline) {
        $c = Get-CimInstance Win32_Service -Filter "Name='AgentMeshServer'" -ErrorAction SilentlyContinue
        if (-not $c -or $c.ProcessId -eq 0) { break }
        Start-Sleep -Milliseconds 400
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

$certDir = Join-Path $DataDir "certs"
$tlsCert = ""
$tlsKey = ""
if ($TLS) {
    $gencertArgs = @("gencert", "-out", $certDir)
    foreach ($h in $CertHost) {
        if (-not [string]::IsNullOrWhiteSpace($h)) { $gencertArgs += @("-host", $h) }
    }
    if ($CertHost.Count -eq 0) {
        Write-Host "[提示] 未指定 -CertHost：证书 SAN 只含本机网卡 IP/主机名。" -ForegroundColor Yellow
        Write-Host "       若客户端将用别的 IP 或域名访问（云主机/隧道/异地），请重装并加 -CertHost。" -ForegroundColor Yellow
    }
    & (Join-Path $InstallDir "server.exe") @gencertArgs | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "证书生成失败" }
    $tlsCert = Join-Path $certDir "server.pem"
    $tlsKey  = Join-Path $certDir "server-key.pem"
    Write-Host "[2/7] 已生成自签 TLS 证书"
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
Write-Host "日志文件: $(Join-Path $logDir 'agent-mesh-server.log')"
