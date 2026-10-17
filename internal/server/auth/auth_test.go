package auth

import (
	"net/http"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse", h) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("wrong password accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 4; i++ {
		l.fail("1.2.3.4")
	}
	if l.wait("1.2.3.4") > 0 {
		t.Fatal("should not wait before 5 failures")
	}
	l.fail("1.2.3.4")
	if l.wait("1.2.3.4") <= 0 {
		t.Fatal("should wait after 5 failures")
	}
	l.reset("1.2.3.4")
	if l.wait("1.2.3.4") > 0 {
		t.Fatal("reset did not clear")
	}
}

func TestSessionToken(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; "+CookieName+"=tok")
	if got := SessionToken(h); got != "tok" {
		t.Fatalf("got %q", got)
	}
}
