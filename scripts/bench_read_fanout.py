"""读扩散实测：建 1000 人群，发一条消息，统计实际写入的 SQL 语句数量。"""
import json
import random
import subprocess
import time
import urllib.request

BASE = "http://127.0.0.1:8080/api/v1"


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read())


def sql(q, quiet=False):
    r = subprocess.run(
        ["docker", "exec", "imchat-mysql", "mysql", "-uroot", "-proot123456", "imchat", "-N", "-B", "-e", q],
        capture_output=True, text=True,
    )
    if r.returncode != 0 and not quiet:
        raise RuntimeError("SQL 执行失败: " + r.stderr.strip())
    return (r.stdout or "").strip()


def redis(*args):
    subprocess.run(["docker", "exec", "imchat-redis", "redis-cli"] + list(args),
                   capture_output=True, text=True)


def main():
    suffix = str(int(time.time()))[-7:]
    reg = call("POST", "/auth/register", body={
        "username": "bench_" + suffix, "password": "bench123456", "nickname": "压测群主"})
    token = reg["data"]["token"]

    grp = call("POST", "/groups", token, {"name": "读扩散实测群" + suffix, "member_ids": []})
    gid = grp["data"]["id"]
    print(f"群 ID: {gid}")

    # 用递归 CTE 一次补齐 999 个假成员（真实邀请接口单次限 20 人，这里直接压库）
    # 主键用随机基数偏移，避免多次运行撞主键被 IGNORE 掉
    base = random.randint(10**9, 10**12)
    sql(f"""
    INSERT IGNORE INTO group_members (id, group_id, user_id, role, joined_at, created_at, updated_at)
    WITH RECURSIVE seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 999)
    SELECT {base} + n, {gid}, 900000000000000000 + n, 0, NOW(), NOW(), NOW() FROM seq
    """)
    total = sql(f"SELECT COUNT(*) FROM group_members WHERE group_id={gid}")
    sql(f"UPDATE `groups` SET member_count={total} WHERE id={gid}")
    print(f"群成员数: {total}")
    if int(total) < 1000:
        raise RuntimeError("成员补齐失败，实际 " + total)

    # 清掉成员缓存，确保发送时真的要处理 1000 人
    redis("DEL", f"im:gmemberids:{gid}")

    # 只记录发消息期间产生的 SQL
    sql("SET GLOBAL log_output='TABLE'")
    sql("SET GLOBAL general_log='OFF'")
    sql("TRUNCATE TABLE mysql.general_log")
    sql("SET GLOBAL general_log='ON'")

    t0 = time.time()
    call("POST", "/messages/group", token, {
        "group_id": gid, "msg_type": 1, "content": "读扩散实测消息",
        "client_msg_id": "bench-" + suffix,
    })
    cost = (time.time() - t0) * 1000

    sql("SET GLOBAL general_log='OFF'")

    # GORM 开启 PrepareStmt，服务端走 prepared statement 协议，
    # general_log 里记录为 command_type='Execute' 而不是 'Query'
    q = "SELECT COUNT(*) FROM mysql.general_log WHERE command_type='Execute'"
    all_sql = sql(q)
    msg_insert = sql(q + " AND argument LIKE 'INSERT INTO `messages`%'")
    conv_write = sql(q + " AND argument LIKE '%conversations%'")
    grp_update = sql(q + " AND argument LIKE '%UPDATE `groups`%'")
    other = sql(q + " AND argument NOT LIKE '%messages%' AND argument NOT LIKE '%`groups`%' "
                   "AND argument NOT LIKE '%conversations%'")

    print()
    print("=" * 60)
    print(f"群成员数:                  {total}")
    print(f"发送一条群消息耗时:        {cost:.1f} ms")
    print(f"期间 SQL 执行次数:         {all_sql}  (prepared statement Execute)")
    print(f"  messages 插入:           {msg_insert}")
    print(f"  conversations 写入:      {conv_write}   <- 读扩散应为 0")
    print(f"  groups 指针更新:         {grp_update}")
    print(f"  其他(校验/查询):         {other}")
    print("=" * 60)

    convs = call("GET", "/conversations", token)["data"]
    target = next((c for c in convs if c["conv_id"] == "g_" + gid), None)
    if target:
        print(f"会话列表读到该群:  是  未读={target['unread']} 摘要=\"{target['last_msg_preview']}\" 人数={target['member_count']}")
    else:
        print("会话列表读到该群:  否  <- 异常")


main()
