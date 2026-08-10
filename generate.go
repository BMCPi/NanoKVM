// Package nanokvmapp anchors repo-wide go:generate directives at the module
// root so `go generate ./...` regenerates every templ package (layouts,
// pages, components).
package nanokvmapp

//go:generate go tool templ generate
