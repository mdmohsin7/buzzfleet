package fleet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ── Docker clamp ──────────────────────────────────────────────────────────────

var testCaps = Limits{DockerCPUs: 1, DockerMemoryMB: 3072}

func clamp(t *testing.T, kind, body string) (map[string]any, []string) {
	t.Helper()
	out, changes, err := ClampContainer(kind, []byte(body), testCaps, "agent-a", 1762300000)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, changes
}

func hostConfig(doc map[string]any) map[string]any { return doc["HostConfig"].(map[string]any) }

func TestClampAddsDefaultsAndLabels(t *testing.T) {
	// `docker run phpunit` with no resource flags: the case that took the box down.
	doc, changes := clamp(t, "create", `{"Image":"php:8","Labels":{"keep":"me"},"HostConfig":{"Binds":["/x:/x"]}}`)
	hc := hostConfig(doc)
	if hc["NanoCpus"].(float64) != 1e9 || hc["Memory"].(float64) != 3072<<20 {
		t.Fatalf("defaults not applied: %v", hc)
	}
	labels := doc["Labels"].(map[string]any)
	if labels[LabelAgent] != "agent-a" || labels[LabelCreated] != "1762300000" || labels["keep"] != "me" {
		t.Fatalf("labels: %v", labels)
	}
	if hc["Binds"] == nil || doc["Image"] != "php:8" {
		t.Fatal("unrelated fields must pass through")
	}
	if len(changes) != 2 {
		t.Fatalf("changes: %v", changes)
	}
}

func TestClampOnlyLowers(t *testing.T) {
	doc, changes := clamp(t, "create", `{"HostConfig":{"NanoCpus":500000000,"Memory":1073741824}}`)
	hc := hostConfig(doc)
	if hc["NanoCpus"].(float64) != 5e8 || hc["Memory"].(float64) != 1<<30 || len(changes) != 0 {
		t.Fatalf("a request under the cap must be kept as asked: %v %v", hc, changes)
	}
	doc, _ = clamp(t, "create", `{"HostConfig":{"NanoCpus":4000000000,"Memory":17179869184}}`)
	hc = hostConfig(doc)
	if hc["NanoCpus"].(float64) != 1e9 || hc["Memory"].(float64) != 3072<<20 {
		t.Fatalf("over the cap must be clamped: %v", hc)
	}
}

func TestClampRespectsCpuQuotaForm(t *testing.T) {
	// Docker rejects NanoCpus together with CpuQuota, so the clamp must stay
	// in the caller's form.
	doc, _ := clamp(t, "create", `{"HostConfig":{"CpuQuota":300000,"CpuPeriod":100000}}`)
	hc := hostConfig(doc)
	if hc["CpuQuota"].(float64) != 100000 {
		t.Fatalf("quota: %v", hc)
	}
	if _, set := hc["NanoCpus"]; set {
		t.Fatal("NanoCpus must not be added next to CpuQuota")
	}
}

func TestClampSwap(t *testing.T) {
	doc, _ := clamp(t, "create", `{"HostConfig":{"Memory":1073741824,"MemorySwap":-1}}`)
	if hostConfig(doc)["MemorySwap"].(float64) != 2<<30 {
		t.Fatalf("unlimited swap must be bounded: %v", hostConfig(doc))
	}
	// A memory clamp below an explicit memory+swap would leave Docker an
	// invalid pair; it must be raised to match.
	doc, _ = clamp(t, "create", `{"HostConfig":{"Memory":17179869184,"MemorySwap":17179869184}}`)
	hc := hostConfig(doc)
	if hc["MemorySwap"].(float64) != 2*(3072<<20) {
		t.Fatalf("swap: %v", hc)
	}
}

func TestClampUpdateCannotLiftTheCap(t *testing.T) {
	doc, _ := clamp(t, "update", `{"NanoCpus":8000000000,"Memory":0}`)
	if doc["NanoCpus"].(float64) != 1e9 {
		t.Fatalf("update: %v", doc)
	}
	if doc["Memory"].(float64) != 0 || doc["Labels"] != nil {
		t.Fatalf("update must not invent fields: %v", doc)
	}
}

func TestClampKeepsLargeIntegersExact(t *testing.T) {
	out, _, err := ClampContainer("create", []byte(`{"HostConfig":{"KernelMemoryTCP":9007199254740993}}`), testCaps, "a", 1)
	if err != nil || !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("integer corrupted: %s %v", out, err)
	}
}

