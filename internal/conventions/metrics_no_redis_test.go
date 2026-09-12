package conventions

import (
	"go/ast"
	"strings"
	"testing"
)

// The relayer's Redis pool is sized as validationWorkers + publishWorkers +
// margin (relayer/sizing.go, RedisPoolSize). There are THREE worker subpools,
// and the third one -- metrics -- is deliberately not a term, because the
// metrics recorder only writes Prometheus observations in memory and issues no
// Redis command.
//
// That is a claim about code, and a comment stating it would quietly become
// false the day someone made a metrics task read Redis: the pool would then be
// undersized by a whole subpool, silently, under exactly the load that makes it
// matter. This is the assertion that goes red instead, and it names the method
// whose comment would be the lie.
//
// It is an IMPORT rule and not a call-graph analysis on purpose: the metric
// recorder cannot reach Redis without importing something that speaks to it,
// and an import is something a reviewer can see in the diff.
var metricRecorderFiles = []string{
	"relayer/metric_recorder.go",
}

// redisImportPaths are the packages that talk to Redis. A file importing any of
// them can issue a command.
var redisImportPaths = []string{
	"pocket-relay-miner/transport/redis",
	"redis/go-redis",
}

// importsRedis reports whether a file imports something that speaks Redis.
func importsRedis(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		for _, p := range redisImportPaths {
			if strings.Contains(path, p) {
				return true
			}
		}
	}
	return false
}

// TestMetricRecorderDoesNotTouchRedis fails when the metrics path gains a Redis
// dependency, because relayer.WorkerSizing.RedisPoolSize leaves the metrics
// subpool out of the pool on the grounds that it has none.
func TestMetricRecorderDoesNotTouchRedis(t *testing.T) {
	// false: production files. The rule is about what the recorder imports, not
	// about its tests.
	files, _ := goFiles(t, false)

	for _, want := range metricRecorderFiles {
		f, ok := files[want]
		if !ok {
			// A rename must not silently retire the rule: an absent file is a
			// rule that stopped running, which looks exactly like a rule that
			// passed.
			t.Errorf("%s not found: if it moved, point this rule at its new path", want)
			continue
		}
		if importsRedis(f) {
			t.Errorf("%s now imports a Redis package. The relayer's pool size "+
				"(relayer/sizing.go, RedisPoolSize) excludes the metrics subpool "+
				"because it issues no Redis command; if that changed, the pool is "+
				"short by metricsWorkers and both the formula and its comment must move.", want)
		}
	}
}

// TestRedisImportMatcherCatchesHostileShapes proves the matcher reads IMPORTS,
// not the word appearing in a comment or a string.
func TestRedisImportMatcherCatchesHostileShapes(t *testing.T) {
	usesIt := `package x

import "github.com/redis/go-redis/v9"

var _ = redis.Nil
`
	mentionsIt := `package x

// redis is only named here: "github.com/redis/go-redis/v9"
var s = "pocket-relay-miner/transport/redis"
`
	f, _ := parseSource(t, usesIt)
	if !importsRedis(f) {
		t.Fatal("matcher missed a real import")
	}
	f, _ = parseSource(t, mentionsIt)
	if importsRedis(f) {
		t.Fatal("matcher fired on a mention in a comment and a string")
	}
}
