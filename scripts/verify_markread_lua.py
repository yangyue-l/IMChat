"""验证已读位置只前进、未读不会反弹（Lua 版 MarkRead 的针对性回归）。"""
import json
import subprocess
import time
import urllib.request

BASE = "http://127.0.0.1:8080/api/v1"
SFX = str(int(time.time()))[-6:]


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def register(name):
    r = call("POST", "/auth/register", body={"username": name, "password": "123456", "nickname": name})
    assert r["code"] == 0, r
    return r["data"]["token"], r["data"]["user"]["id"]


def redis(*args):
    out = subprocess.run(["docker", "exec", "imchat-redis", "redis-cli", *args],
                         capture_output=True, text=True)
    return out.stdout.strip()


a_tok, a_id = register("luaread_a" + SFX)
b_tok, b_id = register("luaread_b" + SFX)
print("accounts:", a_id, b_id)

r = call("POST", "/friend-requests", a_tok, {"to_user_id": b_id, "message": "hi"})
assert r["code"] == 0, r
r = call("GET", "/friend-requests?type=incoming", b_tok)
req_id = [x for x in r["data"] if x["from_user"]["id"] == a_id][0]["id"]
assert call("POST", f"/friend-requests/{req_id}/handle", b_tok, {"accept": True})["code"] == 0

# a 给 b 发消息，seq=1
r = call("POST", "/messages/private", a_tok, {
    "to_id": b_id, "msg_type": 1, "content": "第一条",
    "client_msg_id": "lua-read-1-" + SFX,
})
assert r["code"] == 0, r
conv = r["data"]["conv_id"]
print("conv:", conv, "seq:", r["data"]["seq"])

# b 已读到 1，未读应清零
assert call("POST", "/messages/read", b_tok, {"conv_id": conv, "seq": 1})["code"] == 0
unread_after_first = [c for c in call("GET", "/conversations", b_tok)["data"] if c["conv_id"] == conv][0]["unread"]
read_seq_now = redis("HGET", "im:read:" + b_id, conv)
print(f"after read seq=1 : unread={unread_after_first} redis_read={read_seq_now}")

# b 乱序上报更小的 seq=0，已读必须停在 1，未读不能反弹
assert call("POST", "/messages/read", b_tok, {"conv_id": conv, "seq": 0})["code"] == 0
unread_after_stale = [c for c in call("GET", "/conversations", b_tok)["data"] if c["conv_id"] == conv][0]["unread"]
read_seq_now = redis("HGET", "im:read:" + b_id, conv)
unread_key_ttl = redis("TTL", "im:unread:" + a_id)
print(f"after read seq=0 : unread={unread_after_stale} redis_read={read_seq_now} unread_key_ttl={unread_key_ttl}")

assert unread_after_first == 0, "首次已读后未读应清零"
assert unread_after_stale == 0, "乱序旧请求不应让未读反弹"
assert read_seq_now == "1", f"已读位置不应回退，实际 {read_seq_now}"
print("\n[PASS] 已读位置只前进，未读不反弹")
