package projects

import (
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 容器目录的默认值（见 docs/agent.md#容器目录，界面上 useDefaultDir 用同样的规则显示）：<节点的项目根目录>/<项目名的拼音>/<容器名>。
//   - 项目在这个节点上已经有容器时，沿用它们所在的项目文件夹；
//   - 项目文件夹被别的项目用了（例如两个项目重名），在后面加 -2、-3……
// 目录建好以后就固定了，项目改名不影响已有的容器。

func projectsRoot(conn *nodes.Conn) string {
	if conn != nil {
		if root := conn.LastReport().GetNode().GetProjectsRoot(); root != "" {
			return root
		}
	}
	return "/srv/nagi"
}

// defaultDir 返回容器在节点上的默认目录。projectID 为空表示新项目（还没有容器）。
func (s *Service) defaultDir(ctx context.Context, conn *nodes.Conn, nodeID, projectID, projectName, container string) string {
	root := projectsRoot(conn)
	others := s.nodeDirs(ctx, nodeID)
	// 项目在这个节点上已有的文件夹。
	for _, c := range others {
		if projectID != "" && c.projectID == projectID {
			if parent := path.Dir(c.dir); path.Dir(parent) == root {
				return path.Join(parent, container)
			}
		}
	}
	base := spec.Slug(projectName)
	if base == "" {
		base = "project"
	}
	taken := func(folder string) bool {
		for _, c := range others {
			if c.projectID != projectID && (c.dir == folder || strings.HasPrefix(c.dir, folder+"/")) {
				return true
			}
		}
		return false
	}
	folder := path.Join(root, base)
	for i := 2; taken(folder); i++ {
		folder = path.Join(root, base+"-"+strconv.Itoa(i))
	}
	return path.Join(folder, container)
}

// dirUse 是节点上被某个容器占用的一个目录。
type dirUse struct {
	projectID, name, dir string
}

// nodeDirs 返回节点上被容器占用的目录：放在这个节点上的容器的目录，以及顶替设置里分配给这个节点的目录
// （热备恢复时、原节点切回时要用，不能先分给别的容器）。
func (s *Service) nodeDirs(ctx context.Context, nodeID string) []dirUse {
	all, err := s.Q.ListAllContainers(ctx)
	if err != nil {
		return nil
	}
	var out []dirUse
	for _, c := range all {
		if c.NodeID == nodeID {
			out = append(out, dirUse{c.ProjectID, c.Name, c.Dir})
		}
		if cfg := failoverOf(c); cfg != nil {
			for _, p := range []*nagiv1.FailoverParticipant{cfg.GetA(), cfg.GetB()} {
				// 容器就在这个节点上、目录也一样时上面已经算过了。
				if p.GetNodeId() == nodeID && p.GetDir() != "" && (c.NodeID != nodeID || p.GetDir() != c.Dir) {
					out = append(out, dirUse{c.ProjectID, c.Name, p.GetDir()})
				}
			}
		}
	}
	return out
}

// dirOwner 返回节点上占用目录 dir 的容器（不算 self 本身），没有时 ok 为 false。
func (s *Service) dirOwner(ctx context.Context, nodeID, dir, selfProject, selfName string) (string, bool) {
	for _, u := range s.nodeDirs(ctx, nodeID) {
		if u.dir == dir && !(u.projectID == selfProject && u.name == selfName) {
			return u.name, true
		}
	}
	return "", false
}
