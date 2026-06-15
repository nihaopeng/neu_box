// 设备访问控制 - eBPF CGROUP_DEVICE
// 支持 major:minor 精确匹配 和 major:* 通配
// 完全自包含，不依赖任何系统头文件
//
// 编译: clang -O2 -g -target bpf -c device_block.bpf.c -o device_block.o

// ── 基础类型 ──────────────────────────────────────────────────
typedef unsigned char      __u8;
typedef unsigned short     __u16;
typedef unsigned int       __u32;
typedef unsigned long long __u64;

// ── BPF 杂项 ──────────────────────────────────────────────────
#define SEC(name)  __attribute__((section(name), used))

// BPF helper 函数 ID
static void *(*bpf_map_lookup_elem)(void *map, const void *key) =
    (void *) 1;
static long (*bpf_map_update_elem)(void *map, const void *key,
                                   const void *value, __u64 flags) =
    (void *) 2;

// 地图定义辅助宏
#define __uint(name, val)  int (*name)[val]
#define __type(name, val)  typeof(val) *name

// 地图类型 (来自 include/uapi/linux/bpf.h)
#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2

// ── cgroup device ctx 与常量 ──────────────────────────────────

// struct bpf_cgroup_dev_ctx (来自 include/uapi/linux/bpf.h)
// access_type = (BPF_DEVCG_ACC_* << 16) | BPF_DEVCG_DEV_*
struct bpf_cgroup_dev_ctx {
    __u32 access_type;
    __u32 major;
    __u32 minor;
};

#define BPF_DEVCG_DEV_BLOCK 1
#define BPF_DEVCG_DEV_CHAR  2

// ── 设备号 key ─────────────────────────────────────────────────

struct dev_key {
    __u32 major;
    __u32 minor;
};

char LICENSE[] SEC("license") = "GPL";

// map 1: 精确 major:minor 黑名单 (e.g. 195:1)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, struct dev_key);
    __type(value, __u8);
} blocked_devices SEC(".maps");

// map 2: major 通配黑名单 (e.g. 195:* → 该 major 下所有 minor 都被拦)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, __u32);
    __type(value, __u8);
} blocked_majors SEC(".maps");

// ── BPF 程序入口 ───────────────────────────────────────────────

SEC("cgroup/dev")
int device_block(struct bpf_cgroup_dev_ctx *ctx) {
    // 只拦截字符设备，块设备直接放行
    __u16 dev_type = ctx->access_type & 0xFFFF;
    if (dev_type != BPF_DEVCG_DEV_CHAR)
        return 1;  // 1 = 允许

    // 1) 精确匹配 major:minor
    struct dev_key key = { .major = ctx->major, .minor = ctx->minor };
    __u8 *blocked = bpf_map_lookup_elem(&blocked_devices, &key);
    if (blocked && *blocked)
        return 0;  // 0 = 拒绝

    // 2) 通配匹配 major:*
    __u32 maj = ctx->major;
    __u8 *maj_blocked = bpf_map_lookup_elem(&blocked_majors, &maj);
    if (maj_blocked && *maj_blocked)
        return 0;

    return 1;  // 放行
}
