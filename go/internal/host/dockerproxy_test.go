package host

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// fakeDocker answers on a unix socket like dockerd: it records container
// create bodies, and upgrades attach requests to a raw stream that echoes.
func fakeDocker(t *testing.T, sock string, got chan<- map[string]any) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.47/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var doc map[string]any
		_ = json.NewDecoder(r.Body).Decode(&doc)
		got <- doc
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"abc"}`)
	})
	mux.HandleFunc("/v1.47/containers/abc/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		line, _ := buf.ReadString('\n')
		_, _ = io.WriteString(conn, "echo:"+line)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
}

func startProxy(t *testing.T) (string, chan map[string]any) {
	t.Helper()
	dir := t.TempDir()
	up, sock := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "proxy", "agent.sock")
	got := make(chan map[string]any, 1)
	fakeDocker(t, up, got)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &DockerProxy{Agent: "agent-a", Socket: sock, Upstream: up,
		Limits: fleet.Limits{DockerCPUs: 1, DockerMemoryMB: 512}, Logf: t.Logf}
	go func() { _ = p.Serve(ctx) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock, got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy never listened")
	return "", nil
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sock)
	}}}
}

func TestProxyCapsContainerCreate(t *testing.T) {
	sock, got := startProxy(t)
	resp, err := unixClient(sock).Post("http://docker/v1.47/containers/create?name=x", "application/json",
		strings.NewReader(`{"Image":"php:8","HostConfig":{"Memory":8589934592}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	doc := <-got
	hc := doc["HostConfig"].(map[string]any)
	if hc["Memory"].(float64) != 512<<20 || hc["NanoCpus"].(float64) != 1e9 {
		t.Fatalf("daemon saw an uncapped container: %v", hc)
	}
	if doc["Labels"].(map[string]any)[fleet.LabelAgent] != "agent-a" {
		t.Fatalf("labels: %v", doc["Labels"])
	}
}

func TestProxyPassesUpgradedStreams(t *testing.T) {
	// `docker run -it` and `docker exec` hijack the connection; a proxy that
	// cannot carry that would break interactive use entirely.
	sock, _ := startProxy(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST /v1.47/containers/abc/attach?stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "hello\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || line != "echo:hello\n" {
		t.Fatalf("stream: %q %v", line, err)
	}
}

func TestProxySocketIsPrivate(t *testing.T) {
	sock, _ := startProxy(t)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("the socket is a door to dockerd; mode %o", fi.Mode().Perm())
	}
}

func TestProxyRefusesWhatItCannotRead(t *testing.T) {
	sock, got := startProxy(t)
	big := `{"Image":"x","Env":["` + strings.Repeat("a", maxRewriteBody) + `"],"HostConfig":{"Memory":8589934592}}`
	for name, c := range map[string]struct {
		body   string
		status int
	}{"oversized": {big, http.StatusRequestEntityTooLarge}, "malformed": {`{"HostConfig":`, http.StatusBadRequest}} {
		body := c.body
		resp, err := unixClient(sock).Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, c.status)
		}
		select {
		case <-got:
			t.Fatalf("%s: dockerd saw the request", name)
		default:
		}
	}
}
