package api

import (
	"context"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// 现有目录的导入（见 docs/agent.md#现有目录）：节点读出目录里的 compose 和 .env，面板据此生成容器组的配置。
// 目录就是 compose 的项目目录，数据留在原处；.env 复制成容器组变量；原来的命名卷改成引用原卷。
// 读的是主机上的文件，所以只给管理员，而且和节点文件一样要先确认身份（见 elevationRequired）。

// secretName 判断变量名像不像密钥，像的导入时标为密钥。
var secretName = regexp.MustCompile(`(?i)(PASS|SECRET|TOKEN|KEY|CREDENTIAL|PRIVATE)`)

var importNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// databaseTargets 是常见数据库镜像的数据目录：挂载到这里的子目录，导入时推荐纳入快照。
var databaseTargets = map[string]bool{
	"/var/lib/postgresql/data": true, "/var/lib/postgresql": true, "/var/lib/mysql": true, "/data/db": true,
	"/var/lib/mongodb": true, "/bitnami/postgresql": true, "/bitnami/mysql": true, "/bitnami/mariadb": true,
}

// filepathToSlash 把面板上解析出的路径统一成 /（面板可能跑在 Windows 上做开发，compose 解析出的路径带盘符和 \）。
func filepathToSlash(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if len(p) > 1 && p[1] == ':' {
		p = p[2:]
	}
	return p
}

// composeDir 读取节点上的现有目录，返回导入后的容器组配置和给用户看的预览。
func (a *API) composeDir(ctx context.Context, nodeID, dir string) (*nagiv1.ContainerConfig, *nagiv1.ComposeDirPreview, error) {
	if auth.CurrentUser(ctx).Role != auth.RoleAdmin {
		return nil, nil, connect.NewError(connect.CodePermissionDenied, i18n.New("当前账号没有权限执行这个操作"))
	}
	conn, err := a.Nodes.Get(nodeID)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点不在线"))
	}
	res, err := conn.Agent.InspectComposeDir(ctx, &nagiv1.InspectComposeDirRequest{Path: dir})
	if status.Code(err) == codes.Unimplemented {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点版本过旧，升级节点后才能导入现有目录"))
	} else if err != nil {
		return nil, nil, fromGRPC(err)
	}
	preview := &nagiv1.ComposeDirPreview{Path: res.GetPath(), ComposeFile: res.GetComposeFile(), Project: res.GetProject()}
	for _, c := range res.GetContainers() {
		preview.Containers = append(preview.Containers, c.GetName()+" "+c.GetState())
	}
	for _, f := range res.GetIgnoredFiles() {
		preview.Notes = append(preview.Notes, i18n.T(ctx, "%s 不会带上：compose 会自动叠加它，导入后需要把其中的内容合并进 compose", f))
	}
	vars, err := dotenv.ParseWithLookup(strings.NewReader(res.GetEnv()), func(string) (string, bool) { return "", false })
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf(".env 无法解析：%v", err))
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var env []*nagiv1.EnvVar
	for _, k := range keys {
		v := &nagiv1.EnvVar{Key: k, Value: vars[k], Secret: secretName.MatchString(k)}
		env = append(env, v)
		preview.Variables = append(preview.Variables, k)
		if v.Secret {
			preview.SecretVariables = append(preview.SecretVariables, k)
		}
	}
	text, notes, err := externalVolumes(ctx, res.GetCompose(), res.GetVolumes())
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("%s 无法解析：%v", res.GetComposeFile(), err))
	}
	preview.Notes = append(preview.Notes, notes...)
	preview.Compose = text
	cfg := &nagiv1.ContainerConfig{Compose: text, Env: env, ProjectDir: res.GetPath()}
	name := strings.Trim(importNameRe.ReplaceAllString(strings.ToLower(strings.ReplaceAll(path.Base(res.GetPath()), "_", "-")), ""), "-")
	if name == "" {
		name = "app"
	}
	preview.Name = name
	// 先在这里校验一遍，有问题（例如用到了 .env 里没有的变量）在预览时就报出来。
	proj, err := spec.Validate(ctx, proto.Clone(cfg).(*nagiv1.ContainerConfig), name, nil, "/check", "check")
	if err != nil {
		return nil, nil, toConnect(err)
	}
	for _, svc := range proj.Services {
		if svc.Build != nil {
			return nil, nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("服务 %s 用了 build：节点上不构建镜像，先构建好镜像并推送到镜像仓库，再把 build 换成 image", svc.Name))
		}
		// 推荐纳入快照的子目录：直接挂载到数据库数据目录的一级子目录。照片库这类大目录不推荐。
		for _, v := range svc.Volumes {
			if v.Type != "bind" || !databaseTargets[path.Clean(v.Target)] {
				continue
			}
			if sub, ok := strings.CutPrefix(filepathToSlash(v.Source), res.GetPath()+"/"); ok && sub != "" && !strings.Contains(sub, "/") && !slices.Contains(preview.BackupPaths, sub) {
				preview.BackupPaths = append(preview.BackupPaths, sub)
			}
		}
	}
	sort.Strings(preview.BackupPaths)
	return cfg, preview, nil
}

// externalVolumes 把 compose 里的命名卷改成引用原来那个项目的卷（volumes 是 compose 里的名字到 Docker 里的名字），
// 数据留在原处。返回改写后的 compose 和给用户的说明。原卷已经不在的命名卷保持原样，由 Nagi 新建。
func externalVolumes(ctx context.Context, text string, volumes map[string]string) (string, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return text, nil, nil
	}
	root := doc.Content[0]
	var vols *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "volumes" {
			vols = root.Content[i+1]
		}
	}
	if vols == nil || vols.Kind != yaml.MappingNode {
		return text, nil, nil
	}
	var notes []string
	changed := false
	for i := 0; i+1 < len(vols.Content); i += 2 {
		key, val := vols.Content[i].Value, vols.Content[i+1]
		if val.Kind == yaml.MappingNode {
			external := false
			for j := 0; j+1 < len(val.Content); j += 2 {
				if val.Content[j].Value == "external" && val.Content[j+1].Value == "true" {
					external = true
				}
			}
			if external {
				continue
			}
		}
		full, ok := volumes[key]
		if !ok {
			notes = append(notes, i18n.T(ctx, "命名卷 %s 没有找到原来的卷，会新建一个空卷（在面板的容器目录里）", key))
			continue
		}
		vols.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "external"}, {Kind: yaml.ScalarNode, Value: "true", Tag: "!!bool"},
			{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: full},
		}}
		notes = append(notes, i18n.T(ctx, "命名卷 %s 改为引用原来的卷 %s，数据留在原处", key, full))
		changed = true
	}
	if !changed {
		return text, notes, nil
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", nil, err
	}
	return buf.String(), notes, nil
}

func (s *nodeService) InspectComposeDir(ctx context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.ComposeDirPreview], error) {
	_, preview, err := s.composeDir(ctx, req.Msg.GetNodeId(), req.Msg.GetPath())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(preview), nil
}

func (s *projectService) ImportComposeDir(ctx context.Context, req *connect.Request[nagiv1.ImportComposeDirRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	cfg, preview, err := s.composeDir(ctx, m.GetNodeId(), m.GetDir())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(m.GetName())
	if name == "" {
		name = preview.GetName()
	}
	cfg.BackupPaths = m.GetBackupPaths()
	p, err := s.Projects.AddContainer(ctx, projects.AddRequest{ProjectID: m.GetProjectId(), Name: name, Config: cfg, NodeID: m.GetNodeId()})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}
