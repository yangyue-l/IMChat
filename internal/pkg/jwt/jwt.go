// Package jwt 封装令牌签发与解析，向业务层只暴露 Claims 与统一错误。
package jwt

import (
	"errors"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"imchat/internal/config"
	"imchat/internal/pkg/resp"
)

// Claims 是在标准声明之外附加的业务声明。
type Claims struct {
	UserID int64  `json:"uid"`
	Device string `json:"did"`
	jwtlib.RegisteredClaims
}

// Manager 负责令牌的签发与校验，无状态、可并发使用。
type Manager struct {
	secret []byte
	expire time.Duration
	issuer string
}

// New 创建令牌管理器。
func New(cfg config.JWTConfig) *Manager {
	return &Manager{
		secret: []byte(cfg.Secret),
		expire: cfg.Expire.Get(),
		issuer: cfg.Issuer,
	}
}

// Expire 返回令牌有效期，供前端计算续期时机。
func (m *Manager) Expire() time.Duration { return m.expire }

// Generate 为指定用户与设备签发令牌。
func (m *Manager) Generate(userID int64, device string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Device: device,
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   "",
			IssuedAt:  jwtlib.NewNumericDate(now),
			NotBefore: jwtlib.NewNumericDate(now),
			ExpiresAt: jwtlib.NewNumericDate(now.Add(m.expire)),
		},
	}
	token := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// Parse 校验令牌并返回声明；过期与非法分别返回不同业务错误。
func (m *Manager) Parse(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	// 算法白名单交给 WithValidMethods 而不是在 keyfunc 里断言 HMAC 类型：
	// 类型断言只挡得住非 HMAC 家族（none/RS/ES），HS384/HS512 会照样放行，
	// 而签发侧只有 Generate 的 HS256，多放行一种就等于凭空多一条验签路径。
	token, err := jwtlib.ParseWithClaims(tokenStr, claims, func(*jwtlib.Token) (any, error) {
		return m.secret, nil
	},
		jwtlib.WithIssuer(m.issuer),
		jwtlib.WithExpirationRequired(),
		jwtlib.WithValidMethods([]string{jwtlib.SigningMethodHS256.Alg()}),
	)

	if err != nil {
		if errors.Is(err, jwtlib.ErrTokenExpired) {
			return nil, resp.ErrTokenExpired
		}
		return nil, resp.ErrUnauthorized
	}
	if !token.Valid || claims.UserID <= 0 {
		return nil, resp.ErrUnauthorized
	}
	return claims, nil
}
