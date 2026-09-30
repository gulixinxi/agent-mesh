# 核心升级逻辑（需管理员）。由 upgrade-deployed.bat 提权后调用。
# 作用：备份 -> 停服务 -> 替换 server.exe 与分发包客户端 -> 启服务 -> 验证 -> 作废临时邀请码
$ErrorActionPreference = 'Continue'
$log = 'C:\Users\Administrator\AppData\Local\Temp\am_upgrade.txt'
function W($m) { Add-Content -Path $log -Value $m -Encoding UTF8 }
Remove-Item $log -Force -ErrorAction SilentlyContinue

$dist  = 'D:\guli\projects\agent-mesh\dist'
$srvDir = 'C:\Program Files\AgentMesh\Server'
$packDir = 'C:\Program Files\AgentMesh\Pack'
$base   = 'http://127.0.0.1:8099'
$db     = 'C:\ProgramData\AgentMesh\agent_mesh_center.db'
$py     = 'C:\Users\Administrator\.workbuddy\binaries\python\versions\3.13.12\python.exe'
$stamp  = Get-Date -Format 'yyyyMMdd-HHmmss'

W ("== 部署版升级 " + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss') + " ==")
$pr = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
$admin = $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
W ("身份: " + [Security.Principal.WindowsIdentity]::GetCurrent().Name + "   管理员: " + $admin)
if (-not $admin) { W "!! 非管理员，中止（请右键以管理员身份运行 bat）"; exit 2 }

# ---------- 1. 备份 ----------
foreach ($f in @("$srvDir\server.exe", "$packDir\client-windows-amd64.exe", "$packDir\client-linux-amd64")) {
    if (Test-Path $f) {
        try { Copy-Item $f "$f.bak-$stamp" -Force -ErrorAction Stop; W "[1] 备份 $f" }
        catch { W "[1] 备份失败 $f : $($_.Exception.Message)" }
    }
}

# ---------- 2. 停服务 ----------
try {
    Stop-Service AgentMeshServer -Force -ErrorAction Stop
    $dl = (Get-Date).AddSeconds(30)
    while ((Get-Service AgentMeshServer).Status -ne 'Stopped' -and (Get-Date) -lt $dl) { Start-Sleep -Milliseconds 400 }
    W ("[2] 服务状态: " + (Get-Service AgentMeshServer).Status)
    Start-Sleep -Seconds 1
} catch { W "[2] 停服务失败: $($_.Exception.Message)" }

# ---------- 3. 替换 ----------
$ok = $true
foreach ($pair in @(
    @{ s = "$dist\server.exe";                 d = "$srvDir\server.exe" },
    @{ s = "$dist\client-windows-amd64.exe";   d = "$packDir\client-windows-amd64.exe" },
    @{ s = "$dist\client-linux-amd64";         d = "$packDir\client-linux-amd64" }
)) {
    if (-not (Test-Path $pair.s)) { W "[3] 缺少源文件 $($pair.s)"; $ok = $false; continue }
    try {
        Copy-Item $pair.s $pair.d -Force -ErrorAction Stop
        $fi = Get-Item $pair.d
        W ("[3] 已替换 " + (Split-Path $pair.d -Leaf) + "  " + $fi.LastWriteTime.ToString('MM-dd HH:mm') + "  " + ("{0:N1} MB" -f ($fi.Length / 1MB)))
    } catch { W "[3] 替换失败 $($pair.d): $($_.Exception.Message)"; $ok = $false }
}
if (-not $ok) { W "!! 替换未全部成功，尝试恢复服务"; try { Start-Service AgentMeshServer } catch { }; exit 3 }

# ---------- 4. 启动 ----------
try {
    Start-Service AgentMeshServer
    Start-Sleep -Seconds 5
    W ("[4] 服务状态: " + (Get-Service AgentMeshServer).Status)
} catch { W "[4] 启动失败: $($_.Exception.Message)" }

# ---------- 5. 健康 ----------
try {
    $h = Invoke-RestMethod "$base/healthz" -TimeoutSec 10
    W ("[5] /healthz = " + ($h | ConvertTo-Json -Compress))
} catch { W "[5] /healthz 失败: $($_.Exception.Message)" }

# ---------- 6. audit_logs 是否已有 redacted 列（新版标志）----------
$pyFile = Join-Path $env:TEMP 'am_cols.py'
[IO.File]::WriteAllText($pyFile, "import sqlite3`nc=sqlite3.connect(r'$db')`nprint([r[1] for r in c.execute('PRAGMA table_info(audit_logs)')])`n", (New-Object Text.UTF8Encoding $false))
try {
    $cols = & $py $pyFile 2>&1
    W ("[6] audit_logs 列: " + ($cols -join ' '))
    W ("[6] 含 redacted: " + (($cols -join ' ') -match 'redacted'))
} catch { W "[6] 读表结构失败: $($_.Exception.Message)" }
Remove-Item $pyFile -Force -ErrorAction SilentlyContinue

# ---------- 7. 签发一张临时码验证命令形态，随后作废 ----------
try {
    $cfg = Get-Content "$srvDir\agent-mesh.json" -Raw | ConvertFrom-Json
    $enc = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("admin:" + $cfg.console_pass))
    $H = @{ Authorization = "Basic $enc" }
    $r = Invoke-RestMethod "$base/console/api/invites" -Method Post -Headers $H -ContentType 'application/json' `
         -Body (@{ label = '升级验证-可作废'; ttl_minutes = 1440; max_uses = 1 } | ConvertTo-Json) -TimeoutSec 10
    W ("[7] loopback 字段存在: " + ($r.PSObject.Properties.Name -contains 'loopback'))
    W ("[7] 入网命令: " + $r.windows_cmd)
    $norm = $r.code -replace '-', ''
    $ps1 = (Invoke-WebRequest "$base/join/$norm/install.ps1" -TimeoutSec 10 -UseBasicParsing).Content
    W ("[7] 脚本用 WebClient: " + ($ps1 -match 'WebClient') + "   无 Invoke-WebRequest: " + (-not ($ps1 -match 'Invoke-WebRequest\s')))
    $null = Invoke-RestMethod "$base/console/api/invites/revoke" -Method Post -Headers $H -ContentType 'application/json' `
            -Body (@{ selector = $norm } | ConvertTo-Json) -TimeoutSec 10
    W "[7] 临时邀请码已作废"
} catch { W "[7] 验证失败: $($_.Exception.Message)" }

W "== 完成 =="
