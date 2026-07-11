package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

func SubmissionsCmd() *cobra.Command {
	var (
		supplierAddr string
		serviceID    string
		appAddr      string
		sessionID    string
		sessionEnd   int64
		failedOnly   bool
		successOnly  bool
		jsonOutput   bool
		limit        int
	)

	cmd := &cobra.Command{
		Use:   "submissions",
		Short: "Inspect claim/proof submission tracking records",
		Long: `Inspect claim/proof submission tracking records stored in Redis.

Tracking data is stored at:
  - Key: ha:tx:track:{supplier}:{sessionEndHeight}:{sessionID}
  - TTL: 7 days (for post-mortem analysis)

This helps debug why proofs/claims were missed by showing:
  - Success/failure status for claims and proofs
  - Transaction hashes (if broadcast succeeded)
  - Error reasons (if submission failed)
  - Timing information (submission height, current height, UTC time)
  - Session metadata (relays, compute units, proof requirement)

Examples:
  # List all tracked submissions
  pocket-relay-miner redis submissions

  # Filter by supplier
  pocket-relay-miner redis submissions --supplier pokt1abc...

  # Filter by service
  pocket-relay-miner redis submissions --service develop-http

  # Show specific session
  pocket-relay-miner redis submissions --supplier pokt1abc... --session <session_id> --session-end 140

  # Show only failed submissions
  pocket-relay-miner redis submissions --failed-only

  # Limit results
  pocket-relay-miner redis submissions --limit 10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			// If session ID and end height provided, show specific record
			if sessionID != "" && sessionEnd > 0 {
				if supplierAddr == "" {
					return fmt.Errorf("--supplier is required when using --session and --session-end")
				}
				return showSubmission(ctx, client, supplierAddr, sessionEnd, sessionID, jsonOutput)
			}

			// Otherwise list all submissions with optional filters
			return listSubmissions(ctx, client, supplierAddr, serviceID, appAddr, failedOnly, successOnly, limit, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Filter by supplier operator address")
	cmd.Flags().StringVar(&serviceID, "service", "", "Filter by service ID")
	cmd.Flags().StringVar(&appAddr, "app", "", "Filter by application address")
	cmd.Flags().StringVar(&sessionID, "session", "", "Specific session ID to inspect")
	cmd.Flags().Int64Var(&sessionEnd, "session-end", 0, "Session end height (required with --session)")
	cmd.Flags().BoolVar(&failedOnly, "failed-only", false, "Show only failed submissions")
	cmd.Flags().BoolVar(&successOnly, "success-only", false, "Show only successful submissions")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of records to show (0 = unlimited)")

	return cmd
}

func showSubmission(ctx context.Context, client *DebugRedisClient, supplier string, sessionEnd int64, sessionID string, jsonOutput bool) error {
	key := client.KB().TxTrackKey(supplier, sessionEnd, sessionID)

	data, err := client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return fmt.Errorf("submission tracking record not found: %s (session_end: %d)", sessionID, sessionEnd)
	}
	if err != nil {
		return fmt.Errorf("failed to get submission record: %w", err)
	}

	if jsonOutput {
		// Pretty print JSON
		var record map[string]interface{}
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("failed to parse record: %w", err)
		}
		prettyJSON, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to format JSON: %w", err)
		}
		fmt.Println(string(prettyJSON))
		return nil
	}

	// Parse and display as table
	var record submissionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("failed to parse record: %w", err)
	}

	printSubmissionDetail(&record)
	return nil
}

func listSubmissions(ctx context.Context, client *DebugRedisClient, supplier, service, app string, failedOnly, successOnly bool, limit int, jsonOutput bool) error {
	// Build scan pattern based on filters
	pattern := client.KB().TxTrackAllPattern()
	if supplier != "" {
		pattern = client.KB().TxTrackPattern(supplier)
	}

	keys, err := client.Keys(ctx, pattern).Result()
	if err != nil {
		return fmt.Errorf("failed to scan keys: %w", err)
	}

	if len(keys) == 0 {
		fmt.Printf("No submission tracking records found\n")
		return nil
	}

	var records []submissionRecord
	for _, key := range keys {
		data, err := client.Get(ctx, key).Bytes()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to get %s: %v\n", key, err)
			continue
		}

		var record submissionRecord
		if err := json.Unmarshal(data, &record); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse %s: %v\n", key, err)
			continue
		}

		// Apply filters
		if service != "" && record.Service != service {
			continue
		}
		if app != "" && record.Application != app {
			continue
		}
		if failedOnly {
			if record.ClaimSuccess && record.ProofSuccess {
				continue // Skip successful
			}
		}
		if successOnly {
			if !record.ClaimSuccess || !record.ProofSuccess {
				continue // Skip failed
			}
		}

		records = append(records, record)
	}

	if len(records) == 0 {
		fmt.Printf("No matching submission records found\n")
		return nil
	}

	// Sort by session start height (descending) then timestamp
	sort.Slice(records, func(i, j int) bool {
		if records[i].SessionStart != records[j].SessionStart {
			return records[i].SessionStart > records[j].SessionStart
		}
		return records[i].ClaimSubmitTimestamp > records[j].ClaimSubmitTimestamp
	})

	// Apply limit
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}

	if jsonOutput {
		output, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(output))
		return nil
	}

	// Display as table
	printSubmissionsTable(records)
	return nil
}

func printSubmissionsTable(records []submissionRecord) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	_, _ = fmt.Fprintf(w, "SESSION_END\tSERVICE\tCLAIM_STATUS\tPROOF_STATUS\tRELAYS\tCU\tSESSION_ID\n")
	_, _ = fmt.Fprintf(w, "-----------\t-------\t------------\t------------\t------\t--\t----------\n")

	for _, r := range records {
		claimStatus := "✗ FAILED"
		if r.ClaimSuccess {
			claimStatus = "✓ SUCCESS"
		}

		proofStatus := "-"
		if r.ProofTxHash != "" {
			if r.ProofSuccess {
				proofStatus = "✓ SUCCESS"
			} else {
				proofStatus = "✗ FAILED"
			}
		} else if r.ProofRequired {
			proofStatus = "⊗ MISSING"
		}

		// Truncate session ID for readability
		sessionIDShort := r.SessionID
		if len(sessionIDShort) > 12 {
			sessionIDShort = sessionIDShort[:12] + "..."
		}

		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%d\t%s\n",
			r.SessionEnd,
			r.Service,
			claimStatus,
			proofStatus,
			r.NumRelays,
			r.ComputeUnits,
			sessionIDShort,
		)
	}
}

func printSubmissionDetail(r *submissionRecord) {
	fmt.Println("================================================================================")
	fmt.Println("SUBMISSION TRACKING RECORD")
	fmt.Println("================================================================================")
	fmt.Printf("Session ID:        %s\n", r.SessionID)
	fmt.Printf("Supplier:          %s\n", r.Supplier)
	fmt.Printf("Service:           %s\n", r.Service)
	fmt.Printf("Application:       %s\n", r.Application)
	fmt.Printf("Session Range:     %d - %d\n", r.SessionStart, r.SessionEnd)
	fmt.Printf("Relays:            %d\n", r.NumRelays)
	fmt.Printf("Compute Units:     %d\n", r.ComputeUnits)
	fmt.Printf("Proof Required:    %v\n", r.ProofRequired)
	if r.ProofRequirementSeed != "" {
		fmt.Printf("Proof Req Seed:    %s\n", r.ProofRequirementSeed)
	}

	fmt.Println("\n--- CLAIM SUBMISSION ---")
	fmt.Printf("Claim Hash:        %s\n", r.ClaimHash)
	fmt.Printf("Claim TX Hash:     %s\n", r.ClaimTxHash)
	fmt.Printf("Claim Success:     %v\n", r.ClaimSuccess)
	if r.ClaimErrorReason != "" {
		fmt.Printf("Claim Error:       %s\n", r.ClaimErrorReason)
	}
	fmt.Printf("Claim Height:      %d\n", r.ClaimSubmitHeight)
	fmt.Printf("Current Height:    %d\n", r.ClaimCurrentHeight)
	if r.ClaimSubmitTimeUTC != "" {
		fmt.Printf("Claim Time (UTC):  %s\n", r.ClaimSubmitTimeUTC)
	} else if r.ClaimSubmitTimestamp > 0 {
		fmt.Printf("Claim Timestamp:   %d (%s)\n", r.ClaimSubmitTimestamp, time.Unix(r.ClaimSubmitTimestamp, 0).UTC().Format(time.RFC3339))
	}

	if r.ProofTxHash != "" || r.ProofRequired {
		fmt.Println("\n--- PROOF SUBMISSION ---")
		if r.ProofHash != "" {
			fmt.Printf("Proof Hash:        %s\n", r.ProofHash)
		}
		if r.ProofTxHash != "" {
			fmt.Printf("Proof TX Hash:     %s\n", r.ProofTxHash)
			fmt.Printf("Proof Success:     %v\n", r.ProofSuccess)
		} else {
			fmt.Printf("Proof TX Hash:     <NOT SUBMITTED>\n")
		}
		if r.ProofErrorReason != "" {
			fmt.Printf("Proof Error:       %s\n", r.ProofErrorReason)
		}
		if r.ProofSubmitHeight > 0 {
			fmt.Printf("Proof Height:      %d\n", r.ProofSubmitHeight)
			fmt.Printf("Current Height:    %d\n", r.ProofCurrentHeight)
		}
		if r.ProofSubmitTimeUTC != "" {
			fmt.Printf("Proof Time (UTC):  %s\n", r.ProofSubmitTimeUTC)
		} else if r.ProofSubmitTimestamp > 0 {
			fmt.Printf("Proof Timestamp:   %d (%s)\n", r.ProofSubmitTimestamp, time.Unix(r.ProofSubmitTimestamp, 0).UTC().Format(time.RFC3339))
		}
	}

	fmt.Println("================================================================================")
}

// submissionRecord mirrors the SubmissionTrackingRecord from miner/submission_tracker.go
type submissionRecord struct {
	Supplier             string `json:"supplier"`
	Service              string `json:"service"`
	Application          string `json:"application"`
	SessionID            string `json:"session_id"`
	SessionStart         int64  `json:"session_start"`
	SessionEnd           int64  `json:"session_end"`
	ClaimHash            string `json:"claim_hash"`
	ClaimTxHash          string `json:"claim_tx_hash"`
	ClaimSuccess         bool   `json:"claim_success"`
	ClaimErrorReason     string `json:"claim_error_reason,omitempty"`
	ClaimSubmitHeight    int64  `json:"claim_submit_height"`
	ClaimSubmitTimestamp int64  `json:"claim_submit_timestamp"`
	ClaimSubmitTimeUTC   string `json:"claim_submit_time_utc"`
	ClaimCurrentHeight   int64  `json:"claim_current_height"`
	ProofHash            string `json:"proof_hash,omitempty"`
	ProofTxHash          string `json:"proof_tx_hash,omitempty"`
	ProofSuccess         bool   `json:"proof_success"`
	ProofErrorReason     string `json:"proof_error_reason,omitempty"`
	ProofSubmitHeight    int64  `json:"proof_submit_height,omitempty"`
	ProofSubmitTimestamp int64  `json:"proof_submit_timestamp,omitempty"`
	ProofSubmitTimeUTC   string `json:"proof_submit_time_utc,omitempty"`
	ProofCurrentHeight   int64  `json:"proof_current_height,omitempty"`
	NumRelays            int64  `json:"num_relays"`
	ComputeUnits         int64  `json:"compute_units"`
	ProofRequired        bool   `json:"proof_required"`
	ProofRequirementSeed string `json:"proof_requirement_seed,omitempty"`
}
