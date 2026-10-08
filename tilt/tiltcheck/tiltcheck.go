// Package tiltcheck executes the repository's Tiltfile with the Starlark
// interpreter and Tilt's builtins stubbed, and returns what it would apply:
// every object passed to k8s_yaml and every k8s_resource call. No cluster, no
// image build, no network.
//
// The stubs refuse what they do not know, so a check built on them cannot pass
// over a Tiltfile real Tilt would reject: each takes the arguments Tilt
// documents for it and no others, an ext:// load offers only that extension's
// symbols, and local() runs only the two command shapes the Tiltfiles use.
package tiltcheck

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
	"gopkg.in/yaml.v3"
)

// The dialect Tilt enables.
var fileOptions = &syntax.FileOptions{Set: true, While: true, TopLevelControl: true, GlobalReassign: true, Recursion: true}

// Resource is one k8s_resource call.
type Resource struct {
	Name   string
	Kwargs map[string]any
}

// Result is what a Tiltfile would apply.
type Result struct {
	// Objects are the Kubernetes objects passed to k8s_yaml, parsed.
	Objects []map[string]any
	// Resources are the k8s_resource calls, in order.
	Resources []Resource
	// LocalResources are the local_resource calls, in order.
	LocalResources []Resource
	// Output is what the Tiltfile printed.
	Output string
}

// Render executes <tree>/Tiltfile with <tree> as the working directory.
func Render(tree string) (*Result, error) {
	r := &renderer{tree: tree, cache: map[string]starlark.StringDict{}}
	thread := &starlark.Thread{Name: "Tiltfile", Load: r.load, Print: r.print}
	if _, err := starlark.ExecFileOptions(fileOptions, thread, filepath.Join(tree, "Tiltfile"), nil, r.predeclared()); err != nil {
		return &r.result, describe(err)
	}
	return &r.result, nil
}

func describe(err error) error {
	if ee, ok := err.(*starlark.EvalError); ok {
		return fmt.Errorf("%s", ee.Backtrace())
	}
	return err
}

type renderer struct {
	tree   string
	cache  map[string]starlark.StringDict
	result Result
	out    strings.Builder
}

func (r *renderer) print(_ *starlark.Thread, msg string) {
	r.out.WriteString(msg)
	r.out.WriteString("\n")
	r.result.Output = r.out.String()
}

// path resolves a Tiltfile path the way Tilt does: relative to the Tiltfile's
// directory, which is the tree.
func (r *renderer) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(r.tree, p)
}

// extensions are the ext:// symbols this repository loads, each with the
// keyword arguments its definition in github.com/tilt-dev/tilt-extensions
// takes. "**" is a symbol whose definition forwards **kwargs, so any keyword
// is valid: deployment_create merges them into the container spec, helm_repo
// and docker_build_with_restart pass them on.
var extensions = map[string]map[string][]string{
	"restart_process": {"docker_build_with_restart": {"ref", "context", "entrypoint", "live_update", "base_suffix", "restart_file", "trigger", "exit_policy", "**"}},
	"secret":          {"secret_create_generic": {"name", "namespace", "from_file", "secret_type", "from_env_file"}},
	"deployment":      {"deployment_create": {"name", "image", "command", "namespace", "replicas", "ports", "resource_deps", "**"}},
	"helm_resource": {
		"helm_resource": {"name", "chart", "deps", "release_name", "namespace", "image_deps", "image_keys", "flags", "uninstall_flags", "image_selector", "container_selector", "live_update", "resource_deps", "labels", "port_forwards", "auto_init", "pod_readiness", "update_dependencies", "build_dependencies", "links"},
		"helm_repo":     {"name", "url", "resource_name", "username", "password", "**"},
	},
}

