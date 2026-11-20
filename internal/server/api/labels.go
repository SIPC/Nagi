package api

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 项目分组和节点标签（见 docs/panel.md#分组和标签）。它们决定成员的访问范围，所以只有管理员能改（auth.adminWrites）。

const (
	maxLabelLen  = 32
	maxNodeTags  = 10
	maxScopeList = 200
)

// cleanLabel 去掉首尾空白，检查长度和字符。空的返回空，由调用方决定是否允许。
func cleanLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxLabelLen {
		return "", connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("分组和标签不能超过 %d 个字符", maxLabelLen))
	}
	if strings.ContainsFunc(s, unicode.IsControl) {
		return "", connect.NewError(connect.CodeInvalidArgument, i18n.New("分组和标签不能包含控制字符"))
	}
	return s, nil
}

// cleanLabels 清理一组名称：去掉空的和重复的，按名称排序。
func cleanLabels(list []string, limit int) ([]string, error) {
	var out []string
	for _, s := range list {
		s, err := cleanLabel(s)
		if err != nil {
			return nil, err
		}
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > limit {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("最多 %d 个标签", limit))
	}
	slices.Sort(out)
	return out, nil
}

func (s *projectService) SetProjectGroup(ctx context.Context, req *connect.Request[nagiv1.SetProjectGroupRequest]) (*connect.Response[nagiv1.Project], error) {
	group, err := cleanLabel(req.Msg.GetGroup())
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetProjectId()
	if _, err := s.Projects.Get(ctx, id); err != nil {
		return nil, toConnect(err)
	}
	if err := s.Q.SetProjectGroup(ctx, id, group); err != nil {
		return nil, err
	}
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p.GetProject()), nil
}

// renameLabel 检查重命名的参数：旧名称要存在，新名称为空表示删除。
func renameLabel(ctx context.Context, m *nagiv1.RenameLabelRequest, exists func(context.Context, string) (bool, error)) (from, to string, err error) {
	from = m.GetName()
	if ok, err := exists(ctx, from); err != nil {
		return "", "", err
	} else if !ok {
		return "", "", connect.NewError(connect.CodeNotFound, i18n.New("分组或标签不存在"))
	}
	to, err = cleanLabel(m.GetNewName())
	return from, to, err
}

func (s *projectService) RenameProjectGroup(ctx context.Context, req *connect.Request[nagiv1.RenameLabelRequest]) (*connect.Response[nagiv1.RenameLabelResponse], error) {
	from, to, err := renameLabel(ctx, req.Msg, s.Q.ProjectGroupExists)
	if err != nil {
		return nil, err
	}
	if err := s.Q.RenameProjectGroup(ctx, from, to); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RenameLabelResponse{}), nil
}

func (s *projectService) SetProjectTags(ctx context.Context, req *connect.Request[nagiv1.SetProjectTagsRequest]) (*connect.Response[nagiv1.Project], error) {
	tags, err := cleanLabels(req.Msg.GetTags(), maxNodeTags)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetProjectId()
	if _, err := s.Projects.Get(ctx, id); err != nil {
		return nil, toConnect(err)
	}
	if err := s.Q.SetProjectTags(ctx, id, tags); err != nil {
		return nil, err
	}
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p.GetProject()), nil
}

func (s *projectService) RenameProjectTag(ctx context.Context, req *connect.Request[nagiv1.RenameLabelRequest]) (*connect.Response[nagiv1.RenameLabelResponse], error) {
	from, to, err := renameLabel(ctx, req.Msg, s.Q.ProjectTagExists)
	if err != nil {
		return nil, err
	}
	if err := s.Q.RenameProjectTag(ctx, from, to); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RenameLabelResponse{}), nil
}

func (s *nodeService) SetNodeTags(ctx context.Context, req *connect.Request[nagiv1.SetNodeTagsRequest]) (*connect.Response[nagiv1.SetNodeTagsResponse], error) {
	tags, err := cleanLabels(req.Msg.GetTags(), maxNodeTags)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetNodeId()
	if n, err := s.Q.GetNode(ctx, id); err != nil || n.Revoked != 0 {
		return nil, nodeNotFound()
	}
	if err := s.Q.SetNodeTags(ctx, id, tags); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetNodeTagsResponse{}), nil
}

func (s *nodeService) SetNodeGroup(ctx context.Context, req *connect.Request[nagiv1.SetNodeGroupRequest]) (*connect.Response[nagiv1.SetNodeGroupResponse], error) {
	group, err := cleanLabel(req.Msg.GetGroup())
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetNodeId()
	if n, err := s.Q.GetNode(ctx, id); err != nil || n.Revoked != 0 {
		return nil, nodeNotFound()
	}
	if err := s.Q.SetNodeGroup(ctx, id, group); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetNodeGroupResponse{}), nil
}

func (s *nodeService) RenameNodeGroup(ctx context.Context, req *connect.Request[nagiv1.RenameLabelRequest]) (*connect.Response[nagiv1.RenameLabelResponse], error) {
	from, to, err := renameLabel(ctx, req.Msg, s.Q.NodeGroupExists)
	if err != nil {
		return nil, err
	}
	if err := s.Q.RenameNodeGroup(ctx, from, to); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RenameLabelResponse{}), nil
}

func (s *nodeService) RenameNodeTag(ctx context.Context, req *connect.Request[nagiv1.RenameLabelRequest]) (*connect.Response[nagiv1.RenameLabelResponse], error) {
	from, to, err := renameLabel(ctx, req.Msg, s.Q.NodeTagExists)
	if err != nil {
		return nil, err
	}
	if err := s.Q.RenameNodeTag(ctx, from, to); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RenameLabelResponse{}), nil
}
