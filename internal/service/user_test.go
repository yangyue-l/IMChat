package service

import (
	"context"
	"errors"
	"testing"
)

// stubPresence 是 Presence 的测试替身，只关心「实现被调用了没有」。
type stubPresence struct {
	online map[int64]bool
	err    error
	calls  [][]int64

	devices     []string
	devicesErr  error
	deviceCalls []int64
}

func (s *stubPresence) Online(uids []int64) (map[int64]bool, error) {
	s.calls = append(s.calls, uids)
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[int64]bool, len(uids))
	for _, uid := range uids {
		if s.online[uid] {
			out[uid] = true
		}
	}
	return out, nil
}

func (s *stubPresence) Devices(uid int64) ([]string, error) {
	s.deviceCalls = append(s.deviceCalls, uid)
	if s.devicesErr != nil {
		return nil, s.devicesErr
	}
	return s.devices, nil
}

// TestOnlineSelfSkipsRelation 验证查自己不需要走关系校验。
//
// 这里 rel 是 nil，只要代码走进拉黑/好友判定就会 panic —— 反过来说明
// 「查自己」这条路径是直连 Presence 的，不会被关系判定拦住。
func TestOnlineSelfSkipsRelation(t *testing.T) {
	p := &stubPresence{online: map[int64]bool{7: true}}
	svc := &UserService{deps: Deps{Presence: p}}

	online, err := svc.Online(context.Background(), 7, 7)
	if err != nil {
		t.Fatalf("查自己不应报错: %v", err)
	}
	if !online {
		t.Fatal("期望在线")
	}
	if len(p.calls) != 1 || len(p.calls[0]) != 1 || p.calls[0][0] != 7 {
		t.Fatalf("应恰好查询一次 uid 7，实际 %v", p.calls)
	}
}

// TestOnlineWithoutPresence 验证未装配在线能力时安全降级为离线。
//
// 单测与只装配了 DB 的工具链不会注入 Presence，此时必须返回离线而不是 panic，
// 也不能白跑一次关系查询（能力都没有，判定结果没有意义）。
func TestOnlineWithoutPresence(t *testing.T) {
	svc := &UserService{deps: Deps{}}
	online, err := svc.Online(context.Background(), 7, 7)
	if err != nil {
		t.Fatalf("未装配 Presence 不应报错: %v", err)
	}
	if online {
		t.Fatal("未装配 Presence 时应降级为离线")
	}
}

// TestOnlinePropagatesPresenceError 验证查询失败不会被吞成「离线」。
//
// 「查不到」和「不在线」是两件事：把 error 降级成 false，Redis 一抖动
// 全站用户就集体显示离线，而且日志里什么都看不到。
func TestOnlinePropagatesPresenceError(t *testing.T) {
	boom := errors.New("redis down")
	svc := &UserService{deps: Deps{Presence: &stubPresence{err: boom}}}

	if _, err := svc.Online(context.Background(), 7, 7); !errors.Is(err, boom) {
		t.Fatalf("应透出 Presence 的错误，实际 %v", err)
	}
}
