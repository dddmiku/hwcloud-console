@echo off
setlocal enabledelayedexpansion
chcp 65001 >nul

rem 本机会话保活：刷新 cookie 并同步到服务器。
rem 日常不需要手动跑——服务器上每 20 分钟有 systemd timer 自动做。
rem 只有服务器上的 refresh token 也过期时（约半年一次），才需要在本机执行这个。

set "ROOT=%~dp0.."
set "HWCLOUD_HOME=%ROOT%"
set "PLAYWRIGHT_DRIVER_PATH=C:\Users\<GITCODE_USER>\AppData\Local\ms-playwright-go\1.61.1"
set "BIN=%ROOT%\go\bin\hwcloud.exe"

echo === 1. 刷新本机会话 ===
"%BIN%" keepalive
if errorlevel 1 (
    echo [!] 保活失败，需要重新登录：
    echo     "%BIN%" login
    exit /b 1
)

echo.
echo === 2. 检查会话 ===
"%BIN%" session

echo.
echo === 3. 同步到服务器 ===
python "%~dp0sync_session.py"
if errorlevel 1 (
    echo [!] 同步失败
    exit /b 1
)

echo.
echo [+] 完成
