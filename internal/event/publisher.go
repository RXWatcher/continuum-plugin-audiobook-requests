// Package event publishes named events into continuum's event hub.
package event

import (
	"context"

	"github.com/hashicorp/go-hclog"

	"github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtimehost"
)

type Publisher struct {
	host   *runtimehost.Client
	logger hclog.Logger
}

func New(host *runtimehost.Client, logger hclog.Logger) *Publisher {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	return &Publisher{host: host, logger: logger}
}

func (p *Publisher) Publish(ctx context.Context, name string, payload map[string]any) {
	if p.host == nil {
		p.logger.Warn("host not bound; skipping event", "name", name)
		return
	}
	if err := p.host.PublishEvent(ctx, name, payload); err != nil {
		p.logger.Warn("publish event", "name", name, "err", err)
	}
}
