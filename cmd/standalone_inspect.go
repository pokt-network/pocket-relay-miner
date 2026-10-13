package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cmd/redis"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/standalone"
	"github.com/pokt-network/pocket-relay-miner/standalone/inspect"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
)

const (
	flagInspectAddr    = "addr"
	flagInspectTimeout = "timeout"
)

// standaloneInspectCmd reads a running standalone process's store through its
// inspect server: the standalone mode's counterpart of the redis subcommands.
func standaloneInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Read what a running standalone process holds: sessions, trees, queues, meters, submissions",
		Long: `Read the store of a running standalone process, the way the redis
subcommands read Redis in high-availability mode. No other process can open the
store while the standalone process runs, so it answers through its inspect
server: enable it with

  inspect:
    enabled: true
    addr: "127.0.0.1:9094"   # loopback only; the default

and run these on the same host (or in the same pod).`,
	}
	cmd.PersistentFlags().String(flagInspectAddr, standalone.DefaultInspectAddr, "Address of the inspect server")
	cmd.PersistentFlags().Duration(flagInspectTimeout, 10*time.Second, "Time allowed for an answer")
	cmd.AddCommand(inspectSessionsCmd(), inspectSupplierCmd(), inspectStreamsCmd(),
		inspectSMSTCmd(), inspectDedupCmd(), inspectMeterCmd(), inspectSubmissionsCmd())
	return cmd
}

// inspectGet reads path from the inspect server into out.
func inspectGet(cmd *cobra.Command, path string, query url.Values, out any) error {
	addr, _ := cmd.Flags().GetString(flagInspectAddr)
	timeout, _ := cmd.Flags().GetDuration(flagInspectTimeout)
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	u := url.URL{Scheme: "http", Host: addr, Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("inspect server at %s: %w (is the standalone process running with inspect.enabled?)", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("inspect server at %s: %w", addr, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("inspect server at %s: %s: %s", addr, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("inspect server at %s: %w", addr, err)
	}
	return nil
}

func printJSON(v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func inspectSessionsCmd() *cobra.Command {
	var supplier, sessionID, state string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Sessions of a supplier, as redis sessions shows them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{"supplier": {supplier}}
			if state != "" {
				q.Set("state", state)
			}
			if sessionID != "" {
				q.Set("session", sessionID)
			}
			var sessions []map[string]interface{}
			if err := inspectGet(cmd, inspect.PathSessions, q, &sessions); err != nil {
				return err
			}
			if sessionID != "" {
				if len(sessions) == 0 {
					return fmt.Errorf("session not found: %s", sessionID)
				}
				return redis.PrintSession(sessions[0], supplier, sessionID, jsonOutput)
			}
			return redis.PrintSessions(sessions, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&supplier, "supplier", "", "Supplier operator address (required)")
	cmd.Flags().StringVar(&sessionID, "session", "", "Specific session ID to inspect")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("supplier")
	return cmd
}

func inspectSupplierCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "supplier",
		Short: "Supplier states, as redis supplier --list shows them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var states map[string]json.RawMessage
			if err := inspectGet(cmd, inspect.PathSuppliers, nil, &states); err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(states)
			}
			return redis.PrintSupplierList(states)
		},
	}
	// Accepted so `redis supplier --list` and this read alike: listing is
	// what this command does.
	cmd.Flags().Bool("list", true, "List every supplier (the default)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func inspectStreamsCmd() *cobra.Command {
	var supplier string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "streams",
		Short: "Relay queues: entries stored, pending, released, last ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if supplier != "" {
				q.Set("supplier", supplier)
			}
			var stats []pebblequeue.QueueStats
			if err := inspectGet(cmd, inspect.PathStreams, q, &stats); err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(stats)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "SUPPLIER\tLENGTH\tPENDING\tRELEASED\tLAST ID\n")
			for _, s := range stats {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", s.Supplier, s.Length, s.Pending, s.Released, s.LastID)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&supplier, "supplier", "", "One supplier's queue (default: every queue)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func inspectSMSTCmd() *cobra.Command {
	var supplier, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "smst",
		Short: "A session's trees: nodes, roots, stats",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{"session": {sessionID}}
			if supplier != "" {
				q.Set("supplier", supplier)
			}
			var trees []miner.SMSTView
			if err := inspectGet(cmd, inspect.PathSMST, q, &trees); err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(trees)
			}
			if len(trees) == 0 {
				fmt.Printf("No SMST data found for session: %s\n", sessionID)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "SUPPLIER\tNODES\tCLAIMED ROOT\tLIVE ROOT\tSTATS\tCOMPACTED\n")
			for _, t := range trees {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%v\n", t.Supplier, t.NodeCount, dash(t.ClaimedRoot), dash(t.LiveRoot), dash(t.Stats), t.Compacted)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID (required)")
	cmd.Flags().StringVar(&supplier, "supplier", "", "One supplier's tree (default: every supplier's)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("session")
	return cmd
}

