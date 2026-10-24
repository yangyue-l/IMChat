// Command smoke 是端到端冒烟测试工具：真实调用 HTTP 与 WebSocket 接口，
// 覆盖注册、好友、黑名单、私聊、群聊、朋友圈与实时推送全链路。
//
// 用法：go run ./cmd/smoke -base http://127.0.0.1:8080
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var (
	baseURL     string
	clusterBase string
	passed      int
	failed      int
	failures    []string
)

func main() {
	flag.StringVar(&baseURL, "base", "http://127.0.0.1:8080", "服务地址")
	flag.StringVar(&clusterBase, "cluster-base", "", "第二节点地址；提供时额外验证跨节点推送")
	flag.Parse()

	fmt.Println("=== IMChat 端到端冒烟测试 ===")
	fmt.Println("目标:", baseURL)
	fmt.Println()

	alice := mustUser("smoke_alice", "爱丽丝")
	bob := mustUser("smoke_bob", "鲍勃")
	carol := mustUser("smoke_carol", "卡罗尔")

	testFriendFlow(alice, bob)
	testPrivateChat(alice, bob, baseURL)
	if clusterBase != "" {
		testCrossNode(alice, bob)
	}
	testBlacklist(alice, bob)
	testGroupChat(alice, bob, carol)
	testMoments(alice, bob)
	testMarkRead(alice, bob)

	fmt.Println()
	fmt.Printf("=== 结果：通过 %d，失败 %d ===\n", passed, failed)
	for _, f := range failures {
		fmt.Println("  ✗", f)
	}
	if failed > 0 {
		os.Exit(1)
	}
	fmt.Println("全部通过")
}

// ---------- 测试用例 ----------

func testFriendFlow(a, b *account) {
	section("好友申请与通过")

	// 先清理可能存在的旧关系，保证用例可重复执行
	call("DELETE", "/friends/"+b.ID, a.Token)
	call("DELETE", "/blacklist/"+b.ID, a.Token)
	call("DELETE", "/blacklist/"+b.ID, b.Token)

	res := call("GET", "/users/search?keyword="+b.User.Username, a.Token)
	if !assert("搜索到目标用户", res.Code == 0 && bytes.Contains(res.Data, []byte(b.User.Username))) {
		return
	}

	res = call("POST", "/friend-requests", a.Token, map[string]any{
		"to_user_id": b.IDStr, "message": "冒烟测试：加个好友",
	})
	// 重复执行时可能已有待处理申请，视为就绪
	if res.Code != 0 && !strings.Contains(res.Msg, "已发送过申请") {
		assertWithMsg("alice 发起好友申请", false, res.Msg)
	} else {
		assert("好友申请就绪", true)
	}

	res = call("GET", "/friend-requests?type=incoming", b.Token)
	var reqs []struct {
		ID       string `json:"id"`
		Status   int8   `json:"status"`
		FromUser struct {
			Username string `json:"username"`
		} `json:"from_user"`
	}
	_ = json.Unmarshal(res.Data, &reqs)
	var reqID string
	for _, r := range reqs {
		// 只挑待处理的申请，历史记录不参与
		if r.FromUser.Username == a.User.Username && r.Status == 0 {
			reqID = r.ID
		}
	}
	if !assert("bob 收到申请", reqID != "") {
		return
	}

	res = call("POST", "/friend-requests/"+reqID+"/handle", b.Token, map[string]any{"accept": true})
	check("bob 同意申请", res)

	res = call("GET", "/friends", a.Token)
	assert("双向好友关系生效", res.Code == 0 && bytes.Contains(res.Data, []byte(b.User.Username)))

	// 重复申请应被拦截
	res = call("POST", "/friend-requests", a.Token, map[string]any{"to_user_id": b.IDStr})
	assert("重复申请被拒绝", res.Code != 0)
}

