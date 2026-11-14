@echo off
setlocal
title Cancel and restart GitHub workflow runs
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0cancel_runs.ps1"
set "exitCode=%ERRORLEVEL%"
echo.
if not "%exitCode%"=="0" echo Script exited with code %exitCode%.
pause
exit /b %exitCode%