func inspectDedupCmd() *cobra.Command {
	var sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "dedup",
		Short: "A session's dedup marks: count, TTL, sample hashes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var view miner.DedupView
			if err := inspectGet(cmd, inspect.PathDedup, url.Values{"session": {sessionID}}, &view); err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(view)
			}
			if view.Count == 0 {
				fmt.Printf("No deduplication data found for session: %s\n", sessionID)
				return nil
			}
			fmt.Printf("Deduplication Marks for Session: %s\n", sessionID)
			fmt.Printf("Total Relay Hashes: %d\n", view.Count)
			fmt.Printf("Expires At (unix ms): %d (live: %v)\n\n", view.ExpiresAtUnixMs, view.Live)
			fmt.Printf("Sample Relay Hashes (showing %d of %d):\n", len(view.Sample), view.Count)
			for i, h := range view.Sample {
				fmt.Printf("  %d. %s\n", i+1, h)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("session")
	return cmd
}

func inspectMeterCmd() *cobra.Command {
	var sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "meter",
		Short: "A session's relay meters, or every meter key",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sessionID == "" {
				var keys []string
				if err := inspectGet(cmd, inspect.PathMeter, nil, &keys); err != nil {
					return err
				}
				if jsonOutput {
					return printJSON(keys)
				}
				for _, k := range keys {
					fmt.Println(k)
				}
				return nil
			}
			var meters []inspect.MeterEntry
			if err := inspectGet(cmd, inspect.PathMeter, url.Values{"session": {sessionID}}, &meters); err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(meters)
			}
			if len(meters) == 0 {
				fmt.Printf("No metering data found for session: %s\n", sessionID)
				return nil
			}
			for _, m := range meters {
				consumed := "<unset>"
				if m.ConsumedUpokt != nil {
					consumed = *m.ConsumedUpokt
				}
				fmt.Printf("Key: %s\nMeta: %s\nconsumed_upokt: %s\n\n", m.Key, m.Meta, consumed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID (default: list every meter key)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func inspectSubmissionsCmd() *cobra.Command {
	var supplier, service, app string
	var failedOnly, successOnly, jsonOutput bool
	var limit int
	cmd := &cobra.Command{
		Use:   "submissions",
		Short: "Claim and proof submission tracking records, as redis submissions shows them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if supplier != "" {
				q.Set("supplier", supplier)
			}
			var records []miner.SubmissionTrackingRecord
			if err := inspectGet(cmd, inspect.PathSubmissions, q, &records); err != nil {
				return err
			}
			return redis.PrintSubmissionRecords(records, service, app, failedOnly, successOnly, limit, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&supplier, "supplier", "", "Filter by supplier operator address")
	cmd.Flags().StringVar(&service, "service", "", "Filter by service ID")
	cmd.Flags().StringVar(&app, "app", "", "Filter by application address")
	cmd.Flags().BoolVar(&failedOnly, "failed-only", false, "Show only failed submissions")
	cmd.Flags().BoolVar(&successOnly, "success-only", false, "Show only successful submissions")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of records to show (0 = unlimited)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
