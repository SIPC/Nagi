package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/server/audit"
)

// M7：没登录的人不断发跨站的 POST（路径和来源各几 KB）。之前每次都写一条 security 记录，2000 次就是 2000 条；
// 现在每个 IP 每小时单独记一条、每小时最多 100 条，其余整点汇总成一条，路径和来源截短。
func TestOriginDenialsSummarized(t *testing.T) {
	ctx := context.Background()
	l, err := audit.Open(ctx, filepath.Join(t.TempDir(), "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a := &API{Audit: l}
	h := a.OriginCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	big := "https://x/" + strings.Repeat("A", 8000)
	send := func(ip string) {
		r := httptest.NewRequest("POST", "https://panel/api/upload/"+strings.Repeat("p", 2000), nil)
		r.RemoteAddr = ip + ":40000"
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", big)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("应该拒绝：%d", w.Code)
		}
	}
	// 同一个 IP 2000 次。
	for range 2000 {
		send("203.0.113.9")
	}
	// 换 300 个 IP（IPv6 的攻击者可以有很多地址）。
	for i := range 300 {
		send(fmt.Sprintf("2001:db8::%x", i+1))
	}
	count := func(action string) int {
		var n int
		_ = l.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE action = ?`, action).Scan(&n)
		return n
	}
	if n := count("cross_origin_request"); n != originLogPerHour {
		t.Fatalf("单独记了 %d 条，应该是每小时最多 %d 条", n, originLogPerHour)
	}
	var params string
	_ = l.DB().QueryRow(`SELECT params FROM events WHERE action = 'cross_origin_request' LIMIT 1`).Scan(&params)
	if len(params) > 700 {
		t.Errorf("参数没有截短：%d 字节", len(params))
	}
	// 过了整点：写一条汇总，计入没单独记的 2199 次。
	a.origins.mu.Lock()
	a.origins.hour--
	a.origins.mu.Unlock()
	a.FlushOriginDenials(ctx)
	if n := count("cross_origin_summary"); n != 1 {
		t.Fatalf("汇总 %d 条，应该是 1 条", n)
	}
	_ = l.DB().QueryRow(`SELECT params FROM events WHERE action = 'cross_origin_summary'`).Scan(&params)
	var sum struct {
		Count  int      `json:"count"`
		TopIPs []string `json:"top_ips"`
	}
	_ = json.Unmarshal([]byte(params), &sum)
	if sum.Count != 2000+300-originLogPerHour || len(sum.TopIPs) == 0 || sum.TopIPs[0] != "203.0.113.9" {
		t.Errorf("汇总不对：%s", params)
	}
	// 没有新的请求时不再写汇总。
	a.origins.mu.Lock()
	a.origins.hour--
	a.origins.mu.Unlock()
	a.FlushOriginDenials(ctx)
	if n := count("cross_origin_summary"); n != 1 {
		t.Errorf("没有新请求时不应再写汇总，现在 %d 条", n)
	}
}
