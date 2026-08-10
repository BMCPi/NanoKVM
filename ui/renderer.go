package ui

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
)

// newRender creates a templRenderer with a specific context, status, and component.
func newRender(ctx context.Context, status int, component templ.Component) *templRenderer {
	return &templRenderer{
		ctx:       ctx,
		status:    status,
		component: component,
	}
}

// templRenderer implements gin's render.Render for templ components.
type templRenderer struct {
	ctx       context.Context
	status    int
	component templ.Component
}

func (t templRenderer) Render(w http.ResponseWriter) error {
	t.WriteContentType(w)
	if t.status != -1 {
		w.WriteHeader(t.status)
	}
	if t.component != nil {
		return t.component.Render(t.ctx, w)
	}
	return nil
}

func (t templRenderer) WriteContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}
