// Package assets provides embedded static assets for the NanoKVM BMC web UI.
package assets

import "embed"

// CSS generation is a single Tailwind pass: globals.css (created and updated
// by the shadcn-templ CLI) is the entry point, and Tailwind scans the repo's
// templ sources — including the vendored components under server/components —
// for class names. No transient @source files are involved.
//
//go:generate go tool tailwindcss --cwd ../../ -i server/assets/css/globals.css -o server/assets/css/output.css --minify

// CSS contains embedded CSS files (Tailwind output, xterm).
//
//go:embed css/output.css css/xterm.min.css
var CSS embed.FS

// JS contains embedded JavaScript files (CryptoJS, xterm + addons).
//
//go:embed js/*
var JS embed.FS

// Img contains embedded image files (favicon, logos).
//
//go:embed img/*
var Img embed.FS
