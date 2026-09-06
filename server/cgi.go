// The CGI mode. DSM's web server executes api.cgi for every request the
// desktop app makes; after the shell gate (session, administrator) it execs
// this binary with -mode cgi, which proxies the request to the daemon over
// loopback with the shared daemon token attached.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Auth between the CGI shim and the daemon: the daemon only serves /api/*
// requests carrying the shared secret in this header. The secret lives in
// the package var dir, readable only by the package user both processes run
// as — so other local users/packages cannot drive the daemon directly.
const (
	authTokenHeader = "X-DupFinder-Token"
	authTokenFile   = ".authtoken"
	// readyFile is written (with the port) once the daemon is bound and its
	// token is loaded; the start script waits for it.
	readyFile = "dupfinder.ready"
)

// serviceUser is the name of the account this process runs as, "" when
// the platform cannot say. Pure-Go os/user reads /etc/passwd, where DSM
// lists its package accounts, so this works in the CGO_ENABLED=0 build.
func serviceUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// cgiVarDir resolves the package var dir for the CGI process. api.cgi
// exports DUPFINDER_VAR. Failing that, DSM's layout is the fallback: the
// shim execs /var/packages/<id>/target/bin/dupfinder, and the var dir is
// target's sibling — for whatever <id> this build installed under (the
// hand-built spk and the SynoCommunity build use different ids, so none is
// hardcoded). argv[0] is used, not os.Executable: the latter resolves the
// symlinks to /volumeN/@appstore/<id>/bin/dupfinder, whose parent holds no
// var dir. With neither, the token cannot be found and the daemon refuses
// the request — a visible failure, not a guess.
func cgiVarDir() string {
	if d := os.Getenv("DUPFINDER_VAR"); d != "" {
		return d
	}
	if exe := os.Args[0]; filepath.IsAbs(exe) {
		return filepath.Join(filepath.Dir(filepath.Dir(exe)), "..", "var")
	}
	log.Printf("cgi: DUPFINDER_VAR unset and argv[0] %q is not absolute; cannot locate the package var dir", os.Args[0])
	return ""
}

func runCGI(port int) {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	token := ""
	if b, err := os.ReadFile(filepath.Join(cgiVarDir(), authTokenFile)); err == nil {
		token = strings.TrimSpace(string(b))
	}
	// The daemon executes file mutations through DSM's File Station Web API
	// as the logged-in admin. Their session (cookie + SynoToken) already
	// rides the proxied request headers; the CGI environment tells us which
	// local scheme/port that session is valid against.
	scheme := "http"
	if os.Getenv("HTTPS") == "on" {
		scheme = "https"
	}
	dsmPort := os.Getenv("SERVER_PORT")
	if dsmPort == "" {
		dsmPort = "5000"
	}
	dsmBase := scheme + "://127.0.0.1:" + dsmPort
	orig := proxy.Director
	proxy.Director = func(r *http.Request) {
		orig(r)
		r.Header.Set(dsmBaseHeader, dsmBase)
		// Incoming path looks like /webman/3rdparty/<id>/api.cgi/scan
		// — keep only what follows api.cgi and mount it under /api.
		p := r.URL.Path
		if i := strings.Index(p, "api.cgi"); i >= 0 {
			p = p[i+len("api.cgi"):]
		}
		if p == "" {
			p = "/"
		}
		r.URL.Path = "/api" + strings.TrimSuffix(p, "/")
		r.Host = target.Host
		if token != "" {
			r.Header.Set(authTokenHeader, token)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Duplicate Finder service is not running. Start the package in Package Center.",
		})
	}
	// Bound the body before it reaches the daemon: no request the app sends
	// comes near this, and decoding an arbitrary one would cost the daemon
	// memory in the middle of a scan or a move.
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBody {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]string{"error": "request too large"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		proxy.ServeHTTP(w, r)
	})
	if err := cgi.Serve(limited); err != nil {
		fmt.Fprintf(os.Stderr, "cgi: %v\n", err)
		os.Exit(1)
	}
}
