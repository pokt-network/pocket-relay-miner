package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// supplierCacheState matches cache.SupplierState for JSON unmarshaling
type supplierCacheState struct {
	Status          string   `json:"status"`
	Staked          bool     `json:"staked"`
	Services        []string `json:"services"`
	OperatorAddress string   `json:"operator_address"`
	OwnerAddress    string   `json:"owner_address"`
	LastUpdated     int64    `json:"last_updated"`
	UpdatedBy       string   `json:"updated_by,omitempty"`
}

func SupplierCmd() *cobra.Command {
	var (
		supplierAddr string
		listAll      bool
		showClaims   bool
	)

	cmd := &cobra.Command{
		Use:   "supplier",
		Short: "Inspect supplier registry and claims",
		Long: `Inspect supplier registry and claim distribution in Redis.

Supplier data is stored at:
  - Supplier Cache: ha:supplier:{address} (JSON with staking status)
  - Claims: ha:miner:claim:{supplier} (String with miner instance ID)
  - Active miners: ha:miner:active (Set of active miner instance IDs)

Staking status is written by the miner when it loads keys and checks on-chain status.
Addresses that are in the keys file but NOT staked on-chain will show as "not_staked".

Examples:
  # Show supplier claim distribution with staking status
  pocket-relay-miner redis supplier --claims

  # List all known suppliers (staked and unstaked)
  pocket-relay-miner redis supplier --list

  # Inspect a specific supplier
  pocket-relay-miner redis supplier --supplier pokt1abc...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if showClaims {
				return listSupplierClaims(ctx, client)
			}

			if listAll {
				return listAllSuppliers(ctx, client)
			}

			if supplierAddr != "" {
				return inspectSupplier(ctx, client, supplierAddr)
			}

			// Default to showing claims (most useful)
			return listSupplierClaims(ctx, client)
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Supplier operator address to inspect")
	cmd.Flags().BoolVar(&listAll, "list", false, "List all known suppliers (staked and unstaked)")
	cmd.Flags().BoolVar(&showClaims, "claims", false, "Show supplier claim distribution across miners")

	return cmd
}

// getSupplierCacheState reads a supplier's state from the cache (ha:supplier:{address})
func getSupplierCacheState(ctx context.Context, client *DebugRedisClient, address string) (*supplierCacheState, error) {
	key := fmt.Sprintf("ha:supplier:%s", address)
	data, err := client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}

	var state supplierCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// getAllSupplierCacheStates reads all supplier states from cache
func getAllSupplierCacheStates(ctx context.Context, client *DebugRedisClient) (map[string]*supplierCacheState, error) {
	pattern := "ha:supplier:*"
	keys, err := client.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	states := make(map[string]*supplierCacheState)
	for _, key := range keys {
		// Extract address from key (ha:supplier:{address})
		addr := strings.TrimPrefix(key, "ha:supplier:")
		if addr == key {
			continue // Didn't match pattern
		}

		state, err := getSupplierCacheState(ctx, client, addr)
		if err != nil {
			continue
		}
		states[addr] = state
	}

	return states, nil
}

func listSupplierClaims(ctx context.Context, client *DebugRedisClient) error {
	kb := client.KB()

	// Get all supplier cache states (staked and unstaked)
	allSupplierStates, err := getAllSupplierCacheStates(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get supplier states: %w", err)
	}

	// Get all claim keys using pattern from KeyBuilder
	claimPattern := kb.MinerClaimKey("*")
	claimKeys, err := client.Keys(ctx, claimPattern).Result()
	if err != nil {
		return fmt.Errorf("failed to get claim keys: %w", err)
	}

	// Get active miners using key from KeyBuilder
	activeSetKey := kb.MinerActiveSetKey()
	activeMiners, err := client.SMembers(ctx, activeSetKey).Result()
	if err != nil {
		// Not fatal - might not exist yet
		activeMiners = []string{}
	}

	// Build claim map: miner -> []suppliers
	minerClaims := make(map[string][]string)
	claimedAddresses := make(map[string]string) // address -> miner

	// Calculate the prefix to strip from keys
	claimKeyPrefix := kb.MinerClaimKey("")

	for _, key := range claimKeys {
		owner, err := client.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		// Extract supplier address from key by stripping the prefix
		supplier := strings.TrimPrefix(key, claimKeyPrefix)
		minerClaims[owner] = append(minerClaims[owner], supplier)
		claimedAddresses[supplier] = owner
	}

	// Count staked vs unstaked from cache
	var stakedCount, unstakedCount, unclaimedStaked int
	for addr, state := range allSupplierStates {
		if state.Staked {
			stakedCount++
			if _, claimed := claimedAddresses[addr]; !claimed {
				unclaimedStaked++
			}
		} else {
			unstakedCount++
		}
	}

	// Sort miners by claim count (descending)
	miners := make([]string, 0, len(minerClaims))
	for miner := range minerClaims {
		miners = append(miners, miner)
	}
	sort.Slice(miners, func(i, j int) bool {
		return len(minerClaims[miners[i]]) > len(minerClaims[miners[j]])
	})

	// Calculate fair share based on staked suppliers only
	totalStaked := stakedCount
	totalMiners := len(activeMiners)
	if totalMiners == 0 {
		totalMiners = len(minerClaims) // Fallback to miners with claims
	}
	if totalMiners == 0 {
		totalMiners = 1 // Avoid division by zero
	}
	fairShareHigh := (totalStaked + totalMiners - 1) / totalMiners // ceil division
	fairShareLow := totalStaked / totalMiners                      // floor division

	// Print summary
	fmt.Printf("╔══════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                    SUPPLIER STATUS OVERVIEW                          ║\n")
	fmt.Printf("╠══════════════════════════════════════════════════════════════════════╣\n")
	if fairShareHigh == fairShareLow {
		fmt.Printf("║  Total Keys: %-3d   Staked: %-3d   Not Staked: %-3d   Fair Share: %-2d    ║\n",
			len(allSupplierStates), stakedCount, unstakedCount, fairShareHigh)
	} else {
		fmt.Printf("║  Total Keys: %-3d   Staked: %-3d   Not Staked: %-3d   Fair Share: %d-%-2d  ║\n",
			len(allSupplierStates), stakedCount, unstakedCount, fairShareLow, fairShareHigh)
	}
	fmt.Printf("║  Claimed: %-3d      Active Miners: %-3d                                ║\n",
		len(claimedAddresses), len(activeMiners))
	fmt.Printf("╚══════════════════════════════════════════════════════════════════════╝\n\n")

	// Show warnings
	if unstakedCount > 0 {
		fmt.Printf("⚠ %d addresses in keys are NOT STAKED on-chain (filtered from claiming)\n", unstakedCount)
	}
	if unclaimedStaked > 0 {
		fmt.Printf("⚠ %d staked suppliers are NOT CLAIMED by any miner\n", unclaimedStaked)
	}
	if unstakedCount > 0 || unclaimedStaked > 0 {
		fmt.Printf("\n")
	}

	// Print per-miner breakdown if there are claims
	if len(minerClaims) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "MINER INSTANCE\tCLAIMS\tSTATUS\n")
		_, _ = fmt.Fprintf(w, "──────────────\t──────\t──────\n")

		for _, miner := range miners {
			claims := minerClaims[miner]
			claimStatus := ""
			// Balanced if within fair share range [low, high]
			if len(claims) > fairShareHigh {
				claimStatus = fmt.Sprintf("⚠ over (+%d)", len(claims)-fairShareHigh)
			} else if len(claims) < fairShareLow {
				claimStatus = fmt.Sprintf("↓ under (-%d)", fairShareLow-len(claims))
			} else {
				claimStatus = "✓ balanced"
			}
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", miner, len(claims), claimStatus)
		}
		_ = w.Flush()

		// Print supplier details grouped by miner
		fmt.Printf("\n── Claimed Suppliers (grouped by miner) ──────────────────────────\n")

		for _, miner := range miners {
			claims := minerClaims[miner]
			sort.Strings(claims) // Sort suppliers alphabetically within each miner

			// Print miner header
			fmt.Printf("\n┌─ %s (%d suppliers)\n", miner, len(claims))

			w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, supplier := range claims {
				// Get TTL for this claim
				claimKey := kb.MinerClaimKey(supplier)
				ttl, err := client.TTL(ctx, claimKey).Result()
				ttlStr := "?"
				if err == nil {
					if ttl < 0 {
						ttlStr = "no-expiry"
					} else {
						ttlStr = ttl.String()
					}
				}

				// Get staking status from cache
				if state, ok := allSupplierStates[supplier]; ok {
					if state.Staked {
						_, _ = fmt.Fprintf(w, "│  %s\t%s\t✓ staked\n", supplier, ttlStr)
					} else {
						_, _ = fmt.Fprintf(w, "│  %s\t%s\t✗ NOT STAKED\n", supplier, ttlStr)
					}
				} else {
					_, _ = fmt.Fprintf(w, "│  %s\t%s\t? not in cache\n", supplier, ttlStr)
				}
			}
			_ = w.Flush()
		}
	}

	// Show unclaimed and unstaked addresses
	if unstakedCount > 0 || unclaimedStaked > 0 {
		fmt.Printf("\n── Unclaimed/Unstaked Addresses ──────────────────────────────────\n\n")

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "ADDRESS\tSTATUS\tSERVICES\tLAST UPDATED\n")
		_, _ = fmt.Fprintf(w, "───────\t──────\t────────\t────────────\n")

		// Sort addresses for consistent output
		var addrs []string
		for addr := range allSupplierStates {
			addrs = append(addrs, addr)
		}
		sort.Strings(addrs)

		for _, addr := range addrs {
			state := allSupplierStates[addr]
			_, claimed := claimedAddresses[addr]

			// Show if unclaimed (staked) or unstaked
			if !claimed || !state.Staked {
				statusStr := ""
				if !state.Staked {
					statusStr = "✗ NOT STAKED"
				} else if !claimed {
					statusStr = "⚠ unclaimed"
				}

				servicesStr := "-"
				if len(state.Services) > 0 {
					servicesStr = strings.Join(state.Services, ", ")
				}

				lastUpdated := "-"
				if state.LastUpdated > 0 {
					lastUpdated = time.Unix(state.LastUpdated, 0).Format("15:04:05")
				}

				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", addr, statusStr, servicesStr, lastUpdated)
			}
		}
		_ = w.Flush()
	}

	fmt.Printf("\n")
	return nil
}

func listAllSuppliers(ctx context.Context, client *DebugRedisClient) error {
	// Get all supplier cache states
	states, err := getAllSupplierCacheStates(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get supplier states: %w", err)
	}

	if len(states) == 0 {
		fmt.Printf("No suppliers found in cache\n")
		fmt.Printf("(Miner writes supplier status to cache when it starts)\n")
		return nil
	}

	// Count staked vs unstaked
	var stakedCount, unstakedCount int
	for _, state := range states {
		if state.Staked {
			stakedCount++
		} else {
			unstakedCount++
		}
	}

	fmt.Printf("Supplier Cache (%d total: %d staked, %d not staked):\n\n", len(states), stakedCount, unstakedCount)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "ADDRESS\tSTATUS\tSTAKED\tSERVICES\tLAST UPDATED\n")
	_, _ = fmt.Fprintf(w, "───────\t──────\t──────\t────────\t────────────\n")

	// Sort addresses for consistent output
	var addrs []string
	for addr := range states {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	for _, addr := range addrs {
		state := states[addr]

		stakedStr := "✓ yes"
		if !state.Staked {
			stakedStr = "✗ no"
		}

		servicesStr := "-"
		if len(state.Services) > 0 {
			servicesStr = strings.Join(state.Services, ", ")
		}

		lastUpdated := "-"
		if state.LastUpdated > 0 {
			lastUpdated = time.Unix(state.LastUpdated, 0).Format("2006-01-02 15:04:05")
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", addr, state.Status, stakedStr, servicesStr, lastUpdated)
	}

	_ = w.Flush()

	return nil
}

func inspectSupplier(ctx context.Context, client *DebugRedisClient, supplier string) error {
	kb := client.KB()

	// Get supplier cache state
	state, err := getSupplierCacheState(ctx, client, supplier)
	if err != nil {
		fmt.Printf("Supplier not found in cache: %s\n", supplier)
		fmt.Printf("(Miner writes supplier status to cache when it starts)\n")
		return nil
	}

	fmt.Printf("Supplier: %s\n", supplier)
	fmt.Printf("Redis Key: ha:supplier:%s\n\n", supplier)

	fmt.Printf("Status:\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  Status:\t%s\n", state.Status)

	stakedStr := "✓ yes"
	if !state.Staked {
		stakedStr = "✗ no"
	}
	_, _ = fmt.Fprintf(w, "  Staked:\t%s\n", stakedStr)

	if state.OwnerAddress != "" {
		_, _ = fmt.Fprintf(w, "  Owner:\t%s\n", state.OwnerAddress)
	}

	servicesStr := "-"
	if len(state.Services) > 0 {
		servicesStr = strings.Join(state.Services, ", ")
	}
	_, _ = fmt.Fprintf(w, "  Services:\t%s\n", servicesStr)

	if state.LastUpdated > 0 {
		_, _ = fmt.Fprintf(w, "  Last Updated:\t%s\n", time.Unix(state.LastUpdated, 0).Format("2006-01-02 15:04:05"))
	}
	if state.UpdatedBy != "" {
		_, _ = fmt.Fprintf(w, "  Updated By:\t%s\n", state.UpdatedBy)
	}
	_ = w.Flush()

	// Check claim status
	claimKey := kb.MinerClaimKey(supplier)
	claimedBy, err := client.Get(ctx, claimKey).Result()
	if err == nil {
		ttl, _ := client.TTL(ctx, claimKey).Result()
		fmt.Printf("\nClaim Status:\n")
		fmt.Printf("  Claimed By: %s\n", claimedBy)
		if ttl > 0 {
			fmt.Printf("  Claim TTL: %s\n", ttl.String())
		}
	} else {
		fmt.Printf("\nClaim Status: Not claimed\n")
	}

	return nil
}
