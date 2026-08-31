// Package render adapts a templ component to gin's render.Render, so both
// full-page handlers in ui and the htmx fragment handlers in ui/fragments
// write HTML the same way.
package render

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
)

// New creates a Renderer for a specific context, status, and component.
func New(ctx context.Context, status int, component templ.Component) *Renderer {
	return &Renderer{
		ctx:       ctx,
		status:    status,
		component: component,
	}
}

// Renderer implements gin's render.Render for templ components.
type Renderer struct {
	ctx       context.Context
	status    int
	component templ.Component
}

func (t Renderer) Render(w http.ResponseWriter) error {
	t.WriteContentType(w)
	if t.status != -1 {
		w.WriteHeader(t.status)
	}
	if t.component != nil {
		return t.component.Render(t.ctx, w)
	}
	return nil
}

func (t Renderer) WriteContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}
