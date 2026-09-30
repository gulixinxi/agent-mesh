#Requires -RunAsAdministrator
<#
.SYNOPSIS
卸载 Agent Mesh 服务并清理安装目录。

.DESCRIPTION
按服务注册项定位旧 installation 位置再清理，**不依赖 exe 是否还在**。

这一点是修出来的：旧实现靠调用 `$InstallRoot\Server\server.exe uninstall` 卸载，
只要安装目录变过（或文件已被清理），脚本就报 "未找到 xxx，跳过服务移除"，
而服务还在注册表里跑着 —— 卸载看似完成，重启后旧版本照常起来。

现在改为：
  1. 从 SCM 读服务登记的可执行文件路径（它才是权威来源）
  2. 停止服务并等到进程真正退出
  3. sc delete 注销注册项，并轮询到它彻底消失
  4. 清理服务之外的自启动残留（计划任务 / 注册表 Run / 启动文件夹）
  5. 删除服务登记路径所在目录与默认安装目录

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

# 停服并等到进程真正退出。只看 Stop-Service 的返回不算数：
# 它在"接受停止指令"后就返回了，此时进程可能还在释放句柄。
function Stop-MeshService {
    param([string]$Name, [int]$TimeoutSec = 25)
    $svc = Get-MeshService $Name
    if (-not $svc -or $svc.State -eq 'Stopped') { return }
    Write-Host "  停止服务 $Name（PID $($svc.ProcessId)）..." -ForegroundColor DarkYellow
    Stop-Service -Name $Name -Force -ErrorAction SilentlyContinue
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 400
        $cur = Get-MeshService $Name
        if (-not $cur -or $cur.State -eq 'Stopped' -or $cur.ProcessId -eq 0) { return }
    }
    $cur = Get-MeshService $Name
    if ($cur -and $cur.ProcessId -gt 0) {
        Write-Host "  停止超时（${TimeoutSec}s），强制结束 PID $($cur.ProcessId)" -ForegroundColor Yellow
        Stop-Process -Id $cur.ProcessId -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }
}

# 注销注册项并轮询到它彻底消失。
# sc delete 返回成功不等于注销完成：句柄没放干净时它只是被标记删除，
# 那种状态下重装会撞上 1078（服务已存在）。
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

# 清理服务名之外的自启动途径。
# 服务唯一能保证"同名不会有两个"，管不到别人另投的启动方式；
# 这些残留正是"卸完看着干净、重启后又冒出来"的来源。
function Remove-LegacyAutostart {
    param([string[]]$MatchNames)
    $removed = 0
    foreach ($task in (Get-ScheduledTask -ErrorAction SilentlyContinue)) {
        foreach ($act in $task.Actions) {
            foreach ($n in $MatchNames) {
                if ($act.Execute -and $act.Execute -match $n) {
                    Write-Host "  移除计划任务：$($task.TaskName)" -ForegroundColor DarkYellow
                    Unregister-ScheduledTask -TaskName $task.TaskName -Confirm:$false -ErrorAction SilentlyContinue
                    $removed++
                    break
                }
            }
        }
    }
    foreach ($key in @("HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run",
                       "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\RunOnce",
                       "HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run")) {
        if (-not (Test-Path $key)) { continue }
        $props = Get-ItemProperty $key -ErrorAction SilentlyContinue
        foreach ($prop in $props.PSObject.Properties) {
            if ($prop.Name -match '^PS') { continue }
            foreach ($n in $MatchNames) {
                if ($prop.Value -match $n) {
                    Write-Host "  移除开机自启（$key）：$($prop.Name)" -ForegroundColor DarkYellow
                    Remove-ItemProperty -Path $key -Name $prop.Name -ErrorAction SilentlyContinue
                    $removed++
                    break
                }
            }
        }
    }
    foreach ($dir in @("$env:ProgramData\Microsoft\Windows\Start Menu\Programs\Startup",
                       "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup")) {
        if (-not (Test-Path $dir)) { continue }
        $sh = New-Object -ComObject WScript.Shell
        foreach ($f in (Get-ChildItem $dir -Filter "*.lnk" -ErrorAction SilentlyContinue)) {
            $target = $sh.CreateShortcut($f.FullName).TargetPath
            foreach ($n in $MatchNames) {
                if ($target -and $target -match $n) {
                    Write-Host "  移除启动项快捷方式：$($f.Name)" -ForegroundColor DarkYellow
                    Remove-Item $f.FullName -Force -ErrorAction SilentlyContinue
                    $removed++
                    break
                }
            }
        }
    }
    return $removed
}

# Remove-Role 卸载单个角色。
#
# 目录来源有两条：服务登记的路径（权威，能覆盖装到别处的旧版），
# 以及默认安装路径（服务没注册时的兜底）。
function Remove-Role {
    param([string]$Name, [string]$DefaultDir)

    Write-Host "-- $Name --" -ForegroundColor Cyan
    $svc = Get-MeshService $Name
    $dirs = @()

    if ($svc) {
        $exe = Get-RegisteredExePath $svc.PathName
        if ($exe) {
            Write-Host "  服务登记路径：$exe"
            $dirs += (Split-Path -Parent $exe)
        }
        Stop-MeshService -Name $Name
        if (Remove-MeshServiceRegistration -Name $Name) {
            Write-Host "  已注销服务注册项" -ForegroundColor Green
        } else {
            Write-Host "  [警告] 服务注册项未消失，重启前重装会失败；可再执行一次本脚本" -ForegroundColor Red
        }
    } else {
        Write-Host "  服务未注册（或已注销）"
    }

    $dirs += $DefaultDir
    foreach ($dir in ($dirs | Where-Object { $_ } | Select-Object -Unique)) {
        if (Test-Path $dir) {
            Remove-Item -Path $dir -Recurse -Force -ErrorAction SilentlyContinue
            if (Test-Path $dir) {
                Write-Host "  [警告] $dir 未能完全删除（可能仍有进程占用）" -ForegroundColor Yellow
            } else {
                Write-Host "  已删除 $dir"
            }
        }
    }
}

if ($Role -eq "server" -or $Role -eq "all") {
    Remove-Role -Name "AgentMeshServer" -DefaultDir (Join-Path $InstallRoot "Server")
    Get-NetFirewallRule -DisplayName "Agent Mesh Server*" -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule | Out-Null
    Write-Host "已清理防火墙规则"
}

if ($Role -eq "client" -or $Role -eq "all") {
    Remove-Role -Name "AgentMeshClient" -DefaultDir (Join-Path $InstallRoot "Client")
}

$leftovers = Remove-LegacyAutostart -MatchNames @('AgentMesh', 'agent-mesh')
if ($leftovers -eq 0) { Write-Host "未发现服务之外的自启动残留" -ForegroundColor DarkGray }

if ($RemoveData) {
    if (Test-Path $DataDir) {
        Remove-Item $DataDir -Recurse -Force -ErrorAction SilentlyContinue
        Write-Host "已删除数据目录 $DataDir"
    }
} else {
    Write-Host "保留数据目录 $DataDir（数据库与日志），加 -RemoveData 可一并清除"
}

Write-Host ""
Write-Host "复查是否还有残留：" -ForegroundColor Cyan
Write-Host "  Get-CimInstance Win32_Service -Filter ""Name='AgentMeshServer' or Name='AgentMeshClient'"""
Write-Host ""
Write-Host "卸载完成" -ForegroundColor Green
