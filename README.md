# Leaky Bucket Rate Limiter

The standalone Clojure REST API keeps one in-memory bucket per user. An optional distributed version uses Go and Redis to enforce the same quota across two replicas.

## Quick start

Run this from the repository root with Make, a running Docker engine, and Docker Compose with `--wait` support.

```sh
make up
```

This starts both APIs and the monitoring console. The Clojure API listens on port **8081** and the Go gateway on **8082**. [Open the console](http://127.0.0.1:8080/) · [View screenshot](docs/console.png)

| To | Run |
| --- | --- |
| Start only the Clojure API | `make standard` |
| Start only the distributed API (gateway, two Go replicas, Redis) | `docker compose up -d --build --wait gateway` |
| Run the Clojure unit and HTTP tests | `make test-standard` |
| Run the full suite in an isolated stack | `make test` |
| Try quota recovery, user isolation, and cross-replica sharing after `make up` | `make demo` |
| Stop the stack and retain Redis data | `make down` |

## Try the API

Send a request for Alice, then read her last recorded bucket state. Use port `8082` to try the same calls against Go.

```sh
curl -i http://127.0.0.1:8081/v1/check \
  -H 'Content-Type: application/json' -d '{"user_id":"alice"}'

curl -s http://127.0.0.1:8081/v1/buckets/alice
```

Alice's first `POST /v1/check` request returns HTTP 200 with a body like this.

```json
{
  "allowed": true,
  "instance": "clojure",
  "bucket": { "level": 1, "updated_at": 1700000000, "capacity": 5, "leak_rate": 1.0 }
}
```

`updated_at` is the Unix time in seconds of the last rate-limit decision for this user, and a lookup returns the bucket snapshot recorded at that time.

A check returns **200** when allowed or **429** when the bucket has less than one unit free. Both include the decision and the updated bucket. A lookup consumes no quota and returns **404** for an unknown user.

## Design and trade-offs

- **Leakage on demand.** Defaults are `CAPACITY=5` and `LEAK_RATE=1.0` unit per second. Five simultaneous requests fill an empty bucket. After half a second, a sixth is rejected at level 4.5. At one second, a full unit has drained and the next request fits. Rejections record the settled level and timestamp so retries preserve that recovery time.
- **Standalone Clojure.** `allow-request` takes an explicit timestamp and returns the decision and a new immutable state. The HTTP handler locks the read, decision, and update together so two requests cannot claim the last slot. Updates are serialised across users. Buckets stay in memory until restart. [Core](standard/src/limiter/core.clj) · [HTTP handler](standard/src/limiter/http.clj)
- **Distributed Go.** Both replicas use one Redis Lua script to read server time and update the user's bucket atomically. HAProxy can route to a surviving replica. Redis is a single point of failure on this host, and the API returns **503** if it cannot confirm a decision. [Redis script](distributed/check.lua)

Both implementations use [shared test cases](contracts/cases.json) for bursts, fractional time, clock rollback, and user isolation. The full suite also sends 100 concurrent requests for one user across both Go replicas and checks that exactly five pass before one unit drains. It exercises replica loss and Redis outage and recovery.