func (r *renderer) load(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	if ext, ok := strings.CutPrefix(module, "ext://"); ok {
		symbols, known := extensions[ext]
		if !known {
			return nil, fmt.Errorf("tiltcheck: extension %q is not stubbed", module)
		}
		d := starlark.StringDict{}
		for name, kwargs := range symbols {
			d[name] = r.recorder(name, kwargs)
		}
		return d, nil
	}
	from := thread.CallFrame(0).Pos.Filename()
	path := filepath.Clean(filepath.Join(filepath.Dir(from), module))
	if g, ok := r.cache[path]; ok {
		return g, nil
	}
	child := &starlark.Thread{Name: path, Load: r.load, Print: r.print}
	g, err := starlark.ExecFileOptions(fileOptions, child, path, nil, r.predeclared())
	if err != nil {
		return nil, describe(err)
	}
	r.cache[path] = g
	return g, nil
}

// recorder is a builtin that only records its call, accepting at most one
// positional argument and the named keywords.
func (r *renderer) recorder(name string, kwargs []string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 2 {
			return nil, fmt.Errorf("%s: %d positional arguments", name, len(args))
		}
		rec := Resource{Name: name, Kwargs: map[string]any{}}
		for i, a := range args {
			rec.Kwargs[fmt.Sprintf("arg%d", i)] = toGo(a)
		}
		for _, pair := range kw {
			k := string(pair[0].(starlark.String))
			if !slices.Contains(kwargs, k) && !slices.Contains(kwargs, "**") {
				return nil, fmt.Errorf("%s: unexpected keyword argument %q", name, k)
			}
			rec.Kwargs[k] = toGo(pair[1])
		}
		return starlark.None, nil
	})
}

func (r *renderer) predeclared() starlark.StringDict {
	return starlark.StringDict{
		"struct":             starlark.NewBuiltin("struct", starlarkstruct.Make),
		"analytics_settings": starlark.NewBuiltin("analytics_settings", r.analyticsSettings),
		"allow_k8s_contexts": starlark.NewBuiltin("allow_k8s_contexts", r.allowK8sContexts),
		"docker_build":       r.recorder("docker_build", []string{"ref", "context", "build_args", "dockerfile", "dockerfile_contents", "live_update", "match_in_env_vars", "ignore", "only", "entrypoint", "target", "ssh", "network", "secret", "extra_tag", "container_args", "cache_from", "pull", "platform", "extra_hosts"}),
		"k8s_resource":       starlark.NewBuiltin("k8s_resource", r.k8sResource),
		"local_resource":     starlark.NewBuiltin("local_resource", r.localResource),
		"k8s_yaml":           starlark.NewBuiltin("k8s_yaml", r.k8sYAML),
		"blob":               starlark.NewBuiltin("blob", r.blob),
		"read_file":          starlark.NewBuiltin("read_file", r.readFile),
		"read_yaml":          starlark.NewBuiltin("read_yaml", r.readYAML),
		"read_json":          starlark.NewBuiltin("read_json", r.readYAML),
		"encode_yaml":        starlark.NewBuiltin("encode_yaml", r.encodeYAML),
		"listdir":            starlark.NewBuiltin("listdir", r.listdir),
		"local":              starlark.NewBuiltin("local", r.local),
		"os": &starlarkstruct.Module{Name: "os", Members: starlark.StringDict{
			"path": &starlarkstruct.Module{Name: "path", Members: starlark.StringDict{
				"exists": starlark.NewBuiltin("exists", r.exists),
			}},
		}},
	}
}

func (r *renderer) analyticsSettings(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var enable bool
	return starlark.None, starlark.UnpackArgs(b.Name(), args, kw, "enable", &enable)
}

func (r *renderer) allowK8sContexts(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var contexts starlark.Value
	return starlark.None, starlark.UnpackArgs(b.Name(), args, kw, "contexts", &contexts)
}

