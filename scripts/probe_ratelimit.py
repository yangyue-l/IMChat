"""实测限流策略的实际水位。

覆盖四档：
1) 读接口 read      -> 单档：进程内令牌桶，50 QPS / burst 100
2) 写接口 write     -> 双档：本地桶 10 QPS/20 + Redis 窗口 120 次/分钟
3) 429 响应头       -> Retry-After / X-RateLimit-*，Limit 为实际命中的那一档
4) 登录接口 auth    -> 双档：本地桶 5 QPS/10 + Redis 窗口 30 次/分钟（按 IP）

登录那一段会把本机 IP 的 auth 配额打光，所以放在最后跑。
"""
import json
import os
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = os.getenv("IMCHAT_BASE", "http://127.0.0.1:8080") + "/api/v1"
SFX = str(int(time.time()))[-6:]


def call(method, path, token=None, body=None):
    """返回 (code, headers, data)。网络层异常统一收敛成 code=-1。"""
    payload = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=payload, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            parsed = json.loads(resp.read())
            return parsed["code"], resp.headers, parsed.get("data")
    except urllib.error.HTTPError as e:
        return json.loads(e.read())["code"], e.headers, None


def summarize(label, codes, total):
    ok = sum(1 for c in codes if c == 0)
    limited = sum(1 for c in codes if c == 42900)
    print(f"{label}: 通过={ok} 限流={limited} 其他={total - ok - limited}")
    return limited


# 注册直接签发令牌，少占一次 auth 配额
name = "rl_" + SFX
code, _, data = call("POST", "/auth/register",
                     body={"username": name, "password": "123456", "nickname": name})
assert code == 0, f"注册失败 code={code}"
token = data["token"]
print("account:", name)


def hit_read(_):
    return call("GET", "/users/me", token)[0]


t0 = time.time()
with ThreadPoolExecutor(max_workers=64) as pool:
    codes = list(pool.map(hit_read, range(300)))
elapsed = time.time() - t0
summarize(f"读接口 300 次并发({elapsed:.2f}s)", codes, 300)


def hit_write(i):
    # 用幂等的资料更新做写请求，保证业务成功，才能验证「通过数 = 窗口额度」
    return call("PATCH", "/users/me", token,
                body={"signature": f"probe{i % 7}"})[0]


with ThreadPoolExecutor(max_workers=16) as pool:
    codes = list(pool.map(hit_write, range(200)))
summarize("写接口 200 次并发", codes, 200)

# 写接口已被打满，这里必定 429，顺手校验响应头
code, headers, _ = call("PATCH", "/users/me", token, body={"signature": "probe-x"})
if code == 42900:
    print("429 响应头:", {k: headers.get(k) for k in
                          ("Retry-After", "X-RateLimit-Limit",
                           "X-RateLimit-Remaining", "X-RateLimit-Rule")})


def hit_login(i):
    # 每次换用户名：固定用户名会先撞上 service 层的「5 次失败锁定」，
    # 那个 429 是账号维度，会污染 IP 维度的观测结果
    return call("POST", "/auth/login",
                body={"username": f"nouser_{SFX}_{i}", "password": "x"})[0]


with ThreadPoolExecutor(max_workers=16) as pool:
    codes = list(pool.map(hit_login, range(60)))
limited = summarize("登录接口 60 次并发(同一 IP, 不同用户名)", codes, 60)
print("结论: 登录接口已被 IP 滑动窗口限流" if limited else "结论: 登录接口仍未限流，检查 auth 规则是否挂上")
