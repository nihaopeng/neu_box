"""A Docker stop timeout must be an integer accepted by the Engine API."""

from types import SimpleNamespace

from neu_box.runtime import sandbox


def test_container_stop_uses_docker_accepted_timeout(monkeypatch):
    calls = []
    container = SimpleNamespace(stop=lambda *, timeout: calls.append(timeout))
    client = SimpleNamespace(
        containers=SimpleNamespace(get=lambda _reference: container),
        close=lambda: None,
    )
    monkeypatch.setattr(sandbox, "docker_client", lambda **_kwargs: client)
    monkeypatch.setattr(
        sandbox, "load_docker",
        lambda: SimpleNamespace(errors=SimpleNamespace(NotFound=KeyError)),
    )

    manager = sandbox.SbxManager.__new__(sandbox.SbxManager)
    assert manager._stop_container("container-id")
    assert calls == [10]
    assert isinstance(calls[0], int)
