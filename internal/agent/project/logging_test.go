package project_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

func TestImageCheckHistoryDistinguishesUnknownAndUpdate(t *testing.T) {
	var phase atomic.Int32
	local, remote := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"one","Image":"nginx:latest","ImageID":"image1","Labels":{"com.docker.compose.service":"app"}}]`)
		case strings.Contains(r.URL.Path, "/images/"):
			fmt.Fprintf(w, `{"RepoDigests":["nginx@%s"]}`, local)
		case strings.Contains(r.URL.Path, "/distribution/"):
			if phase.Load() == 0 {
				http.Error(w, "registry unavailable", 503)
				return
			}
			digest := local
			if phase.Load() == 1 {
				digest = remote
			}
			fmt.Fprintf(w, `{"Descriptor":{"digest":%q}}`, digest)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.56")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	dc, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	d, err := db.OpenMigrated(t.Context(), filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m, err := project.NewManager(d, dc, project.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.Journal, err = logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Journal.Close()
	const pid = "0123456789abcdef"
	spec := &nagiv1.ContainerSpec{ProjectId: pid, Name: "web", Dir: t.TempDir(), Config: &nagiv1.ContainerConfig{}}
	b, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodestore.New(d).UpsertContainer(t.Context(), nodestore.UpsertContainerParams{ProjectID: pid, Name: "web", Dir: spec.Dir, Spec: b}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"unknown", "update_available", "unchanged"} {
		phase.Store(int32(i))
		updates, err := m.CheckImages(t.Context(), pid, "web")
		if err != nil || (len(updates) == 1) != (i == 1) {
			t.Fatalf("business result changed: %+v %v", updates, err)
		}
		rows, err := m.Journal.Query(t.Context(), logstore.Filter{Category: logstore.Task, Project: pid})
		if err != nil || len(rows) != (i+1)*3 {
			t.Fatalf("history: %+v %v", rows, err)
		}
		row := rows[i*3+1]
		var data struct{ Result, Service, Error string }
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if row.Kind != "image.check" || row.Object != "web" || data.Service != "app" || data.Result != want || (data.Error != "") != (i == 0) {
			t.Fatalf("missing/error was called unchanged: %+v %s", row, row.Data)
		}
	}
}

func TestTaskHistoryWithoutViewer(t *testing.T) {
	ctx := t.Context()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m, err := project.NewManager(d, nil, project.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(t.TempDir(), "logs.db")
	m.Journal, err = logstore.Open(ctx, journalPath, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Journal.Close() }()
	const pid = "0123456789abcdef"
	spec := &nagiv1.ContainerSpec{ProjectId: pid, Name: "web", Dir: t.TempDir(), Config: &nagiv1.ContainerConfig{}}
	b, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodestore.New(d).UpsertContainer(ctx, nodestore.UpsertContainerParams{ProjectID: pid, Name: "web", Dir: spec.Dir, Spec: b}); err != nil {
		t.Fatal(err)
	}
	// 未部署的缓存恢复是无副作用的成功操作，也会发进度；没有 UI 订阅者仍要保存。
	if err := m.MigrateResume(ctx, pid, "web", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Control(ctx, pid, "missing", nagiv1.ContainerAction_CONTAINER_ACTION_STOP, nil); !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("业务错误被改变: %v", err)
	}
	if err := m.Journal.Close(); err != nil {
		t.Fatal(err)
	}
	m.Journal, err = logstore.Open(ctx, journalPath, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Journal.Query(ctx, logstore.Filter{Category: logstore.Task, Project: pid})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("期待成功任务三条及失败任务两条，得到 %d", len(got))
	}
	var firstID string
	for i, e := range got {
		var data struct {
			TaskID                             string `json:"task_id"`
			Action, Phase, Result, Text, Error string
		}
		if err := json.Unmarshal(e.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = data.TaskID
		}
		if data.TaskID == "" || (i < 3 && data.TaskID != firstID) || (i >= 3 && data.TaskID == firstID) {
			t.Fatalf("任务关联丢失: %s", e.Data)
		}
		wantPhase := []string{"started", "output", "finished", "started", "finished"}[i]
		if data.Phase != wantPhase {
			t.Fatalf("阶段: %s", e.Data)
		}
		if i < 3 && (e.Object != "web" || data.Action != "migrate_resume") {
			t.Fatalf("归属: %+v", e)
		}
		if i == 1 && !strings.Contains(data.Text, "重新启动容器 web") {
			t.Fatalf("未保存输出: %s", e.Data)
		}
		if i == 2 && data.Result != "ok" {
			t.Fatalf("成功结果: %s", e.Data)
		}
		if i == 4 && (data.Result != "failed" || data.Error == "" || e.Object != "missing") {
			t.Fatalf("错误结果: %s", e.Data)
		}
	}
	// 新库不可写也不能阻止任务，既有输出调用者照样收到原文。
	if err := m.Journal.Close(); err != nil {
		t.Fatal(err)
	}
	var output string
	if err := m.MigrateResume(ctx, pid, "web", func(s string) { output += s }); err != nil || !strings.Contains(output, "重新启动容器 web") {
		t.Fatalf("日志失败影响业务: %q %v", output, err)
	}
}
