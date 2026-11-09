package api

import (
	"context"
	"errors"
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
// 操作员只能改和这些内容无关的部分：compose、现有目录不变，变量和文件来源也不变——容器以特权运行时，
// 变量（例如 LD_PRELOAD、启动命令里引用的变量）和 git 仓库里的文件决定了它以 root 运行什么。
// projectID 为空表示新建容器。
func (s *projectService) checkOperatorConfig(ctx context.Context, projectID, container string, cfg *nagiv1.ContainerConfig) error {
	if !operatorLimited(ctx) {
		return nil
	}
	reasons, _ := spec.PrivilegeReasons(cfg, container)
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
				if cur != nil && sameCompose(cur, cfg, container) && sameDirs(cur, cfg) && sameVars(cur, cfg) && proto.Equal(cur.GetSource(), cfg.GetSource()) {
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

// sameVars 比较容器变量和服务级环境变量。
func sameVars(a, b *nagiv1.ContainerConfig) bool {
	if len(a.GetEnv()) != len(b.GetEnv()) || len(a.GetServiceEnv()) != len(b.GetServiceEnv()) {
		return false
	}
	for i := range a.GetEnv() {
		if !proto.Equal(a.GetEnv()[i], b.GetEnv()[i]) {
			return false
		}
	}
	for svc, x := range a.GetServiceEnv() {
		if y, ok := b.GetServiceEnv()[svc]; !ok || !proto.Equal(x, y) {
			return false
		}
	}
	return true
}

// checkPrivilegedEntry：容器部署的配置有特权（以特权运行、挂载主机目录等）时，进入容器就等于拿到节点的 root，
// 终端和执行命令只有管理员能用。role 是当前的角色（打开着的终端重查时传重新读出的）。
func (a *API) checkPrivilegedEntry(ctx context.Context, role, projectID, container string) error {
	if role == auth.RoleAdmin {
		return nil
	}
	cfg, err := a.Projects.RunningConfig(ctx, projectID, container)
	if err != nil {
		return err
	}
	if reasons, _ := spec.PrivilegeReasons(cfg, container); len(reasons) > 0 {
		return errPrivilegedEntry
	}
	return nil
}

var errPrivilegedEntry = i18n.New("特权容器仅限管理员进入")

// checkPrivilegedWrite：同样的道理，往特权容器的卷和容器目录里写文件（上传、MCP 写文件）只给管理员，
// 容器会以 root 运行这些文件。
func (a *API) checkPrivilegedWrite(ctx context.Context, role, projectID, container string) error {
	if err := a.checkPrivilegedEntry(ctx, role, projectID, container); errors.Is(err, errPrivilegedEntry) {
		return errPrivilegedWrite
	} else if err != nil {
		return err
	}
	return nil
}

var errPrivilegedWrite = i18n.New("特权容器的文件仅限管理员修改")

// checkOperatorSharedEnv：有特权的容器引用的共享变量，操作员不能改（和容器自己的变量一样，见 checkOperatorConfig）。
// 和已部署的共享变量或者现有的草稿比，这些变量的值都没变才放行；容器自己的同名变量覆盖了的不算。
func (s *projectService) checkOperatorSharedEnv(ctx context.Context, projectID string, vars []*nagiv1.EnvVar) error {
	if !operatorLimited(ctx) {
		return nil
	}
	summary, err := s.Projects.Get(ctx, projectID)
	if err != nil {
		return toConnect(err)
	}
	p := summary.GetProject()
	for _, c := range p.GetContainers() {
		for _, cfg := range []*nagiv1.ContainerConfig{c.GetDraft(), c.GetConfig()} {
			if cfg == nil {
				continue
			}
			if reasons, _ := spec.PrivilegeReasons(cfg, c.GetName()); len(reasons) == 0 {
				continue
			}
			same := !spec.SharedEnvChanged(cfg, c.GetName(), p.GetSettings().GetSharedEnv(), vars) ||
				p.GetHasSharedEnvDraft() && !spec.SharedEnvChanged(cfg, c.GetName(), p.GetSharedEnvDraft(), vars)
			if !same {
				return connect.NewError(connect.CodePermissionDenied, i18n.Errorf("容器 %s 有特权，它引用的共享变量仅管理员可修改", c.GetName()))
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
