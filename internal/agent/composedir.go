package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

// 现有目录的导入（见 docs/agent.md#现有目录）：读取目录里的 compose 和 .env，找出在这个目录里运行着的 compose 项目。
// 读主机上的文件，所以和节点文件一样受节点访问的开关控制；Nagi 自己的数据目录和容器目录不能导入。

// composeNames 是 compose 找 compose 文件的顺序；overrideNames 是它会自动叠加的文件。
var (
	composeNames  = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	overrideNames = []string{"compose.override.yaml", "compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml"}
)

const composeFileLimit = 1 << 20

func readLimited(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, composeFileLimit+1))
	if err != nil {
		return "", err
	}
	if len(b) > composeFileLimit {
		return "", status.Errorf(codes.InvalidArgument, "%s 超过 1 MB", filepath.Base(path))
	}
	return string(b), nil
}

// within 判断 path 是不是 dir 本身或者在 dir 里面。
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *agentService) InspectComposeDir(ctx context.Context, req *nagiv1.InspectComposeDirRequest) (*nagiv1.InspectComposeDirResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	dir, err := spec.CleanProjectDir(req.GetPath())
	if err != nil {
		return nil, toStatus(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, status.Errorf(codes.InvalidArgument, "节点上没有目录 %s", dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "读取目录 %s: %v", dir, err)
	}
	denied := []string{s.node.cfg.DataDir}
	if s.node.cfg.Embedded {
		denied = append(denied, filepath.Dir(s.node.cfg.DataDir))
	}
	for _, d := range denied {
		if within(real, d) {
			return nil, status.Error(codes.InvalidArgument, "这是 Nagi 自己的数据目录，不能导入")
		}
	}
	for _, d := range s.projects.Dirs(ctx) {
		if within(real, d) || within(d, real) {
			return nil, status.Errorf(codes.InvalidArgument, "%s 是 Nagi 管理的容器目录（或包含它），不能导入", d)
		}
	}
	out := &nagiv1.InspectComposeDirResponse{Path: dir, Volumes: map[string]string{}}
	for _, n := range composeNames {
		text, err := readLimited(filepath.Join(dir, n))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, toStatus(err)
		}
		out.ComposeFile, out.Compose = n, text
		break
	}
	if out.ComposeFile == "" {
		return nil, status.Errorf(codes.InvalidArgument, "%s 里没有 compose 文件（compose.yaml、docker-compose.yml 等）", dir)
	}
	for _, n := range overrideNames {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			out.IgnoredFiles = append(out.IgnoredFiles, n)
		}
	}
	if text, err := readLimited(filepath.Join(dir, ".env")); err == nil {
		out.Env = text
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, toStatus(err)
	}
	list, err := s.node.docker.ComposeContainersIn(ctx, dir)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查找目录里的容器: %v", err)
	}
	for _, c := range list {
		if strings.HasPrefix(c.Project, "nagi-") {
			continue
		}
		if out.Project == "" {
			out.Project = c.Project
		}
		out.Containers = append(out.Containers, &nagiv1.ComposeDirContainer{Name: c.Name, Service: c.Service, State: c.State})
	}
	// 容器已经删了的时候，按 compose 的规则推出项目名，原来的命名卷可能还在。
	project := out.Project
	if project == "" {
		project = defaultComposeProject(out.Compose, dir)
	}
	if vols, err := s.node.docker.ComposeVolumes(ctx, project); err == nil {
		out.Volumes = vols
	}
	return out, nil
}

var composeNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// defaultComposeProject 是在目录里直接执行 docker compose 时的项目名：compose 文件里的 name，没有时是目录名。
func defaultComposeProject(text, dir string) string {
	var doc struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal([]byte(text), &doc) == nil && doc.Name != "" && !strings.Contains(doc.Name, "$") {
		return doc.Name
	}
	return strings.TrimLeft(composeNameRe.ReplaceAllString(strings.ToLower(filepath.Base(dir)), ""), "_-")
}
