package docker

import "testing"

func TestPullTracker(t *testing.T) {
	p := newPullTracker()
	p.add("1.36", "Pulling from library/busybox", 0, 0)
	p.add("a1", "Pulling fs layer", 0, 0)
	p.add("b2", "Already exists", 0, 0)
	p.add("a1", "Downloading", 40, 100)
	got := p.snapshot()
	if got.Status != "Downloading" || got.Current != 40 || got.Total != 100 || got.Layers != 2 || got.LayersDone != 1 {
		t.Fatalf("进度 = %+v", got)
	}
	p.add("a1", "Download complete", 0, 0)
	p.add("a1", "Extracting", 10, 100)
	p.add("a1", "Pull complete", 0, 0)
	p.add("", "Digest: sha256:abc", 0, 0)
	got = p.snapshot()
	if got.Current != 100 || got.LayersDone != 2 || got.Status != "Digest: sha256:abc" {
		t.Fatalf("进度 = %+v", got)
	}
}
