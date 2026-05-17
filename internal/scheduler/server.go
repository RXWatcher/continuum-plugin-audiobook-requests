// Package scheduler adapts the Reconciler.Tick to scheduled_task.v1.
package scheduler

import (
	"context"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/reconciler"
)

type Server struct {
	pluginv1.UnimplementedScheduledTaskServer
	depsFn func() *reconciler.Reconciler
}

func New(depsFn func() *reconciler.Reconciler) *Server {
	return &Server{depsFn: depsFn}
}

func (s *Server) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	if req.GetTaskKey() != "reconciler" {
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}
	r := s.depsFn()
	if r == nil {
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}
	if err := r.Tick(ctx); err != nil {
		return nil, err
	}
	return &pluginv1.RunScheduledTaskResponse{}, nil
}
