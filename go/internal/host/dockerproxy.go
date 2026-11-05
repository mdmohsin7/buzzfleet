package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// DockerSocketFor is where an agent's Docker proxy listens. It lives in the
// runtime dir, which only this user can enter and which is cleared at boot, so
// a stale socket can never outlive the supervisor that served it.
func DockerSocketFor(name string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	return filepath.Join(dir, "buzzfleet", "docker-"+name+".sock")
}

// UpstreamDocker is the real daemon socket: the supervisor's own DOCKER_HOST if
// it names a unix socket, otherwise the standard one.
func UpstreamDocker() string {
	if v, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && v != "" {
		return v
	}
	return "/var/run/docker.sock"
}

// The largest container create body worth reading. Real ones are a few KB.
const maxRewriteBody = 8 << 20

// DockerProxy serves one agent's view of the Docker API.
type DockerProxy struct {
	Agent    string
	Socket   string
	Upstream string
	Limits   fleet.Limits
	Logf     func(format string, a ...any)
}

func (p *DockerProxy) handler() http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "docker"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", p.Upstream)
			},
			// Each agent is one user; a handful of idle connections is plenty.
			MaxIdleConns:    4,
			IdleConnTimeout: 90 * time.Second,
		},
		// Logs, events and build output stream; buffering them would make
		// `docker logs -f` look hung.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.Logf("docker proxy %s: %s %s: %v", p.Agent, r.Method, r.URL.Path, err)
			http.Error(w, "buzzfleet docker proxy: cannot reach the Docker daemon: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind := fleet.RewriteKind(r.Method, r.URL.Path); kind != "" {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxRewriteBody+1))
			if err != nil {
				http.Error(w, "buzzfleet docker proxy: "+err.Error(), http.StatusBadRequest)
				return
			}
			if len(body) > maxRewriteBody {
				// Passing it through unread would be a way round the caps.
				http.Error(w, "buzzfleet docker proxy: container request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body.Close()
			out, changes, err := fleet.ClampContainer(kind, body, p.Limits, p.Agent, time.Now().Unix())
			if err != nil {
				// Let Docker produce its own error for a malformed request.
				out = body
			} else if len(changes) > 0 {
				p.Logf("docker proxy %s: container %s: %s", p.Agent, kind, strings.Join(changes, ", "))
			}
			r.Body = io.NopCloser(bytes.NewReader(out))
			r.ContentLength = int64(len(out))
			r.Header.Set("Content-Length", strconv.Itoa(len(out)))
			r.Header.Del("Transfer-Encoding")
			r.TransferEncoding = nil
		}
		rp.ServeHTTP(w, r)
	})
}

// Serve listens until ctx is cancelled.
func (p *DockerProxy) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(p.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(p.Socket)
	ln, err := net.Listen("unix", p.Socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.Socket, err)
	}
	// The socket is a door to the Docker daemon, which is root in all but
	// name: only this user may open it.
	if err := os.Chmod(p.Socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: p.handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = os.Remove(p.Socket)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
