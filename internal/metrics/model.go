package metrics

import (
	"context"
	"time"

	"github.com/Raamia/Rojo/internal/agents/model"
)

// InstrumentModel wraps a model client so every call is measured. A decorator
// rather than code inside the client: the client's job is talking to the API,
// and the agents that use it should not know or care that anyone is counting.
func InstrumentModel(inner model.Client, r *Registry) model.Client {
	return &instrumentedModel{inner: inner, registry: r}
}

type instrumentedModel struct {
	inner    model.Client
	registry *Registry
}

func (m *instrumentedModel) Generate(ctx context.Context, req model.Request) (model.Response, error) {
	start := time.Now()
	resp, err := m.inner.Generate(ctx, req)
	// Usage is recorded even on the error path: a call that failed after the
	// provider had already generated tokens still cost money, and a cost figure
	// that silently omits failures understates exactly the runs worth knowing
	// about. A zero Usage — the provider did not say — adds nothing.
	m.registry.ModelCall(time.Since(start), err, resp.Usage)
	return resp, err
}
