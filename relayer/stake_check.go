package relayer

import (
	"sort"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// StakedPair is a single (supplier, service, transport) tuple a supplier is
// staked for on-chain, resolved to the relayer's canonical backend-type key.
//
// The relayer routes relays to a backend by EXACT (service, transport) match
// with no cross-transport fallback, so being staked for a transport whose
// backend is absent means every relay of that transport is rejected and the
// supplier earns nothing on it — silently. This type is the unit of that check.
type StakedPair struct {
	// Supplier is the operator address the pair belongs to (money-loss is
	// per-supplier, so it is carried through to the error line).
	Supplier string
	// ServiceID is the on-chain service ID (map key in Config.Services).
	ServiceID string
	// RPCType is the raw on-chain enum (kept so unmapped values can be logged).
	RPCType sharedtypes.RPCType
	// BackendType is the canonical relayer backend key (jsonrpc|rest|websocket|
	// grpc|cometbft), or "" when RPCType has no mapping.
	BackendType string
}

// ExtraBackend is a backend configured locally that no configured supplier is
// staked for — a harmless surplus, reported as info rather than an error.
type ExtraBackend struct {
	ServiceID   string
	BackendType string
}

// StakeCheckReport is the outcome of cross-checking on-chain stake against the
// local relayer config.
type StakeCheckReport struct {
	// Missing lists staked (service, transport) pairs with no local backend.
	// These are ERRORS: the supplier is staked but earns nothing on them.
	Missing []StakedPair
	// Extra lists local backends that no supplier is staked for. These are
	// WARN/info: surplus config, not a fault.
	Extra []ExtraBackend
	// Unknown lists staked pairs whose on-chain RPCType the relayer cannot map
	// (e.g. a transport added to the protocol this build predates). Surfaced for
	// a debug line, never counted as an error.
	Unknown []StakedPair
}

// HasErrors reports whether the config would leave staked traffic unserved.
func (r StakeCheckReport) HasErrors() bool { return len(r.Missing) > 0 }

// rpcTypeToBackendType maps the on-chain RPCType enum to the relayer's canonical
// backend-type key. ok is false for RPCType_UNKNOWN_RPC and any future/unknown
// value — callers must treat !ok as "cannot check", never as a specific type.
//
// This is the enum counterpart of RPCTypeToBackendType (which maps the numeric
// STRING form carried on the Rpc-Type request header).
func rpcTypeToBackendType(t sharedtypes.RPCType) (string, bool) {
	switch t {
	case sharedtypes.RPCType_GRPC:
		return BackendTypeGRPC, true
	case sharedtypes.RPCType_WEBSOCKET:
		return BackendTypeWebSocket, true
	case sharedtypes.RPCType_JSON_RPC:
		return BackendTypeJSONRPC, true
	case sharedtypes.RPCType_REST:
		return BackendTypeREST, true
	case sharedtypes.RPCType_COMET_BFT:
		return BackendTypeCometBFT, true
	default:
		return "", false
	}
}

// ExtractStakedPairs flattens a supplier's on-chain service configs into
// deduplicated (service, transport) StakedPairs. Endpoints of the same rpc_type
// within a service collapse to one pair. Nil configs/endpoints are skipped.
// An unmapped RPCType still produces a pair (with BackendType == "") so the
// caller can report it rather than lose it.
func ExtractStakedPairs(supplier string, configs []*sharedtypes.SupplierServiceConfig) []StakedPair {
	seen := make(map[string]struct{})
	var pairs []StakedPair
	for _, svc := range configs {
		if svc == nil {
			continue
		}
		for _, ep := range svc.Endpoints {
			if ep == nil {
				continue
			}
			backendType, _ := rpcTypeToBackendType(ep.RpcType)
			// Dedup on (service, rpc_type): a service may declare the same
			// transport on several endpoint URLs, but the stake is one pair.
			key := svc.ServiceId + "\x00" + backendType + "\x00" + rpcTypeKey(ep.RpcType)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			pairs = append(pairs, StakedPair{
				Supplier:    supplier,
				ServiceID:   svc.ServiceId,
				RPCType:     ep.RpcType,
				BackendType: backendType,
			})
		}
	}
	return pairs
}

// rpcTypeKey disambiguates the dedup key for unmapped types (all of which share
// BackendType ""), so two distinct unknown enums are not collapsed.
func rpcTypeKey(t sharedtypes.RPCType) string {
	return t.String()
}

// CrossCheckStake compares the on-chain staked pairs of one or more suppliers
// against the relayer config's declared backends, producing the three buckets:
//
//   - Missing: staked but no local backend (ERROR — money lost).
//   - Extra:   local backend nobody staked (WARN — surplus).
//   - Unknown: staked pair with an unmappable RPCType (debug).
//
// A backend counts as Extra only if NO supplier stakes it: with a shared config
// serving many suppliers, one supplier's surplus is another's requirement.
// Output is sorted for stable, deterministic CLI reporting.
func CrossCheckStake(cfg *Config, staked []StakedPair) StakeCheckReport {
	var report StakeCheckReport

	// Set of (service, backendType) staked by ANY supplier, mapped types only.
	stakedSet := make(map[string]struct{})
	for _, p := range staked {
		if p.BackendType == "" {
			report.Unknown = append(report.Unknown, p)
			continue
		}
		stakedSet[p.ServiceID+"\x00"+p.BackendType] = struct{}{}

		if !cfg.hasBackend(p.ServiceID, p.BackendType) {
			report.Missing = append(report.Missing, p)
		}
	}

	// Any configured backend not staked by anyone is surplus.
	for svcID, svc := range cfg.Services {
		for backendType := range svc.Backends {
			if _, ok := stakedSet[svcID+"\x00"+backendType]; !ok {
				report.Extra = append(report.Extra, ExtraBackend{ServiceID: svcID, BackendType: backendType})
			}
		}
	}

	sortMissing(report.Missing)
	sortExtra(report.Extra)
	sortMissing(report.Unknown)
	return report
}

// hasBackend reports whether the config declares a backend of backendType for
// serviceID. It reads the declarative Backends map directly (not GetPool) so the
// check works without BuildPools and does not bump runtime routing metrics.
func (c *Config) hasBackend(serviceID, backendType string) bool {
	svc, ok := c.Services[serviceID]
	if !ok {
		return false
	}
	_, ok = svc.Backends[backendType]
	return ok
}

func sortMissing(s []StakedPair) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Supplier != s[j].Supplier {
			return s[i].Supplier < s[j].Supplier
		}
		if s[i].ServiceID != s[j].ServiceID {
			return s[i].ServiceID < s[j].ServiceID
		}
		return s[i].BackendType < s[j].BackendType
	})
}

func sortExtra(s []ExtraBackend) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].ServiceID != s[j].ServiceID {
			return s[i].ServiceID < s[j].ServiceID
		}
		return s[i].BackendType < s[j].BackendType
	})
}
