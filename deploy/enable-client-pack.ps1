<#
.SYNOPSIS
  为服务端开启「客户端分发」能力：把编译好的客户端放进分发目录，并写入 client_pack 配置。

.DESCRIPTION
  背景：一键入网（邀请码 → 落地页 → 一行命令 → 自动自检）依赖服务端能把客户端二进制
  分发给客户机。服务端用配置项 client_pack 指定这个目录：

    · 配好   → 落地页显示可执行命令，客户机执行后自动下载客户端并 enroll
    · 没配   → /join/<码>/client 返回 503，落地页提示「客户端安装包尚未就绪」，
               客户机照命令执行必然失败

  本脚本做四件事：
    1. 建立（或复用）分发目录，从源目录拷入双平台客户端（按服务端约定的文件名）
    2. 在 agent-mesh.json 里写入 client_pack（保留其余字段，写入前自动备份）
    3. 重启 Windows 服务使配置生效
    4. 健康自检，打印结果

  必须先编译好客户端（在 agent-mesh-client 目录下）：
    $env:GOOS='windows'; $env:GOARCH='amd64'; go build -o ..\dist\client-windows-amd64.exe .
    $env:GOOS='linux';   $env:GOARCH='amd64'; go build -o ..\dist\client-linux-amd64 .

  必须以管理员身份运行：目标目录在 Program Files 下，且需要重启服务。

.PARAMETER ClientSource
  客户端二进制所在目录。默认取仓库的 dist\。

.PARAMETER PackDir
  分发目录（服务端 client_pack 指向它）。默认 C:\Program Files\AgentMesh\Pack。

.PARAMETER ConfigPath
  服务端配置文件路径。默认 C:\Program Files\AgentMesh\Server\agent-mesh.json。

.PARAMETER ServiceName
  服务端 Windows 服务名。默认 AgentMeshServer。

.PARAMETER PublicUrl
  可选。写入 public_url，固定邀请页/控制台对外声明的基址。
  注意：本机 IP 是 DHCP 动态分配时不要写死——IP 变了反而更麻烦；
  要固定就该同时把网卡设成静态 IP，或在路由器上做 DHCP 保留。

.PARAMETER SkipRestart
  只写文件不重启服务（下次服务自启动时生效）。

.EXAMPLE
  # 用仓库 dist\ 里的二进制，默认路径
  .\enable-client-pack.ps1

.EXAMPLE
  # 顺手固定对外基址（仅当 IP 不会变时）
  .\enable-client-pack.ps1 -PublicUrl "http://192.168.2.131:8099"
#>
[CmdletBinding()]
param(
    [string]$ClientSource = "",
    [string]$PackDir      = "C:\Program Files\AgentMesh\Pack",
    [string]$ConfigPath   = "C:\Program Files\AgentMesh\Server\agent-mesh.json",
    [string]$ServiceName  = "AgentMeshServer",
    [string]$PublicUrl    = "",
    [switch]$SkipRestart
)

$ErrorActionPreference = 'Stop'

function Say($msg, $color = 'Gray') { Write-Host $msg -ForegroundColor $color }
function Ok($msg)   { Say "[OK]   $msg"   'Green' }
function Warn($msg) { Say "[注意] $msg"   'Yellow' }
function Die($msg)  { Say "[失败] $msg"   'Red'; exit 1 }

# ---------- 0. 前置检查 ----------
Say "=== 开启客户端分发（client_pack） ===" 'Cyan'
Say ""

$isAdmin = ([Security.Principal.WindowsPrincipal] `
    [Security.Principal.WindowsIdentity]::GetCurrent()
).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Die "需要管理员权限。请右键「以管理员身份运行 PowerShell」后重试。"
}
Ok "管理员权限已确认"

if (-not (Test-Path -LiteralPath $ConfigPath)) {
    Die "找不到服务端配置：$ConfigPath`n请确认服务端已用 deploy\install-server.ps1 安装。"
}
Ok "配置文件存在：$ConfigPath"

# 源目录：默认取仓库 dist\
if ([string]::IsNullOrWhiteSpace($ClientSource)) {
    $ClientSource = Join-Path (Split-Path -Parent $PSScriptRoot) 'dist'
}
if (-not (Test-Path -LiteralPath $ClientSource)) {
    Die @"
找不到客户端源目录：$ClientSource
请先编译客户端：
  cd <仓库>\agent-mesh-client
  `$env:GOOS='windows'; `$env:GOARCH='amd64'; go build -o ..\dist\client-windows-amd64.exe .
  `$env:GOOS='linux';   `$env:GOARCH='amd64'; go build -o ..\dist\client-linux-amd64 .
"@
}

# 与服务端 findClientPack 的候选名保持一致（顺序即优先级）
$winNames   = @('client-windows-amd64.exe', 'mesh-client.exe', 'client.exe', 'agent-mesh-client.exe')
$linuxNames = @('client-linux-amd64', 'mesh-client-linux-amd64', 'agent-mesh-client', 'client')

function Find-Client {
    param([string[]]$Names, [string]$Dir)
    foreach ($n in $Names) {
        $p = Join-Path $Dir $n
        if (Test-Path -LiteralPath $p -PathType Leaf) { return $p }
    }
    return $null
}

