#!/bin/sh
# ============================================================================
#  局域网快传 · 启动脚本（macOS / Linux）
#
#  用法：
#      ./start.sh                 使用默认设置启动
#      ./start.sh -port 9000      换端口
#      ./start.sh -data-dir /srv/nearsend/data
#
#  首次使用如果提示没有执行权限，先执行：
#      chmod +x start.sh nearsend
# ============================================================================

cd "$(dirname "$0")" || exit 1

if [ ! -x "./nearsend" ]; then
  echo ""
  echo "  [错误] 当前目录下找不到可执行的 nearsend"
  echo "  请确认本文件与 nearsend 在同一个目录，并执行："
  echo "      chmod +x start.sh nearsend"
  echo ""
  exit 1
fi

echo ""
echo "  正在启动局域网快传，请稍候..."
echo "  ─────────────────────────────────────────────"
echo "  启动后会显示可访问的网址，手机请连接同一个 Wi-Fi 后扫码。"
echo "  按 Ctrl+C 可停止服务。"
echo ""

if [ "$#" -eq 0 ]; then
  exec ./nearsend -data-dir "./nearsend-data"
else
  exec ./nearsend "$@"
fi
