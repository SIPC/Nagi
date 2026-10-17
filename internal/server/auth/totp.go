package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP（RFC 6238）：30 秒一步，6 位数字，HMAC-SHA1，和常见的验证器 App 兼容。

const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret 生成 160 位的随机密钥（base32）。
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPURI 返回验证器 App 扫码用的 otpauth:// 地址。
func TOTPURI(secret, account, issuer string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// checkTOTP 校验验证码，允许前后各一步的时间误差。返回匹配的时间步（用于防止同一个码用两次）。
func checkTOTP(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / totpStep
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		if hmac.Equal([]byte(totpCode(key, s)), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}