func (r *renderer) k8sResource(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		workload, newName                                        string
		portForwards, extraSelectors, resourceDeps, objects      starlark.Value
		links, labels, triggerMode, autoInit, podReadiness, disc starlark.Value
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw,
		"workload?", &workload, "new_name?", &newName, "port_forwards?", &portForwards,
		"extra_pod_selectors?", &extraSelectors, "trigger_mode?", &triggerMode,
		"resource_deps?", &resourceDeps, "objects?", &objects, "auto_init?", &autoInit,
		"pod_readiness?", &podReadiness, "links?", &links, "labels?", &labels,
		"discovery_strategy?", &disc); err != nil {
		return nil, err
	}
	rec := Resource{Name: workload, Kwargs: map[string]any{}}
	for _, pair := range kw {
		rec.Kwargs[string(pair[0].(starlark.String))] = toGo(pair[1])
	}
	r.result.Resources = append(r.result.Resources, rec)
	return starlark.None, nil
}

// localResource records a local_resource call; its command is not run. The
// keywords are Tilt's.
func (r *renderer) localResource(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		name, cmd, serveCmd, cmdBat, serveCmdBat, dir, serveDir string
		allowParallel, autoInit                                 bool
		deps, triggerMode, resourceDeps, ignore, links, tags    starlark.Value
		env, serveEnv, readiness, labels                        starlark.Value
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw,
		"name", &name, "cmd?", &cmd, "deps?", &deps, "trigger_mode?", &triggerMode,
		"resource_deps?", &resourceDeps, "ignore?", &ignore, "auto_init?", &autoInit,
		"serve_cmd?", &serveCmd, "cmd_bat?", &cmdBat, "serve_cmd_bat?", &serveCmdBat,
		"allow_parallel?", &allowParallel, "links?", &links, "tags?", &tags, "env?", &env,
		"serve_env?", &serveEnv, "readiness_probe?", &readiness, "dir?", &dir,
		"serve_dir?", &serveDir, "labels?", &labels); err != nil {
		return nil, err
	}
	rec := Resource{Name: name, Kwargs: map[string]any{}}
	for _, pair := range kw {
		rec.Kwargs[string(pair[0].(starlark.String))] = toGo(pair[1])
	}
	r.result.LocalResources = append(r.result.LocalResources, rec)
	return starlark.None, nil
}

// Blob is Tilt's blob: text, as opposed to a string, which k8s_yaml reads as a
// path.
type Blob string

func (b Blob) String() string        { return string(b) }
func (b Blob) Type() string          { return "blob" }
func (b Blob) Freeze()               {}
func (b Blob) Truth() starlark.Bool  { return len(b) > 0 }
func (b Blob) Hash() (uint32, error) { return starlark.String(b).Hash() }

func (r *renderer) blob(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "input", &s); err != nil {
		return nil, err
	}
	return Blob(s), nil
}

func (r *renderer) k8sYAML(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		v     starlark.Value
		dupes bool
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "yaml", &v, "allow_duplicates?", &dupes); err != nil {
		return nil, err
	}
	var text string
	switch x := v.(type) {
	case Blob:
		text = string(x)
	case starlark.String:
		data, err := os.ReadFile(r.path(string(x)))
		if err != nil {
			return nil, fmt.Errorf("k8s_yaml: a string is a path: %w", err)
		}
		text = string(data)
	default:
		return nil, fmt.Errorf("k8s_yaml: want a blob or a path, got %s", v.Type())
	}
	dec := yaml.NewDecoder(strings.NewReader(text))
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("k8s_yaml: %w", err)
		}
		if obj != nil {
			r.result.Objects = append(r.result.Objects, obj)
		}
	}
	return starlark.None, nil
}

func (r *renderer) readFile(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		path string
		def  starlark.Value
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "file_path", &path, "default?", &def); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(r.path(path))
	if os.IsNotExist(err) && def != nil {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return Blob(data), nil
}

// readYAML serves read_yaml and read_json (JSON is YAML). Only a missing file
// falls back to the default: a parse error is an error.
func (r *renderer) readYAML(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		path string
		def  starlark.Value
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "paths", &path, "default?", &def); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(r.path(path))
	if os.IsNotExist(err) && def != nil {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("%s(%s): %w", b.Name(), path, err)
	}
	if len(n.Content) == 0 {
		return starlark.None, nil
	}
	return fromNode(n.Content[0])
}

