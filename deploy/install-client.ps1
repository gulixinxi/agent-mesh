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
    [int]$P2PPort = 6001
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($ExePath)) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    $candidate = Join-Path $repoRoot "bin\client.exe"
    if (-not (Test-Path $candidate)) {
        throw "找不到 client.exe，请先用 -ExePath 指定，或先编译到 bin\client.exe"
    }
    $ExePath = $candidate
}
if (-not (Test-Path $ExePath)) { throw "client.exe 不存在: $ExePath" }

if ([string]::IsNullOrWhiteSpace($ClientID)) { $ClientID = $env:COMPUTERNAME }

# 服务端是 HTTPS 时，必须信任它的自签 CA，否则 TLS 握手直接失败。
$tlsCA = ""
if ($ServerURL -match '^https://') {
    if ([string]::IsNullOrWhiteSpace($CaPath)) {
        throw "服务端为 HTTPS，请用 -CaPath 指定服务端生成的 ca.pem"
    }
    if (-not (Test-Path $CaPath)) { throw "CA 证书不存在: $CaPath" }
}

Write-Host "== 安装 Agent Mesh 客户端 ==" -ForegroundColor Cyan

$logDir    = Join-Path $DataDir "logs"
$downloads = Join-Path $DataDir "downloads"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
New-Item -ItemType Directory -Force -Path $downloads | Out-Null

Copy-Item -Path $ExePath -Destination (Join-Path $InstallDir "client.exe") -Force
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
Write-Host ""
Write-Host "节点 ID   : $ClientID"
Write-Host "上报目标  : $ServerURL"
Write-Host "日志文件  : $(Join-Path $logDir 'agent-mesh-client.log')"
Write-Host "P2P 端口  : $P2PPort（0 表示关闭 P2P）"
