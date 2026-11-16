#!/bin/bash
# 对照实验：验证并发安全、优雅关闭和 panic 恢复确实起作用。
#
# 做的事：在临时目录里编译"当前代码"和三个"对照版本"，各跑同一组请求，打印结果。
# 对照版本是当前 main.go 只去掉一处得到的，其余完全相同：
#   去掉加锁       = 删掉 Store 里的 s.mu.Lock() / defer s.mu.Unlock()
#   不监听退出信号 = signal.Notify 不再登记 SIGINT/SIGTERM，收到信号时按系统默认行为立即结束
#   去掉恢复中间件 = Chain(logged, recovered) 改成 Chain(logged)
# 不修改仓库里的任何文件，跑完自动删除临时目录。全程约 20 秒。
#
# 用法（在仓库根目录）：bash scripts/measure.sh
# 需要：Go、curl（7.66 以上，用到 --parallel）、perl、lsof
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$W"' EXIT

PORT=18080     # 实验用的端口（通过环境变量 HTTP_ADDR 指定）
N=20000        # 并发实验的请求总数
C=200          # 并发实验同时进行的请求数
INFLIGHT=200   # 关闭实验里"处理到一半"的请求数
NPANIC=1000    # panic 实验里会 panic 的请求数
NOK=5000       # panic 实验里同时进行的正常请求数

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
busy() { lsof -iTCP:"$1" -sTCP:LISTEN -n -P >/dev/null 2>&1; }
wait_up() { for _ in $(seq 1 50); do curl -s -o /dev/null "http://127.0.0.1:$PORT/healthz" && return 0; sleep 0.1; done; return 1; }
line() { echo; echo "──────── $1 ────────"; }

# ---------- 编译 ----------
# variant <名称> <sed 表达式>：把 main.go 按表达式改一处，编译成 bin/<名称>
variant() {
  local name="$1" expr="$2"
  mkdir -p "$W/$name"
  cp "$ROOT/go.mod" "$W/$name/"
  sed "$expr" "$ROOT/main.go" >"$W/$name/main.go"
  if cmp -s "$ROOT/main.go" "$W/$name/main.go"; then
    echo "提示：main.go 里找不到 [$name] 要改的那一行，跳过这个对照"
    return
  fi
  (cd "$W/$name" && go build -o "$W/bin/$name" .) || exit 1
}

build() {
  mkdir -p "$W/cur" "$W/bin"
  cp "$ROOT/main.go" "$ROOT/go.mod" "$W/cur/"
  (cd "$W/cur" && go build -race -o "$W/bin/cur-race" . && go build -o "$W/bin/cur" .) || exit 1
  variant nolock    '/s\.mu\.Lock()/d; /defer s\.mu\.Unlock()/d'
  variant nosignal  's|signal\.Notify(ch, os\.Interrupt, syscall\.SIGTERM)|signal.Notify(ch, syscall.SIGUSR1)|'
  variant norecover 's|Chain(logged, recovered)|Chain(logged)|'
}

# ---------- 实验 1：并发创建 ----------
# 同时发 N 个 POST /tasks，统计状态码、返回的 id 是否重复、竞态告警、进程是否还活着
exp_concurrency() { # <二进制> <名称>
  local bin="$1" tag="$2" o="$W/out-c-$RANDOM"; mkdir -p "$o"
  [ -x "$bin" ] || return
  HTTP_ADDR=":$PORT" "$bin" >"$o/server.log" 2>&1 &
  local pid=$!; wait_up
  curl -s --parallel --parallel-immediate --parallel-max "$C" \
    -X POST -d '{"title":"t"}' -w '%{stderr}%{http_code}\n' \
    "http://127.0.0.1:$PORT/tasks?n=[1-$N]" >"$o/bodies.txt" 2>"$o/codes.txt"
  sleep 0.3
  local alive="是"; kill -0 "$pid" 2>/dev/null || alive="否，已崩溃"
  kill -TERM "$pid" 2>/dev/null; wait "$pid" 2>/dev/null

  local ok ids uniq
  ok=$(grep -c '^201$' "$o/codes.txt")
  ids=$(grep -o '"id":[0-9]*' "$o/bodies.txt" | wc -l | tr -d ' ')
  uniq=$(grep -o '"id":[0-9]*' "$o/bodies.txt" | sort -u | wc -l | tr -d ' ')
  echo "[$tag] 成功 $ok / $N；返回 id $ids 个，其中不重复 $uniq 个；竞态告警 $(grep -c 'WARNING: DATA RACE' "$o/server.log") 条；访问日志 $(grep -c 'status=201' "$o/server.log") 行；压测后进程还活着：$alive"
  grep -o -m1 'fatal error: [a-z ]*' "$o/server.log" | sed 's/^/        崩溃原因：/'
}

# ---------- 实验 2：关闭时处理到一半的请求 ----------
# 先让 INFLIGHT 个慢请求进入处理，0.5 秒后发 SIGTERM，看它们能否收到完整响应
exp_shutdown() { # <二进制> <每个请求时长> <关闭预算> <名称>
  local bin="$1" d="$2" budget="$3" tag="$4" o="$W/out-s-$RANDOM"; mkdir -p "$o"
  [ -x "$bin" ] || return
  HTTP_ADDR=":$PORT" SHUTDOWN_TIMEOUT="$budget" "$bin" >"$o/server.log" 2>&1 &
  local pid=$!; wait_up
  curl -s --parallel --parallel-immediate --parallel-max "$INFLIGHT" \
    -o /dev/null -w '%{http_code}\n' \
    "http://127.0.0.1:$PORT/debug/slow?d=$d&n=[1-$INFLIGHT]" >"$o/codes.txt" 2>/dev/null &
  local cpid=$!
  sleep 0.5
  local t0 t1 code; t0=$(now)
  kill -TERM "$pid"
  wait "$pid" 2>/dev/null; code=$?
  t1=$(now)
  wait "$cpid" 2>/dev/null
  echo "[$tag] 收到完整响应 $(grep -c '^200$' "$o/codes.txt") / $INFLIGHT；进程退出码 $code；从发信号到退出 $(perl -e "printf '%.2f', $t1-$t0") 秒"
}

