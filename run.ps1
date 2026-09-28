# =======================================================================
# Agent Mesh - 双端一键编译 + 本地联调运行脚本 (PowerShell)
# 用法：
#   .\run.ps1             编译并以背景方式拉起服务端，然后前台启动客户端
#   .\run.ps1 -BuildOnly  只编译，不启动
# =======================================================================
param(
    [switch]$BuildOnly,
    [string]$ServerBinName = "mesh-server.exe",
    [string]$ClientBinName = "mesh-client.exe"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Definition

# 便携 Go 工具链（本项目自带，不依赖系统 PATH）
$go = "D:\guli\tools\go\bin\go.exe"
if (-not (Test-Path $go)) { $go = "go" }

$env:GOPROXY = "https://goproxy.cn,direct"   # 国内模块镜像
$env:GOSUMDB = "off"
$env:GOFLAGS = "-mod=mod"
$env:GOCACHE = "D:\guli\tools\gocache"       # 绕开 AppData 下的权限限制
$env:GOLOG_LOG_LEVEL = "error"               # 压掉 libp2p mDNS 在 Windows 上的刷屏告警

Write-Host "[1/3] 编译服务端…" -ForegroundColor Cyan
Push-Location (Join-Path $root "agent-mesh-server")
& $go mod tidy
& $go build -o (Join-Path $root "bin\$ServerBinName") .
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "服务端编译失败" }
Pop-Location

Write-Host "[2/3] 编译客户端…" -ForegroundColor Cyan
Push-Location (Join-Path $root "agent-mesh-client")
& $go mod tidy
& $go build -o (Join-Path $root "bin\$ClientBinName") .
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "客户端编译失败" }
Pop-Location

Write-Host "[OK] 双端编译完成 -> $root\bin" -ForegroundColor Green
if ($BuildOnly) { exit 0 }

Write-Host "[3/3] 拉起服务端（背景），随后前台启动客户端…" -ForegroundColor Cyan
$srv = Join-Path $root "bin\$ServerBinName"
$cli = Join-Path $root "bin\$ClientBinName"

$proc = Start-Process -FilePath $srv -WorkingDirectory $root -PassThru `
        -RedirectStandardOutput (Join-Path $root "server.log") `
        -RedirectStandardError  (Join-Path $root "server.err.log")
Write-Host "服务端已启动 PID=$($proc.Id)，日志: server.log / server.err.log" -ForegroundColor DarkGray
Start-Sleep -Seconds 2

try {
    & $cli -server http://127.0.0.1:8080 -p2p-port 6001
}
finally {
    if ($proc -and -not $proc.HasExited) {
        Write-Host "`n正在停止服务端 PID=$($proc.Id)…" -ForegroundColor DarkGray
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
    }
}
