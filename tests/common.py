"""测试共用模块 — HTTP 请求、断言、框架。"""

import json
import sys
import urllib.request
import urllib.error

MASTER = "http://202.199.13.164:25565"

_passed = 0
_failed = 0


# ═══════════════════════════════════════════════════════════════
# HTTP helpers
# ═══════════════════════════════════════════════════════════════

def _req(method, path, body=None, timeout=15):
    url = f"{MASTER}{path}"
    data_bytes = json.dumps(body).encode() if body else None
    req = urllib.request.Request(url, data=data_bytes, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {"error": f"HTTP {e.code}"}
        except json.JSONDecodeError:
            return e.code, {"error": raw.decode(errors='replace')[:200] if raw else f"HTTP {e.code}"}
    except Exception as e:
        return 0, {"error": str(e)}


def get(path, timeout=10):
    return _req("GET", path, timeout=timeout)

def post(path, body, timeout=15):
    return _req("POST", path, body, timeout=timeout)

def put(path, body, timeout=15):
    return _req("PUT", path, body, timeout=timeout)

def delete(path, body=None, timeout=10):
    return _req("DELETE", path, body, timeout=timeout)


# ═══════════════════════════════════════════════════════════════
# 测试框架
# ═══════════════════════════════════════════════════════════════

def test(name):
    def deco(fn):
        def wrapper():
            global _passed, _failed
            try:
                print(f"\n  [{name}]", flush=True)
                fn()
                _passed += 1
                print(f"    ✓ 通过", flush=True)
            except AssertionError as e:
                _failed += 1
                print(f"    ✗ 失败: {e}", flush=True)
            except Exception as e:
                _failed += 1
                print(f"    ✗ 异常: {e}", flush=True)
        return wrapper
    return deco


def ok(status):
    return 200 <= status < 300


def assert_ok(status, msg="HTTP 状态码异常"):
    assert ok(status), f"{msg}: {status}"

def assert_eq(a, b, msg=""):
    assert a == b, f"{msg}: 期望={b!r} 实际={a!r}"

def assert_gt(a, b, msg=""):
    assert a > b, f"{msg}: {a!r} <= {b!r}"

def assert_in(sub, full, msg=""):
    assert sub in full, f"{msg}: {sub!r} not in {full!r}"


def run_tests(tests, title=None):
    """tests: [(name, fn), ...]"""
    global _passed, _failed
    _passed = 0
    _failed = 0

    # 筛选
    filter_test = None
    for a in sys.argv:
        if a.startswith("--test="):
            filter_test = a.split("=", 1)[1]
    if filter_test:
        tests = [(n, f) for n, f in tests if filter_test.lower() in n.lower()]
        if not tests:
            print(f"未找到匹配的测试: {filter_test}")
            sys.exit(1)

    print("=" * 60)
    print(f"  {title or '测试'}")
    print(f"  Master: {MASTER}")
    print(f"  测试项: {len(tests)} 个")
    print("=" * 60)

    for name, fn in tests:
        try:
            print(f"\n  [{name}]", flush=True)
            fn()
            _passed += 1
            print(f"    ✓ 通过", flush=True)
        except AssertionError as e:
            _failed += 1
            print(f"    ✗ 失败: {e}", flush=True)
        except Exception as e:
            _failed += 1
            print(f"    ✗ 异常: {type(e).__name__}: {e}", flush=True)

    print("\n" + "=" * 60)
    total = _passed + _failed
    print(f"  结果: {_passed}/{total} 通过", end="")
    if _failed > 0:
        print(f", {_failed} 失败")
        sys.exit(1)
    else:
        print(" ✓")
    print("=" * 60)


def get_gpunode_id():
    """获取 gpunode2 的 node_id。"""
    _, data = post("/nodes/get_all_nodes", {})
    for n in data.get("nodes", []):
        if "gpu" in n.get("name", "").lower():
            return n["node_id"]
    for n in data.get("nodes", []):
        if n.get("total_gpu", 0) > 0:
            return n["node_id"]
    raise AssertionError("找不到 GPU 节点")
