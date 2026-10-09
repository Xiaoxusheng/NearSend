@echo off
rem ===========================================================================
rem  局域网快传 · 启动脚本
rem
rem  双击运行即可。想换端口/换数据目录，可以这样改：
rem      set NEARSEND_ARGS=-port 9000 -data-dir D:\快传数据
rem  也可以直接用命令行传参：启动服务.bat -port 9000
rem ===========================================================================

chcp 65001 >nul 2>&1
title 局域网快传 {{VERSION}} 服务
cd /d "%~dp0"

if not exist "nearsend.exe" (
  echo.
  echo   [错误] 当前目录下找不到 nearsend.exe
  echo   请确认本文件与 nearsend.exe 在同一个文件夹里。
  echo.
  pause
  exit /b 1
)

echo.
echo   正在启动局域网快传，请稍候...
echo   ─────────────────────────────────────────────
echo   启动后会显示可访问的网址，手机请连接同一个 Wi-Fi 后扫码。
echo   本窗口不要关闭，关闭即停止服务。
echo.

set NEARSEND_ARGS=%*
if "%NEARSEND_ARGS%"=="" set NEARSEND_ARGS=-data-dir "%~dp0nearsend-data"

nearsend.exe %NEARSEND_ARGS%
set EXITCODE=%ERRORLEVEL%

echo.
echo   ─────────────────────────────────────────────
if not "%EXITCODE%"=="0" (
  echo   程序已退出，退出码 %EXITCODE%。
  echo   如果是端口被占用，请按上面提示的替代端口重试。
) else (
  echo   服务已停止。
)
echo.
pause
