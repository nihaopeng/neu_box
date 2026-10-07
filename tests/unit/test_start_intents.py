"""start 借条账本：一次性、按属主隔离、到点作废。

账本本身没有 I/O，这里用假时钟把时间推着走 —— 真等 10 秒不值得。
"""

from neu_box.runtime.container_intents import StartIntentStore

CONTAINER = 'b' * 64


class _Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds


def _store(ttl=10.0):
    clock = _Clock()
    return StartIntentStore(ttl_seconds=ttl, clock=clock), clock


def test_claim_only_becomes_consumed_after_complete():
    store, _clock = _store()
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')

    claim = store.claim(CONTAINER, 'user')
    assert claim['sandbox_name'] == 'sbx_user_1.slice'
    assert store.claim(CONTAINER, 'user') is None
    assert store.peek(CONTAINER, 'user')['consumed_at'] is None
    assert store.complete(CONTAINER, 'user', claim['sequence'])
    assert store.peek(CONTAINER, 'user')['consumed_at'] is not None
    assert not store.complete(CONTAINER, 'user', claim['sequence'])


def test_intents_are_scoped_by_owner():
    store, _clock = _store()
    store.lend(CONTAINER, 'alice', 'sbx_alice_1.slice')

    assert store.claim_state(CONTAINER, 'bob') == ('owner_mismatch', None)
    assert store.claim(CONTAINER, 'bob') is None
    assert store.claim(CONTAINER, 'alice')['sandbox_name'] == 'sbx_alice_1.slice'


def test_other_owner_intent_blocks_old_annotation_until_acknowledged():
    store, clock = _store()
    store.lend(CONTAINER, 'alice', 'sbx_alice_1.slice', borrower_pid=123)
    claim = store.claim(CONTAINER, 'alice')
    assert store.complete(CONTAINER, 'alice', claim['sequence'])
    assert store.claim_state(CONTAINER, 'bob') == ('owner_mismatch', None)

    store.acknowledge(CONTAINER, 'alice', 123)
    assert store.claim_state(CONTAINER, 'bob') == ('missing', None)
    store.lend(CONTAINER, 'bob', 'sbx_bob_1.slice', borrower_pid=456)
    # The older acknowledged Alice result must not outrank Bob's new start.
    assert store.claim_state(CONTAINER, 'alice') == ('owner_mismatch', None)
    assert store.claim_state(CONTAINER, 'bob')[0] == 'claimed'

    clock.advance(10)
    assert store.claim_state(CONTAINER, 'alice') == ('missing', None)


def test_relending_rejects_a_pending_intent():
    store, _clock = _store()
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')
    assert store.lend(CONTAINER, 'user', 'sbx_user_2.slice') is None

    assert store.claim(CONTAINER, 'user')['sandbox_name'] == 'sbx_user_1.slice'


def test_another_owner_cannot_reserve_the_same_container_in_flight():
    store, _clock = _store()
    store.lend(CONTAINER, 'alice', 'sbx_alice_1.slice', borrower_pid=123)

    assert store.lend(CONTAINER, 'bob', 'sbx_bob_1.slice', borrower_pid=456) is None
    assert store.claim(CONTAINER, 'alice') is not None
    assert store.claim(CONTAINER, 'bob') is None


def test_old_claim_cannot_complete_a_new_lend():
    store, clock = _store()
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')
    old = store.claim(CONTAINER, 'user')
    clock.advance(10)
    store.lend(CONTAINER, 'user', 'sbx_user_2.slice')

    assert not store.complete(CONTAINER, 'user', old['sequence'])
    assert store.peek(CONTAINER, 'user')['consumed_at'] is None
    new = store.claim(CONTAINER, 'user')
    assert store.complete(CONTAINER, 'user', new['sequence'])


def test_claim_state_distinguishes_busy_and_consumed_without_a_second_peek():
    store, _clock = _store()
    assert store.claim_state(CONTAINER, 'user') == ('missing', None)
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')

    state, claim = store.claim_state(CONTAINER, 'user')
    assert state == 'claimed'
    assert store.claim_state(CONTAINER, 'user')[0] == 'busy'
    assert store.complete(CONTAINER, 'user', claim['sequence'])
    assert store.claim_state(CONTAINER, 'user')[0] == 'consumed'


def test_abort_releases_only_the_current_unconsumed_claim():
    store, clock = _store()
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')
    old = store.claim(CONTAINER, 'user')
    clock.advance(10)
    store.lend(CONTAINER, 'user', 'sbx_user_2.slice')

    assert not store.abort(CONTAINER, 'user', old['sequence'])
    state, current = store.claim_state(CONTAINER, 'user')
    assert state == 'claimed'
    assert store.abort(CONTAINER, 'user', current['sequence'])
    assert store.claim_state(CONTAINER, 'user')[0] == 'claimed'


def test_new_lend_waits_for_original_cli_to_observe_consumption():
    store, _clock = _store()
    first = store.lend(CONTAINER, 'user', 'sbx_user_1.slice', borrower_pid=123)
    claim = store.claim(CONTAINER, 'user')
    assert store.lend(CONTAINER, 'user', 'sbx_user_2.slice', borrower_pid=456) is None
    assert store.complete(CONTAINER, 'user', claim['sequence'])
    assert store.lend(CONTAINER, 'user', 'sbx_user_2.slice', borrower_pid=456) is None

    # A different CLI can read the state, but cannot acknowledge it.
    assert store.acknowledge(CONTAINER, 'user', 456)['acknowledged_at'] is None
    assert store.lend(CONTAINER, 'user', 'sbx_user_2.slice', borrower_pid=456) is None
    assert store.acknowledge(CONTAINER, 'user', 123)['acknowledged_at'] is not None

    second = store.lend(CONTAINER, 'user', 'sbx_user_2.slice', borrower_pid=456)
    assert second['sequence'] != first['sequence']
    assert second['sandbox_name'] == 'sbx_user_2.slice'


def test_intents_expire():
    store, clock = _store(ttl=10.0)
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')

    clock.advance(9.5)
    assert store.claim(CONTAINER, 'user')['sandbox_name'] == 'sbx_user_1.slice'

    store.lend(CONTAINER, 'user', 'sbx_user_2.slice')
    clock.advance(10.0)
    assert store.peek(CONTAINER, 'user') is None
    assert store.claim(CONTAINER, 'user') is None


def test_a_consumed_intent_expires_too():
    """消费过的借条也带寿命，不能让 client 的确认接口对外一直可读。"""
    store, clock = _store(ttl=10.0)
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')
    claim = store.claim(CONTAINER, 'user')
    assert store.complete(CONTAINER, 'user', claim['sequence'])

    clock.advance(10.0)

    assert store.peek(CONTAINER, 'user') is None


def test_clear_drops_everything():
    store, _clock = _store()
    store.lend(CONTAINER, 'user', 'sbx_user_1.slice')

    store.clear()

    assert store.peek(CONTAINER, 'user') is None
