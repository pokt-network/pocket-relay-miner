package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func redisDebugPubSubCmd() *cobra.Command {
	var (
		channel  string
		duration int
	)

	cmd := &cobra.Command{
		Use:   "pubsub",
		Short: "Monitor pub/sub channels",
		Long: `Monitor Redis pub/sub channels for events.

Common channels:
  - ha:events:cache:{type}:invalidate - Cache invalidation events
  - ha:events:supplier_update - Supplier registry updates
  - ha:meter:cleanup - Meter cleanup signals
  - ha:events:session:rewardable - Session rewardability updates

Press Ctrl+C to stop monitoring.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return monitorPubSub(ctx, client, channel, duration)
		},
	}

	cmd.Flags().StringVar(&channel, "channel", "", "Channel to monitor (required)")
	cmd.Flags().IntVar(&duration, "duration", 0, "Duration in seconds (0 = until Ctrl+C)")
	_ = cmd.MarkFlagRequired("channel")

	return cmd
}

func monitorPubSub(ctx context.Context, client *redisClient, channel string, durationSec int) error {
	pubsub := client.Subscribe(ctx, channel)
	defer func() { _ = pubsub.Close() }()

	// Wait for confirmation
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", channel, err)
	}

	fmt.Printf("Monitoring channel: %s\n", channel)
	fmt.Printf("Press Ctrl+C to stop...\n\n")

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Setup timeout if specified
	var timeoutChan <-chan time.Time
	if durationSec > 0 {
		timeoutChan = time.After(time.Duration(durationSec) * time.Second)
		fmt.Printf("Will stop after %d seconds\n\n", durationSec)
	}

	msgCount := 0
	ch := pubsub.Channel()

	for {
		select {
		case <-sigChan:
			fmt.Printf("\n\nReceived interrupt signal. Stopping...\n")
			fmt.Printf("Total messages received: %d\n", msgCount)
			return nil

		case <-timeoutChan:
			fmt.Printf("\n\nTimeout reached. Stopping...\n")
			fmt.Printf("Total messages received: %d\n", msgCount)
			return nil

		case msg := <-ch:
			if msg == nil {
				fmt.Printf("\nChannel closed\n")
				return nil
			}

			msgCount++
			timestamp := time.Now().Format("15:04:05.000")
			fmt.Printf("[%s] Message #%d\n", timestamp, msgCount)
			fmt.Printf("Channel: %s\n", msg.Channel)
			fmt.Printf("Payload: %s\n\n", msg.Payload)

		case <-ctx.Done():
			fmt.Printf("\nContext cancelled\n")
			return ctx.Err()
		}
	}
}
