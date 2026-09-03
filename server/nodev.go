//go:build !dev

package main

import "net/http"

// Release builds carry no development affordances: the daemon serves only the
// API, volume roots come solely from /volume* discovery, and DSM sessions
// come solely from the CGI-forwarded header.

func devServeUI(api http.Handler) http.Handler { return api }

func devVolumeRoots() []string { return nil }

func devStateDir() string { return "" }

func devDSMBase() string { return "" }

func devBindHost() string { return "" }
