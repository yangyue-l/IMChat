package service

import (
	"context"
	"errors"
	"testing"

	"imchat/internal/pkg/resp"
)

// kickCall 记录一次踢线调用，便于断言「踢的是谁、踢的哪台设备」。
type kickCall struct {
	uid    int64
	device string
}

// stubKicker 是 DeviceKicker 的测试替身。
type stubKicker struct{ calls []kickCall }

func (k *stubKicker) KickDevice(uid int64, device string) int {
	k.calls = append(k.calls, kickCall{uid: uid, device: device})
	return 1
}

// codeOf 取错误的业务码，非 *resp.Error 时返回 -1（说明抛的是原始错误）。
func codeOf(err error) int {
	var ae *resp.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return -1
}

// TestValidateDevice 覆盖设备标识的字符集与长度边界。
//
// 设备名会原样写进 zset 成员、访问日志和接口返回体，放任换行与空格进来，
// 排障时日志里就分不清「一行是一条记录」；长度上限必须与握手处的 32 位截断一致，
// 否则登录时被截短的名字在踢线接口里永远匹配不到。
func TestValidateDevice(t *testing.T) {
	cases := []struct {
		name   string
		device string
		wantOK bool
	}{
		{"常见端标识", "web", true},
		{"带短横线与下划线", "ios-17_pro", true},
		{"纯数字也放行", "2", true},
		{"空串被拒", "", false},
		{"含空格被拒", "web pad", false},
		{"含换行被拒", "web\npad", false},
		{"中文被拒", "网页", false},
		{"含斜杠被拒", "web/pad", false},
		{"32 位刚好放行", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"33 位被拒", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDevice(tc.device)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("期望通过，实际 %v", err)
				}
				return
			}
			if code := codeOf(err); code != resp.CodeInvalidParam {
				t.Fatalf("期望 %d，实际 code=%d err=%v", resp.CodeInvalidParam, code, err)
			}
		})
	}
}

// TestSessionsReturnsDevices 验证设备列表原样透传。
func TestSessionsReturnsDevices(t *testing.T) {
	p := &stubPresence{devices: []string{"web", "ios"}}
	svc := &UserService{deps: Deps{Presence: p}}

	devices, err := svc.Sessions(context.Background(), 7)
	if err != nil {
		t.Fatalf("查询设备列表不应报错: %v", err)
	}
	if len(devices) != 2 || devices[0] != "web" || devices[1] != "ios" {
		t.Fatalf("期望 [web ios]，实际 %v", devices)
	}
	if len(p.deviceCalls) != 1 || p.deviceCalls[0] != 7 {
		t.Fatalf("应恰好查询一次 uid 7，实际 %v", p.deviceCalls)
	}
}

// TestSessionsWithoutPresence 验证未装配在线能力时返回空切片而不是 nil。
//
// 这里返回 nil 的话，handler 组出来的 data.devices 会是 null，客户端按数组遍历
// 直接炸；而空切片至少是「这个账号当前没有在线设备」这个语义。
func TestSessionsWithoutPresence(t *testing.T) {
	svc := &UserService{deps: Deps{}}

	devices, err := svc.Sessions(context.Background(), 7)
	if err != nil {
		t.Fatalf("未装配 Presence 不应报错: %v", err)
	}
	if devices == nil {
		t.Fatal("应返回空切片而不是 nil")
	}
	if len(devices) != 0 {
		t.Fatalf("期望空列表，实际 %v", devices)
	}
}

// TestSessionsPropagatesPresenceError 验证回源失败会如实上报。
//
// 不能吞成空列表：用户会以为「只剩本机登录着」，而这正是账号被盗时
// 最不该给的结论。
func TestSessionsPropagatesPresenceError(t *testing.T) {
	wantErr := errors.New("redis down")
	svc := &UserService{deps: Deps{Presence: &stubPresence{devicesErr: wantErr}}}

	devices, err := svc.Sessions(context.Background(), 7)
	if !errors.Is(err, wantErr) {
		t.Fatalf("期望原样上抛 %v，实际 %v", wantErr, err)
	}
	if devices != nil {
		t.Fatalf("出错时不应返回数据，实际 %v", devices)
	}
}

// TestKickDeviceRejectsBadDeviceBeforeLookup 验证非法设备名在查在线列表之前就被拦下。
//
// 设备名校验是唯一能挡住「拿奇怪字符串去探测在线表」的地方，放到底层去拦，
// 意味着每个非法请求都会先打一次 Redis 或一次本地分片遍历。
func TestKickDeviceRejectsBadDeviceBeforeLookup(t *testing.T) {
	p := &stubPresence{devices: []string{"web"}}
	k := &stubKicker{}
	svc := &UserService{deps: Deps{Presence: p, Kicker: k}}

	err := svc.KickDevice(context.Background(), 7, "web pad")
	if code := codeOf(err); code != resp.CodeInvalidParam {
		t.Fatalf("期望 %d，实际 code=%d err=%v", resp.CodeInvalidParam, code, err)
	}
	if len(p.deviceCalls) != 0 {
		t.Fatalf("非法设备名不该触发在线查询，实际 %v", p.deviceCalls)
	}
	if len(k.calls) != 0 {
		t.Fatalf("非法设备名不该触发踢线，实际 %v", k.calls)
	}
}

// TestKickDeviceUnknownDevice 验证踢一台不在线的设备返回 404 而不是静默成功。
func TestKickDeviceUnknownDevice(t *testing.T) {
	k := &stubKicker{}
	svc := &UserService{deps: Deps{Presence: &stubPresence{devices: []string{"web"}}, Kicker: k}}

	err := svc.KickDevice(context.Background(), 7, "ios")
	if code := codeOf(err); code != resp.CodeNotFound {
		t.Fatalf("期望 %d，实际 code=%d err=%v", resp.CodeNotFound, code, err)
	}
	if len(k.calls) != 0 {
		t.Fatalf("不在线的设备不该触发踢线，实际 %v", k.calls)
	}
}

// TestKickDeviceAllowsSelfKick 验证踢自己的当前设备是允许的。
//
// 客户端收到 4001 会重连，等价于「退出后立刻重登」，没有理由单独拦一道；
// 拦掉反而会让「我在别处点了退出、这台也跟着掉线」这类正常诉求无路可走。
func TestKickDeviceAllowsSelfKick(t *testing.T) {
	k := &stubKicker{}
	p := &stubPresence{devices: []string{"web", "ios"}}
	svc := &UserService{deps: Deps{Presence: p, Kicker: k}}

	if err := svc.KickDevice(context.Background(), 7, "web"); err != nil {
		t.Fatalf("踢当前设备不应报错: %v", err)
	}
	if len(k.calls) != 1 {
		t.Fatalf("应恰好踢一次，实际 %v", k.calls)
	}
	if k.calls[0].uid != 7 || k.calls[0].device != "web" {
		t.Fatalf("踢错了对象: %+v", k.calls[0])
	}
}

// TestKickDeviceWithoutKicker 验证没有踢线能力时报错而不是假装成功。
func TestKickDeviceWithoutKicker(t *testing.T) {
	svc := &UserService{deps: Deps{Presence: &stubPresence{devices: []string{"web"}}}}

	err := svc.KickDevice(context.Background(), 7, "web")
	if code := codeOf(err); code != resp.CodeInternal {
		t.Fatalf("期望 %d，实际 code=%d err=%v", resp.CodeInternal, code, err)
	}
}
