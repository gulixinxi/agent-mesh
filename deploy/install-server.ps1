#Requires -RunAsAdministrator
<#
.SYNOPSIS
安装 Agent Mesh 中央服务端为 Windows 服务。

.DESCRIPTION
完成以下动作：
  1. 建安装目录与数据目录
  2. 复制 server.exe
  3. （可选）生成自签 TLS 证书
  4. 写 agent-mesh.json 配置（含随机生成的集群密钥与控制台口令）
  5. 防火墙放行监听端口
  6. 注册并启动 Windows 服务

未显式指定 -Secret / -ConsolePass 时会自动生成强随机值，安装完成后打印出来，
请务必保存——配置文件里虽然可读，但那是唯一一次以明文形式呈现给操作者的机会。
#>
param(
    [string]$Addr = ":8080",
    [string]$Secret = "",
    [string]$ConsoleUser = "admin",
    [string]$ConsolePass = "",
    [string]$InstallDir = "C:\Program Files\AgentMesh\Server",
    [string]$DataDir = "C:\ProgramData\AgentMesh",
    [string]$ExePath = "",
    [switch]$TLS
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

$logDir = Join-Path $DataDir "logs"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

Copy-Item -Path $ExePath -Destination (Join-Path $InstallDir "server.exe") -Force
Write-Host "[1/6] 已安装程序到 $InstallDir"

$certDir = Join-Path $DataDir "certs"
$tlsCert = ""
$tlsKey = ""
if ($TLS) {
    & (Join-Path $InstallDir "server.exe") gencert $certDir | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "证书生成失败" }
    $tlsCert = Join-Path $certDir "server.pem"
    $tlsKey  = Join-Path $certDir "server-key.pem"
    Write-Host "[2/6] 已生成自签 TLS 证书"
} else {
    Write-Host "[2/6] 跳过 TLS（未加 -TLS，链路为明文 HTTP）" -ForegroundColor Yellow
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
Write-Host "[3/6] 已写入配置 $configPath"

$port = Get-ListenPort $Addr
$ruleName = "Agent Mesh Server ($port)"
if (-not (Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Protocol TCP `
        -LocalPort $port -Action Allow -Profile Any | Out-Null
    Write-Host "[4/6] 已放行防火墙端口 $port"
} else {
    Write-Host "[4/6] 防火墙规则已存在，跳过"
}

Push-Location $InstallDir
try {
    & (Join-Path $InstallDir "server.exe") install
    if ($LASTEXITCODE -ne 0) { throw "服务注册失败" }
} finally {
    Pop-Location
}
Write-Host "[5/6] 服务已注册并启动"

Start-Sleep -Seconds 2
$scheme = if ($TLS) { "https" } else { "http" }
$probePort = Get-ListenPort $Addr

Write-Host "[6/6] 完成" -ForegroundColor Green
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
Write-Host "本机自测: $($scheme)://127.0.0.1:$probePort/console"
Write-Host "日志文件: $(Join-Path $logDir 'agent-mesh-server.log')"