# ---------- 实验 3：处理函数 panic ----------
# NPANIC 个会 panic 的请求和 NOK 个正常请求同时进行
exp_panic() { # <二进制> <名称>
  local bin="$1" tag="$2" o="$W/out-p-$RANDOM"; mkdir -p "$o"
  [ -x "$bin" ] || return
  HTTP_ADDR=":$PORT" "$bin" >"$o/server.log" 2>&1 &
  local pid=$!; wait_up
  curl -s --parallel --parallel-immediate --parallel-max 50 -w '%{stderr}%{http_code}\n' \
    "http://127.0.0.1:$PORT/debug/panic?n=[1-$NPANIC]" >"$o/bodies.txt" 2>"$o/codes.txt" &
  local a=$!
  curl -s --parallel --parallel-immediate --parallel-max 50 -o /dev/null -w '%{http_code}\n' \
    "http://127.0.0.1:$PORT/healthz?n=[1-$NOK]" >"$o/ok.txt" 2>/dev/null &
  local b=$!
  wait "$a" "$b"
  echo "[$tag] panic 请求：拿到 500 错误 JSON 的 $(grep -o '"reason":"INTERNAL"' "$o/bodies.txt" | wc -l | tr -d ' ') / $NPANIC，服务器无任何响应的 $(grep -c '^000$' "$o/codes.txt")；同时的正常请求成功 $(grep -c '^200$' "$o/ok.txt") / $NOK；status=500 的访问日志 $(grep -c 'status=500' "$o/server.log") 行"
  kill -TERM "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
}

# ---------- 实验 4：写错配置 ----------
exp_config() {
  local v out code
  for v in abc 5 0s -1s; do
    out=$(HTTP_ADDR=":$PORT" SHUTDOWN_TIMEOUT="$v" "$W/bin/cur" 2>&1); code=$?
    echo "SHUTDOWN_TIMEOUT=$v → 退出码 $code，开始监听前就结束：$([ "$(echo "$out" | grep -c 服务启动)" = 0 ] && echo 是 || echo 否)"
  done
}

# ---------- 实验 5：各种出错情形的响应格式 ----------
exp_errors() {
  HTTP_ADDR=":$PORT" "$W/bin/cur" >/dev/null 2>&1 &
  local pid=$!; wait_up
  req() { local name="$1"; shift; printf '%s → ' "$name"; curl -s -w ' [%{http_code}]' "$@" | tr -d '\n'; echo; }
  req "请求体不是 JSON" -X POST -d '{bad' "localhost:$PORT/tasks"
  req "标题为空"       -X POST -d '{"title":"  "}' "localhost:$PORT/tasks"
  req "id 不是数字"    "localhost:$PORT/tasks/abc"
  req "id 为 0"        "localhost:$PORT/tasks/0"
  req "任务不存在"     "localhost:$PORT/tasks/999"
  req "时长参数非法"   "localhost:$PORT/debug/slow?d=xx"
  req "处理函数 panic" "localhost:$PORT/debug/panic"
  kill -TERM "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
}

# ---------- 主流程 ----------
if busy "$PORT"; then echo "端口 $PORT 已被占用，请先释放或修改脚本里的 PORT"; exit 2; fi
echo "环境：$(go version | awk '{print $3, $4}')，$(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m)，$(date '+%Y-%m-%d %H:%M')"
build

line "自动化测试（竞态检测下）"
(cd "$ROOT" && go test -race -count=1 -v . 2>&1) >"$W/test.txt"
echo "测试函数通过 $(grep -c '^--- PASS' "$W/test.txt") 个，失败 $(grep -c '^--- FAIL' "$W/test.txt") 个；含表驱动子用例共通过 $(( $(grep -c '^--- PASS' "$W/test.txt") - 1 + $(grep -c '^    --- PASS' "$W/test.txt") )) 个用例"
tail -1 "$W/test.txt"

line "实验 1：$C 并发、$N 次创建请求"
exp_concurrency "$W/bin/cur-race" "当前代码(带竞态检测)"
for i in 1 2 3; do exp_concurrency "$W/bin/nolock" "去掉加锁-第${i}次"; done

line "实验 2：$INFLIGHT 个请求处理到一半时关闭服务"
exp_shutdown "$W/bin/cur" 2s 5s "当前代码(请求 2 秒，预算 5 秒)"
exp_shutdown "$W/bin/nosignal" 2s 5s "不监听退出信号"
exp_shutdown "$W/bin/cur" 6s 1s "当前代码(请求 6 秒，预算 1 秒，应超时)"

line "实验 3：$NPANIC 个 panic 请求 + $NOK 个正常请求同时进行"
exp_panic "$W/bin/cur" "当前代码"
exp_panic "$W/bin/norecover" "去掉恢复中间件"

line "实验 4：写错配置"
exp_config

line "实验 5：出错时的响应格式"
exp_errors
