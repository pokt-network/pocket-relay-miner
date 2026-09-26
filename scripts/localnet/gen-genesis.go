//go:build ignore

// gen-genesis writes a localnet genesis sized for load: a number of
// applications per service and a number of suppliers, grown from the default
// localnet genesis as a template. Everything it does not add it copies.
//
//	go run scripts/localnet/gen-genesis.go                  # tilt/config -> tilt/config/scale
//	go run scripts/localnet/gen-genesis.go -check <genesis> # invariants only, on any genesis
//
// Every existing account, application and supplier is kept as it is, so the
// default identities (app1, supplier1, ...) still exist at scale. New accounts
// get a mnemonic derived from -seed and their name, so the same flags always
// produce the same keys; account-init imports them with `keys add --recover`,
// which is why the private key is DERIVED from the mnemonic here and never
// generated beside it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/go-bip39"
	"gopkg.in/yaml.v3"
)

const (
	hrp    = "pokt"
	denom  = "upokt"
	hdPath = "m/44'/118'/0'/0/0"
)

type key struct {
	Name       string `yaml:"name"`
	Address    string `yaml:"address"`
	PrivateKey string `yaml:"private_key"`
	Mnemonic   string `yaml:"mnemonic"`
}

type obj = map[string]any

func main() {
	in := flag.String("in", "tilt/config", "directory holding the template genesis.json and all-keys.yaml")
	out := flag.String("out", "tilt/config/scale", "directory to write genesis.json and all-keys.yaml to")
	appsPerService := flag.Int("apps-per-service", 5, "applications staked for each service")
	suppliers := flag.Int("suppliers", 50, "suppliers, which is also num_suppliers_per_session")
	appStake := flag.String("app-stake", "", "stake of EVERY application in upokt, template ones included (default: each keeps its template's)")
	seed := flag.String("seed", "pocket-relay-miner-localnet-scale-v1", "seed the new mnemonics are derived from")
	check := flag.String("check", "", "only run the invariants on this genesis file and exit")
	flag.Parse()

	if *check != "" {
		g := mustRead(*check)
		if errs := checkInvariants(g); len(errs) > 0 {
			fail(*check, errs)
		}
		fmt.Printf("%s: invariants hold\n", *check)
		return
	}

	g := mustRead(filepath.Join(*in, "genesis.json"))
	keysText, err := os.ReadFile(filepath.Join(*in, "all-keys.yaml"))
	must(err)
	var ak struct {
		Accounts []key `yaml:"accounts"`
	}
	must(yaml.Unmarshal(keysText, &ak))

	// THE HD CONTROL, before anything is written: every template account must
	// re-derive from its mnemonic to the private key and address stored beside
	// it. The new keys are produced by the same function, so if this does not
	// hold for keys the chain already accepts, nothing generated here would.
	var mismatched []string
	for _, k := range ak.Accounts {
		priv, addr := derive(k.Mnemonic)
		if priv != k.PrivateKey || addr != k.Address {
			mismatched = append(mismatched, fmt.Sprintf("%s: mnemonic re-derives to %s / %s, stored %s / %s",
				k.Name, addr, priv, k.Address, k.PrivateKey))
		}
	}
	if len(mismatched) > 0 {
		fail("HD control", mismatched)
	}
	fmt.Printf("HD control: %d/%d template accounts re-derive to their stored key and address\n",
		len(ak.Accounts), len(ak.Accounts))

	app := at(g, "app_state").(obj)
	accounts := at(app, "auth", "accounts").([]any)
	balances := at(app, "bank", "balances").([]any)
	apps := at(app, "application", "applicationList").([]any)
	supps := at(app, "supplier", "supplierList").([]any)

	nextAccNum := int64(0)
	for _, a := range accounts {
		if n := atoi(a.(obj)["account_number"]); n >= nextAccNum {
			nextAccNum = n + 1
		}
	}
	balanceOf := map[string]any{}
	for _, b := range balances {
		balanceOf[b.(obj)["address"].(string)] = b.(obj)["coins"]
	}
	taken := map[string]bool{}
	for _, k := range ak.Accounts {
		taken[k.Name] = true
	}

	var newKeys []key
	addAccount := func(name, templateAddr string) string {
		if taken[name] {
			fail("naming", []string{name + " already exists in the template all-keys.yaml"})
		}
		taken[name] = true
		mnemonic := mnemonicFor(*seed, name)
		priv, addr := derive(mnemonic)
		newKeys = append(newKeys, key{Name: name, Address: addr, PrivateKey: priv, Mnemonic: mnemonic})
		accounts = append(accounts, obj{
			"@type":          "/cosmos.auth.v1beta1.BaseAccount",
			"address":        addr,
			"pub_key":        nil,
			"account_number": fmt.Sprint(nextAccNum),
			"sequence":       "0",
		})
		nextAccNum++
		coins, ok := balanceOf[templateAddr]
		if !ok {
			fail("template", []string{"no balance for template account " + templateAddr})
		}
		balances = append(balances, obj{"address": addr, "coins": clone(coins)})
		return addr
	}

	// Applications: complete every service up to -apps-per-service, each new
	// one a copy of that service's first template application.
	byService := map[string][]obj{}
	for _, a := range apps {
		svc := serviceOf(a.(obj))
		byService[svc] = append(byService[svc], a.(obj))
	}
	for _, s := range at(app, "service", "serviceList").([]any) {
		svc := s.(obj)["id"].(string)
		tmpl := byService[svc]
		if len(tmpl) == 0 {
			fail("template", []string{"service " + svc + " has no application to copy"})
		}
		for i := len(tmpl) + 1; i <= *appsPerService; i++ {
			a := clone(tmpl[0]).(obj)
			a["address"] = addAccount(fmt.Sprintf("app_scale_%s_%d", svc, i), tmpl[0]["address"].(string))
			apps = append(apps, a)
		}
	}
	// Every application, not only the new ones: the relay meter caps each at
	// stake x service factor per (session, supplier), so one application left
	// at the template's stake becomes the limit the whole load runs into.
	if *appStake != "" {
		amount(*appStake) // fails on a non-integer before anything is written
		for _, a := range apps {
			a.(obj)["stake"].(obj)["amount"] = *appStake
		}
	}

	// Suppliers: copies of the first template supplier, which already carries
	// every service with the endpoint and rpc_type the relayer serves it on.
	tmplSupp := supps[0].(obj)
	for i := len(supps) + 1; i <= *suppliers; i++ {
		s := clone(tmplSupp).(obj)
		addr := addAccount(fmt.Sprintf("supplier_scale_%d", i), tmplSupp["operator_address"].(string))
		s["operator_address"], s["owner_address"] = addr, addr
		for _, svc := range s["services"].([]any) {
			for _, rs := range svc.(obj)["rev_share"].([]any) {
				rs.(obj)["address"] = addr
			}
		}
		supps = append(supps, s)
	}

	setAt(app, accounts, "auth", "accounts")
	setAt(app, apps, "application", "applicationList")
	setAt(app, supps, "supplier", "supplierList")
	setAt(app, json.Number(fmt.Sprint(*suppliers)), "session", "params", "num_suppliers_per_session")

	// Module accounts hold exactly what is staked against them, and supply is
	// the sum of every balance. The template's application module holds less
	// than its applications' stakes; that is not copied.
	stakeSums := map[string]*big.Int{
		"application": sumStakes(apps),
		"supplier":    sumStakes(supps),
		"gateway":     sumStakes(at(app, "gateway", "gatewayList").([]any)),
	}
	for module, sum := range stakeSums {
		addr := moduleAddress(module)
		coins := []any{obj{"denom": denom, "amount": sum.String()}}
		found := false
		for _, b := range balances {
			if b.(obj)["address"] == addr {
				b.(obj)["coins"], found = coins, true
			}
		}
		if !found {
			balances = append(balances, obj{"address": addr, "coins": coins})
		}
	}
	setAt(app, balances, "bank", "balances")
	setAt(app, supplyOf(balances), "bank", "supply")

	var errs []string
	errs = append(errs, checkInvariants(g)...)
	errs = append(errs, checkScale(g, *appsPerService, *suppliers)...)
	if len(errs) > 0 {
		fail("generated genesis", errs)
	}

	must(os.MkdirAll(*out, 0o755))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	must(enc.Encode(g))
	must(os.WriteFile(filepath.Join(*out, "genesis.json"), buf.Bytes(), 0o644))

	var ky strings.Builder
	ky.Write(bytes.TrimRight(keysText, "\n"))
	ky.WriteString("\n")
	for _, k := range newKeys {
		fmt.Fprintf(&ky, "  - name: %s\n    address: %s\n    private_key: %s\n    mnemonic: %q\n",
			k.Name, k.Address, k.PrivateKey, k.Mnemonic)
	}
	must(os.WriteFile(filepath.Join(*out, "all-keys.yaml"), []byte(ky.String()), 0o600))

	fmt.Printf("wrote %s: %d applications (%d per service), %d suppliers, %d new accounts, num_suppliers_per_session=%d\n",
		*out, len(apps), *appsPerService, len(supps), len(newKeys), *suppliers)
}

