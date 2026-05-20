// Package scheduler adapts the Reconciler.Tick to scheduled_task.v1.
package scheduler

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/reconciler"
)

// taskID extracts the capability id from a scheduled-task key. The Continuum
// host sends "plugin:<installationID>:<capabilityID>" (task_registry
// pluginTaskKey); bare ids may arrive from host integration tests. This
// plugin's only task id ("reconciler") contains no ':'.
func taskID(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[i+1:]
	}
	return key
}

type Server struct {
	pluginv1.UnimplementedScheduledTaskServer
	depsFn func() *reconciler.Reconciler
}

func New(depsFn func() *reconciler.Reconciler) *Server {
	return &Server{depsFn: depsFn}
}

func (s *Server) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	if taskID(req.GetTaskKey()) != "reconciler" {
		return nil, fmt.Errorf("unknown task key %q", req.GetTaskKey())
	}
	r := s.depsFn()
	if r == nil {
		// Capability servers serve before Configure runs. Return an error so
		// the host retries this tick once configured, instead of reporting a
		// successful no-op (which would silently skip every reconcile).
		return nil, fmt.Errorf("plugin not configured yet")
	}
	if err := r.Tick(ctx); err != nil {
		return nil, err
	}
	return &pluginv1.RunScheduledTaskResponse{}, nil
}
