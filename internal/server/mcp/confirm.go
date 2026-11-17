package mcp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// 两步操作的确认码（见 docs/mcp.md#确认码）：预览时给出，用户同意后带着它再调用一次才执行。
// 确认码由面板进程启动时生成的随机密钥、令牌、时间段和工具参数算出（HMAC-SHA256 取前 6 字节）：
//   - 预览之后改了任何一项参数都对不上；
//   - 调用方自己算不出来，只能来自预览，agent 跳过预览直接执行会被拦下；
//   - 别的令牌拿到的确认码用不了；面板重启以后作废；
//   - 有效期至少 confirmWindow，至多两倍（按时间段算，当前和上一个时间段的都认）。
//
// 这只保证执行的就是预览过的操作，不能证明用户同意过：用户同意靠 agent 和客户端的确认提示，不是权限边界。

// confirmWindow 是确认码的时间段长度。
const confirmWindow = 10 * time.Minute

// confirmSecret 是面板进程的随机密钥，不落盘。
var confirmSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

// confirmNow 是当前时间，测试时替换。
var confirmNow = time.Now

// confirmCodeAt 是令牌 tokenID 在时间段 slot 里对 parts 的确认码。
func confirmCodeAt(userID, tokenID, slot int64, parts ...string) string {
	mac := hmac.New(sha256.New, confirmSecret)
	mac.Write([]byte(strconv.FormatInt(userID, 10) + "\x00" + strconv.FormatInt(tokenID, 10) + "\x00" + strconv.FormatInt(slot, 10)))
	for _, p := range parts {
		mac.Write([]byte{0})
		mac.Write([]byte(p))
	}
	return hex.EncodeToString(mac.Sum(nil)[:6])
}

func confirmSlot() int64 { return confirmNow().Unix() / int64(confirmWindow/time.Second) }

// confirmCode 是这次预览给出的确认码。
func (c *call) confirmCode(parts ...string) string {
	return confirmCodeAt(c.u.ID, c.u.TokenID, confirmSlot(), parts...)
}

// confirm 返回这次预览的确认码，以及调用方给的 got 是不是同一个令牌对同样的参数预览得到、还没过期的确认码。
func (c *call) confirm(got string, parts ...string) (code string, ok bool) {
	slot := confirmSlot()
	code = confirmCodeAt(c.u.ID, c.u.TokenID, slot, parts...)
	if got == "" {
		return code, false
	}
	for _, s := range []int64{slot, slot - 1} {
		if hmac.Equal([]byte(got), []byte(confirmCodeAt(c.u.ID, c.u.TokenID, s, parts...))) {
			return code, true
		}
	}
	return code, false
}
