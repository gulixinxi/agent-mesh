#Requires -RunAsAdministrator
<#
.SYNOPSIS
卸载 Agent Mesh 服务并清理安装目录。

.PARAMETER Role
server | client | all

.PARAMETER RemoveData
加上后会连数据目录（数据库、日志、下载文件）一并删除，默认保留。
#>
param(
    [ValidateSet("server", "client", "all")]
    [string]$Role = "all",
    [string]$InstallRoot = "C:\Program Files\AgentMesh",
    [string]$DataDir = "C:\ProgramData\AgentMesh",
    [switch]$RemoveData
)

$ErrorActionPreference = "Stop"

function Remove-AgentMeshService {
    param([string]$Name, [string]$Dir, [string]$ExeName)

    $exe = Join-Path $Dir $ExeName
    if (Test-Path $exe) {
        Write-Host "停止并移除服务 $Name ..."
        Push-Location $Dir
        try {
            & $exe uninstall
        } catch {
            Write-Host "  uninstall 返回异常（可能本就未安装），继续清理" -ForegroundColor Yellow
        } finally {
            Pop-Location
        }
    } else {
        Write-Host "未找到 $exe，跳过服务移除"
    }

    if (Test-Path $Dir) {
        Remove-Item -Path $Dir -Recurse -Force
        Write-Host "已删除 $Dir"
    }
}

if ($Role -eq "server" -or $Role -eq "all") {
    Remove-AgentMeshService -Name "AgentMeshServer" -Dir (Join-Path $InstallRoot "Server") -ExeName "server.exe"
    Get-NetFirewallRule -DisplayName "Agent Mesh Server*" -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule | Out-Null
    Write-Host "已清理防火墙规则"
}

if ($Role -eq "client" -or $Role -eq "all") {
    Remove-AgentMeshService -Name "AgentMeshClient" -Dir (Join-Path $InstallRoot "Client") -ExeName "client.exe"
}

if ($RemoveData) {
    if (Test-Path $DataDir) {
        Remove-Item -Path $DataDir -Recurse -Force
        Write-Host "已删除数据目录 $DataDir"
    }
} else {
    Write-Host "保留数据目录 $DataDir（数据库与日志），加 -RemoveData 可一并清除"
}

Write-Host "卸载完成" -ForegroundColor Green
