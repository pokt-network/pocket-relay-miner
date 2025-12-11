package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

func redisDebugSessionsCmd() *cobra.Command {
	var (
		supplierAddr string
		sessionID    string
		state        string
		jsonOutput   bool
	)

	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Inspect session metadata",
		Long: `Inspect session snapshots stored in Redis.

Session data is stored at:
  - Key: ha:miner:sessions:{supplier}:{sessionID}
  - Index: ha:miner:sessions:{supplier}:index
  - State Index: ha:miner:sessions:{supplier}:state:{state}

States: active, claiming, claimed, proving, settled, expired`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			// If session ID provided, show specific session
			if sessionID != "" {
				return showSession(ctx, client, supplierAddr, sessionID, jsonOutput)
			}

			// If state filter provided, show sessions by state
			if state != "" {
				return listSessionsByState(ctx, client, supplierAddr, state, jsonOutput)
			}

			// Otherwise list all sessions for supplier
			return listAllSessions(ctx, client, supplierAddr, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Supplier operator address (required)")
	cmd.Flags().StringVar(&sessionID, "session", "", "Specific session ID to inspect")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state (active|claiming|claimed|proving|settled|expired)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("supplier")

	return cmd
}

func showSession(ctx context.Context, client *redisClient, supplier, sessionID string, jsonOutput bool) error {
	key := fmt.Sprintf("ha:miner:sessions:%s:%s", supplier, sessionID)

	data, err := client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	if jsonOutput {
		fmt.Println(string(data))
		return nil
	}

	// Parse and display formatted
	var snapshot map[string]interface{}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("failed to parse session data: %w", err)
	}

	fmt.Printf("Session: %s\n", sessionID)
	fmt.Printf("Supplier: %s\n", supplier)
	fmt.Printf("State: %v\n", snapshot["state"])
	fmt.Printf("Service ID: %v\n", snapshot["service_id"])
	fmt.Printf("Application: %v\n", snapshot["application_address"])
	fmt.Printf("Relay Count: %v\n", snapshot["relay_count"])
	fmt.Printf("Total Compute Units: %v\n", snapshot["total_compute_units"])
	fmt.Printf("Session Start Height: %v\n", snapshot["session_start_height"])
	fmt.Printf("Session End Height: %v\n", snapshot["session_end_height"])
	fmt.Printf("Last WAL Entry ID: %v\n", snapshot["last_wal_entry_id"])
	fmt.Printf("Created At: %v\n", snapshot["created_at"])
	fmt.Printf("Last Updated At: %v\n", snapshot["last_updated_at"])

	if rootHash, ok := snapshot["claimed_root_hash"]; ok && rootHash != nil {
		fmt.Printf("Claimed Root Hash: %v\n", rootHash)
	}

	return nil
}

func listSessionsByState(ctx context.Context, client *redisClient, supplier, state string, jsonOutput bool) error {
	indexKey := fmt.Sprintf("ha:miner:sessions:%s:state:%s", supplier, state)

	sessionIDs, err := client.SMembers(ctx, indexKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get sessions by state: %w", err)
	}

	if len(sessionIDs) == 0 {
		fmt.Printf("No sessions found in state '%s' for supplier %s\n", state, supplier)
		return nil
	}

	return fetchAndDisplaySessions(ctx, client, supplier, sessionIDs, jsonOutput)
}

func listAllSessions(ctx context.Context, client *redisClient, supplier string, jsonOutput bool) error {
	indexKey := fmt.Sprintf("ha:miner:sessions:%s:index", supplier)

	sessionIDs, err := client.SMembers(ctx, indexKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get session index: %w", err)
	}

	if len(sessionIDs) == 0 {
		fmt.Printf("No sessions found for supplier %s\n", supplier)
		return nil
	}

	return fetchAndDisplaySessions(ctx, client, supplier, sessionIDs, jsonOutput)
}

func fetchAndDisplaySessions(ctx context.Context, client *redisClient, supplier string, sessionIDs []string, jsonOutput bool) error {
	// Fetch all session data
	pipe := client.Pipeline()
	cmds := make([]*redis.StringCmd, len(sessionIDs))

	for i, sessionID := range sessionIDs {
		key := fmt.Sprintf("ha:miner:sessions:%s:%s", supplier, sessionID)
		cmds[i] = pipe.Get(ctx, key)
	}

	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return fmt.Errorf("failed to fetch sessions: %w", err)
	}

	var sessions []map[string]interface{}
	for _, cmd := range cmds {
		data, err := cmd.Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			continue
		}

		var snapshot map[string]interface{}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			continue
		}
		sessions = append(sessions, snapshot)
	}

	if jsonOutput {
		output, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(output))
		return nil
	}

	// Display as table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "SESSION ID\tSTATE\tSERVICE\tRELAYS\tCOMPUTE UNITS\tSTART HEIGHT\tEND HEIGHT\n")

	for _, s := range sessions {
		_, _ = fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			s["session_id"],
			s["state"],
			s["service_id"],
			s["relay_count"],
			s["total_compute_units"],
			s["session_start_height"],
			s["session_end_height"],
		)
	}

	_ = w.Flush()
	fmt.Printf("\nTotal: %d sessions\n", len(sessions))

	return nil
}
