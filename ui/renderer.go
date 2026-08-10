package ui

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
	"github.com/gin-gonic/gin/render"
)

// HTMLTemplRenderer renders templ components as HTML via Gin's HTMLRender
// interface. If the data passed is not a templ.Component and a
// FallbackHTMLRenderer is set, it delegates to the fallback.
type HTMLTemplRenderer struct {
	FallbackHTMLRenderer render.HTMLRender
}

func (r *HTMLTemplRenderer) Instance(s string, d any) render.Render {
	templData, ok := d.(templ.Component)
	if !ok {
		if r.FallbackHTMLRenderer != nil {
			return r.FallbackHTMLRenderer.Instance(s, d)
		}
	}
	return &templRenderer{
		ctx:       context.Background(),
		status:    -1,
		component: templData,
	}
}

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
