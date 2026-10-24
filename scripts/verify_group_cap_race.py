"""验证「群成员数上限」在并发邀请下不会被突破。

做法：把群填到 995 人，然后并发发起 3 个各邀请 3 人的请求。
加法上只有一次能成功（995+3=998），后两次必然 998+3 > 1000 被拒。
没有行锁时三次都会读到 995 并全部通过，最终 1004 人。
"""
import json
import subprocess
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "http://127.0.0.1:8080/api/v1"
SFX = str(int(time.time()))[-6:]
CAP = 1000


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def sql(statement):
    out = subprocess.run(
        ["docker", "exec", "imchat-mysql", "mysql", "-uroot", "-proot123456", "imchat", "-N", "-B", "-e", statement],
        capture_output=True, text=True)
    if out.returncode != 0:
        raise RuntimeError(out.stderr)
    return out.stdout.strip()


name = "race_" + SFX
r = call("POST", "/auth/register", body={"username": name, "password": "123456", "nickname": name})
assert r["code"] == 0, r
token, owner_id = r["data"]["token"], r["data"]["user"]["id"]

r = call("POST", "/groups", token, {"name": "并发上限验证" + SFX, "member_ids": []})
assert r["code"] == 0, r
gid = int(r["data"]["id"])
print("group:", gid, "owner:", owner_id)

# 直接补 994 个成员，把群推到 995 人
base_uid = 8_000_000_000_000_000_000
sql(f"""
INSERT INTO group_members (id, created_at, updated_at, group_id, user_id, role, muted, joined_at)
WITH RECURSIVE seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 994)
SELECT 700000 + n, NOW(3), NOW(3), {gid}, {base_uid} + n, 0, 0, NOW(3) FROM seq;
UPDATE `groups` SET member_count = 995 WHERE id = {gid};
""")
print("seeded:", sql(f"SELECT member_count FROM `groups` WHERE id = {gid}"))


def invite(offset):
    ids = [str(base_uid + 5000 + offset * 10 + i) for i in range(3)]
    return call("POST", f"/groups/{gid}/members", token, {"user_ids": ids})


with ThreadPoolExecutor(max_workers=3) as pool:
    results = list(pool.map(invite, range(3)))

ok = sum(1 for r in results if r["code"] == 0)
print("并发结果:", [r["code"] for r in results], "成功数:", ok)

count = int(sql(f"SELECT member_count FROM `groups` WHERE id = {gid}"))
real = int(sql(f"SELECT COUNT(*) FROM group_members WHERE group_id = {gid}"))
print(f"member_count={count} real_members={real}")

assert count <= CAP, f"突破上限：{count} > {CAP}"
assert count == real, f"计数与成员行不一致：{count} vs {real}"
assert ok == 1, f"应只有一次邀请成功，实际 {ok}"
print("\n[PASS] 并发邀请未突破上限，且计数与成员行一致")
