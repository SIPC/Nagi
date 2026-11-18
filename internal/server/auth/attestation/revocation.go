package attestation

import (
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 证明证书的吊销列表（见 docs/android.md#吊销列表）：Google 公布的被吊销的证明证书，按序列号列出。
// 发布时 CI 把当时的列表放进 status.json（release.yml），面板运行时每天再从 StatusURL 取一次。

//go:embed status.json
var statusJSON []byte

// status-date.txt 是 status.json 的日期（Google 返回的 Last-Modified，RFC 3339）。
//
//go:embed status-date.txt
var statusDate string

// StatusURL 是 Google 的吊销列表。
const StatusURL = "https://android.googleapis.com/attestation/status"

// MaxStatusBytes 是吊销列表的上限。现在约 180 KiB。
const MaxStatusBytes = 16 << 20

// Revocations 是一份吊销列表。
type Revocations struct {
	// entries：序列号（小写十六进制，没有前导 0）对应原因（例如 KEY_COMPROMISE）。
	entries map[string]string
	// Updated 是列表的日期。
	Updated time.Time
}

// ParseRevocations 读出 Google 格式的吊销列表：{"entries": {"<序列号>": {"status": "REVOKED", "reason": "..."}}}。
// 列在里面的都算吊销（status 是 REVOKED 或 SUSPENDED），原因取 reason，没有时取 status。
func ParseRevocations(b []byte, updated time.Time) (*Revocations, error) {
	if len(b) > MaxStatusBytes {
		return nil, errors.New("吊销列表太大")
	}
	var doc struct {
		Entries map[string]struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, errors.New("吊销列表格式不正确")
	}
	if doc.Entries == nil {
		return nil, errors.New("吊销列表里没有 entries")
	}
	r := &Revocations{entries: make(map[string]string, len(doc.Entries)), Updated: updated}
	for k, v := range doc.Entries {
		reason := v.Reason
		if reason == "" {
			reason = v.Status
		}
		if reason == "" {
			reason = "REVOKED"
		}
		r.entries[normalSerial(k)] = reason
	}
	return r, nil
}

// Embedded 是随面板发布的吊销列表。
func Embedded() *Revocations { return embedded }

var embedded = func() *Revocations {
	updated, err := time.Parse(time.RFC3339, strings.TrimSpace(statusDate))
	if err != nil {
		panic(err)
	}
	r, err := ParseRevocations(statusJSON, updated)
	if err != nil {
		panic(err)
	}
	return r
}()

// Len 是列表里的序列号个数。
func (r *Revocations) Len() int { return len(r.entries) }

// Check 返回 serials 里第一个被吊销的序列号和原因。
func (r *Revocations) Check(serials []string) (serial, reason string, revoked bool) {
	for _, s := range serials {
		if reason, ok := r.entries[normalSerial(s)]; ok {
			return s, reason, true
		}
	}
	return "", "", false
}

// normalSerial 把序列号写成小写、去掉前导 0（Google 的列表这样写，big.Int.Text(16) 也是）。
func normalSerial(s string) string {
	s = strings.TrimLeft(strings.ToLower(strings.TrimSpace(s)), "0")
	if s == "" {
		return "0"
	}
	return s
}