func (r *renderer) encodeYAML(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var obj starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kw, "obj", &obj); err != nil {
		return nil, err
	}
	n, err := toNode(obj)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	return Blob(buf.String()), nil
}

func (r *renderer) listdir(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		dir       string
		recursive bool
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "directory", &dir, "recursive?", &recursive); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(r.path(dir))
	if err != nil {
		return nil, err
	}
	var l []starlark.Value
	for _, e := range ents {
		l = append(l, starlark.String(filepath.Join(r.path(dir), e.Name())))
	}
	return starlark.NewList(l), nil
}

func (r *renderer) exists(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &path); err != nil {
		return nil, err
	}
	_, err := os.Stat(r.path(path))
	return starlark.Bool(err == nil), nil
}

// localCommands are the command prefixes the Tiltfiles run: writing the
// generated tilt_config.yaml, hashing a rendered config, and writing a rendered
// config for its check.
var localCommands = []string{
	"cat > tilt_config.yaml << 'EOF'\n",
	"cat << 'PRM_CFG_EOF' | sha256sum | cut -c1-16\n",
	"mkdir -p .tilt-tmp && cat > .tilt-tmp/",
}

func (r *renderer) local(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		cmd             string
		quiet, echoOff  bool
		commandBat, dir string
		env, stdin      starlark.Value
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "command", &cmd, "quiet?", &quiet,
		"command_bat?", &commandBat, "echo_off?", &echoOff, "env?", &env, "dir?", &dir, "stdin?", &stdin); err != nil {
		return nil, err
	}
	allowed := false
	for _, prefix := range localCommands {
		if strings.HasPrefix(cmd, prefix) {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("local: command not allowed in the render check: %q", firstLine(cmd))
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = r.tree
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("local(%q): %w", firstLine(cmd), err)
	}
	return Blob(out), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func fromNode(n *yaml.Node) (starlark.Value, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return fromNode(n.Alias)
	case yaml.MappingNode:
		d := starlark.NewDict(len(n.Content) / 2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := fromNode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(n.Content[i].Value), v); err != nil {
				return nil, err
			}
		}
		return d, nil
	case yaml.SequenceNode:
		l := make([]starlark.Value, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c)
			if err != nil {
				return nil, err
			}
			l = append(l, v)
		}
		return starlark.NewList(l), nil
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, err
		}
		switch x := v.(type) {
		case nil:
			return starlark.None, nil
		case bool:
			return starlark.Bool(x), nil
		case int:
			return starlark.MakeInt(x), nil
		case float64:
			return starlark.Float(x), nil
		case string:
			return starlark.String(x), nil
		default:
			return starlark.String(n.Value), nil
		}
	}
	return nil, fmt.Errorf("unsupported YAML node kind %d", n.Kind)
}

func toNode(v starlark.Value) (*yaml.Node, error) {
	switch x := v.(type) {
	case *starlark.Dict:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, item := range x.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("encode_yaml: non-string key %s", item[0])
			}
			c, err := toNode(item[1])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: string(k)}, c)
		}
		return n, nil
	case *starlark.List, starlark.Tuple:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		it := x.(starlark.Iterable).Iterate()
		defer it.Done()
		var e starlark.Value
		for it.Next(&e) {
			c, err := toNode(e)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	}
	n := &yaml.Node{}
	if err := n.Encode(toGo(v)); err != nil {
		return nil, err
	}
	return n, nil
}

func toGo(v starlark.Value) any {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil
	case starlark.Bool:
		return bool(x)
	case starlark.Int:
		i, _ := x.Int64()
		return i
	case starlark.Float:
		return float64(x)
	case starlark.String:
		return string(x)
	case Blob:
		return string(x)
	case *starlark.List, starlark.Tuple:
		var out []any
		it := x.(starlark.Iterable).Iterate()
		defer it.Done()
		var e starlark.Value
		for it.Next(&e) {
			out = append(out, toGo(e))
		}
		return out
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			out[fmt.Sprint(toGo(item[0]))] = toGo(item[1])
		}
		return out
	}
	return v.String()
}
