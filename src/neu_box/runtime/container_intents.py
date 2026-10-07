"""start 借条：把"这次 start 该用哪个沙盒"从容器 annotation 里解耦出来。

``docker start`` 复用的是容器自己那份配置，annotation 里写的是建容器时的沙盒
名；而 annotation 建好就改不了 —— ``docker start`` 没有 ``--annotation``，容器
配置也只有重启 dockerd 才能改写。所以沙盒一旦 release，老容器的可写层仍在，
但按旧 annotation 启动无法获得授权，runtime 会拒绝启动。

``neubox docker start`` 因此走两段式：先拿本 shell 的沙盒存一张借条（键是
container_id + 属主），随后 hook 报上来的 register 先认领、登记成功后确认它。
借条**一次性**、默认 10 秒过期；认领本身不算授权成功，客户端只把确认后的
`consumed` 当成绑定成功。

这里只放账本本身（进程内、带锁）；HTTP 端点在 ``api/containers.py``，语义写在
``docs/worker-api.md`` 和 ``docs/container-registration.md``。
"""

from __future__ import annotations

from contextlib import contextmanager
import logging
import threading
import time

logger = logging.getLogger(__name__)

# 借条寿命。正常链是「client 存借条 → docker start → hook 登记」，1 秒内走完；
# 给 10 秒是为了容忍慢机器，又不至于让一张没人认领的借条躺到下一次无关的 start。
START_INTENT_TTL = 10.0


class StartIntentStore:
    """``(container_id, 属主) → 沙盒名`` 的短期借条簿。"""

    _instance: StartIntentStore | None = None
    _instance_lock = threading.Lock()

    def __init__(self, ttl_seconds: float = START_INTENT_TTL,
                 clock=time.monotonic):
        self._ttl = float(ttl_seconds)
        self._clock = clock
        self._lock = threading.Lock()
        self._entries: dict[tuple[str, str], dict] = {}
        self._sequence = 0
        # A newer hook must not observe an older request's registration before
        # that request has either committed its intent or rolled it back.
        # Striping avoids one stalled container blocking all other starts.
        self._registration_locks = tuple(threading.Lock() for _ in range(64))

    @classmethod
    def get_instance(cls) -> StartIntentStore:
        with cls._instance_lock:
            if cls._instance is None:
                cls._instance = cls()
            return cls._instance

    def lend(self, container_id: str, owner: str, sandbox_name: str,
             *, borrower_pid: int | None = None) -> dict | None:
        """Reserve an unused intent slot; never overwrite an in-flight start.

        A consumed intent stays reserved until its original CLI has observed
        the result.  Once acknowledged, the caller may reserve another start
        after the prior container run has ended (checked by the HTTP layer).
        """
        now = self._clock()
        with self._lock:
            self._sweep_locked(now)
            # The Docker container ID is global on this node.  Even a second
            # username must not reserve the same container while another CLI
            # is still awaiting its start result.
            if any(existing_id == container_id
                   and entry['acknowledged_at'] is None
                   for (existing_id, _), entry in self._entries.items()):
                return None
            self._sequence += 1
            entry = {
                'container_id': container_id,
                'owner': owner,
                'sandbox_name': sandbox_name,
                'borrower_pid': borrower_pid,
                'created_at': now,
                'sequence': self._sequence,
                'claimed_at': None,
                'consumed_at': None,
                'acknowledged_at': None,
            }
            self._entries[(container_id, owner)] = entry
        return dict(entry)

    def acknowledge(self, container_id: str, owner: str, pid: int) -> dict | None:
        """Return the current state and mark a consumed intent seen by its CLI."""
        with self._lock:
            self._sweep_locked(self._clock())
            entry = self._entries.get((container_id, owner))
            if entry is None:
                return None
            if (entry['consumed_at'] is not None
                    and entry['borrower_pid'] == pid
                    and entry['acknowledged_at'] is None):
                entry['acknowledged_at'] = self._clock()
            return dict(entry)

    def claim(self, container_id: str, owner: str) -> dict | None:
        """为一次登记保留借条；尚未向客户端宣告绑定成功。"""
        state, entry = self.claim_state(container_id, owner)
        return entry if state == 'claimed' else None

    def claim_state(self, container_id: str, owner: str) -> tuple[str, dict | None]:
        """Atomically distinguish absent, wrong-owner, busy and claimed intents."""
        now = self._clock()
        with self._lock:
            self._sweep_locked(now)
            # An old annotation may still name a former owner while another
            # user's CLI has reserved this container for its current start.
            # Never let that hook fall back to the old annotation. Acknowledged
            # results are no longer in flight and do not block a new owner.
            if any(existing_id == container_id and existing_owner != owner
                   and existing['acknowledged_at'] is None
                   for (existing_id, existing_owner), existing in self._entries.items()):
                return 'owner_mismatch', None
            entry = self._entries.get((container_id, owner))
            if entry is None:
                return 'missing', None
            if entry['consumed_at'] is not None:
                return 'consumed', dict(entry)
            if entry['claimed_at'] is not None:
                return 'busy', dict(entry)
            entry['claimed_at'] = now
            return 'claimed', dict(entry)

    def abort(self, container_id: str, owner: str, sequence: int) -> bool:
        """Allow a failed registration to retry the *same* unconsumed intent."""
        with self._lock:
            self._sweep_locked(self._clock())
            entry = self._entries.get((container_id, owner))
            if (entry is None or entry['sequence'] != sequence
                    or entry['claimed_at'] is None
                    or entry['consumed_at'] is not None):
                return False
            entry['claimed_at'] = None
            return True

    @contextmanager
    def registration(self, container_id: str):
        """Serialize hook registrations for one container through rollback."""
        lock = self._registration_locks[hash(container_id) % len(self._registration_locks)]
        with lock:
            yield

    def complete(self, container_id: str, owner: str, sequence: int) -> bool:
        """完整登记后确认同一张借条，避免后来的 lend 被旧请求误确认。"""
        now = self._clock()
        with self._lock:
            self._sweep_locked(now)
            entry = self._entries.get((container_id, owner))
            if (entry is None or entry['sequence'] != sequence
                    or entry['claimed_at'] is None
                    or entry['consumed_at'] is not None):
                return False
            entry['consumed_at'] = now
            return True

    def peek(self, container_id: str, owner: str) -> dict | None:
        """查一张借条（不改状态）；没有或已过期返回 None。"""
        now = self._clock()
        with self._lock:
            self._sweep_locked(now)
            entry = self._entries.get((container_id, owner))
            return dict(entry) if entry is not None else None

    def clear(self) -> None:
        """丢掉所有借条（测试用）。"""
        with self._lock:
            self._entries.clear()

    def _sweep_locked(self, now: float) -> None:
        expired = [
            key for key, entry in self._entries.items()
            if now - (entry['consumed_at'] or entry['claimed_at']
                      or entry['created_at']) >= self._ttl
        ]
        for key in expired:
            del self._entries[key]