// checkInvariants holds for any genesis this chain will start from.
func checkInvariants(g obj) []string {
	var errs []string
	app := at(g, "app_state").(obj)
	balances := at(app, "bank", "balances").([]any)

	// x/bank InitGenesis panics when supply differs from the sum of balances.
	want := map[string]string{}
	for _, c := range supplyOf(balances) {
		want[c.(obj)["denom"].(string)] = c.(obj)["amount"].(string)
	}
	got := map[string]string{}
	for _, c := range at(app, "bank", "supply").([]any) {
		got[c.(obj)["denom"].(string)] = fmt.Sprint(c.(obj)["amount"])
	}
	for _, d := range sortedKeys(want, got) {
		if want[d] != got[d] {
			errs = append(errs, fmt.Sprintf("supply of %s is %q but the balances sum to %q", d, got[d], want[d]))
		}
	}

	holdings := map[string]*big.Int{}
	for _, b := range balances {
		for _, c := range b.(obj)["coins"].([]any) {
			if c.(obj)["denom"] == denom {
				holdings[b.(obj)["address"].(string)] = amount(c.(obj)["amount"])
			}
		}
	}
	for module, list := range map[string][]any{
		"application": at(app, "application", "applicationList").([]any),
		"supplier":    at(app, "supplier", "supplierList").([]any),
		"gateway":     at(app, "gateway", "gatewayList").([]any),
	} {
		held, ok := holdings[moduleAddress(module)]
		if !ok {
			held = new(big.Int)
		}
		if staked := sumStakes(list); held.Cmp(staked) != 0 {
			errs = append(errs, fmt.Sprintf("the %s module account holds %s%s but its stakes sum to %s%s",
				module, held, denom, staked, denom))
		}
	}

	seen := map[string]bool{}
	for _, a := range at(app, "auth", "accounts").([]any) {
		addr := a.(obj)["address"].(string)
		if seen[addr] {
			errs = append(errs, "duplicate auth account "+addr)
		}
		seen[addr] = true
	}
	minApp := amount(at(app, "application", "params", "min_stake", "amount"))
	for _, a := range at(app, "application", "applicationList").([]any) {
		if !seen[a.(obj)["address"].(string)] {
			errs = append(errs, "application "+a.(obj)["address"].(string)+" has no auth account")
		}
		if amount(at(a.(obj), "stake", "amount")).Cmp(minApp) < 0 {
			errs = append(errs, "application "+a.(obj)["address"].(string)+" is staked below min_stake")
		}
	}
	minSupp := amount(at(app, "supplier", "params", "min_stake", "amount"))
	for _, s := range at(app, "supplier", "supplierList").([]any) {
		if !seen[s.(obj)["operator_address"].(string)] {
			errs = append(errs, "supplier "+s.(obj)["operator_address"].(string)+" has no auth account")
		}
		if amount(at(s.(obj), "stake", "amount")).Cmp(minSupp) < 0 {
			errs = append(errs, "supplier "+s.(obj)["operator_address"].(string)+" is staked below min_stake")
		}
	}
	return errs
}

