package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func redisDebugStreamsCmd() *cobra.Command {
	var (
		supplierAddr string
		streamPrefix string
		limit        int64
		group        string
	)

	cmd := &cobra.Command{
		Use:   "streams",
		Short: "Inspect Redis Streams (WAL)",
		Long: `Inspect Redis Streams used for relay message coordination.

Stream data is stored at:
  - Key: {streamPrefix}:{supplierAddress}
  - Default prefix: ha:relays

Shows stream length, consumer groups, and pending messages.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return inspectStream(ctx, client, streamPrefix, supplierAddr, group, limit)
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Supplier operator address (required)")
	cmd.Flags().StringVar(&streamPrefix, "prefix", "ha:relays", "Stream key prefix")
	cmd.Flags().StringVar(&group, "group", "", "Consumer group name to inspect")
	cmd.Flags().Int64Var(&limit, "limit", 10, "Number of messages to display")
	_ = cmd.MarkFlagRequired("supplier")

	return cmd
}

func inspectStream(ctx context.Context, client *redisClient, prefix, supplier, group string, limit int64) error {
	streamKey := fmt.Sprintf("%s:%s", prefix, supplier)

	// Check if stream exists
	exists, err := client.Exists(ctx, streamKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check stream existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("No stream found for supplier: %s\n", supplier)
		fmt.Printf("Stream key: %s\n", streamKey)
		return nil
	}

	// Get stream info
	info, err := client.XInfoStream(ctx, streamKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get stream info: %w", err)
	}

	fmt.Printf("Stream: %s\n", streamKey)
	fmt.Printf("Length: %d messages\n", info.Length)
	fmt.Printf("Consumer Groups: %d\n", info.Groups)
	fmt.Printf("First Entry ID: %s\n", info.FirstEntry.ID)
	fmt.Printf("Last Entry ID: %s\n", info.LastEntry.ID)
	fmt.Printf("\n")

	// Show consumer groups if present
	if info.Groups > 0 {
		groups, err := client.XInfoGroups(ctx, streamKey).Result()
		if err == nil {
			fmt.Printf("Consumer Groups:\n")
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "GROUP\tCONSUMERS\tPENDING\tLAST DELIVERED\n")
			for _, g := range groups {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", g.Name, g.Consumers, g.Pending, g.LastDeliveredID)
			}
			_ = w.Flush()
			fmt.Printf("\n")
		}

		// If specific group requested, show consumer details
		if group != "" {
			consumers, err := client.XInfoConsumers(ctx, streamKey, group).Result()
			if err == nil && len(consumers) > 0 {
				fmt.Printf("Consumers in Group '%s':\n", group)
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintf(w, "CONSUMER\tPENDING\tIDLE (ms)\n")
				for _, c := range consumers {
					_, _ = fmt.Fprintf(w, "%s\t%d\t%d\n", c.Name, c.Pending, c.Idle)
				}
				_ = w.Flush()
				fmt.Printf("\n")
			}

			// Show pending messages for the group
			pending, err := client.XPending(ctx, streamKey, group).Result()
			if err == nil && pending.Count > 0 {
				fmt.Printf("Pending Messages in Group '%s':\n", group)
				fmt.Printf("Count: %d\n", pending.Count)
				fmt.Printf("Lower ID: %s\n", pending.Lower)
				fmt.Printf("Higher ID: %s\n", pending.Higher)
				fmt.Printf("\n")
			}
		}
	}

	// Read recent messages
	messages, err := client.XRevRange(ctx, streamKey, "+", "-").Result()
	if err != nil {
		return fmt.Errorf("failed to read messages: %w", err)
	}

	if len(messages) == 0 {
		fmt.Printf("No messages in stream\n")
		return nil
	}

	displayCount := int(limit)
	if len(messages) < displayCount {
		displayCount = len(messages)
	}

	fmt.Printf("Recent Messages (showing %d of %d):\n\n", displayCount, len(messages))

	for i := 0; i < displayCount; i++ {
		msg := messages[i]
		fmt.Printf("Message ID: %s\n", msg.ID)

		if data, ok := msg.Values["data"].(string); ok {
			// Try to parse as JSON for pretty printing
			var relay map[string]interface{}
			if err := json.Unmarshal([]byte(data), &relay); err == nil {
				fmt.Printf("  Session ID: %v\n", relay["session_id"])
				fmt.Printf("  Service ID: %v\n", relay["service_id"])
				fmt.Printf("  Supplier: %v\n", relay["supplier_operator_address"])
				fmt.Printf("  Published At: %v\n", relay["published_at_unix_nano"])
			} else {
				fmt.Printf("  Data: %s\n", data)
			}
		}
		fmt.Printf("\n")
	}

	return nil
}
