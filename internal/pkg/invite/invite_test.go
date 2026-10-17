package invite

import "testing"

func TestRoundTrip(t *testing.T) {
	in := Invite{Address: "203.0.113.10:34567", Fingerprint: "sha256//abc=", Token: "tok"}
	out, err := Decode("  " + Encode(in) + "\n")
	if err != nil || out != in {
		t.Fatalf("got %+v, %v", out, err)
	}
	for _, bad := range []string{"", "abc", "nagi-invite:!!", "nagi-invite:e30"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("%q 应该解析失败", bad)
		}
	}
}
