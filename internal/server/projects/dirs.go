package projects

import (
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/spec"
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
	others, _ := s.Q.ListNodeContainers(ctx, nodeID)
	// 项目在这个节点上已有的文件夹。
	for _, c := range others {
		if projectID != "" && c.ProjectID == projectID {
			if parent := path.Dir(c.Dir); path.Dir(parent) == root {
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
			if c.ProjectID != projectID && (c.Dir == folder || strings.HasPrefix(c.Dir, folder+"/")) {
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
