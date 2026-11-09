package agent

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 同时最多 maxUploads 个上传，多的直接拒绝，释放后又能上传。
func TestAcquireUpload(t *testing.T) {
	var releases []func()
	for range maxUploads {
		r, err := acquireUpload()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	if _, err := acquireUpload(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("满了应该拒绝: %v", err)
	}
	releases[0]()
	r, err := acquireUpload()
	if err != nil {
		t.Fatalf("释放后应该能上传: %v", err)
	}
	r()
	for _, r := range releases[1:] {
		r()
	}
}
