#!/bin/sh
# 组装 neu-box-runtime 的 RPM：把三个二进制摆成 rootfs，打源码包、渲染 spec、
# 跑 rpmbuild。产物落在 dist/rpm/。
#
# 包里**没有** runtime.env：那份配置归 neu-box-config 生成和迁移，包只提供
# /etc/neu-box 目录。包自己写配置是"一份文件两个写者"的老毛病，见
# scripts/install.sh 开头。
#
#   bash deploy/rpm/build_rpm.sh                 # 编译 + 打包
#   bash deploy/rpm/build_rpm.sh --no-build      # 用 dist/ 里已有的二进制
#   bash deploy/rpm/build_rpm.sh --source-only   # 只出 tar.gz 和渲染后的 spec
#
# 版本号只有一处：仓库根的 VERSION。--version / --release 可以覆盖，平时不用。
#
# 编译在 rpmbuild 之前做完，spec 里只落文件 —— 和 worker 的 build_rpm.py 一个
# 分工。spec 自己不带工具链，也不该在打包中途去编译。
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RPM_DIR=$(cd "$(dirname "$0")" && pwd)
SPEC=$RPM_DIR/neu-box-runtime.spec
CONFIG=$ROOT/deploy/config/runtime.env.example

build=1
source_only=
release=1
version=
output_dir=$ROOT/dist/rpm

for arg in "$@"; do
    case $arg in
        --version=*) version=${arg#--version=} ;;
        --release=*) release=${arg#--release=} ;;
        --output-dir=*) output_dir=${arg#--output-dir=} ;;
        --no-build)    build= ;;
        --source-only) source_only=1 ;;
        -h|--help)
            cat <<'EOF'
用法: bash deploy/rpm/build_rpm.sh [选项]

  --no-build            用 dist/ 里已有的二进制，不编译
  --source-only         只出源码包和渲染后的 spec，不跑 rpmbuild
  --version=<版本>       覆盖 VERSION 文件（默认读它）
  --release=<发布号>     默认 1
  --output-dir=<目录>    产物目录，默认 dist/rpm
EOF
            exit 0 ;;
        *) echo "未知参数: $arg（--help 看用法）" >&2; exit 2 ;;
    esac
done

die() { echo "build_rpm: $*" >&2; exit 1; }

# ── 0. 版本号与架构 ─────────────────────────────────────────────────────
if [ -z "$version" ]; then
    [ -f "$ROOT/VERSION" ] || die "没有 $ROOT/VERSION"
    version=$(tr -d '[:space:]' < "$ROOT/VERSION")
fi
[ -n "$version" ] || die "版本号是空的"

# RPM 的 Version 不许有 '-'（它会变成 Version-Release 的分隔符），也不该带路径
# 分隔符 —— 这个值要进文件名。
for field in "版本号:$version" "发布号:$release"; do
    label=${field%%:*}; value=${field#*:}
    case $value in
        ''|[!A-Za-z0-9]*|*[!A-Za-z0-9._+~]*) die "$label 只能是字母数字和 . _ + ~，且不以符号开头：$value" ;;
    esac
done

arch=$(uname -m)
case $arch in
    x86_64|aarch64) ;;
    *) die "不支持的构建架构：$arch（spec 的 ExclusiveArch 只列了 x86_64 aarch64）" ;;
esac
command -v rpmbuild >/dev/null || die "找不到 rpmbuild"

name=neu-box-runtime-$version
echo "打包 $name-$release.$arch（输出 $output_dir）"

# ── 1. 编译 ─────────────────────────────────────────────────────────────
if [ -n "$build" ]; then
    command -v go >/dev/null || die "找不到 go；或加 --no-build 用 dist/ 里已有的二进制"
    mkdir -p "$ROOT/dist"
    # 版本号链接期注入 neu-box-config：迁移的报错要靠它说清"这份二进制期待哪个
    # schema"。wrapper 和 hook 不加 —— wrapper 的 argv 是 runc 的，加 --version
    # 会撞。
    for cmd in runtime hook config; do
        (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
            -ldflags "-s -w -X main.version=$version" \
            -o "dist/neu-box-$cmd" "./cmd/neu-$cmd")
    done
fi

# ── 2. 摆 rootfs ────────────────────────────────────────────────────────
# rootfs/ 下面就是要装到目标机的样子，spec 的 %install 只是把它摊到 buildroot。
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
rootfs=$tmp/source/$name/rootfs
mkdir -p "$rootfs/usr/local/bin" "$rootfs/etc/neu-box"

