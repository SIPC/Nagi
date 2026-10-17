package agent

import (
	"context"
	"errors"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// agentService 实现 AgentService，供面板通过反向 gRPC 调用。
type agentService struct {
	nagiv1.UnimplementedAgentServiceServer
	projects *project.Manager
	changed  func()
}

func (s *agentService) ListProjects(ctx context.Context, _ *nagiv1.ListProjectsRequest) (*nagiv1.ListProjectsResponse, error) {
	list, err := s.projects.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListProjectsResponse{Projects: list}, nil
}

func (s *agentService) CreateProject(ctx context.Context, req *nagiv1.CreateProjectRequest) (*nagiv1.CreateProjectResponse, error) {
	p, err := s.projects.Create(ctx, req.GetName(), req.GetDir(), req.GetCompose())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.CreateProjectResponse{Project: p}, nil
}

func (s *agentService) GetProject(ctx context.Context, req *nagiv1.GetProjectRequest) (*nagiv1.GetProjectResponse, error) {
	p, err := s.projects.Get(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.GetProjectResponse{Project: p}, nil
}

func (s *agentService) DeployProject(req *nagiv1.DeployProjectRequest, stream nagiv1.AgentService_DeployProjectServer) error {
	ctx := stream.Context()
	send := func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	}
	version, err := s.projects.Deploy(ctx, req.GetId(), req.GetBaseRevision(), send)
	s.changed()
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrConflict) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil, Version: version}
	if err != nil {
		result.Error = err.Error()
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) StreamLogs(req *nagiv1.StreamLogsRequest, stream nagiv1.AgentService_StreamLogsServer) error {
	err := s.projects.Logs(stream.Context(), req.GetId(), req.GetService(), int(req.GetTail()), req.GetFollow(), func(l docker.LogLine) {
		_ = stream.Send(&nagiv1.LogLine{
			Service: l.Service, Stream: l.Stream, Text: l.Text, Time: timestamppb.New(l.Time),
		})
	})
	return toStatus(err)
}

func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, project.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, project.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, project.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