func testPrivateChat(a, b *account, nodeBase string) {
	section("私聊消息与实时推送")

	ws := dialWS(nodeBase, b.Token)
	defer ws.Close()
	time.Sleep(300 * time.Millisecond)

	convID := privateConvID(a.ID, b.ID)
	clientMsgID := fmt.Sprintf("smoke-private-%d", time.Now().UnixNano())

	res := call("POST", "/messages/private", a.Token, map[string]any{
		"to_id": b.IDStr, "msg_type": 1, "content": "你好，这是一条冒烟测试消息",
		"client_msg_id": clientMsgID,
	})
	if !assert("发送私聊消息成功", res.Code == 0 && bytes.Contains(res.Data, []byte("冒烟测试"))) {
		return
	}

	var sent struct {
		ID  string `json:"id"`
		Seq int64  `json:"seq"`
	}
	_ = json.Unmarshal(res.Data, &sent)

	// 幂等：同一 client_msg_id 重发，应返回同一条消息
	res2 := call("POST", "/messages/private", a.Token, map[string]any{
		"to_id": b.IDStr, "msg_type": 1, "content": "重复内容",
		"client_msg_id": clientMsgID,
	})
	var dup struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res2.Data, &dup)
	assert("相同 client_msg_id 幂等去重", dup.ID == sent.ID)

	frame := waitFrame(ws, "message", 3*time.Second)
	if assert("bob 通过 WebSocket 收到消息", frame != nil) {
		var payload struct {
			Data struct {
				Content string `json:"content"`
				ConvID  string `json:"conv_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(frame, &payload)
		assert("推送内容与发送一致", strings.Contains(payload.Data.Content, "冒烟测试"))
		assert("会话 ID 一致", payload.Data.ConvID == convID)
	}

	res = call("GET", "/messages?conv_id="+convID+"&limit=10", a.Token)
	assert("历史消息可拉取", res.Code == 0 && bytes.Contains(res.Data, []byte("冒烟测试消息")))

	res = call("GET", "/conversations", b.Token)
	assert("bob 会话列表出现未读", res.Code == 0 && bytes.Contains(res.Data, []byte(convID)))

	// 撤回
	res = call("POST", "/messages/"+sent.ID+"/recall", a.Token)
	check("撤回自己发送的消息", res)

	res = call("POST", "/messages/"+sent.ID+"/recall", b.Token)
	assert("他人不能撤回", res.Code != 0)
}

func testBlacklist(a, b *account) {
	section("黑名单拦截")

	res := call("POST", "/blacklist", a.Token, map[string]any{"user_id": b.IDStr})
	check("alice 拉黑 bob", res)

	res = call("POST", "/messages/private", b.Token, map[string]any{
		"to_id": a.IDStr, "msg_type": 1, "content": "被拉黑后还能发吗",
		"client_msg_id": fmt.Sprintf("smoke-blocked-%d", time.Now().UnixNano()),
	})
	assert("拉黑后消息被拦截", res.Code != 0)

	res = call("GET", "/blacklist", a.Token)
	assert("黑名单列表包含 bob", res.Code == 0 && bytes.Contains(res.Data, []byte(b.User.Username)))

	res = call("DELETE", "/blacklist/"+b.ID, a.Token)
	check("解除拉黑", res)

	res = call("POST", "/messages/private", b.Token, map[string]any{
		"to_id": a.IDStr, "msg_type": 1, "content": "解除拉黑后恢复",
		"client_msg_id": fmt.Sprintf("smoke-unblocked-%d", time.Now().UnixNano()),
	})
	check("解除后恢复通信", res)
}

func testGroupChat(a, b, c *account) {
	section("群聊")

	name := fmt.Sprintf("冒烟测试群-%d", time.Now().Unix()%10000)
	res := call("POST", "/groups", a.Token, map[string]any{
		"name": name, "member_ids": []string{b.IDStr, c.IDStr},
	})
	if !assertWithMsg("建群成功", res.Code == 0, res.Msg) {
		return
	}
	var group struct {
		ID          string `json:"id"`
		MemberCount int    `json:"member_count"`
		MaxMember   int    `json:"max_member"`
	}
	_ = json.Unmarshal(res.Data, &group)
	assert("群成员数为 3", group.MemberCount == 3)
	assertWithMsg("群成员上限为 1000", group.MaxMember == 1000, fmt.Sprintf("max_member=%d", group.MaxMember))

	res = call("GET", "/groups/"+group.ID, a.Token)
	assert("群详情包含成员列表", res.Code == 0 && bytes.Contains(res.Data, []byte(c.User.Username)))

	res = call("POST", "/groups/"+group.ID+"/members", a.Token, map[string]any{
		"user_ids": []string{},
	})
	assert("空邀请被拒绝", res.Code != 0)

	ws := dialWS(baseURL, b.Token)
	defer ws.Close()
	time.Sleep(300 * time.Millisecond)

	res = call("POST", "/messages/group", a.Token, map[string]any{
		"group_id": group.ID, "msg_type": 1, "content": "群里的大家好",
		"client_msg_id": fmt.Sprintf("smoke-group-%d", time.Now().UnixNano()),
	})
	check("发送群消息", res)
	var groupMsg struct {
		Seq int64 `json:"seq"`
	}
	_ = json.Unmarshal(res.Data, &groupMsg)

	frame := waitFrame(ws, "message", 3*time.Second)
	assert("群成员收到群消息", frame != nil && bytes.Contains(frame, []byte("群里的大家好")))

	// 读扩散验证：成员从未写过会话行，也能在会话列表看到该群与未读数
	convID := "g_" + group.ID
	unread, found := convUnread(c.Token, convID)
	assertWithMsg("群会话通过读扩散出现在成员列表", found, "会话列表中未找到该群")
	assertWithMsg("群未读数由 seq 差值算出", found && unread == 1, fmt.Sprintf("unread=%d", unread))

	// 已读后未读归零，同样是算出来的
	call("POST", "/messages/read", b.Token, map[string]any{"conv_id": convID, "seq": groupMsg.Seq})
	unreadAfter, _ := convUnread(b.Token, convID)
	assertWithMsg("群已读后未读归零", unreadAfter == 0, fmt.Sprintf("unread=%d", unreadAfter))

	// 非成员不能发言
	res = call("POST", "/messages/group", mustUser("smoke_outsider", "路人").Token, map[string]any{
		"group_id": group.ID, "msg_type": 1, "content": "我不是成员",
		"client_msg_id": fmt.Sprintf("smoke-outsider-%d", time.Now().UnixNano()),
	})
	assert("非群成员发言被拒绝", res.Code != 0)

	// 管理员操作
	res = call("POST", "/groups/"+group.ID+"/admin", a.Token, map[string]any{
		"user_id": b.IDStr, "admin": true,
	})
	check("群主设置管理员", res)

	// 退群
	res = call("DELETE", "/groups/"+group.ID+"/members/"+c.ID, c.Token)
	check("成员自主退群", res)

	res = call("GET", "/groups/"+group.ID, c.Token)
	assert("退群后无法查看群", res.Code != 0)
}

func testMoments(a, b *account) {
	section("朋友圈")

	res := call("POST", "/moments", a.Token, map[string]any{
		"content": "冒烟测试动态：今天天气不错", "visibility": 2,
	})
	if !assert("发布动态", res.Code == 0) {
		return
	}
	var moment struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Data, &moment)

	res = call("GET", "/moments/timeline?limit=20", b.Token)
	assert("好友时间线可见", res.Code == 0 && bytes.Contains(res.Data, []byte("冒烟测试动态")))

	res = call("POST", "/moments/"+moment.ID+"/like", b.Token)
	check("bob 点赞", res)

	res = call("GET", "/moments/timeline?limit=20", a.Token)
	assert("点赞数与点赞人被记录", res.Code == 0 && bytes.Contains(res.Data, []byte(`"like_count":1`)))

	res = call("POST", "/moments/"+moment.ID+"/comments", b.Token, map[string]any{
		"content": "确实不错",
	})
	var comment struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Data, &comment)
	check("bob 评论", res)

	res = call("GET", "/moments/"+moment.ID+"/comments", a.Token)
	assert("评论列表可查", res.Code == 0 && bytes.Contains(res.Data, []byte("确实不错")))

	// 拉黑后不可见
	call("POST", "/blacklist", a.Token, map[string]any{"user_id": b.IDStr})
	res = call("GET", "/moments/timeline?limit=20", b.Token)
	assert("被拉黑后看不到对方动态", res.Code == 0 && !bytes.Contains(res.Data, []byte("冒烟测试动态")))
	call("DELETE", "/blacklist/"+b.ID, a.Token)

	res = call("DELETE", "/moments/"+moment.ID, b.Token)
	assert("他人不能删除我的动态", res.Code != 0)

	res = call("DELETE", "/moments/"+moment.ID, a.Token)
	check("作者删除动态", res)
}

func testMarkRead(a, b *account) {
	section("已读回执")

	convID := privateConvID(a.ID, b.ID)
	res := call("POST", "/messages/private", a.Token, map[string]any{
		"to_id": b.IDStr, "msg_type": 1, "content": "已读测试",
		"client_msg_id": fmt.Sprintf("smoke-read-%d", time.Now().UnixNano()),
	})
	var msg struct {
		Seq int64 `json:"seq"`
	}
	_ = json.Unmarshal(res.Data, &msg)

	ws := dialWS(baseURL, a.Token)
	defer ws.Close()
	time.Sleep(200 * time.Millisecond)

	res = call("POST", "/messages/read", b.Token, map[string]any{
		"conv_id": convID, "seq": msg.Seq,
	})
	check("上报已读位置", res)

	res = call("GET", "/conversations", b.Token)
	var convs []struct {
		ConvID string `json:"conv_id"`
		Unread int64  `json:"unread"`
	}
	_ = json.Unmarshal(res.Data, &convs)
	unread := int64(-1)
	for _, cv := range convs {
		if cv.ConvID == convID {
			unread = cv.Unread
		}
	}
	assert("已读后未读清零", unread == 0)
}

// ---------- 基础设施 ----------

type account struct {
	ID    string
	IDStr string
	Token string
	User  struct {
		Username string `json:"username"`
		Nickname string `json:"nickname"`
	} `json:"user"`
}

// mustUser 注册或登录一个测试账号。
func mustUser(username, nickname string) *account {
	body := map[string]any{"username": username, "password": "smoke123456", "nickname": nickname, "device": "smoke"}
	res := rawCall("POST", "/auth/register", "", body)

	if res.Code != 0 {
		// 已注册过则直接登录
		res = rawCall("POST", "/auth/login", "", map[string]any{
			"username": username, "password": "smoke123456", "device": "smoke",
		})
		if res.Code != 0 {
			fmt.Printf("无法准备账号 %s: %s\n", username, res.Msg)
			os.Exit(1)
		}
	}

	var data struct {
		Token string `json:"token"`
		User  struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Nickname string `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(res.Data, &data); err != nil {
		fmt.Println("解析登录响应失败:", err)
		os.Exit(1)
	}
	a := &account{ID: data.User.ID, IDStr: data.User.ID, Token: data.Token}
	a.User.Username = data.User.Username
	a.User.Nickname = data.User.Nickname
	return a
}

type apiResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func rawCall(method, path, token string, body any) apiResp {
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, err := http.NewRequest(method, baseURL+"/api/v1"+path, buf)
	if err != nil {
		return apiResp{Code: -1, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return apiResp{Code: -1, Msg: err.Error()}
	}
	defer resp.Body.Close()

	var out apiResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return apiResp{Code: -1, Msg: "响应解析失败: " + err.Error()}
	}
	return out
}

func call(method, path, token string, body ...any) apiResp {
	var payload any
	if len(body) > 0 {
		payload = body[0]
	}
	return rawCall(method, path, token, payload)
}

func dialWS(base, token string) *websocket.Conn {
	u := strings.Replace(base, "http://", "ws://", 1) + "/ws?token=" + token + "&device=smoke"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		fmt.Println("WebSocket 连接失败:", err)
		os.Exit(1)
	}
	return conn
}

// testCrossNode 验证跨节点推送：观察者连在第二节点，发送方通过第一节点投递。
// 该用例只有在 cluster.enabled=true 且两个节点共用同一 Redis 时才会通过。
func testCrossNode(a, b *account) {
	section("跨节点推送（集群模式）")

	ws := dialWS(clusterBase, b.Token)
	defer ws.Close()
	time.Sleep(400 * time.Millisecond)

	convID := privateConvID(a.ID, b.ID)
	res := call("POST", "/messages/private", a.Token, map[string]any{
		"to_id": b.IDStr, "msg_type": 1, "content": "跨节点测试消息",
		"client_msg_id": fmt.Sprintf("smoke-cross-%d", time.Now().UnixNano()),
	})
	if !assertWithMsg("节点 1 发送成功", res.Code == 0, res.Msg) {
		return
	}

	frame := waitFrame(ws, "message", 3*time.Second)
	if assert("节点 2 的在线用户收到消息", frame != nil && bytes.Contains(frame, []byte("跨节点测试消息"))) {
		var payload struct {
			Data struct {
				ConvID string `json:"conv_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(frame, &payload)
		assert("跨节点会话 ID 一致", payload.Data.ConvID == convID)
	}
}

// waitFrame 等待指定类型的下行帧，忽略心跳等其他帧。
func waitFrame(conn *websocket.Conn, want string, timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		var f struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &f) == nil && f.Type == want {
			return data
		}
	}
	return nil
}

// convUnread 从会话列表中取出指定会话的未读数。
func convUnread(token, convID string) (int64, bool) {
	res := call("GET", "/conversations", token)
	if res.Code != 0 {
		return 0, false
	}
	var list []struct {
		ConvID string `json:"conv_id"`
		Unread int64  `json:"unread"`
	}
	if err := json.Unmarshal(res.Data, &list); err != nil {
		return 0, false
	}
	for _, c := range list {
		if c.ConvID == convID {
			return c.Unread, true
		}
	}
	return 0, false
}

func privateConvID(a, b string) string {
	ai, _ := strconv.ParseInt(a, 10, 64)
	bi, _ := strconv.ParseInt(b, 10, 64)
	if ai > bi {
		ai, bi = bi, ai
	}
	return "p_" + strconv.FormatInt(ai, 10) + "_" + strconv.FormatInt(bi, 10)
}

func section(name string) {
	fmt.Printf("\n--- %s ---\n", name)
}

func check(name string, res apiResp) {
	if res.Code == 0 {
		passed++
		fmt.Printf("  ✓ %s\n", name)
		return
	}
	failed++
	msg := fmt.Sprintf("%s: %s", name, res.Msg)
	failures = append(failures, msg)
	fmt.Printf("  ✗ %s (%s)\n", name, res.Msg)
}

func assert(name string, ok bool) bool {
	return assertWithMsg(name, ok, "")
}

func assertWithMsg(name string, ok bool, detail string) bool {
	if ok {
		passed++
		fmt.Printf("  ✓ %s\n", name)
		return true
	}
	failed++
	entry := name
	if detail != "" {
		entry += ": " + detail
	}
	failures = append(failures, entry)
	fmt.Printf("  ✗ %s\n", entry)
	return false
}