func TestRewriteKind(t *testing.T) {
	cases := map[string]string{
		"POST /v1.47/containers/create":     "create",
		"POST /containers/create":           "create",
		"POST /v1.47/containers/abc/update": "update",
		"GET /v1.47/containers/create":      "",
		"POST /v1.47/containers/abc/start":  "",
		"POST /v1.47/build":                 "",
	}
	for in, want := range cases {
		m, p, _ := strings.Cut(in, " ")
		if got := RewriteKind(m, p); got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestClampLowercaseFieldsCannotSlipPast(t *testing.T) {
	// dockerd matches JSON keys case-insensitively, so these are real caps.
	for _, body := range []string{
		`{"HostConfig":{"memory":4294967296,"nanoCpus":4000000000}}`,
		`{"hostConfig":{"Memory":4294967296,"NanoCpus":4e9}}`,
		`{"hostconfig":{"MEMORY":4294967296}}`,
	} {
		out, _, err := ClampContainer("create", []byte(body), testCaps, "agent-a", 1)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if strings.Contains(strings.ToLower(s), "4294967296") || strings.Contains(s, "4000000000") || strings.Contains(s, "4e9") {
			t.Fatalf("%s: uncapped value survived: %s", body, s)
		}
		var doc map[string]any
		_ = json.Unmarshal(out, &doc)
		hc, ok := doc["HostConfig"].(map[string]any)
		if !ok || len(doc) != 2 || hc["Memory"].(float64) != 3072<<20 || hc["NanoCpus"].(float64) != 1e9 {
			t.Fatalf("%s: want one canonical, capped HostConfig: %s", body, s)
		}
	}
}

func TestClampLowercaseLabelsCannotSpoofTheAgent(t *testing.T) {
	out, _, err := ClampContainer("create", []byte(`{"labels":{"buzzfleet.agent":"someone-else","keep":"me"}}`), testCaps, "agent-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(out, &doc)
	if _, stray := doc["labels"]; stray {
		t.Fatalf("the lowercase key must be folded in: %s", out)
	}
	labels := doc["Labels"].(map[string]any)
	if labels[LabelAgent] != "agent-a" || labels["keep"] != "me" {
		t.Fatalf("labels: %v", labels)
	}
}

func TestClampRefusesTwoSpellingsOfOneField(t *testing.T) {
	for _, body := range []string{
		`{"HostConfig":{"Memory":1},"hostConfig":{"Memory":4294967296}}`,
		`{"HostConfig":{"Memory":1,"memory":4294967296}}`,
		`{"Labels":{},"labels":{"buzzfleet.agent":"x"}}`,
	} {
		if _, _, err := ClampContainer("create", []byte(body), testCaps, "agent-a", 1); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("%s: want ErrAmbiguous, got %v", body, err)
		}
	}
}

func TestClampPeriodWithoutQuota(t *testing.T) {
	// dockerd refuses NanoCpus next to CpuPeriod; the cap has to be a quota.
	doc, _ := clamp(t, "create", `{"HostConfig":{"CpuPeriod":50000}}`)
	hc := hostConfig(doc)
	if hc["CpuQuota"].(float64) != 50000 {
		t.Fatalf("quota: %v", hc)
	}
	if _, set := hc["NanoCpus"]; set {
		t.Fatal("NanoCpus must not be added next to CpuPeriod")
	}
}

func TestClampRefusesMalformedBodies(t *testing.T) {
	for _, body := range []string{`{not json`, `[1,2]`, `"x"`} {
		if _, _, err := ClampContainer("create", []byte(body), testCaps, "agent-a", 1); err == nil {
			t.Fatalf("%s: must be refused, not passed through", body)
		}
	}
}

func TestUpdateCannotLiftTheCapWithNegativeValues(t *testing.T) {
	// dockerd reads -1 as unlimited, even on a container capped with
	// NanoCpus, and writes "max" to the cgroup.
	for _, body := range []string{
		`{"CpuQuota":-1}`,
		`{"cpuQuota":-1}`,
		`{"CpuQuota":-1,"CpuPeriod":100000}`,
		`{"NanoCpus":-1}`,
		`{"Memory":-1}`,
	} {
		if _, _, err := ClampContainer("update", []byte(body), testCaps, "agent-a", 1); err == nil {
			t.Fatalf("update %s must be refused", body)
		}
	}
	if _, _, err := ClampContainer("create", []byte(`{"HostConfig":{"CpuQuota":-1}}`), testCaps, "agent-a", 1); err == nil {
		t.Fatal("create with an unlimited quota must be refused")
	}
	// The CLI sends zeroes for everything it was not asked to change.
	if _, _, err := ClampContainer("update", []byte(`{"CpuQuota":0,"NanoCpus":0,"Memory":0,"CpuPeriod":0}`), testCaps, "agent-a", 1); err != nil {
		t.Fatalf("zeroes mean unchanged: %v", err)
	}
}

func TestUpdateSwapAloneIsBounded(t *testing.T) {
	doc, _ := clamp(t, "update", `{"MemorySwap":-1}`)
	if doc["MemorySwap"].(float64) != 2*(3072<<20) {
		t.Fatalf("swap: %v", doc)
	}
	// Memory unknown: a small swap stays small.
	doc, _ = clamp(t, "update", `{"MemorySwap":209715200}`)
	if doc["MemorySwap"].(float64) != 209715200 {
		t.Fatalf("swap must not be raised without knowing memory: %v", doc)
	}
}