// checkScale holds for a genesis this tool generated with these flags.
func checkScale(g obj, appsPerService, suppliers int) []string {
	var errs []string
	app := at(g, "app_state").(obj)
	count := map[string]int{}
	for _, a := range at(app, "application", "applicationList").([]any) {
		count[serviceOf(a.(obj))]++
	}
	for _, s := range at(app, "service", "serviceList").([]any) {
		if id := s.(obj)["id"].(string); count[id] != appsPerService {
			errs = append(errs, fmt.Sprintf("service %s has %d applications, want %d", id, count[id], appsPerService))
		}
	}
	if n := len(at(app, "supplier", "supplierList").([]any)); n != suppliers {
		errs = append(errs, fmt.Sprintf("%d suppliers, want %d", n, suppliers))
	}
	if n := atoi(at(app, "session", "params", "num_suppliers_per_session")); n != int64(suppliers) {
		errs = append(errs, fmt.Sprintf("num_suppliers_per_session is %d, want %d", n, suppliers))
	}
	return errs
}

func mnemonicFor(seed, name string) string {
	entropy := sha256.Sum256([]byte(seed + "/" + name))
	m, err := bip39.NewMnemonic(entropy[:])
	must(err)
	return m
}

// derive returns the hex private key and bech32 address for a mnemonic, the
// way `keys add --recover` with the default HD path derives them.
func derive(mnemonic string) (string, string) {
	bz, err := hd.Secp256k1.Derive()(mnemonic, "", hdPath)
	must(err)
	priv := hd.Secp256k1.Generate()(bz)
	addr, err := bech32.ConvertAndEncode(hrp, priv.PubKey().Address())
	must(err)
	return hex.EncodeToString(bz), addr
}

