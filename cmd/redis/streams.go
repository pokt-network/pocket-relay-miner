package redis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/miner"
)

func StreamsCmd() *cobra.Command {
	var (
		supplierAddr string
		listAll      bool
		listOrphaned bool
		deleteEmpty  bool
		yes          bool
		limit        int64
	)

	cmd := &cobra.Command{
		Use:   "streams",
		Short: "Inspect Redis Streams (relay WAL)",
		Long: `Inspect Redis Streams used as the Write-Ahead Log for relays.

Stream data is stored at:
  - Key: ha:relays:{supplierAddress} (Stream)
  - Messages contain relay data awaiting SMST updates

This shows stream length, consumer groups, and pending messages.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if listOrphaned {
				return orphanedStreams(ctx, client, deleteEmpty, yes)
			}

			if listAll {
				return listAllStreams(ctx, client, limit)
			}

			if supplierAddr == "" {
				return fmt.Errorf("--supplier is required (or use --all to list all streams)")
			}

			return inspectStream(ctx, client, supplierAddr, limit)
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Supplier address")
	cmd.Flags().BoolVar(&listAll, "all", false, "List all relay streams")
	cmd.Flags().BoolVar(&listOrphaned, "orphaned", false,
		"List relay streams whose supplier this deployment no longer knows about")
	cmd.Flags().BoolVar(&deleteEmpty, "delete-empty", false,
		"With --orphaned: delete orphaned streams that hold NOTHING (no entries, no pending). Never deletes a stream with data")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt for --delete-empty")
	cmd.Flags().Int64Var(&limit, "limit", 10, "Number of messages to display")

	return cmd
}

func listAllStreams(ctx context.Context, client *DebugRedisClient, _ int64) error {
	streams, err := clusterAwareScanAllKeys(ctx, client, client.KB().StreamPattern())
	if err != nil {
		return fmt.Errorf("failed to scan streams: %w", err)
	}

	if len(streams) == 0 {
		fmt.Println("No relay streams found")
		return nil
	}

	fmt.Printf("Relay Streams (%d found):\n\n", len(streams))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "STREAM KEY\tLENGTH\tCONSUMER GROUPS\n")

	for _, stream := range streams {
		length, err := client.XLen(ctx, stream).Result()
		if err != nil {
			continue
		}

		groups, err := client.XInfoGroups(ctx, stream).Result()
		groupCount := 0
		if err == nil {
			groupCount = len(groups)
		}

		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\n", stream, length, groupCount)
	}

	_ = w.Flush()
	return nil
}

func inspectStream(ctx context.Context, client *DebugRedisClient, supplierAddr string, limit int64) error {
	streamKey := client.KB().StreamKey(supplierAddr)

	// Check if stream exists
	exists, err := client.Exists(ctx, streamKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check stream existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("No stream found for supplier: %s\n", supplierAddr)
		return nil
	}

	// Get stream length
	length, err := client.XLen(ctx, streamKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get stream length: %w", err)
	}

	fmt.Printf("Stream: %s\n", streamKey)
	fmt.Printf("Length: %d messages\n\n", length)

	// Get consumer groups
	groups, err := client.XInfoGroups(ctx, streamKey).Result()
	if err == nil && len(groups) > 0 {
		fmt.Printf("Consumer Groups:\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "  GROUP\tCONSUMERS\tPENDING\tLAST ID\n")

		for _, group := range groups {
			_, _ = fmt.Fprintf(w, "  %s\t%d\t%d\t%s\n",
				group.Name, group.Consumers, group.Pending, group.LastDeliveredID)
		}
		_ = w.Flush()
		fmt.Println()
	}

	// Show recent messages
	if length > 0 && limit > 0 {
		messages, err := client.XRevRange(ctx, streamKey, "+", "-").Result()
		if err != nil {
			return fmt.Errorf("failed to read stream messages: %w", err)
		}

		displayCount := int(limit)
		if displayCount > len(messages) {
			displayCount = len(messages)
		}

		fmt.Printf("Recent Messages (showing %d of %d):\n\n", displayCount, len(messages))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "MESSAGE ID\tSESSION\tFIELDS\n")

		for i := 0; i < displayCount; i++ {
			msg := messages[i]
			sessionID := ""
			if sid, ok := msg.Values["session_id"]; ok {
				sessionID = fmt.Sprintf("%v", sid)
			}
			fields := make([]string, 0, len(msg.Values))
			for k := range msg.Values {
				fields = append(fields, k)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", msg.ID, sessionID, strings.Join(fields, ","))
		}
		_ = w.Flush()
	}

	return nil
}

// streamStats reads a stream's length and total pending count (summed across
// every consumer group) in one place, so the listing's classification and the
// pre-delete re-check can never disagree about what "safe" requires.
//
// ok=false on ANY read error (XLen or XInfoGroups) and NEVER on a partial
// result: before this, a failed XLen alone left length at its zero value
// silently, and a stream that genuinely had relays in it printed
// "SAFE TO DELETE true". A transient Redis error must make a stream UNKNOWN,
// not empty -- the two look identical in the length/pending numbers, but only
// one of them is safe to act on.
func streamStats(ctx context.Context, client *DebugRedisClient, key string) (length, pending int64, ok bool) {
	n, err := client.XLen(ctx, key).Result()
	if err != nil {
		return 0, 0, false
	}
	length = n

	groups, err := client.XInfoGroups(ctx, key).Result()
	if err != nil {
		return length, 0, false
	}
	for _, g := range groups {
		pending += g.Pending
	}
	return length, pending, true
}

// orphanedStreams reports relay streams whose supplier this deployment no longer
// knows about, and optionally deletes the ones that hold nothing.
//
// It reports rather than cleans up on its own, and that is a deliberate split.
// Deleting a stream key deletes its consumer group and that group's pending
// entries list along with it, and the consumer recreates the group empty on its
// next connect (XGroupCreateMkStream), so a live consumer would silently resume
// from an empty stream and the un-acknowledged relays would be gone with no
// trace. Automatic deletion is only safe once the supplier lifecycle is
// trustworthy end to end; until then an operator decides, with the numbers in
// front of them.
//
// "Known" is the union of the registry index and the supplier cache, on purpose:
// either one alone would over-report. A supplier torn down by this fleet keeps
// its cache entry (that entry is the chain's answer, and the relayer needs it to
// keep refusing), while a miner that crashed leaves a stale registry index entry.
// Taking the union means only a stream nobody claims at all is called an orphan.
func orphanedStreams(ctx context.Context, client *DebugRedisClient, deleteEmpty, yes bool) error {
	streams, err := clusterAwareScanAllKeys(ctx, client, client.KB().StreamPattern())
	if err != nil {
		return fmt.Errorf("failed to scan streams: %w", err)
	}

	// The "known" rule lives in the miner, which owns supplier lifecycle: two
	// copies of it drifting apart would surface as a delete that should not have
	// happened, long after the change that caused it.
	known, err := miner.KnownSupplierAddresses(ctx, client.Client, func(ctx context.Context, pattern string) ([]string, error) {
		return clusterAwareScanAllKeys(ctx, client, pattern)
	})
	if err != nil {
		return err
	}

	type orphan struct {
		key     string
		addr    string
		length  int64
		pending int64
		statsOK bool
	}
	var orphans []orphan

	for _, key := range streams {
		addr, ok := client.KB().StreamAddress(key)
		if !ok {
			continue // not a supplier stream under this namespace
		}
		if _, ok := known[addr]; ok {
			continue
		}

		length, pending, statsOK := streamStats(ctx, client, key)
		orphans = append(orphans, orphan{key: key, addr: addr, length: length, pending: pending, statsOK: statsOK})
	}

	if len(orphans) == 0 {
		fmt.Printf("No orphaned relay streams (%d stream(s) scanned, all belong to a known supplier)\n", len(streams))
		return nil
	}

	fmt.Printf("Orphaned relay streams (%d of %d scanned):\n\n", len(orphans), len(streams))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "SUPPLIER\tENTRIES\tPENDING\tSAFE TO DELETE\n")
	deletable := make([]orphan, 0, len(orphans))
	for _, o := range orphans {
		// !statsOK must never read as safe: a stream we failed to read is
		// unknown, not empty, and unknown is never a green light to delete.
		safe := o.statsOK && o.length == 0 && o.pending == 0
		if safe {
			deletable = append(deletable, o)
		}
		safeCol := "unknown (read error)"
		if o.statsOK {
			safeCol = fmt.Sprintf("%v", safe)
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", o.addr, o.length, o.pending, safeCol)
	}
	_ = w.Flush()

	if !deleteEmpty {
		fmt.Printf("\n%d of them hold nothing and could be deleted with --delete-empty.\n", len(deletable))
		fmt.Printf("Streams with entries or pending deliveries are NEVER deleted by this command:\n")
		fmt.Printf("that data is relays nobody has been paid for yet.\n")
		return nil
	}

	if len(deletable) == 0 {
		fmt.Printf("\nNothing to delete: every orphaned stream still holds entries or pending deliveries.\n")
		return nil
	}

	fmt.Printf("\nAbout to delete %d EMPTY orphaned stream(s).\n", len(deletable))
	if !yes && !confirmProceed() {
		fmt.Println("Aborted")
		return nil
	}

	for _, o := range deletable {
		// Re-check immediately before deleting, with the SAME two-part guard as
		// the listing above (streamStats). A supplier can be re-claimed by
		// another miner between the scan and this line, and length==0 alone is
		// not enough: XACKDEL/DELREF or XDEL can leave a stream at length 0
		// while another group's pending list still references those entry IDs.
		// Re-running only XLen here (as this used to) would pass that stream
		// straight through and Del would take the pending list with it.
		length, pending, statsOK := streamStats(ctx, client, o.key)
		if !statsOK || length != 0 || pending != 0 {
			fmt.Printf("  skip %s: stream is no longer empty (or could not be re-checked)\n", o.addr)
			continue
		}
		if err := client.Del(ctx, o.key).Err(); err != nil {
			fmt.Printf("  error deleting %s: %v\n", o.addr, err)
			continue
		}
		fmt.Printf("  deleted %s\n", o.addr)
	}
	return nil
}
