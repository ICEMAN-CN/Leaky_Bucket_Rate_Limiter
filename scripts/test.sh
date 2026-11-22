#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export STANDARD_PORT=0 GO_PORT=0 CONSOLE_PORT=0 CAPACITY=5 LEAK_RATE=1
project="bucket-test-$(date +%s)-$$"
dc() { docker compose -p "$project" -f compose.yaml -f compose.test.yaml "$@"; }
cleanup() {
    code=$?
    trap - EXIT INT TERM
    if [ "$code" -ne 0 ]; then dc logs --tail=20 clojure go-a go-b gateway console redis >&2 || :; fi
    dc down -v --remove-orphans --rmi local >/dev/null || :
    exit "$code"
}
browser() {
    label="$1"
    shift
    container="$project-browser-$label"
    if dc run --name "$container" --no-deps "$@" browser-tests; then
        docker rm "$container" >/dev/null
    else
        destination="test-results/$project-$label"
        mkdir -p "$destination"
        docker cp "$container:/suite/test-results/." "$destination/" || :
        echo "浏览器测试失败，诊断文件目录 $destination" >&2
        return 1
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
dc build standard-tests go-tests console-tests browser-tests
dc up --wait -d redis
dc run --rm --no-deps standard-tests
dc run --rm --no-deps go-tests
dc run --rm --no-deps console-tests
dc up --wait -d --build clojure go-a go-b gateway console
dc run --rm --no-deps checks api
browser normal
# 较慢漏速让故障演练能观察故障前同一个桶的状态。
export LEAK_RATE=0.01
dc up --wait -d go-a go-b
dc run --rm --no-deps checks seed
dc stop -t 1 go-a
dc run --rm --no-deps checks failover
dc stop -t 2 redis
dc run --rm --no-deps checks outage
browser outage -e OUTAGE=1
dc start redis go-a
dc run --rm --no-deps checks restored
echo '全部验收通过。'
