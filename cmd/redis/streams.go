package redis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func StreamsCmd() *cobra.Command {
	var (
		supplierAddr string
		listAll      bool
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
	cmd.Flags().Int64Var(&limit, "limit", 10, "Number of messages to display")

	return cmd
}

func listAllStreams(ctx context.Context, client *DebugRedisClient, _ int64) error {
	pattern := "ha:relays:*"
	var cursor uint64
	var streams []string

	for {
		keys, newCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("failed to scan streams: %w", err)
		}

		streams = append(streams, keys...)
		cursor = newCursor
		if cursor == 0 {
			break
		}
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
	streamKey := fmt.Sprintf("ha:relays:%s", supplierAddr)

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