$winSrc   = Find-Client -Names $winNames   -Dir $ClientSource
$linuxSrc = Find-Client -Names $linuxNames -Dir $ClientSource

if (-not $winSrc) {
    Die "在 $ClientSource 里找不到 Windows 客户端（期望 $($winNames[0])）。"
}
Ok "Windows 客户端：$winSrc"

if ($linuxSrc) {
    Ok "Linux 客户端：$linuxSrc"
} else {
    Warn "未找到 Linux 客户端，只分发 Windows。Linux 机器可稍后补（重跑本脚本即可）。"
}

# ---------- 1. 建立分发目录并拷入 ----------
if (-not (Test-Path -LiteralPath $PackDir)) {
    New-Item -ItemType Directory -Path $PackDir -Force | Out-Null
    Ok "已创建分发目录：$PackDir"
} else {
    Ok "分发目录已存在：$PackDir"
}

# 统一用服务端首选的规范文件名，避免「名字对不上」这类排查成本
$winDst = Join-Path $PackDir 'client-windows-amd64.exe'
Copy-Item -LiteralPath $winSrc -Destination $winDst -Force
Ok "已放入：client-windows-amd64.exe ($([math]::Round((Get-Item $winDst).Length / 1MB, 1)) MB)"

if ($linuxSrc) {
    $linuxDst = Join-Path $PackDir 'client-linux-amd64'
    Copy-Item -LiteralPath $linuxSrc -Destination $linuxDst -Force
    Ok "已放入：client-linux-amd64 ($([math]::Round((Get-Item $linuxDst).Length / 1MB, 1)) MB)"
}

# ---------- 2. 写入配置（备份 + 保留其余字段） ----------
$backup = "$ConfigPath.bak"
Copy-Item -LiteralPath $ConfigPath -Destination $backup -Force
Ok "配置已备份：$backup"

$raw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
$cfg = $raw | ConvertFrom-Json

function Set-Field($obj, [string]$name, $value) {
    if ($obj.PSObject.Properties.Name -contains $name) {
        $obj.$name = $value
    } else {
        $obj | Add-Member -NotePropertyName $name -NotePropertyValue $value
    }
}

Set-Field $cfg 'client_pack' $PackDir
Ok "配置项 client_pack = $PackDir"

if (-not [string]::IsNullOrWhiteSpace($PublicUrl)) {
    Set-Field $cfg 'public_url' $PublicUrl
    Ok "配置项 public_url  = $PublicUrl"
}

# 关键：Go 的 encoding/json 不认 BOM，必须写成「无 BOM 的 UTF-8」
$json = $cfg | ConvertTo-Json -Depth 8
[System.IO.File]::WriteAllText($ConfigPath, $json, (New-Object System.Text.UTF8Encoding($false)))
Ok "配置已写入（UTF-8 无 BOM）"

# ---------- 3. 重启服务 ----------
if ($SkipRestart) {
    Warn "已跳过重启服务：配置将在下次服务启动时生效。"
} else {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) {
        Warn "未找到服务 $ServiceName，跳过重启。若服务端是手工启动的，请自行重启该进程。"
    } else {
        Restart-Service -Name $ServiceName -Force
        Ok "已重启服务：$ServiceName"
    }
}

# ---------- 4. 健康自检 ----------
$port = ''
$addr = [string]$cfg.addr
if ($addr -match ':(\d+)\s*$') { $port = $Matches[1] }
if (-not $port) { $port = '8099' }

$healthy = $false
for ($i = 0; $i -lt 10; $i++) {
    Start-Sleep -Milliseconds 800
    try {
        # 直连回环：显式清空代理，避免被系统代理劫持成「假成功」
        $req = [System.Net.HttpWebRequest]::Create("http://127.0.0.1:$port/healthz")
        $req.Proxy = $null
        $req.Timeout = 4000
        $resp = $req.GetResponse()
        $body = (New-Object System.IO.StreamReader($resp.GetResponseStream())).ReadToEnd()
        if ($body -match 'ok') { $healthy = $true; break }
    } catch { }
}

Say ""
if ($healthy) {
    Ok "健康自检通过：http://127.0.0.1:$port/healthz 返回 ok"
} else {
    Warn "健康自检未通过（端口 $port 无响应）。请检查服务状态与日志："
    Warn "  C:\ProgramData\AgentMesh\logs\"
}

# ---------- 5. 收尾提示 ----------
$ips = @(Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' -and
                   $_.PrefixOrigin -ne 'WellKnown' } |
    Select-Object -ExpandProperty IPAddress)

Say ""
Say "=== 下一步 ===" 'Cyan'
Say "1) 打开控制台签发邀请码（用客户机可达的地址，不要用 127.0.0.1）："
foreach ($ip in $ips) { Say "     http://${ip}:$port/console" }
if ([string]::IsNullOrWhiteSpace($PublicUrl)) {
    Say "   提示：本机 IP 若为 DHCP 分配，建议在路由器上做 DHCP 保留或改静态 IP，"
    Say "         或用本脚本 -PublicUrl 参数固定对外基址。"
}
Say "2) 审核落地页：命令里应出现上面某个 IP，且不再显示「客户端未就绪」。"
Say "3) 把落地页链接发给客户，让他自己在浏览器打开并复制命令执行。"
Say ""
Ok "完成。"
