// 设备独占控制 - eBPF CGROUP_DEVICE
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
static __u64 (*bpf_get_current_cgroup_id)(void) =
    (void *) 80;

// 地图定义辅助宏
#define __uint(name, val)  int (*name)[val]
#define __type(name, val)  typeof(val) *name

// 地图类型 (来自 include/uapi/linux/bpf.h)
#define BPF_MAP_TYPE_HASH 1

// ── cgroup device ctx 与常量 ──────────────────────────────────

// struct bpf_cgroup_dev_ctx (来自 include/uapi/linux/bpf.h)
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

// 通配 minor 的哨兵值 (0xFFFFFFFF)
#define MINOR_WILDCARD  ((__u32)-1)

char LICENSE[] SEC("license") = "GPL";

// 设备预留表: key=(major,minor) → value=cgroup_id
// minor=MINOR_WILDCARD 表示该 major 下所有 minor 都被预留
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, struct dev_key);
    __type(value, __u64);
} reserved_devices SEC(".maps");

// 预留涉及的 major 集合: key=(cgroup_id, major) → value=1
// 限制粒度到 major 级别: 沙盒进程只在该 major 内被限制，其他 major 不受影响
struct cg_major_key {
    __u64 cgid;
    __u32 major;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, struct cg_major_key);
    __type(value, __u8);
} reserved_majors SEC(".maps");

// ── BPF 程序入口 ───────────────────────────────────────────────

SEC("cgroup/dev")
int device_reserve(struct bpf_cgroup_dev_ctx *ctx) {
    // 只拦截字符设备，块设备直接放行
    __u16 dev_type = ctx->access_type & 0xFFFF;
    if (dev_type != BPF_DEVCG_DEV_CHAR)
        return 1;  // 1 = 允许

    // nvidiactl (minor=255) 始终放行
    if (ctx->minor == 255)
        return 1;

    __u64 my_cgid = bpf_get_current_cgroup_id();

    // 1) 精确匹配 major:minor
    struct dev_key exact_key = { .major = ctx->major, .minor = ctx->minor };
    struct cg_major_key mk = { .cgid = my_cgid, .major = ctx->major };
    __u64 *owner = bpf_map_lookup_elem(&reserved_devices, &exact_key);
    __u8 *has_major = bpf_map_lookup_elem(&reserved_majors, &mk);

    if (owner) {
        if (*owner == my_cgid)
            return 1;   // 我预留的 → 放行
        else
            return 0;   // 别人预留的 → 拒绝
    }
    if (has_major && *has_major){
        return 0;   // 该 major 被预留了，但不是我预留的 → 拒绝
    }else{
        return 1;
    }

    return 1;  // 未预留的 major,所有其他设备，一定要放行！！
}
