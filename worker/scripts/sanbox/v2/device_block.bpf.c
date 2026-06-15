// 设备访问控制 - eBPF CGROUP_DEVICE
// 支持 major:minor 精确匹配 和 major:* 通配
// 自包含版本，仅依赖系统 /usr/include/linux/bpf.h
//
// 编译: clang -O2 -g -target bpf -c device_block.bpf.c -o device_block.o

#include <linux/bpf.h>

// ── libbpf 内联宏 ─────────────────────────────────────────────

#define SEC(name)  __attribute__((section(name), used))

static void *(*bpf_map_lookup_elem)(void *map, const void *key) =
    (void *) 1;
static long (*bpf_map_update_elem)(void *map, const void *key,
                                   const void *value, __u64 flags) =
    (void *) 2;

#define __uint(name, val)  int (*name)[val]
#define __type(name, val)  typeof(val) *name

// ── 设备号 key ─────────────────────────────────────────────────

struct dev_key {
    __u32 major;
    __u32 minor;
};

char LICENSE[] SEC("license") = "GPL";

// map 1: 精确 major:minor 黑名单 (e.g. 235:0)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, struct dev_key);
    __type(value, __u8);
} blocked_devices SEC(".maps");

// map 2: major 通配黑名单 (e.g. 235:* → 该 major 下所有 minor 都被拦)
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
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