func moduleAddress(name string) string {
	addr, err := bech32.ConvertAndEncode(hrp, authtypes.NewModuleAddress(name))
	must(err)
	return addr
}

func serviceOf(a obj) string {
	return a["service_configs"].([]any)[0].(obj)["service_id"].(string)
}

func sumStakes(list []any) *big.Int {
	sum := new(big.Int)
	for _, x := range list {
		sum.Add(sum, amount(at(x.(obj), "stake", "amount")))
	}
	return sum
}

func supplyOf(balances []any) []any {
	sums := map[string]*big.Int{}
	for _, b := range balances {
		for _, c := range b.(obj)["coins"].([]any) {
			d := c.(obj)["denom"].(string)
			if sums[d] == nil {
				sums[d] = new(big.Int)
			}
			sums[d].Add(sums[d], amount(c.(obj)["amount"]))
		}
	}
	var out []any
	for _, d := range sortedKeys(sums) {
		out = append(out, obj{"denom": d, "amount": sums[d].String()})
	}
	return out
}

func amount(v any) *big.Int {
	n, ok := new(big.Int).SetString(fmt.Sprint(v), 10)
	if !ok {
		fail("amount", []string{fmt.Sprintf("%v is not an integer", v)})
	}
	return n
}

func atoi(v any) int64 { return amount(v).Int64() }

func at(m obj, path ...string) any {
	var cur any = m
	for _, p := range path {
		next, ok := cur.(obj)[p]
		if !ok {
			fail("genesis shape", []string{"missing " + strings.Join(path, ".")})
		}
		cur = next
	}
	return cur
}

func setAt(m obj, v any, path ...string) {
	at(m, path[:len(path)-1]...).(obj)[path[len(path)-1]] = v
}

func clone(v any) any {
	bz, err := json.Marshal(v)
	must(err)
	var out any
	dec := json.NewDecoder(bytes.NewReader(bz))
	dec.UseNumber()
	must(dec.Decode(&out))
	return out
}

func sortedKeys[V any](maps ...map[string]V) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			set[k] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mustRead(path string) obj {
	bz, err := os.ReadFile(path)
	must(err)
	var g obj
	dec := json.NewDecoder(bytes.NewReader(bz))
	dec.UseNumber()
	must(dec.Decode(&g))
	return g
}

func fail(what string, errs []string) {
	fmt.Fprintf(os.Stderr, "%s: %d check(s) failed\n", what, len(errs))
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  - "+e)
	}
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-genesis:", err)
		os.Exit(1)
	}
}
