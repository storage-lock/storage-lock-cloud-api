"""Storage Lock 免费云锁客户端。

无需注册：第一次运行会自动向服务端领取一个 secret，存到本地文件，
之后所有请求带上它。secret 就是身份，丢了就找不回原来的租户。

多节点：服务端在 /api/v1/endpoints 公布所有共用同一存储的节点。
客户端逐个测延迟，选最快的；某个节点挂了会自动换下一个。

用法：

    from client import CloudLock

    lock = CloudLock()
    with lock.hold("order-42", ttl="30s") as token:
        # token 是 fencing token，业务资源侧用它做原子护栏
        ...
"""

from __future__ import annotations

import json
import os
import threading
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from pathlib import Path
from typing import Any

DEFAULT_SERVICE = os.environ.get("STORAGE_LOCK_CLOUD_URL", "https://locks.storage-lock.dev")
DEFAULT_SECRET_FILE = Path(
    os.environ.get("STORAGE_LOCK_SECRET_FILE", Path.home() / ".storage-lock" / "secret")
)


class LockBusy(Exception):
    """锁被其他 owner 持有（HTTP 409）。"""


class NotOwner(Exception):
    """非持有者尝试续租或释放（HTTP 403）。"""


class AuthError(Exception):
    """凭据无效（HTTP 401）。"""


class CloudLock:
    def __init__(self, base_url: str | None = None, secret: str | None = None,
                 secret_file: Path | None = None, timeout: float = 10.0):
        self.base_url = (base_url or DEFAULT_SERVICE).rstrip("/")
        self.secret_file = secret_file or DEFAULT_SECRET_FILE
        self.timeout = timeout
        self._secret = secret or self._load_or_issue_secret()

    # ---- 身份 ----

    def _load_or_issue_secret(self) -> str:
        if self.secret_file.exists():
            cached = self.secret_file.read_text(encoding="utf-8").strip()
            if cached:
                return cached
        secret = self._issue_secret()
        self.secret_file.parent.mkdir(parents=True, exist_ok=True)
        # 只给用户本人读写；secret 泄露等于别人能用你的租户。
        os.chmod(self.secret_file.parent, 0o700)
        self.secret_file.write_text(secret, encoding="utf-8")
        os.chmod(self.secret_file, 0o600)
        return secret

    def _issue_secret(self) -> str:
        body = self._raw("POST", "/api/v1/tenant", None, auth=False)
        return body["secret"]

    # ---- 多节点选择 ----

    def endpoints(self) -> list[str]:
        """返回服务端公布的所有节点地址。"""
        try:
            body = self._raw("GET", "/api/v1/endpoints", None, auth=False, base=self.base_url)
        except (urllib.error.URLError, AuthError):
            # 旧版/单节点服务没有该接口，就只用自己的地址。
            return [self.base_url]
        return [e["url"] for e in body.get("endpoints", [])] or [self.base_url]

    def pick_fastest(self) -> str:
        """测每个节点的 /health 延迟，返回最快的那个。"""
        best, best_ms = self.base_url, float("inf")
        for url in self.endpoints():
            try:
                start = time.monotonic()
                self._raw("GET", "/health", None, auth=False, base=url, timeout=3.0)
                ms = (time.monotonic() - start) * 1000
            except Exception:
                continue  # 不可用的节点直接跳过
            if ms < best_ms:
                best, best_ms = url, ms
        return best

    # ---- 锁操作 ----

    def acquire(self, lock_id: str, owner_id: str = "", mode: str = "write",
                ttl: str = "", wait: bool = False) -> dict[str, Any]:
        body = {"owner_id": owner_id, "mode": mode, "ttl": ttl, "wait": wait}
        return self._lock_call("POST", f"/api/v1/locks/{lock_id}/acquire", body)

    def renew(self, lock_id: str, owner_id: str, ttl: str = "") -> dict[str, Any]:
        return self._lock_call("POST", f"/api/v1/locks/{lock_id}/renew",
                               {"owner_id": owner_id, "ttl": ttl})

    def release(self, lock_id: str, owner_id: str) -> dict[str, Any]:
        return self._lock_call("POST", f"/api/v1/locks/{lock_id}/release",
                               {"owner_id": owner_id})

    def inspect(self, lock_id: str) -> dict[str, Any]:
        return self._lock_call("GET", f"/api/v1/locks/{lock_id}", None)

    def waiters(self, lock_id: str) -> list[dict[str, Any]]:
        return self._lock_call("GET", f"/api/v1/locks/{lock_id}/waiters", None)

    def list_locks(self) -> list[dict[str, Any]]:
        return self._lock_call("GET", "/api/v1/locks", None)

    @contextmanager
    def hold(self, lock_id: str, owner_id: str = "", ttl: str = "30s",
             mode: str = "write", renew_every: float | None = None):
        """上下文管理器：自动获取、后台周期续租、退出时释放。

        renew_every 默认是 ttl 的 1/3，保证在租约过期前一定续上。
        with 块里拿到的是 fencing token。如果后台续租发现锁已被抢占，
        会抛 NotOwner —— 此时 finally 不会再去释放（锁已经不是你的了，
        强行释放会误伤新持有者）。
        """
        owner = owner_id or f"py-{os.getpid()}-{time.time_ns()}"
        got = self.acquire(lock_id, owner, mode=mode, ttl=ttl, wait=True)
        fencing = got.get("fencing_token", 0)
        interval = renew_every or (_parse_seconds(ttl) / 3)

        stop = threading.Event()
        lost = threading.Event()
        failure: list[BaseException] = []

        def keep_alive() -> None:
            # 后台续租：只有在租约过半之前续上，才不会被别人抢走。
            while not stop.wait(max(interval, 0.5)):
                try:
                    self.renew(lock_id, owner, ttl=ttl)
                except Exception as exc:  # 续租失败意味着持有权可能已丢
                    failure.append(exc)
                    lost.set()
                    return

        worker = threading.Thread(target=keep_alive, daemon=True)
        worker.start()
        try:
            yield fencing
        finally:
            stop.set()
            worker.join(timeout=5)
            if not lost.is_set():
                try:
                    self.release(lock_id, owner)
                except Exception:
                    # 释放失败时让租约自然过期，不要吞掉 with 块里原来的异常。
                    pass
            elif failure:
                # 锁在后续过程里被抢走，把原因暴露给调用方。
                raise NotOwner(str(failure[0])) from failure[0]

    # ---- HTTP ----

    def _lock_call(self, method: str, path: str, body: Any) -> Any:
        return self._raw(method, path, body)

    def _raw(self, method: str, path: str, body: Any, auth: bool = True,
             base: str | None = None, timeout: float | None = None) -> Any:
        url = (base or self.base_url) + path
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if auth:
            headers["X-Tenant-Secret"] = self._secret
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read().decode()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            payload = e.read().decode(errors="replace")
            if e.code == 401:
                raise AuthError(payload) from e
            if e.code == 403:
                raise NotOwner(payload) from e
            if e.code == 409:
                raise LockBusy(payload) from e
            raise


