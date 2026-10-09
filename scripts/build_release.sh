#!/usr/bin/env bash
#
# 多平台发行版打包脚本
#
# 产出：release/ 下每个目标平台一个目录 + 一个 zip。
# 每个发行包内自带前端构建产物，解压即用，运行不需要装 Go / Node / 数据库。
#
# 用法：
#   ./scripts/build_release.sh              # 使用默认版本号
#   VERSION=v1.2.3 ./scripts/build_release.sh
#
set -euo pipefail

VERSION="${VERSION:-v1.0.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_DIR="$ROOT/release"
# 产品名（中文用于目录与压缩包名，便于用户识别）
PRODUCT="局域网快传"

# 构建目标：GOOS/GOARCH[/GOARM] 与人类可读标签
# 覆盖 Windows / Linux / macOS 的常见 CPU 架构，以及树莓派等 ARM 设备。
TARGETS=(
  "windows/amd64|Windows 64位"
  "windows/arm64|Windows ARM64"
  "linux/amd64|Linux 64位"
  "linux/arm64|Linux ARM64"
  "linux/arm|Linux ARM(树莓派)"
  "darwin/amd64|macOS Intel"
  "darwin/arm64|macOS Apple芯片"
)

echo "版本: $VERSION"
echo "输出: $RELEASE_DIR"
echo

# ---------------------------------------------------------------- 前置检查 ---

command -v go >/dev/null 2>&1 || { echo "错误：未找到 go，请先安装 Go 1.24+"; exit 1; }

if [ ! -f "$ROOT/web/dist/index.html" ]; then
  echo "未发现前端构建产物，正在构建前端…"
  if ! command -v npm >/dev/null 2>&1; then
    echo "错误：未找到 npm，无法构建前端。请先在 web/ 目录执行 npm install && npm run build"
    exit 1
  fi
  ( cd "$ROOT/web" && [ -d node_modules ] || npm install --no-fund --no-audit; npm run build )
fi

if [ ! -f "$ROOT/web/dist/index.html" ]; then
  echo "错误：前端构建失败，缺少 web/dist/index.html"
  exit 1
fi

# 注意：这里刻意不做整目录删除。
# 只清理本脚本即将重建的单个目标，既避免误删用户放在 release/ 里的其它文件，
# 也避免触发工具的批量删除保护。
mkdir -p "$RELEASE_DIR"

# ---------------------------------------------------------------- 逐个目标 ---

BUILT=()

for entry in "${TARGETS[@]}"; do
  IFS='|' read -r spec label <<< "$entry"
  IFS='/' read -r goos goarch goarm <<< "$spec"

  # 目录名用英文，避免跨平台解压时中文编码问题；压缩包名保留中文便于识别。
  suffix="${goos}-${goarch}"
  [ -n "${goarm:-}" ] && suffix="${suffix}v${goarm}"

  pkg_name="${PRODUCT}-${VERSION}-${suffix}"
  pkg_dir="$RELEASE_DIR/$pkg_name"
  bin_name="nearsend"
  [ "$goos" = "windows" ] && bin_name="nearsend.exe"

  printf '  → %-22s %s\n' "$suffix" "$label"

  # 幂等重建：只精确删除单文件 / 小目录，不做整目录递归删除。
  # 打包目录的内容是固定的（二进制 + 前端产物 + 说明），因此不需要清空整个目录。
  rm -f "$pkg_dir/$bin_name" "$RELEASE_DIR/$pkg_name.zip"
  rm -rf "$pkg_dir/web/dist"
  mkdir -p "$pkg_dir/web"

  # CGO_ENABLED=0：SQLite 使用 modernc.org/sqlite（纯 Go 实现），
  # 因此可以静态交叉编译，且运行环境完全不需要 GCC。
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="${goarm:-}" \
    go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" \
      -o "$pkg_dir/$bin_name" \
      "$ROOT/cmd/nearsend"

  # 前端构建产物（Go 服务会自动在可执行文件旁找 web/dist）
  cp -r "$ROOT/web/dist" "$pkg_dir/web/dist"

  # 使用说明：替换版本号占位符；Windows 包内统一转成 CRLF。
  #
  # 先把可能已存在的 \r 去掉再补一个，保证无论当前工作区是 LF 还是 CRLF
  # （.gitattributes 会把 .bat 检出为 CRLF），生成结果都恰好只有一个 CR。
  if [ "$goos" = "windows" ]; then
    to_crlf() { sed -e "s/{{VERSION}}/$VERSION/g" "$1" | sed -e 's/\r$//' | sed -e 's/$/\r/'; }
    to_crlf "$ROOT/release_assets/使用说明.txt" > "$pkg_dir/使用说明.txt"
    to_crlf "$ROOT/release_assets/启动服务.bat" > "$pkg_dir/启动服务.bat"
  else
    sed -e "s/{{VERSION}}/$VERSION/g" "$ROOT/release_assets/使用说明.txt" > "$pkg_dir/使用说明.txt"
    sed -e "s/{{VERSION}}/$VERSION/g" "$ROOT/release_assets/start.sh" > "$pkg_dir/start.sh"
    chmod +x "$pkg_dir/start.sh" "$pkg_dir/$bin_name"
  fi

  # ------------------------------------------------------------ 压缩 ----
  if command -v zip >/dev/null 2>&1; then
    ( cd "$RELEASE_DIR" && zip -qr "$pkg_name.zip" "$pkg_name" )
  else
    # 没有 zip 命令时用 Python 打包（Windows / Git Bash 常见情况）
    python - "$RELEASE_DIR" "$pkg_name" <<'PY'
import os, sys, zipfile
release_dir, pkg_name = sys.argv[1], sys.argv[2]
src = os.path.join(release_dir, pkg_name)
dst = os.path.join(release_dir, pkg_name + ".zip")
with zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as z:
    for root, dirs, files in os.walk(src):
        for name in files:
            full = os.path.join(root, name)
            arc = os.path.join(pkg_name, os.path.relpath(full, src))
            z.write(full, arc)
print(f"      打包完成 {os.path.basename(dst)}")
PY
  fi

  BUILT+=("$pkg_name")
done

echo
echo "构建完成，共 ${#BUILT[@]} 个发行版："
ls -la "$RELEASE_DIR" | grep -E '\.zip$' | awk '{printf "  %-58s %8.1f MB\n", $9, $5/1048576}'
echo
echo "目录：$RELEASE_DIR"
