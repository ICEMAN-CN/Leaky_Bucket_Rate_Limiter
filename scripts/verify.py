"""在隔离的 Compose 网络中验证真实 HTTP、状态转移和依赖故障。"""
import concurrent.futures
import json
import sys
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

CLJ = "http://clojure:8080"
GO = "http://gateway:8080"
A = "http://go-a:8080"
B = "http://go-b:8080"
UI = "http://console:8080"
FAIL_USER = "test-before-failover"


def call(base, path, payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    req = Request(base + path, data=body, headers={"Content-Type": "application/json"})
    try:
        response = urlopen(req, timeout=3)
    except HTTPError as exc:
        response = exc
    with response:
        data = response.read()
        return response.status, json.loads(data) if data else {}


def expect(base, path, code, payload=None):
    status, body = call(base, path, payload)
    assert status == code, (base, path, status, body, code)
    return body


def until(fn, seconds=20):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        try:
            result = fn()
            if result:
                return result
        except (OSError, ValueError, AssertionError, URLError):
            pass
        time.sleep(0.2)
    raise AssertionError(f"等待超时 {fn.__name__}")


def api():
    for url in (CLJ, GO):
        user = "测 a/b+%-" + uuid.uuid4().hex
        path = "/v1/buckets/" + quote(user, safe="")
        expect(url, path, 404)
        expect(url, "/v1/check", 400, {"user_id": ""})
        expect(url, "/v1/check", 400, {"user_id": 3})
        first = expect(url, "/v1/check", 200, {"user_id": user})
        assert first["allowed"] and expect(url, path, 200)["bucket"] == first["bucket"]
        assert expect(url, "/v1/bucket-state", 200, {"user_id": user})["bucket"] == first["bucket"]
        for special in (".", "..", "长" * 7000):
            body = {"user_id": special}
            expect(url, "/v1/bucket-state", 404, body)
            stored = expect(url, "/v1/check", 200, body)["bucket"]
            for _ in range(2):
                assert expect(url, "/v1/bucket-state", 200, body)["bucket"] == stored
        # 冷启动时首个请求可能较慢。在一个可测量的恢复窗口内才断言准确数量。
        for _ in range(3):
            burst = {"user_id": "api-" + uuid.uuid4().hex}
            with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
                start = time.monotonic()
                codes = list(pool.map(lambda _: call(url, "/v1/check", burst)[0], range(6)))
            if time.monotonic() - start < 1:
                assert sorted(codes) == [200] * 5 + [429], codes
                break
        else:
            raise AssertionError("环境无法在一秒内完成突发，未形成有效的测试窗口")


def seed():
    user = {"user_id": FAIL_USER}
    for _ in range(5):
        expect(A, "/v1/check", 200, user)
    assert expect(B, "/v1/buckets/" + FAIL_USER, 200)["bucket"]["level"] > 4
    # 以真实 HTTP 分别打到两个独立容器。0.01/s 需要 100 秒才能恢复一份额度。
    burst = "burst-" + uuid.uuid4().hex
    def attempt(i):
        return call(A if i % 2 else B, "/v1/check", {"user_id": burst})[0]
    start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:
        codes = list(pool.map(attempt, range(100)))
    assert time.monotonic() - start < 100
    assert codes.count(200) == 5 and codes.count(429) == 95, codes
    expect(B, "/v1/check", 200, {"user_id": burst + "-other"})


def failover():
    def only_b():
        for _ in range(6):
            line = expect(GO, "/v1/buckets/" + FAIL_USER, 200)
            if line["instance"] != "go-b" or line["bucket"]["level"] <= 4:
                return False
            time.sleep(0.25)
        return True
    until(only_b)
    for _ in range(5):
        assert expect(GO, "/v1/check", 429, {"user_id": FAIL_USER})["instance"] == "go-b"


def outage():
    expect(B, "/livez", 200)
    until(lambda: call(B, "/readyz")[0] == 503)
    before = expect(B, "/internal/metrics", 200)["errors"]
    start = time.monotonic()
    expect(GO, "/v1/check", 503, {"user_id": FAIL_USER})
    assert time.monotonic() - start < 2
    expect(B, "/v1/buckets/" + FAIL_USER, 503)
    assert expect(B, "/internal/metrics", 200)["errors"] > before
    expect(CLJ, "/v1/check", 200, {"user_id": "still-available"})
    def observed():
        history = expect(UI, "/api/history?target=go-b", 200)
        return any((p["errors"] or 0) >= 1 for p in history["points"]) and not history["instances"][0]["ready"]
    until(observed)


def restored():
    until(lambda: call(A, "/readyz")[0] == call(B, "/readyz")[0] == 200)
    expect(GO, "/v1/check", 200, {"user_id": "after-recovery"})
    # 此处 Redis 被正常停止并重启，数据卷应保留仍未排空的旧桶。
    assert expect(GO, "/v1/buckets/" + FAIL_USER, 200)["bucket"]["level"] > 1


def gateway_outage():
    def both_unavailable():
        value = expect(GO, "/v1/check", 503, {"user_id": FAIL_USER})
        return value == {"instance": "gateway", "error": "no_backend_available"}
    until(both_unavailable)
    expect(CLJ, "/v1/check", 200, {"user_id": "gateway-independent"})


if __name__ == "__main__":
    phase = sys.argv[1]
    {"api": api, "seed": seed, "failover": failover, "outage": outage, "restored": restored, "gateway_outage": gateway_outage}[phase]()
    print(f"通过 {phase}", flush=True)
