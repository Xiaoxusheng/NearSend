#!/usr/bin/env bash
#
# 多平台发行版打包脚本
#
# 产出：release/ 下每个目标平台一个 zip（内含程序本体、前端产物、使用说明、
# 启动脚本与 LICENSE）。解压即用，运行不需要装 Go / Node / 数据库。
#
# 用法：
#   ./scripts/build_release.sh              # 使用默认版本号
#   VERSION=v1.2.3 ./scripts/build_release.sh
#
# 依赖：Go 1.24+、Python 3（打包 zip 并写入可执行权限位）、
#       已有 web/dist 或可用的 npm（会自动构建前端）。
#
# 设计说明（重要）：
#   每个发行包在「全新的临时目录」里组装，最后只把 zip 放进 release/。
#   因此每次运行都不需要对已有目录做递归删除 —— 既天然幂等，
#   也绝不会误删用户放在 release/ 里的其它文件。
#   唯一的删除动作是覆盖同名的旧 zip。暂存目录交给系统回收（路径会打印）。
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
# Python 3 用于打包 zip 并显式写入可执行权限位（Windows 上 chmod 无效）
command -v python >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1 || {
  echo "错误：未找到 python / python3，打包需要它来写入文件权限位"; exit 1; }

if [ ! -f "$ROOT/web/dist/index.html" ]; then
  echo "未发现前端构建产物，正在构建前端…"
  if ! command -v npm >/dev/null 2>&1; then
    echo "错误：未找到 npm，无法构建前端。请先在 web/ 目录执行 npm install && npm run build"
    exit 1
  fi
  (
    cd "$ROOT/web"
    if [ ! -d node_modules ]; then
      npm install --no-fund --no-audit
    fi
    npm run build
  )
fi

if [ ! -f "$ROOT/web/dist/index.html" ]; then
  echo "错误：前端构建失败，缺少 web/dist/index.html"
  exit 1
fi

mkdir -p "$RELEASE_DIR"

# LICENSE 是 MIT 协议要求随发行包提供的内容，缺失时提前告警
if [ ! -f "$ROOT/LICENSE" ]; then
  echo "警告：未找到 LICENSE 文件，发行包将不含许可证"
fi

# 全新的暂存目录：每次运行路径都不同，因此永远不需要清理已有内容。
WORK="$(mktemp -d 2>/dev/null || mktemp -d -t nearsend)"
echo "暂存目录: $WORK"
echo

# ---------------------------------------------------------------- 逐个目标 ---

BUILT=()

for entry in "${TARGETS[@]}"; do
  IFS='|' read -r spec label <<< "$entry"
  IFS='/' read -r goos goarch goarm <<< "$spec"

  # 目录名用英文，避免跨平台解压时中文编码问题；压缩包名保留中文便于识别。
  suffix="${goos}-${goarch}"
  [ -n "${goarm:-}" ] && suffix="${suffix}v${goarm}"

  pkg_name="${PRODUCT}-${VERSION}-${suffix}"
  pkg_dir="$WORK/$pkg_name"
  bin_name="nearsend"
  [ "$goos" = "windows" ] && bin_name="nearsend.exe"

  printf '  → %-22s %s\n' "$suffix" "$label"

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

  # MIT 协议要求分发二进制时一并附上许可证与版权声明
  cp "$ROOT/LICENSE" "$pkg_dir/LICENSE"

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
  #
  # 统一用 Python 打包，而不是优先用 zip 命令：Windows 的文件系统没有
  # 可执行位概念（chmod 在 NTFS 上无效），直接打包会让 Linux / macOS 用户
  # 解压后无法运行 ./nearsend 与 ./start.sh。这里显式写入权限位。
  #
  # 唯一的删除动作：覆盖同名的旧 zip（单文件）。
  rm -f "$RELEASE_DIR/$pkg_name.zip"
  PYTHON_BIN="$(command -v python || command -v python3)"
  "$PYTHON_BIN" - "$WORK" "$pkg_name" "$RELEASE_DIR" <<'PY'
import os, stat, sys, zipfile

work, pkg_name, release_dir = sys.argv[1], sys.argv[2], sys.argv[3]
src = os.path.join(work, pkg_name)
dst = os.path.join(release_dir, pkg_name + ".zip")

# 需要保留可执行权限的文件（按文件名判断，跨平台一致）
EXECUTABLES = {"nearsend", "start.sh"}

with zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as z:
    for root, dirs, files in os.walk(src):
        dirs.sort()
        for name in sorted(files):
            full = os.path.join(root, name)
            arc = os.path.join(pkg_name, os.path.relpath(full, src))
            mode = 0o755 if name in EXECUTABLES else 0o644
            info = zipfile.ZipInfo.from_file(full, arc)
            info.external_attr = (stat.S_IFREG | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            with open(full, "rb") as fh:
                z.writestr(info, fh.read())

print(f"      打包完成 {os.path.basename(dst)}")
PY

  BUILT+=("$pkg_name")
done

echo
echo "构建完成，共 ${#BUILT[@]} 个发行版："
ls -la "$RELEASE_DIR" | grep -E '\.zip$' | awk '{printf "  %-58s %8.1f MB\n", $9, $5/1048576}'
echo
echo "目录：$RELEASE_DIR"
echo "暂存目录（可手动删除）：$WORK"
