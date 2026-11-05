package service

import (
	"context"
	"regexp"

	"imchat/internal/pkg/resp"
)

// devicePattern 限制设备标识的字符集。
//
// 设备名来自登录请求的 device 字段，会原样进入 zset 成员、访问日志与接口返回体：
// 不限制的话，一串带换行或控制符的内容就能把日志排版搅乱，排障时反而认不出
// 是哪台设备。长度上限 32 与握手处的截断值保持一致 —— 两边不一致会出现
// 「握手时被截成 32 位、踢线时按原名找不到」这种只有生产才复现的问题。
var devicePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Sessions 返回当前账号在线的设备名。
//
// 未装配在线能力（单测、只装 DB 的工具链）时按空列表处理而不是 panic，
// 与 Online 的降级口径一致。返回空切片而非 nil：JSON 里 [] 与 null 是两种类型，
// 客户端不该为此多写一个分支。
func (s *UserService) Sessions(ctx context.Context, uid int64) ([]string, error) {
	devices, err := s.onlineDevices(ctx, uid)
	if err != nil {
		return nil, err
	}
	if devices == nil {
		return []string{}, nil
	}
	return devices, nil
}

// KickDevice 踢掉指定设备的登录，返回错误表示这次踢线没有发生。
//
// 允许踢自己当前的设备：客户端收到关闭码 4001 后会重连，等价于「退出后立刻重登」，
// 没有理由单独拦一道。真正要拦的是「踢一台根本不在线的设备」—— 那多半是界面上
// 的过期数据，回 404 比回成功更能说明问题。
//
// 先校验在线再动手，是因为踢线的唯一凭据就是设备名：不存在的设备名让底层
// 静默返回 0 个断开连接，调用方却以为踢成功了。
func (s *UserService) KickDevice(ctx context.Context, uid int64, device string) error {
	if err := validateDevice(device); err != nil {
		return err
	}
	if s.deps.Kicker == nil {
		// 没有踢线能力时明确报错，不假装成功：用户以为「那台设备已下线」、
		// 实际它还连着，这是账号安全场景里最坏的假阳性。
		return resp.WithMsg(resp.ErrInternal, "当前部署未启用连接管理")
	}

	devices, err := s.onlineDevices(ctx, uid)
	if err != nil {
		return err
	}
	found := false
	for _, d := range devices {
		if d == device {
			found = true
			break
		}
	}
	if !found {
		return resp.WithMsg(resp.ErrNotFound, "该设备不在线")
	}

	s.deps.Kicker.KickDevice(uid, device)
	return nil
}

// onlineDevices 取在线设备，未装配在线能力时返回 nil 表示「无从得知」。
// 与 Sessions 的空切片区分开：nil 是「没有这个能力」，空切片是「确实没有在线设备」。
func (s *UserService) onlineDevices(ctx context.Context, uid int64) ([]string, error) {
	if s.deps.Presence == nil {
		return nil, nil
	}
	return s.deps.Presence.Devices(uid)
}

// validateDevice 校验设备标识。
func validateDevice(device string) error {
	if !devicePattern.MatchString(device) {
		return resp.WithMsg(resp.ErrInvalidParam, "设备标识需为 1-32 位字母、数字、下划线或短横线")
	}
	return nil
}
