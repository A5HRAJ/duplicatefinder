// Duplicate Finder for Synology DSM — backend service.
//
// Runs in two modes:
//
//	-mode daemon : long-running HTTP server on 127.0.0.1 that performs scans
//	-mode cgi    : CGI shim executed by DSM's web server; proxies the request
//	               to the local daemon so the UI can talk to it through the
//	               authenticated /webman/3rdparty/ path.
package main

import (
	"flag"
)

// appVersion is the package's full version, stamped at build time with
// -ldflags -X (build.sh's VERSION). A var rather than a const because -X
// cannot write a const. /api/info reports it, so the installed build can be
// identified without going through Package Center. The bare fallback is what
// a plain `go build` (dev runs, tests) produces.
var appVersion = "1.0.0-dev"

// defaultPort is the one port the daemon, the package's start script and the
// CGI shim agree on. There is deliberately no environment override: the CGI
// shim is exec'd by DSM's web server with an environment the package does not
// control, so a port read from the environment by the daemon and the start
// script would be one the shim could never learn.
const defaultPort = 9807

func main() {
	mode := flag.String("mode", "daemon", "daemon | cgi")
	port := flag.Int("port", defaultPort, "daemon listen port (127.0.0.1)")
	varDir := flag.String("var", "", "writable state dir (logs)")
	flag.Parse()

	if *mode == "cgi" {
		runCGI(*port)
		return
	}
	runDaemon(*port, *varDir)
}
