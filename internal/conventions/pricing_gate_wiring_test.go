package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The relayer refuses relays it cannot price: without the miner's service factor
// manifest there is no price, and a relay charged wrong is revenue that never
// comes back. That refusal is only real if the meter is actually given the
// client that holds the manifest.
//
// A nil provider does NOT fail loudly on its own: GetServiceFactor returns
// (0, false) for a nil provider exactly as it does for "no factor configured",
// so a mis-wired meter would charge by the base formula and say nothing. This
// rule is what makes that wiring impossible to forget, rather than a parameter
// the constructor could take and a test could pass nil to.

// pricingGateViolations reports what is wrong with the pricing wiring in f.
func pricingGateViolations(f *ast.File, fset *token.FileSet) []string {
	var problems []string

	unconditional, conditional := callsOf(f, "NewServiceFactorClient")
	for _, pos := range conditional {
		problems = append(problems, "NewServiceFactorClient is called under a condition at "+fset.Position(pos).String())
	}
	if len(unconditional) != 1 {
		problems = append(problems, "NewServiceFactorClient must be called unconditionally exactly once")
	}

	// The meter must receive the client, not nil: the two are indistinguishable
	// at the point of use, and only one of them can price a relay.
	meterCalls := 0
	meterGetsProvider := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !callsName(call, "NewRelayMeter") {
			return true
		}
		meterCalls++
		for _, arg := range call.Args {
			if ident, isIdent := arg.(*ast.Ident); isIdent && ident.Name == "serviceFactorClient" {
				meterGetsProvider = true
			}
		}
		return true
	})

	if meterCalls == 0 {
		problems = append(problems, "NewRelayMeter is not called: the relayer cannot charge anything")
	}
	if meterCalls > 0 && !meterGetsProvider {
		problems = append(problems,
			"NewRelayMeter must be passed serviceFactorClient: a nil provider prices by the base formula silently")
	}

	return problems
}

func TestTheRelayerWiresThePricingGate(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, p := range pricingGateViolations(f, fset) {
		t.Errorf("%s: %s", path, p)
	}
}

// TestPricingGateViolationsReadsTheWiring proves the rule reads the call, its
// guard and what the meter is actually handed -- not merely the names.
func TestPricingGateViolationsReadsTheWiring(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"wired", `package x
func a() {
	serviceFactorClient := relayer.NewServiceFactorClient(logger, redisClient)
	relayMeter := relayer.NewRelayMeter(logger, redisClient, app, serviceFactorClient, cfg)
}`, 0},
		{"client never built", `package x
func a() {
	relayMeter := relayer.NewRelayMeter(logger, redisClient, app, serviceFactorClient, cfg)
}`, 1},
		{"client built under a condition", `package x
func a() {
	if on {
		serviceFactorClient := relayer.NewServiceFactorClient(logger, redisClient)
	}
	relayMeter := relayer.NewRelayMeter(logger, redisClient, app, serviceFactorClient, cfg)
}`, 2},
		{"meter handed nil instead of the client", `package x
func a() {
	serviceFactorClient := relayer.NewServiceFactorClient(logger, redisClient)
	relayMeter := relayer.NewRelayMeter(logger, redisClient, app, nil, cfg)
}`, 1},
		{"meter never built", `package x
func a() {
	serviceFactorClient := relayer.NewServiceFactorClient(logger, redisClient)
}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, tc.src)
			got := pricingGateViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}
