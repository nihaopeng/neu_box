#!/bin/sh
# 卸载 neu-box-runtime，并把 daemon.json 还原。
#
# 顺序和 install.sh 相反，但同样是硬的：**先还原 daemon.json，再删二进制**。
# default-runtime 还指着 neu-box-runtime 的时候把二进制删掉，dockerd 会起不了任何容器。
#
# daemon.json 改完要重启 dockerd 才生效；本脚本不替你重启。
#
# 二进制是 RPM 装的话（deploy/rpm/），本脚本只还原 daemon.json，文件交给
# `sudo dnf remove neu-box-runtime`。包自己的 %preun 也认这条规矩：daemon.json 还
# 指着 neu-box-runtime 时拒绝卸载 —— 和上面那条顺序是同一件事，只是换了个方向守。
set -eu

BIN=/usr/local/bin
CONF=/etc/neu-box/runtime.env
DAEMON=/etc/docker/daemon.json
BAK=$DAEMON.neu-box-bak

purge=
for arg in "$@"; do
    case $arg in
        --purge) purge=1 ;;
        -h|--help)
            cat <<'EOF'
用法: sudo bash scripts/uninstall.sh [--purge]

  --purge  连 /etc/neu-box/runtime.env 一起删
EOF
            exit 0 ;;
        *) echo "未知参数: $arg（--help 看用法）" >&2; exit 2 ;;
    esac
done

die() { echo "uninstall: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "需要 root：sudo bash $0"

# ── 1. 还原 daemon.json（必须在删二进制之前）─────────────────────────────
if [ -f "$BAK" ]; then
    # 备份是安装时的快照，装完之后手工改过的内容会被一起抹掉。
    cp -a "$BAK" "$DAEMON"
    echo "已从 $BAK 还原"
elif [ -f "$DAEMON" ]; then
    # 装机时 daemon.json 本来不存在，没有快照可还，只能摘掉我们加的两个键。
    command -v jq >/dev/null || die "需要 jq 来摘键"
    jq -e 'type == "object"' "$DAEMON" >/dev/null || die "$DAEMON 顶层不是 JSON 对象，拒绝改"
    jq 'if .["default-runtime"] == "neu-box-runtime" then del(.["default-runtime"]) else . end
        | if (.runtimes | type) == "object" then del(.runtimes["neu-box-runtime"]) else . end
        | if .runtimes == {} then del(.runtimes) else . end' \
        "$DAEMON" > "$DAEMON.new" && mv "$DAEMON.new" "$DAEMON"
    echo "已摘掉 default-runtime / runtimes.neu-box-runtime"
else
    echo "没有 $DAEMON，跳过"
fi

# ── 2. 删二进制 ──────────────────────────────────────────────────────────
# RPM 装的交给包管理器删：脚本 rm 掉包里的文件，包数据库就过期了（rpm -V 会一直
# 报缺失）。daemon.json 上一步已经还原，这时候删是安全的，dnf remove 同理。
rpm_owned=
if command -v rpm >/dev/null 2>&1 && rpm -qf "$BIN/neu-box-runtime" >/dev/null 2>&1; then
    rpm_owned=1
else
    rm -f "$BIN/neu-box-runtime" "$BIN/neu-box-hook"
fi

# ── 3. 配置文件 ──────────────────────────────────────────────────────────
if [ -n "$purge" ]; then
    # 这份配置不归包管（是 neu-box-config 生成的），dnf remove 不会碰它，
    # --purge 就自己删。
    rm -f "$CONF"
    rmdir /etc/neu-box 2>/dev/null || true       # 空才删得掉
    echo "已删除 $CONF"
else
    echo "保留 $CONF（要一起删：sudo bash $0 --purge）"
fi

cat <<EOF

还得手工做（脚本不替你重启 dockerd，重启会杀掉所有运行中的容器）：

    docker ps
    systemctl restart docker
    docker info --format '{{.DefaultRuntime}}'   # 不应再是 neu-box-runtime
EOF

if [ -n "$rpm_owned" ]; then
    cat <<EOF

二进制还归 RPM 管，重启完之后再删包（反了 dockerd 会起不了容器）。runtime.env
不归包管：包不会删它，要清就 `sudo bash $0 --purge`：

    sudo dnf remove neu-box-runtime
EOF
fi
