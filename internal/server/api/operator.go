package api

import (
	"context"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/proto"
)

// 操作员不能借容器配置拿到节点的 root（见 docs/node-access.md#操作员不能借-compose-拿到节点-root）。
// 管理员不受这些限制。

func operatorLimited(ctx context.Context) bool {
	return auth.CurrentUser(ctx).Role != auth.RoleAdmin
}

func adminOnlyConfig(reasons []string) error {
	return connect.NewError(connect.CodePermissionDenied, i18n.Errorf("以下配置仅管理员可保存：%s", privilegeReasons(reasons)))
}

// privilegeReasons 是 spec.PrivilegeReasons 的结果，显示时把面板自己的字段换成界面上的名称（compose 的写法照原样）。
type privilegeReasons []string

func (r privilegeReasons) String() string { return r.Localize(i18n.Zh) }

func (r privilegeReasons) Localize(l i18n.Lang) string {
	out := make(i18n.List, len(r))
	for i, s := range r {
		switch {
		case s == "project_dir":
			s = i18n.Sprintf(l, "现有目录")
		case strings.HasPrefix(s, "backup_paths: "):
			s = i18n.Sprintf(l, "快照包含的子目录：%s", strings.TrimPrefix(s, "backup_paths: "))
		}
		out[i] = s
	}
	return out.Localize(l)
}

// checkOperatorConfig 检查配置里有没有只有管理员能保存的内容。管理员保存过的配置可以留着，
// 操作员只能改和这些内容无关的部分：compose 不变；特权字段用了变量时，变量也不能变。
// projectID 为空表示新建容器。
func (s *projectService) checkOperatorConfig(ctx context.Context, projectID, container string, cfg *nagiv1.ContainerConfig) error {
	if !operatorLimited(ctx) {
		return nil
	}
	reasons, dynamic := spec.PrivilegeReasons(cfg, container)
	if len(reasons) == 0 {
		return nil
	}
	if projectID != "" {
		summary, err := s.Projects.Get(ctx, projectID)
		if err != nil {
			return toConnect(err)
		}
		for _, c := range summary.GetProject().GetContainers() {
			if c.GetName() != container {
				continue
			}
			for _, cur := range []*nagiv1.ContainerConfig{c.GetDraft(), c.GetConfig()} {
				// 现有目录和快照子目录也是特权内容，要和 compose 一起不变：只比 compose 的话，
				// 操作员先存一份没有特权的草稿，再只改目录，就能把相对挂载指到节点上任意目录。
				if cur != nil && sameCompose(cur, cfg, container) && sameDirs(cur, cfg) && (!dynamic || sameVars(cur, cfg)) {
					return nil
				}
			}
		}
	}
	return adminOnlyConfig(reasons)
}

func sameCompose(a, b *nagiv1.ContainerConfig, container string) bool {
	x, err1 := spec.ComposeText(a, container)
	y, err2 := spec.ComposeText(b, container)
	return err1 == nil && err2 == nil && x == y
}

// sameDirs 比较现有目录和快照子目录（按保存时的规范化比较，cfg 这时还没规范化）。
func sameDirs(a, b *nagiv1.ContainerConfig) bool {
	clean := func(c *nagiv1.ContainerConfig) (string, []string, bool) {
		dir := ""
		if d := c.GetProjectDir(); d != "" {
			var err error
			if dir, err = spec.CleanProjectDir(d); err != nil {
				return "", nil, false
			}
		}
		paths, err := spec.CleanBackupPaths(c.GetBackupPaths())
		return dir, paths, err == nil
	}
	da, pa, ok1 := clean(a)
	db, pb, ok2 := clean(b)
	return ok1 && ok2 && da == db && slices.Equal(pa, pb)
}

func sameVars(a, b *nagiv1.ContainerConfig) bool {
	if len(a.GetEnv()) != len(b.GetEnv()) {
		return false
	}
	for i := range a.GetEnv() {
		if !proto.Equal(a.GetEnv()[i], b.GetEnv()[i]) {
			return false
		}
	}
	return true
}

// checkOperatorSharedEnv：项目里有容器的特权字段用了变量时，共享变量能改变它的含义，只有管理员能改。
func (s *projectService) checkOperatorSharedEnv(ctx context.Context, projectID string) error {
	if !operatorLimited(ctx) {
		return nil
	}
	summary, err := s.Projects.Get(ctx, projectID)
	if err != nil {
		return toConnect(err)
	}
	for _, c := range summary.GetProject().GetContainers() {
		for _, cfg := range []*nagiv1.ContainerConfig{c.GetDraft(), c.GetConfig()} {
			if cfg == nil {
				continue
			}
			if reasons, dynamic := spec.PrivilegeReasons(cfg, c.GetName()); dynamic {
				return adminOnlyConfig(reasons)
			}
		}
	}
	return nil
}

// checkOperatorDir：操作员不能把容器目录放到目标节点的项目根目录以外。目录由节点创建，权限是 root，
// 放到 /etc 这样的地方再上传文件，一样能改节点。只看目标节点自己上报的根目录：各节点的根目录可以不同，
// 待批准的节点也不能算。
func (s *projectService) checkOperatorDir(ctx context.Context, nodeID, dir string) error {
	if !operatorLimited(ctx) || dir == "" {
		return nil
	}
	root := ""
	if c, err := s.Nodes.Get(nodeID); err == nil {
		root = path.Clean(c.LastReport().GetNode().GetProjectsRoot())
	}
	clean := path.Clean(dir)
	if root != "." && root != "/" && path.IsAbs(root) && strings.HasPrefix(clean, root+"/") {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, i18n.New("项目根目录以外的容器目录仅管理员可设置"))
}
