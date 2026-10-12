// Package deploy_test checks the release compose asset as a reader receives it: rendered by
// hack/render-compose.sh, then resolved by docker compose.
package deploy_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	renderTag        = "v0.0.0-test"
	defaultReaderKey = "horo_dead00000002_" +
		"11111111111111111111111111111111111111111111111111111111111111"
	pinnedImageTag = ":" + renderTag
)

type composeProject struct {
	Services map[string]composeService `json:"services"`
	Configs  map[string]struct {
		Content string `json:"content"`
	} `json:"configs"`
}

type composeService struct {
	Image       string            `json:"image"`
	Profiles    []string          `json:"profiles"`
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	Ports       []struct {
		HostIP    string `json:"host_ip"`
		Target    int    `json:"target"`
		Published string `json:"published"`
	} `json:"ports"`
	Configs []struct {
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"configs"`
}

// render runs hack/render-compose.sh in a scratch directory holding a copy of the template,
// so the test neither reads nor writes the checkout's .render/.
func render(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "hack", "render-compose.sh"))
	if err != nil {
		t.Fatal(err)
	}
	template, err := os.ReadFile("docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "deploy"), 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: a fixed name under t.TempDir().
	err = os.WriteFile(filepath.Join(work, "deploy", "docker-compose.yaml"), template, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, renderTag) //nolint:gosec // G204: the repository's own script.
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render-compose.sh %s: %v\n%s", renderTag, err, out)
	}
	return filepath.Join(work, ".render", "docker-compose.yaml")
}

// resolve checks the rendered file with `config --quiet`, as CI's reader would meet it, and
// returns the project docker compose resolves from it.
func resolve(t *testing.T, file string, profiles ...string) composeProject {
	t.Helper()
	args := []string{"compose", "-f", file}
	for _, p := range profiles {
		args = append(args, "--profile", p)
	}
	//nolint:gosec // G204: docker compose over a file this test rendered.
	quiet := exec.Command("docker", append(args, "config", "--quiet")...)
	if out, err := quiet.CombinedOutput(); err != nil {
		t.Fatalf("docker %s config --quiet: %v\n%s", strings.Join(args, " "), err, out)
	}
	//nolint:gosec // G204: as above.
	out, err := exec.Command("docker", append(args, "config", "--format", "json")...).Output()
	if err != nil {
		t.Fatalf("docker %s config --format json: %v", strings.Join(args, " "), err)
	}
	var project composeProject
	if err := json.Unmarshal(out, &project); err != nil {
		t.Fatalf("parse docker compose config: %v", err)
	}
	return project
}

// mounted returns the content a service's container sees at target. `config` prints inline
// content with its `$$` escapes intact; the container receives each as a single `$`.
func mounted(t *testing.T, p composeProject, service, target string) string {
	t.Helper()
	for _, c := range p.Services[service].Configs {
		if c.Target == target {
			return strings.ReplaceAll(p.Configs[c.Source].Content, "$$", "$")
		}
	}
	t.Fatalf("service %s mounts nothing at %s", service, target)
	return ""
}

func unmarshalYAML(t *testing.T, content string, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict([]byte(content), into); err != nil {
		t.Fatalf("parse %q: %v", content, err)
	}
}

func serviceNames(p composeProject) []string {
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func clearComposeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"HOROPTER_IMAGE_TAG", "HOROPTER_DEV_PRODUCER_KEY", "HOROPTER_DEV_READER_KEY",
		"COMPOSE_PROFILES", "COMPOSE_FILE", "COMPOSE_PROJECT_NAME",
	} {
		t.Setenv(name, "")
	}
}

func TestWithoutProfileFiveServicesRunWithMetricsOffTheHost(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t))

	want := []string{"bootstrap", "db", "egress", "horopter-migrate", "ingress"}
	if got := serviceNames(p); !slices.Equal(got, want) {
		t.Errorf("services without a profile = %v, want %v", got, want)
	}
	for _, name := range []string{"ingress", "egress"} {
		s := p.Services[name]
		if got := s.Environment["OTEL_EXPORTER_PROMETHEUS_HOST"]; got != "0.0.0.0" {
			t.Errorf("%s OTEL_EXPORTER_PROMETHEUS_HOST = %q, want 0.0.0.0", name, got)
		}
		for _, port := range s.Ports {
			if port.Target == 9464 {
				t.Errorf("%s publishes its metrics port on the host: %+v", name, port)
			}
		}
	}
}

// The asset is a trial stack on one machine, and its dev keys are public, so the APIs
// answer only on the host's loopback.
func TestDaemonsPublishTheirAPIsOnLoopbackOnly(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t))

	for name, port := range map[string]int{"ingress": 8080, "egress": 8081} {
		s := p.Services[name]
		if len(s.Ports) != 1 || s.Ports[0].HostIP != "127.0.0.1" ||
			s.Ports[0].Target != port || s.Ports[0].Published != strconv.Itoa(port) {
			t.Errorf("%s ports = %+v, want only 127.0.0.1:%d:%d", name, s.Ports, port, port)
		}
	}
}

func TestGrafanaProfileAddsPrometheusAndGrafanaOnLoopback(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t), "grafana")

	want := []string{
		"bootstrap", "db", "egress", "grafana", "horopter-migrate", "ingress", "prometheus",
	}
	if got := serviceNames(p); !slices.Equal(got, want) {
		t.Errorf("services with the grafana profile = %v, want %v", got, want)
	}
	for name, port := range map[string]int{"prometheus": 9090, "grafana": 3000} {
		s := p.Services[name]
		if !slices.Equal(s.Profiles, []string{"grafana"}) {
			t.Errorf("%s profiles = %v, want [grafana]", name, s.Profiles)
		}
		if len(s.Ports) != 1 || s.Ports[0].HostIP != "127.0.0.1" ||
			s.Ports[0].Target != port || s.Ports[0].Published != strconv.Itoa(port) {
			t.Errorf("%s ports = %+v, want only 127.0.0.1:%d:%d", name, s.Ports, port, port)
		}
	}
}

// The asset's third-party pins are exact versions written in the template, not ${VAR:-…}
// defaults: the tutorial tests the file a reader downloads.
func TestGrafanaProfilePinsAreLiterals(t *testing.T) {
	template, err := os.ReadFile("docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Services map[string]struct {
			Image       string            `json:"image"`
			Environment map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(template, &raw); err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{
		"prometheus image":           raw.Services["prometheus"].Image,
		"grafana image":              raw.Services["grafana"].Image,
		"GF_PLUGINS_PREINSTALL_SYNC": raw.Services["grafana"].Environment["GF_PLUGINS_PREINSTALL_SYNC"],
	}
	for what, pin := range pins {
		if !isLiteralPin(pin) {
			t.Errorf("%s = %q, want a literal exact version", what, pin)
		}
	}
	if !strings.HasPrefix(pins["GF_PLUGINS_PREINSTALL_SYNC"], "yesoreyeram-infinity-datasource@") {
		t.Errorf("GF_PLUGINS_PREINSTALL_SYNC = %q, want Infinity at a pinned version",
			pins["GF_PLUGINS_PREINSTALL_SYNC"])
	}
}

var exactVersion = regexp.MustCompile(`^[^$]+[:@]v?[0-9]+\.[0-9]+\.[0-9]+$`)

func isLiteralPin(pin string) bool {
	return exactVersion.MatchString(pin)
}

func TestPrometheusScrapesBothDaemons(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t), "grafana")

	var config struct {
		Global struct {
			ScrapeInterval string `json:"scrape_interval"`
		} `json:"global"`
		ScrapeConfigs []struct {
			JobName       string `json:"job_name"`
			StaticConfigs []struct {
				Targets []string `json:"targets"`
			} `json:"static_configs"`
		} `json:"scrape_configs"`
	}
	unmarshalYAML(t, mounted(t, p, "prometheus", "/etc/prometheus/prometheus.yml"), &config)

	if config.Global.ScrapeInterval != "5s" {
		t.Errorf("scrape_interval = %q, want 5s", config.Global.ScrapeInterval)
	}
	jobs := map[string][]string{}
	for _, sc := range config.ScrapeConfigs {
		for _, s := range sc.StaticConfigs {
			jobs[sc.JobName] = append(jobs[sc.JobName], s.Targets...)
		}
	}
	for _, job := range []string{"ingress", "egress"} {
		if want := []string{job + ":9464"}; !slices.Equal(jobs[job], want) {
			t.Errorf("job %s targets = %v, want %v", job, jobs[job], want)
		}
	}
	if len(jobs) != 2 {
		t.Errorf("scrape jobs = %v, want only ingress and egress", jobs)
	}
}

type datasource struct {
	Name           string            `json:"name"`
	UID            string            `json:"uid"`
	Type           string            `json:"type"`
	Access         string            `json:"access"`
	URL            string            `json:"url"`
	IsDefault      bool              `json:"isDefault"`
	JSONData       map[string]any    `json:"jsonData"`
	SecureJSONData map[string]string `json:"secureJsonData"`
}

func datasources(t *testing.T, p composeProject) map[string]datasource {
	t.Helper()
	var file struct {
		APIVersion  int          `json:"apiVersion"`
		Datasources []datasource `json:"datasources"`
	}
	unmarshalYAML(t, mounted(t, p, "grafana",
		"/etc/grafana/provisioning/datasources/horopter.yaml"), &file)
	byUID := map[string]datasource{}
	for _, ds := range file.Datasources {
		byUID[ds.UID] = ds
	}
	return byUID
}

func TestGrafanaPrometheusDatasource(t *testing.T) {
	clearComposeEnv(t)
	prom := datasources(t, resolve(t, render(t), "grafana"))["prometheus"]

	if prom.Name != "Prometheus" || prom.Type != "prometheus" ||
		prom.URL != "http://prometheus:9090" {
		t.Errorf("Prometheus datasource = %+v", prom)
	}
}

// The Horopter datasource sends the reader key bootstrap issues, and only to egress.
func TestGrafanaHoropterDatasource(t *testing.T) {
	clearComposeEnv(t)
	h := datasources(t, resolve(t, render(t), "grafana"))["horopter"]

	if h.Name != "Horopter" || h.Type != "yesoreyeram-infinity-datasource" ||
		h.URL != "http://egress:8081" {
		t.Errorf("Horopter datasource = %+v", h)
	}
	if got, ok := h.JSONData["allowedHosts"].([]any); !ok ||
		!slices.Equal(got, []any{"http://egress:8081"}) {
		t.Errorf("Horopter allowedHosts = %v, want only http://egress:8081",
			h.JSONData["allowedHosts"])
	}
	if got := h.JSONData["httpHeaderName1"]; got != "Authorization" {
		t.Errorf("Horopter httpHeaderName1 = %v, want Authorization", got)
	}
	if got, want := h.SecureJSONData["httpHeaderValue1"], "Bearer "+defaultReaderKey; got != want {
		t.Errorf("Horopter httpHeaderValue1 = %q, want %q", got, want)
	}
}

// One variable sets the reader key both bootstrap issues and Grafana sends, so overriding
// it cannot leave the datasource holding a key egress no longer accepts.
func TestReaderKeyOverrideReachesBootstrapAndGrafana(t *testing.T) {
	clearComposeEnv(t)
	const key = "horo_beef00000002_" +
		"22222222222222222222222222222222222222222222222222222222222222"
	t.Setenv("HOROPTER_DEV_READER_KEY", key)
	p := resolve(t, render(t), "grafana")

	header := datasources(t, p)["horopter"].SecureJSONData["httpHeaderValue1"]
	if header != "Bearer "+key {
		t.Errorf("Horopter httpHeaderValue1 = %q, want Bearer %s", header, key)
	}
	if cmd := p.Services["bootstrap"].Command; !slices.Contains(cmd, key) {
		t.Errorf("bootstrap command = %v, want it to issue reader key %s", cmd, key)
	}
}

type dashboard struct {
	UID     string `json:"uid"`
	Refresh string `json:"refresh"`
	Time    struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"time"`
	Panels []struct {
		Type    string `json:"type"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
	Annotations struct {
		List []struct {
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
			Target struct {
				URL string `json:"url"`
			} `json:"target"`
			Mappings map[string]struct {
				Value string `json:"value"`
			} `json:"mappings"`
		} `json:"list"`
	} `json:"annotations"`
}

const dashboardPath = "/etc/grafana/dashboards/try-horopter.json"

func tryHoropterDashboard(t *testing.T, p composeProject) dashboard {
	t.Helper()
	var d dashboard
	if err := json.Unmarshal([]byte(mounted(t, p, "grafana", dashboardPath)), &d); err != nil {
		t.Fatalf("dashboard is not JSON: %v", err)
	}
	return d
}

func TestTryHoropterDashboardIsHome(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t), "grafana")
	d := tryHoropterDashboard(t, p)

	if d.UID != "try-horopter" || d.Refresh != "5s" || d.Time.From != "now-15m" ||
		d.Time.To != "now" {
		t.Errorf("dashboard uid/refresh/time = %q/%q/%+v", d.UID, d.Refresh, d.Time)
	}
	home := p.Services["grafana"].Environment["GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH"]
	if home != dashboardPath {
		t.Errorf("home dashboard = %q, want %s", home, dashboardPath)
	}
}

func TestTryHoropterDashboardPlotsRequestRates(t *testing.T) {
	clearComposeEnv(t)
	d := tryHoropterDashboard(t, resolve(t, render(t), "grafana"))

	wantExpr := "sum by (job) (rate(http_server_request_duration_seconds_count[30s]))"
	if len(d.Panels) != 1 || d.Panels[0].Type != "timeseries" ||
		len(d.Panels[0].Targets) != 1 || d.Panels[0].Targets[0].Expr != wantExpr {
		t.Errorf("panels = %+v, want one time series of %s", d.Panels, wantExpr)
	}
}

func TestTryHoropterDashboardAnnotatesChanges(t *testing.T) {
	clearComposeEnv(t)
	d := tryHoropterDashboard(t, resolve(t, render(t), "grafana"))

	if len(d.Annotations.List) != 1 {
		t.Fatalf("annotations = %+v, want one", d.Annotations.List)
	}
	a := d.Annotations.List[0]
	if a.Datasource.UID != "horopter" ||
		a.Target.URL != "/adapters/grafana/annotations?from=${__from}&to=${__to}" {
		t.Errorf("annotation datasource/url = %q/%q", a.Datasource.UID, a.Target.URL)
	}
	for _, field := range []string{"time", "timeEnd", "title", "text", "tags"} {
		if a.Mappings[field].Value != field {
			t.Errorf("annotation mapping %s = %+v, want field %s", field, a.Mappings[field], field)
		}
	}
}

func TestGrafanaProvisionsDashboardsFromTheMountedDirectory(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t), "grafana")

	var file struct {
		APIVersion int `json:"apiVersion"`
		Providers  []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Options struct {
				Path string `json:"path"`
			} `json:"options"`
		} `json:"providers"`
	}
	unmarshalYAML(t, mounted(t, p, "grafana",
		"/etc/grafana/provisioning/dashboards/horopter.yaml"), &file)
	if len(file.Providers) != 1 || file.Providers[0].Type != "file" ||
		file.Providers[0].Options.Path != filepath.Dir(dashboardPath) {
		t.Errorf("dashboard providers = %+v, want one file provider of %s",
			file.Providers, filepath.Dir(dashboardPath))
	}
}

func TestGrafanaIsAnonymousViewerWithoutLogin(t *testing.T) {
	clearComposeEnv(t)
	env := resolve(t, render(t), "grafana").Services["grafana"].Environment

	for name, want := range map[string]string{
		"GF_AUTH_ANONYMOUS_ENABLED":  "true",
		"GF_AUTH_ANONYMOUS_ORG_ROLE": "Viewer",
		"GF_AUTH_DISABLE_LOGIN_FORM": "true",
	} {
		if env[name] != want {
			t.Errorf("grafana %s = %q, want %q", name, env[name], want)
		}
	}
}

func TestDaemonImagesCarryTheRenderedTag(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, render(t))
	for _, name := range []string{"horopter-migrate", "bootstrap", "ingress", "egress"} {
		if img := p.Services[name].Image; !strings.HasSuffix(img, pinnedImageTag) {
			t.Errorf("%s image = %q, want tag %s", name, img, renderTag)
		}
	}
}

// The repository's development stack publishes Postgres with a known password and runs on
// public dev keys, so nothing it publishes answers beyond the host's loopback.
func TestDevStackPublishesOnLoopbackOnly(t *testing.T) {
	clearComposeEnv(t)
	p := resolve(t, filepath.Join("..", "docker-compose.yaml"), "*")

	for name, s := range p.Services {
		for _, port := range s.Ports {
			if port.HostIP != "127.0.0.1" {
				t.Errorf("%s publishes %+v beyond loopback", name, port)
			}
		}
	}
}
