package dashboard

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

const keepAlive = 20 * time.Second

// Handler serves the page, the event stream and a JSON snapshot. It is
// read-only: every method but GET and HEAD is refused, and a request whose
// Host is not a loopback name is refused too, which is what stops a hostile
// web page from reaching the dashboard through DNS rebinding.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.State())
	})
	mux.HandleFunc("/api/bead", h.serveBead)
	mux.HandleFunc("/api/report", h.serveReport)
	mux.HandleFunc("/stream", h.serveStream)
	return guard(mux)
}

func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a Host header names this machine.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackAddr reports whether addr (host:port) binds only to loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return loopbackHost(host)
}

func (h *Hub) serveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	sub, initial := h.Subscribe()
	defer h.Unsubscribe(sub)
	for _, f := range initial {
		if _, err := w.Write(f); err != nil {
			return
		}
	}
	fl.Flush()
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-sub.C:
			if !ok {
				return
			}
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		case <-tick.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// serveBead returns one bead's text for the queue's expandable rows. The query
// is validated before it reaches bd, a read failure says nothing about why, and
// reads are cached and serialized in the hub.
func (h *Hub) serveBead(w http.ResponseWriter, r *http.Request) {
	rig, id := r.URL.Query().Get("rig"), r.URL.Query().Get("id")
	if !ValidBeadRef(rig, id) {
		http.Error(w, "bad bead reference", http.StatusBadRequest)
		return
	}
	d, err := h.beadDetail(rig, id)
	if err != nil {
		http.Error(w, "bead unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// serveReport returns one earlier report's text for the Report pane, named by
// the timestamp its file carries. The name has to have the shape a report's
// name has and to be one the pane lists; the file is rebuilt from that instant,
// so no path is taken from the query string. A name that fails either check is
// refused before the directory is read, and a read that cannot be made says
// only that.
func (h *Hub) serveReport(w http.ResponseWriter, r *http.Request) {
	at, ok := reportsNameTime(r.URL.Query().Get("name"))
	if !ok {
		http.Error(w, "bad report name", http.StatusBadRequest)
		return
	}
	text, err := NewReportsReader(h.cfg.ReportsDir).ReadNamed(at)
	switch {
	case errors.Is(err, errReportsUnlisted):
		http.Error(w, "no such report", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "report unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ReportText{Text: text})
}
