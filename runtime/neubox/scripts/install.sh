#!/bin/sh
# 安装/升级 neu-box-runtime（runc wrapper + OCI hook + 配置工具），并设为 Docker
# 默认 runtime。
#
# 三件事，顺序是硬的：
#
#   1. dnf 装二进制。包只落文件 —— 不碰 daemon.json，也不碰配置。
#   2. neu-box-config 生成/迁移 /etc/neu-box/runtime.env。配置归应用自己管：
#      包和这个脚本都不再手写它的内容，只把现场发现的事实（真 runc 的路径、
#      worker 的地址）交给它。
#   3. 改 daemon.json，把这台机器的 default-runtime 指过来。
#
# 为什么第 3 步必须最后：neu-box-runtime 是这台机器的 default-runtime，**所有**
# 容器（包括跟我们无关的业务容器）启动都要过它。指向一个跑不起来的二进制，
# dockerd 会起不了任何容器。所以先验证它真能注入，再改全局配置。
#
# runtimes / default-runtime 不支持热加载，改完必须重启 dockerd（会杀掉当时所有
# 运行中的容器）。本脚本不替你重启。
#
# 升级也走这个脚本，同一个入口：换二进制、迁移配置、确认 daemon.json，一次做完。
set -eu

BIN=/usr/local/bin
RUNTIME=$BIN/neu-box-runtime
HOOK=$BIN/neu-box-hook
CONFIG_BIN=$BIN/neu-box-config
CONF=/etc/neu-box/runtime.env
WORKER_CONF=/etc/neu-box/worker.env
DAEMON=/etc/docker/daemon.json
BAK=$DAEMON.neu-box-bak
DEFAULT_PORT=59075

ROOT=$(cd "$(dirname "$0")/.." && pwd)
rpm_arg=
no_pack=
force=
configure_only=
for arg in "$@"; do
    case $arg in
        --rpm=*)    rpm_arg=${arg#--rpm=} ;;
        --no-build) no_pack=1 ;;
        --force)    force=1 ;;
        --configure-only) configure_only=1 ;;
        -h|--help)
            cat <<'EOF'
用法: sudo bash scripts/install.sh [选项]

  --rpm=<文件>   装这个 RPM。默认用 dist/rpm/ 里最新的；找不到就现场打一个
  --no-build     不打包，只用 dist/rpm/ 里已有的 RPM
  --force        把 runtime.env 整份重写（默认只迁移/补键，手改过的值不动）
  --configure-only  二进制已由配套 RPM 安装；只生成配置并设置 Docker runtime

装完还得手工重启 dockerd（会杀掉当时所有运行中的容器）：
    docker ps && sudo systemctl restart docker
EOF
            exit 0 ;;
        *) echo "未知参数: $arg（--help 看用法）" >&2; exit 2 ;;
    esac
done

