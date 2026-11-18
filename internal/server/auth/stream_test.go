package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// M1：订阅流每一轮按成员当前的角色和访问范围重查；App 的令牌过期、换网段、管理员不在允许的地址、
// 手机被退出时结束；网页会话被退出时结束。
func TestStreamUser(t *testing.T) {
	h := newSessionHarness(t)
	keys := newAppKeys()
	id := h.pairSoftware("op", keys)
	res, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+res.Token)
	at := func(ip string) context.Context { return WithMeta(context.Background(), Meta{IP: ip}) }
	opened, err := h.s.AppTokenUser(context.Background(), res.Token, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.d.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	retoken := func(err error) string {
		ce := new(connect.Error)
		if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
			return "not unauthenticated: " + errString(err)
		}
		return ce.Meta().Get(RetokenHeader)
	}

	// 角色、访问范围改了：按新的算。
	exec(`UPDATE users SET role_id = 3, all_projects = 0 WHERE username = 'op'`)
	u, err := h.s.StreamUser(at("198.51.100.20"), opened, hdr)
	if err != nil || u.Role != RoleViewer || !u.LimitProjects || u.LimitNodes || u.DeviceID != id {
		t.Fatalf("应按成员当前的设置：%+v，%v", u, err)
	}

	// 升为管理员后，管理员登录地址不包括这个地址：结束。
	exec(`UPDATE users SET role = 'admin' WHERE username = 'op'`)
	if _, err := h.s.SetAdminNetworks(context.Background(), []string{"192.0.2.0/24"}, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.StreamUser(at("198.51.100.20"), opened, hdr); err == nil {
		t.Error("管理员不在允许的地址时流应结束")
	}
	if _, err := h.s.SetAdminNetworks(context.Background(), nil, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE users SET role = 'operator' WHERE username = 'op'`)

	// 换了网段（同一条流不会，防御）：要重新换取。
	if r := retoken(func() error { _, err := h.s.StreamUser(at("203.0.113.50"), opened, hdr); return err }()); r != "network" {
		t.Errorf("换网段：%s", r)
	}
	// 令牌过期：要重新换取，App 换新令牌后重新打开流。
	h.s.app.mu.Lock()
	tok := h.s.app.tokens[HashToken(res.Token)]
	tok.expires = time.Now().Add(-time.Second)
	h.s.app.tokens[HashToken(res.Token)] = tok
	h.s.app.mu.Unlock()
	if r := retoken(func() error { _, err := h.s.StreamUser(at("198.51.100.20"), opened, hdr); return err }()); r != "expired" {
		t.Errorf("令牌过期：%s", r)
	}
	// 拿别的手机的令牌冒充：拒绝。
	res, err = h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	hdr.Set("Authorization", "Bearer "+res.Token)
	other := opened
	other.DeviceID = "other"
	if _, err := h.s.StreamUser(at("198.51.100.20"), other, hdr); err == nil {
		t.Error("令牌和打开流的手机不一致时应拒绝")
	}
	// 手机被退出：结束。
	if ok, err := h.s.RemoveAppDevice(context.Background(), opened.ID, opened.Name, id, "test"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := h.s.StreamUser(at("198.51.100.20"), opened, hdr); err == nil {
		t.Error("手机被退出后流应结束")
	}

	// 网页会话：角色改了按新的算，不带高权限确认；会话被退出后结束。
	web := h.webUser("op")
	exec(`UPDATE users SET role_id = 3 WHERE username = 'op'`)
	u, err = h.s.StreamUser(at("198.51.100.7"), web, http.Header{})
	if err != nil || u.Role != RoleViewer || u.SessionID != web.SessionID || u.ElevatedUntil != 0 {
		t.Fatalf("网页会话应按成员当前的设置：%+v，%v", u, err)
	}
	exec(`DELETE FROM sessions WHERE id = ?`, web.SessionID)
	if _, err := h.s.StreamUser(at("198.51.100.7"), web, http.Header{}); connectCode(err) != connect.CodeUnauthenticated {
		t.Errorf("会话被退出后流应结束：%v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
