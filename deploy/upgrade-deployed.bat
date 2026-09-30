@echo off
chcp 936 >nul
cd /d "%~dp0"

REM --- Self-elevate: relaunch with UAC prompt if not already admin ---
net session >nul 2>&1
if errorlevel 1 (
  echo Requesting administrator rights. Click Yes on the UAC prompt.
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)

echo Admin confirmed. Upgrading AgentMesh server + client pack...
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0upgrade-deployed.ps1"

echo.
echo Done. Log file: %TEMP%\am_upgrade.txt
notepad "%TEMP%\am_upgrade.txt"
pause
