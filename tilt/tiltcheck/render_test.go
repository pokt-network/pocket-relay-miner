package tiltcheck

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Tiltfile renders of both modes, from the repository's tracked files. Each
// rendered relay-miner config is checked with `<binary> <mode> validate`, the
// same load the pod runs at startup with unknown keys fatal. PRM_BIN names the
// binary; scripts/gates/static.sh builds it. Without it the validate checks are
// skipped, and the gate fails on any skip.

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// trackedTree copies the repository's tracked files into a temporary
// directory, so what renders is what is committed: never a developer's
// tilt_config.yaml, and never a write to the checkout.
func trackedTree(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-c", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	dst := t.TempDir()
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) {
			continue // deleted in the working tree, not committed yet
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func render(t *testing.T, tiltConfig string) (*Result, string) {
	t.Helper()
	tree := trackedTree(t)
	if tiltConfig != "" {
		if err := os.WriteFile(filepath.Join(tree, "tilt_config.yaml"), []byte(tiltConfig), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Render(tree)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return res, tree
}

func exampleConfig(t *testing.T, mode string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "tilt_config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const key = "\nrelay_miner_mode: ha\n"
	if !bytes.Contains(data, []byte(key)) {
		t.Fatalf("premise: tilt_config.example.yaml sets %q", strings.TrimSpace(key))
	}
	return strings.Replace(string(data), key, "\nrelay_miner_mode: "+mode+"\n", 1)
}

func objects(res *Result, kind string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range res.Objects {
		if o["kind"] == kind {
			out[dig(o, "metadata", "name").(string)] = o
		}
	}
	return out
}

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// validate writes a ConfigMap's config.yaml and runs `<binary> <mode> validate`
// on it.
func validate(t *testing.T, res *Result, configMap, mode string) {
	t.Helper()
	bin := os.Getenv("PRM_BIN")
	if bin == "" {
		t.Skip("PRM_BIN is not set: scripts/gates/static.sh builds the binary and sets it")
	}
	cm, ok := objects(res, "ConfigMap")[configMap]
	if !ok {
		t.Fatalf("no ConfigMap %s rendered", configMap)
	}
	text, _ := dig(cm, "data", "config.yaml").(string)
	if text == "" {
		t.Fatalf("ConfigMap %s has no config.yaml", configMap)
	}
	path := filepath.Join(t.TempDir(), configMap+".yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, mode, "validate", "--config", path).CombinedOutput()
	if err != nil {
		t.Fatalf("%s validate rejects the rendered %s: %v\n%s", mode, configMap, err, out)
	}
}

// container returns the Deployment's one container, and fails unless it reads
// its config from the volume of the named ConfigMap: a validated config the pod
// does not read proves nothing.
func container(t *testing.T, dep map[string]any, configMap string) map[string]any {
	t.Helper()
	spec := dig(dep, "spec", "template", "spec")
	c, _ := dig(spec, "containers", 0).(map[string]any)
	if c == nil {
		t.Fatalf("Deployment %v has no container", dig(dep, "metadata", "name"))
	}
	if !contains(c["command"], "--config=/config/config.yaml") {
		t.Fatalf("Deployment %v: command %v does not read /config/config.yaml", dig(dep, "metadata", "name"), c["command"])
	}
	mounted := false
	for _, m := range asList(c["volumeMounts"]) {
		if dig(m, "mountPath") == "/config" {
			for _, v := range asList(dig(spec, "volumes")) {
				if dig(v, "name") == dig(m, "name") && dig(v, "configMap", "name") == configMap {
					mounted = true
				}
			}
		}
	}
	if !mounted {
		t.Fatalf("Deployment %v: /config is not the ConfigMap %s", dig(dep, "metadata", "name"), configMap)
	}
	return c
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func contains(list any, want string) bool {
	for _, v := range asList(list) {
		if v == want {
			return true
		}
	}
	return false
}

func requireLocalnetConfigMaps(t *testing.T, res *Result) {
	t.Helper()
	cms := objects(res, "ConfigMap")
	for _, name := range []string{"genesis-config", "all-keys-config"} {
		if _, ok := cms[name]; !ok {
			t.Errorf("ConfigMap %s not rendered: a file the Tiltfile skips when absent is missing", name)
		}
	}
}

func requireHA(t *testing.T, res *Result) {
	t.Helper()
	deps := objects(res, "Deployment")
	for _, side := range []string{"relayer", "miner"} {
		dep, ok := deps[side]
		if !ok {
			t.Fatalf("high-availability mode: no %s Deployment", side)
		}
		container(t, dep, side+"-config")
	}
	if _, ok := deps["standalone"]; ok {
		t.Fatal("high-availability mode rendered a standalone Deployment")
	}
	requireLocalnetConfigMaps(t, res)
	validate(t, res, "relayer-config", "relayer")
	validate(t, res, "miner-config", "miner")
}

func requireStandalone(t *testing.T, res *Result) {
	t.Helper()
	deps := objects(res, "Deployment")
	for _, side := range []string{"relayer", "miner"} {
		if _, ok := deps[side]; ok {
			t.Fatalf("standalone mode rendered a %s Deployment: both would serve and claim the same suppliers", side)
		}
	}
	dep, ok := deps["standalone"]
	if !ok {
		t.Fatal("standalone mode: no standalone Deployment")
	}
	if r := dig(dep, "spec", "replicas"); r != 1 {
		t.Fatalf("standalone replicas = %v, want 1: the store takes an exclusive lock", r)
	}
	if s := dig(dep, "spec", "strategy", "type"); s != "Recreate" {
		t.Fatalf("standalone strategy = %v, want Recreate: a new pod must not start while the old one holds the store", s)
	}
	c := container(t, dep, "standalone-config")
	if !contains(c["command"], "standalone") || !contains(c["command"], "--strict-config") {
		t.Fatalf("standalone command %v: want the standalone subcommand with --strict-config", c["command"])
	}
	svc, ok := objects(res, "Service")["relayer"]
	if !ok {
		t.Fatal("standalone mode: no Service relayer, the name the suppliers are staked at")
	}
	if sel := dig(svc, "spec", "selector", "app"); sel != "standalone" {
		t.Fatalf("Service relayer selects app=%v, want standalone", sel)
	}
	requireLocalnetConfigMaps(t, res)
	validate(t, res, "standalone-config", "standalone")
}

// A first run: no tilt_config.yaml, so the Tiltfile generates one, in the copy.
func TestRender_HighAvailability_FirstRun(t *testing.T) {
	root := repoRoot(t)
	before, beforeErr := os.ReadFile(filepath.Join(root, "tilt_config.yaml"))
	res, tree := render(t, "")
	if _, err := os.Stat(filepath.Join(tree, "tilt_config.yaml")); err != nil {
		t.Fatalf("premise: a first run generates tilt_config.yaml: %v", err)
	}
	after, afterErr := os.ReadFile(filepath.Join(root, "tilt_config.yaml"))
	if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
		t.Fatal("the render changed the checkout's tilt_config.yaml")
	}
	requireHA(t, res)
}

func TestRender_HighAvailability_Example(t *testing.T) {
	res, _ := render(t, exampleConfig(t, "ha"))
	requireHA(t, res)
}

func TestRender_Standalone_Minimal(t *testing.T) {
	res, _ := render(t, "relay_miner_mode: standalone\n")
	requireStandalone(t, res)
}

func TestRender_Standalone_Example(t *testing.T) {
	res, _ := render(t, exampleConfig(t, "standalone"))
	requireStandalone(t, res)
}

// A value the relayer and the miner configs set differently is refused, and
// for that reason: an unrelated error would read as the same failure.
func TestRender_Standalone_RefusesConflictingSides(t *testing.T) {
	tree := trackedTree(t)
	cfg := "relay_miner_mode: standalone\nrelayer:\n  config:\n    pocket_node:\n      chain_id: some-other-chain\n"
	if err := os.WriteFile(filepath.Join(tree, "tilt_config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Render(tree)
	if err == nil {
		t.Fatal("rendered a standalone config whose relayer and miner disagree on pocket_node.chain_id")
	}
	if !strings.Contains(err.Error(), "standalone: pocket_node.chain_id is") {
		t.Fatalf("refused for another reason: %v", err)
	}
}

// The stubs refuse what real Tilt would: a check built on stubs that accept
// anything passes over a broken Tiltfile.
func TestStubs_RefuseWhatTiltRefuses(t *testing.T) {
	cases := map[string]string{
		"misspelled keyword":         `k8s_resource("x", resource_dep=["y"])`,
		"symbol from another ext":    `load("ext://secret", "docker_build_with_restart")`,
		"unknown extension":          `load("ext://nope", "x")`,
		"unlisted local command":     `local("kubectl get pods")`,
		"k8s_yaml of a missing path": `k8s_yaml("does-not-exist.yaml")`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			if err := os.WriteFile(filepath.Join(tree, "Tiltfile"), []byte(body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Render(tree); err == nil {
				t.Fatalf("accepted: %s", body)
			}
		})
	}
	t.Run("control: a valid call renders", func(t *testing.T) {
		tree := t.TempDir()
		body := fmt.Sprintf("k8s_yaml(blob(%q))\nk8s_resource(%q, resource_deps=[%q])\n", "kind: ConfigMap\nmetadata:\n  name: c\n", "x", "y")
		if err := os.WriteFile(filepath.Join(tree, "Tiltfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := Render(tree)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if len(res.Objects) != 1 || len(res.Resources) != 1 {
			t.Fatalf("got %d objects, %d resources; want 1 and 1", len(res.Objects), len(res.Resources))
		}
	})
}