for cmd in runtime hook config; do
    src=$ROOT/dist/neu-box-$cmd
    [ -f "$src" ] || die "没有 $src（先编译，或去掉 --no-build）"
    install -m 0755 "$src" "$rootfs/usr/local/bin/neu-box-$cmd"
done

# 部署机只有 RPM 时也需要配置 Docker default-runtime 的入口。
mkdir -p "$rootfs/usr/libexec/neu-box/runtime/scripts"
install -m 0755 "$ROOT/scripts/install.sh" \
    "$rootfs/usr/libexec/neu-box/runtime/scripts/install.sh"
install -m 0755 "$ROOT/scripts/uninstall.sh" \
    "$rootfs/usr/libexec/neu-box/runtime/scripts/uninstall.sh"

# 仓库里那份 runtime.env.example 只作文档，不进包 —— 包里的配置由
# neu-box-config 在部署时生成（/etc/neu-box 这个目录还是要包的，包负责它的
# 属主和权限）。顺手挡住"有人把仓库路径写进模板"的回归。
if [ -f "$CONFIG" ] && grep -q "$ROOT" "$CONFIG"; then
    die "$CONFIG 里出现了仓库路径（$ROOT）；模板里的路径必须是安装后的路径"
fi

# 二进制得是给这台机器编的：换了 GOARCH 而没换架构，包能打出来但装上去跑不了。
for cmd in runtime hook config; do
    bin=$rootfs/usr/local/bin/neu-box-$cmd
    magic=$(head -c 4 "$bin" | od -An -tx1 | tr -d ' \n')
    [ "$magic" = 7f454c46 ] || die "$bin 不是 ELF 文件"
    if command -v readelf >/dev/null; then
        case $arch in
            x86_64)  want='Advanced Micro Devices X86-64' ;;
            aarch64) want='AArch64' ;;
        esac
        machine=$(readelf -h "$bin" | sed -n 's/^ *Machine: *//p')
        [ "$machine" = "$want" ] || die "$bin 的 ELF Machine 是 '$machine'，期望 '$want'"
    fi
done

# ── 3. 源码包 ───────────────────────────────────────────────────────────
# 时间戳和属主都钉死，同样的输入打出来的 tar.gz 逐字节相同。
mkdir -p "$tmp/SOURCES" "$tmp/SPECS" "$tmp/RPMS"
for dir in BUILD BUILDROOT SRPMS TMP; do mkdir -p "$tmp/$dir"; done
archive=$tmp/SOURCES/$name-$release.tar.gz
(cd "$tmp/source" && tar --sort=name --mtime=@0 --owner=0 --group=0 \
    --numeric-owner -cf - "$name" | gzip -n > "$archive")

# ── 4. 渲染 spec ────────────────────────────────────────────────────────
# 把开头的两个可覆盖默认值换成实参，渲染后的 spec 不依赖命令行 --define 也能自己
# 跑起来（worker 的 build_rpm.py 做的事一样）。
sed -e "s|^%{!?neu_box_version:%global neu_box_version 0.0.0}$|%global neu_box_version $version|" \
    -e "s|^%{!?neu_box_release:%global neu_box_release 1}$|%global neu_box_release $release|" \
    "$SPEC" > "$tmp/SPECS/neu-box-runtime.spec"
grep -q "^%global neu_box_version $version\$" "$tmp/SPECS/neu-box-runtime.spec" \
    || die "渲染 spec 失败：没换上版本号（$SPEC 开头的可覆盖默认值改动过？）"
if grep -q '^%{!?neu_box_version' "$tmp/SPECS/neu-box-runtime.spec"; then
    die "渲染 spec 失败：可覆盖默认值还在"
fi

mkdir -p "$output_dir"

if [ -n "$source_only" ]; then
    install -m 0644 "$archive" "$output_dir/$name-$release.tar.gz"
    install -m 0644 "$tmp/SPECS/neu-box-runtime.spec" \
        "$output_dir/$name-$release.spec"
    echo "$output_dir/$name-$release.tar.gz"
    echo "$output_dir/$name-$release.spec"
    exit 0
fi

# ── 5. rpmbuild ─────────────────────────────────────────────────────────
rpmbuild -bb \
    --define "_topdir $tmp" \
    --define "_tmppath $tmp/TMP" \
    --define "neu_box_version $version" \
    --define "neu_box_release $release" \
    "$tmp/SPECS/neu-box-runtime.spec"

packages=$(find "$tmp/RPMS" -name '*.rpm' | sort)
[ -n "$packages" ] || die "rpmbuild 跑完了却没产出 RPM"
for package in $packages; do
    install -m 0644 "$package" "$output_dir/$(basename "$package")"
    echo "$output_dir/$(basename "$package")"
done
