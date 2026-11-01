package jwt

import (
	"errors"
	"strings"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"imchat/internal/config"
	"imchat/internal/pkg/resp"
)

const (
	testSecret = "unit-test-secret-0123456789abcdef"
	testIssuer = "imchat"
)

func newManager(expire time.Duration) *Manager {
	return New(config.JWTConfig{
		Secret: testSecret,
		Expire: config.Duration(expire),
		Issuer: testIssuer,
	})
}

// signWith 用给定算法与密钥直接签一个令牌，绕开 Manager.Generate，
// 才能构造出「算法不同 / 已过期 / 缺 exp」这类 Generate 本身不会产出的样本。
func signWith(t *testing.T, method jwtlib.SigningMethod, secret string, claims Claims) string {
	t.Helper()
	signed, err := jwtlib.NewWithClaims(method, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("构造令牌失败: %v", err)
	}
	return signed
}

func validClaims(expire time.Duration) Claims {
	now := time.Now()
	return Claims{
		UserID: 1001,
		Device: "web",
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer:    testIssuer,
			IssuedAt:  jwtlib.NewNumericDate(now),
			NotBefore: jwtlib.NewNumericDate(now),
			ExpiresAt: jwtlib.NewNumericDate(now.Add(expire)),
		},
	}
}

func TestExpireReturnsConfiguredValue(t *testing.T) {
	if got := newManager(2 * time.Hour).Expire(); got != 2*time.Hour {
		t.Fatalf("Expire() = %v, 期望 %v", got, 2*time.Hour)
	}
}

func TestGenerateParseRoundTrip(t *testing.T) {
	const expire = 2 * time.Hour
	m := newManager(expire)

	token, err := m.Generate(12345, "ios")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	claims, err := m.Parse(token)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if claims.UserID != 12345 {
		t.Errorf("UserID = %d, 期望 12345", claims.UserID)
	}
	if claims.Device != "ios" {
		t.Errorf("Device = %q, 期望 ios", claims.Device)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("Issuer = %q, 期望 %q", claims.Issuer, testIssuer)
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatalf("exp / iat 未写入: exp=%v iat=%v", claims.ExpiresAt, claims.IssuedAt)
	}
	// 有效期用 exp-iat 反推，不比对绝对时间，避免用例依赖机器时钟精度。
	if got := claims.ExpiresAt.Sub(claims.IssuedAt.Time); got != expire {
		t.Errorf("有效期 = %v, 期望 %v", got, expire)
	}
}

// 过期与非法必须分流：前端只在 40101 时刷新令牌，40100 会直接踢回登录页。
func TestParseReportsExpiredSeparately(t *testing.T) {
	m := newManager(-time.Minute)

	token, err := m.Generate(1, "web")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	_, err = m.Parse(token)
	if !errors.Is(err, resp.ErrTokenExpired) {
		t.Fatalf("过期令牌返回 %v, 期望 ErrTokenExpired", err)
	}
	var ae *resp.Error
	if !errors.As(err, &ae) || ae.Code != resp.CodeTokenExpired {
		t.Fatalf("业务码不是 %d: %v", resp.CodeTokenExpired, err)
	}
}

// 以下全部是伪造或畸形令牌，统一收敛成 ErrUnauthorized，不能漏成 500。
func TestParseRejectsForgedOrMalformedTokens(t *testing.T) {
	m := newManager(time.Hour)
	legit, err := m.Generate(1001, "web")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	parts := strings.Split(legit, ".")
	if len(parts) != 3 {
		t.Fatalf("令牌不是三段式: %q", legit)
	}
	tampered := parts[0] + "." + parts[1] + "." + flipLastRune(parts[2])

	foreignIssuer := validClaims(time.Hour)
	foreignIssuer.Issuer = "someone-else"

	noExpiry := validClaims(time.Hour)
	noExpiry.ExpiresAt = nil

	hs512 := validClaims(time.Hour)

	noneAlg, err := jwtlib.NewWithClaims(jwtlib.SigningMethodNone, validClaims(time.Hour)).
		SignedString(jwtlib.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("构造 alg=none 令牌失败: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"签名改一位", tampered},
		{"换了密钥签发", signWith(t, jwtlib.SigningMethodHS256, "another-secret-but-same-length", validClaims(time.Hour))},
		{"issuer 不一致", signWith(t, jwtlib.SigningMethodHS256, testSecret, foreignIssuer)},
		{"算法换成 HS512", signWith(t, jwtlib.SigningMethodHS512, testSecret, hs512)},
		{"缺少 exp", signWith(t, jwtlib.SigningMethodHS256, testSecret, noExpiry)},
		{"alg=none", noneAlg},
		{"空串", ""},
		{"不是三段", "abc.def"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims, err := m.Parse(c.token)
			if claims != nil {
				t.Fatalf("畸形令牌竟然返回了声明: %+v", claims)
			}
			if !errors.Is(err, resp.ErrUnauthorized) {
				t.Fatalf("返回 %v, 期望 ErrUnauthorized", err)
			}
		})
	}
}

// 签名合法但 uid 非正的令牌只可能来自签发侧 bug，必须在解析侧拦住，
// 否则中间件会把 uid=0 写进上下文，后续权限判断全部失去主体。
func TestParseRejectsNonPositiveUserID(t *testing.T) {
	m := newManager(time.Hour)

	for _, uid := range []int64{0, -1} {
		claims := validClaims(time.Hour)
		claims.UserID = uid
		token := signWith(t, jwtlib.SigningMethodHS256, testSecret, claims)

		if _, err := m.Parse(token); !errors.Is(err, resp.ErrUnauthorized) {
			t.Errorf("uid=%d 返回 %v, 期望 ErrUnauthorized", uid, err)
		}
	}
}

// flipLastRune 把签名末尾的字符换掉一位，保持长度与字符集不变，
// 这样校验失败发生在验签而不是 base64 解码，才真正测到签名比对。
func flipLastRune(s string) string {
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	if last == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}
