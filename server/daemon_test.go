package main

import (
	"net/http"
	"testing"
)

// The server reaps what it must and nothing more: a header timeout against a
// connection that never sends its request, an idle timeout against keep-alive
// connections nobody uses — and no read or write deadline, because a 64 MiB
// move list and a move batch that runs for hours are both legitimate.
func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("header/idle timeouts must be set: %v / %v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("whole-request deadlines would cut off long moves: read %v write %v", srv.ReadTimeout, srv.WriteTimeout)
	}
}

func TestFmtBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 1 << 30: "1.0 GB", 5 << 40: "5.0 TB"} {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
