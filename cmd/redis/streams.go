package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
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

	known, err := knownSupplierAddresses(ctx, client)
	if err != nil {
		return err
	}

	streamPrefix := client.KB().StreamPrefix() + ":"
	type orphan struct {
		key     string
		addr    string
		length  int64
		pending int64
	}
	var orphans []orphan

	for _, key := range streams {
		addr := strings.TrimPrefix(key, streamPrefix)
		if addr == key || addr == "" {
			continue // not a supplier stream under this namespace
		}
		if _, ok := known[addr]; ok {
			continue
		}

		o := orphan{key: key, addr: addr}
		if n, lenErr := client.XLen(ctx, key).Result(); lenErr == nil {
			o.length = n
		}
		// Pending is summed across every group: a stream can carry more than one,
		// and an entry pending in ANY of them is unfinished work.
		if groups, groupErr := client.XInfoGroups(ctx, key).Result(); groupErr == nil {
			for _, g := range groups {
				o.pending += g.Pending
			}
		}
		orphans = append(orphans, o)
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
		safe := o.length == 0 && o.pending == 0
		if safe {
			deletable = append(deletable, o)
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%v\n", o.addr, o.length, o.pending, safe)
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
		// Re-check immediately before deleting. The listing above is a snapshot,
		// and a supplier can be re-claimed by another miner between the scan and
		// this line; deleting then would destroy live deliveries.
		length, lenErr := client.XLen(ctx, o.key).Result()
		if lenErr != nil || length != 0 {
			fmt.Printf("  skip %s: stream is no longer empty\n", o.addr)
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

// knownSupplierAddresses returns every supplier address this deployment has a
// record of, from the registry index and from the supplier cache.
func knownSupplierAddresses(ctx context.Context, client *DebugRedisClient) (map[string]struct{}, error) {
	known := make(map[string]struct{})

	indexed, err := client.SMembers(ctx, client.KB().SuppliersRegistryIndexKey()).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("failed to read the supplier registry index: %w", err)
	}
	for _, addr := range indexed {
		known[addr] = struct{}{}
	}

	cachePrefix := client.KB().SupplierKeyPrefix() + ":"
	cached, err := clusterAwareScanAllKeys(ctx, client, cachePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("failed to scan supplier cache keys: %w", err)
	}
	for _, key := range cached {
		if addr := strings.TrimPrefix(key, cachePrefix); addr != key && addr != "" {
			known[addr] = struct{}{}
		}
	}

	return known, nil
}
