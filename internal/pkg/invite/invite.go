// Package invite 编码和解析面板的接入码：把面板地址、公钥指纹和加入令牌打包成一段文本，
// 方便在面板之间、或者到节点上执行本地命令时复制粘贴。
package invite

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

const prefix = "nagi-invite:"

// Invite 是接入码的内容。
type Invite struct {
	Address     string `json:"a"`
	Fingerprint string `json:"f"`
	Token       string `json:"t"`
}

// Encode 生成接入码。
func Encode(i Invite) string {
	data, _ := json.Marshal(i)
	return prefix + base64.RawURLEncoding.EncodeToString(data)
}

// Decode 解析接入码。
func Decode(code string) (Invite, error) {
	var i Invite
	raw, ok := strings.CutPrefix(strings.TrimSpace(code), prefix)
	if !ok {
		return i, i18n.Errorf("接入码格式不对，应该以 %s 开头", prefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(data, &i) != nil || i.Address == "" || i.Fingerprint == "" || i.Token == "" {
		return i, i18n.New("接入码无效，请重新复制")
	}
	return i, nil
}
