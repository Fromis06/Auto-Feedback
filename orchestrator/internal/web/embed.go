package web

import _ "embed"

// DashboardHTML holds the single-file embedded Web Dashboard served at GET /dashboard.
//
//go:embed dashboard.html
var DashboardHTML []byte