def _parse_seconds(ttl: str) -> float:
    """把 '30s' / '5m' / '1h' 转成秒。"""
    ttl = (ttl or "30s").strip()
    units = {"ms": 0.001, "s": 1, "m": 60, "h": 3600}
    for suffix, mult in units.items():
        if ttl.endswith(suffix):
            return float(ttl[: -len(suffix)]) * mult
    return float(ttl)


def _main() -> None:
    import argparse

    p = argparse.ArgumentParser(description="Storage Lock 免费云锁客户端")
    p.add_argument("--url", default=DEFAULT_SERVICE, help="服务地址")
    p.add_argument("--fastest", action="store_true", help="测速后选最快节点")
    p.add_argument("action", choices=["tenant", "endpoints", "locks", "acquire", "release", "inspect"])
    p.add_argument("lock_id", nargs="?", default="")
    p.add_argument("--owner", default="")
    p.add_argument("--ttl", default="30s")
    args = p.parse_args()

    lock = CloudLock(base_url=args.url)
    if args.fastest:
        lock.base_url = lock.pick_fastest()
        print(f"selected: {lock.base_url}", file=__import__("sys").stderr)

    if args.action == "tenant":
        # tenant id 由 secret 派生，这里只是把本地 secret 对应关系打印出来。
        print(json.dumps({"secret_file": str(lock.secret_file),
                          "secret_prefix": lock._secret[:8] + "..."}, indent=2))
    elif args.action == "endpoints":
        print(json.dumps(lock.endpoints(), indent=2))
    elif args.action == "locks":
        print(json.dumps(lock.list_locks(), indent=2))
    elif args.action == "acquire":
        print(json.dumps(lock.acquire(args.lock_id, args.owner, ttl=args.ttl), indent=2))
    elif args.action == "release":
        print(json.dumps(lock.release(args.lock_id, args.owner), indent=2))
    elif args.action == "inspect":
        print(json.dumps(lock.inspect(args.lock_id), indent=2))


if __name__ == "__main__":
    _main()