die() { echo "install: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "需要 root：sudo bash $0"

command -v jq >/dev/null || die "需要 jq（改 daemon.json 用）"
if [ -n "$configure_only" ]; then
    [ -z "$rpm_arg" ] || die "--configure-only 不能与 --rpm 同用"
else
    command -v dnf >/dev/null || die "需要 dnf 装二进制"
fi

# ── 1. 找一个 RPM（装或升级都走它） ──────────────────────────────────────
# 二进制归包管理器管：升级只替换 /usr/local/bin 下那几个文件，路径不变，也不会
# 和手工 install 的文件互相覆盖（那种覆盖会让 rpm -V 一直报校验和不符）。
if [ -z "$configure_only" ]; then
    if [ -z "$rpm_arg" ]; then
        if [ -z "$no_pack" ]; then
            echo "没指定 --rpm，现场打一个……"
            bash "$ROOT/deploy/rpm/build_rpm.sh"
        fi
        rpm_arg=$(ls -1t "$ROOT"/dist/rpm/neu-box-runtime-*.rpm 2>/dev/null | head -1 || true)
        [ -n "$rpm_arg" ] || die "dist/rpm/ 里没有 RPM（去掉 --no-build，或用 --rpm=<文件>）"
    fi
    [ -f "$rpm_arg" ] || die "RPM 不存在：$rpm_arg"
    echo "用 $rpm_arg"
    # 已经装了同一个版本时 dnf 会说"nothing to do"，那不是错误。
    dnf install -y "$rpm_arg" || die "dnf 装包失败"
fi

for bin in "$RUNTIME" "$HOOK" "$CONFIG_BIN"; do
    [ -x "$bin" ] || die "$bin 不在或不可执行（包没装成？）"
done

# ── 2. 把现场发现的事实交给 neu-box-config ───────────────────────────────
# 为什么发现动作留在这个脚本里、而不是让二进制自己去 LookPath：dockerd 的环境
# 未必有 /usr/local/bin 在 PATH 上，运行时找不到真 runc；这里是运维的 shell，
# PATH 是可信的。配置的 schema、版本、迁移归 neu-box-config，发现归脚本。
real_runc=${NEU_BOX_REAL_RUNC:-$(command -v runc || true)}
[ -n "$real_runc" ] || die "找不到 runc；或显式给 NEU_BOX_REAL_RUNC=/path/to/runc"

port=$(sed -n 's/^NEU_BOX_PORT=//p' "$WORKER_CONF" 2>/dev/null | tail -1 | tr -cd '0-9')
if [ -z "$port" ]; then
    echo "没从 $WORKER_CONF 读到 NEU_BOX_PORT，用 $DEFAULT_PORT"
    port=$DEFAULT_PORT
fi

# 不加 --force 就是"迁移 + 补键 + 修还等于内置默认值的键"，手改过的值不动。
set -- init --path "$CONF" --real-runc "$real_runc" \
    --worker-url "http://127.0.0.1:$port" --hook "$HOOK"
[ -z "$force" ] || set -- "$@" --force
"$CONFIG_BIN" "$@" || die "生成/迁移 $CONF 失败"

# ── 3. 验证 wrapper 真能注入 ─────────────────────────────────────────────
# 只跑 --help 不算验证 —— 那压根没碰注入逻辑。这里拿临时 bundle + 假 runc 走
# 一遍真路径：wrapper 读 config.json、注 hook、把 argv 转发给真 runtime。
# 验的是**要部署的那一份**，也就是 dnf 刚换上去的那个。
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT INT TERM
mkdir -p "$tmp/bundle"
printf '%s' '#!/bin/sh
printf "%s\n" "$*" > "$FAKE_RUNC_LOG"' > "$tmp/runc"
chmod +x "$tmp/runc"
# root 不参与注入，给个占位；关键是 annotations 里那条 sandbox_cgroup。
printf '%s' '{"ociVersion":"1.0.2","root":{"path":"rootfs"},"annotations":{"sandbox_cgroup":"sbx_probe.slice"}}' \
    > "$tmp/bundle/config.json"

FAKE_RUNC_LOG=$tmp/argv NEU_BOX_REAL_RUNC=$tmp/runc \
    "$RUNTIME" --root "$tmp/state" create --bundle "$tmp/bundle" probe \
    || die "wrapper 跑不通"
grep -q -- --bundle "$tmp/argv" || die "wrapper 没把 argv 转发给真 runtime"
jq -e '.hooks | length > 0' "$tmp/bundle/config.json" >/dev/null \
    || die "wrapper 没注入 hook（那是它存在的唯一理由）"

# 空 state 应当退非零（缺 sandbox_cgroup），且不能挂死。
if printf '{}' | timeout 5 "$HOOK" >/dev/null 2>&1; then
    die "hook 收到空 state 竟然退 0，登记逻辑不对"
fi
echo "验证通过：wrapper 会注入 hook，hook 缺参数时拒绝"

# ── 4. 配置指到的两个东西必须真在 ────────────────────────────────────────
# NEU_BOX_REAL_RUNC 指错 = wrapper 找不到真 runtime，**任何容器都起不来**，和
# default-runtime 指错一个后果。所以在改 daemon.json 之前拦下来。
#
# 取值宽容一点：引号、行尾注释、两边空白都当没写（和 config.go 的解析对齐）——
# 这个检查宁可漏判也不能误判，误判会拦住一次本来能用的部署。
for key in NEU_BOX_REAL_RUNC NEU_BOX_HOOK; do
    value=$(sed -n "s/^$key=[[:space:]]*//p" "$CONF" 2>/dev/null | tail -1 | tr -d '"' | tr -d "'")
    value=${value%%[[:space:]]*}
    [ -n "$value" ] || continue      # 没写就用二进制的内置默认值
    [ -x "$value" ] || die "$CONF 里 $key=$value 不存在或不可执行；改掉这一行，或加 --force 重写"
done

# ── 5. 注册成默认 runtime ────────────────────────────────────────────────
[ -f "$DAEMON" ] || { mkdir -p "$(dirname "$DAEMON")"; echo '{}' > "$DAEMON"; }
[ -f "$BAK" ] || cp -a "$DAEMON" "$BAK"
jq -e 'type == "object"' "$DAEMON" >/dev/null || die "$DAEMON 顶层不是 JSON 对象，拒绝改"
# 键和值都叫 neu-box-runtime：和二进制同名，也和 RPM 包同名。别退回短名
# "neu-box" —— 那个名字同时被运维命令和 OCI runtime 占用过，运维命令已经改名成
# neu-box-installer，这里再留短名就又分不清说的是谁了。
jq --arg bin "$RUNTIME" \
   '. + {"default-runtime":"neu-box-runtime"} | .runtimes["neu-box-runtime"] = {"path":$bin}' \
   "$DAEMON" > "$DAEMON.new" || die "改 $DAEMON 失败"
mv "$DAEMON.new" "$DAEMON"
echo "已设 default-runtime=neu-box-runtime → $RUNTIME（备份 $BAK）"

# ── 6. 还没生效 ──────────────────────────────────────────────────────────
cat <<EOF

还得手工做（脚本不替你重启 dockerd，重启会杀掉所有运行中的容器）：

    docker ps                                     # 先看哪些会被杀
    systemctl restart docker
    docker info --format '{{.DefaultRuntime}}'    # 应当输出 neu-box-runtime

看配置现状：$CONFIG_BIN show
回滚：     sudo bash "$ROOT/scripts/uninstall.sh"
EOF
