package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func redisDebugSupplierCmd() *cobra.Command {
	var (
		supplierAddr string
		listAll      bool
	)

	cmd := &cobra.Command{
		Use:   "supplier",
		Short: "Inspect supplier registry",
		Long: `Inspect supplier registry data in Redis.

Supplier data is stored at:
  - Key: ha:suppliers:{supplier} (Hash)
  - Index: ha:suppliers:index (Set of all suppliers)

Shows configured suppliers and their metadata.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if listAll {
				return listAllSuppliers(ctx, client)
			}

			if supplierAddr != "" {
				return inspectSupplier(ctx, client, supplierAddr)
			}

			return fmt.Errorf("specify --supplier or --list")
		},
	}

	cmd.Flags().StringVar(&supplierAddr, "supplier", "", "Supplier operator address")
	cmd.Flags().BoolVar(&listAll, "list", false, "List all suppliers")

	return cmd
}

func listAllSuppliers(ctx context.Context, client *redisClient) error {
	indexKey := "ha:suppliers:index"

	suppliers, err := client.SMembers(ctx, indexKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get supplier index: %w", err)
	}

	if len(suppliers) == 0 {
		fmt.Printf("No suppliers found in registry\n")
		return nil
	}

	fmt.Printf("Registered Suppliers (%d total):\n\n", len(suppliers))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "SUPPLIER ADDRESS\tKEY\n")

	for _, supplier := range suppliers {
		key := fmt.Sprintf("ha:suppliers:%s", supplier)
		_, _ = fmt.Fprintf(w, "%s\t%s\n", supplier, key)
	}

	_ = w.Flush()

	return nil
}

func inspectSupplier(ctx context.Context, client *redisClient, supplier string) error {
	key := fmt.Sprintf("ha:suppliers:%s", supplier)

	// Check if exists
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check supplier existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("Supplier not found: %s\n", supplier)
		return nil
	}

	// Get all fields
	data, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get supplier data: %w", err)
	}

	fmt.Printf("Supplier: %s\n", supplier)
	fmt.Printf("Redis Key: %s\n\n", key)

	if len(data) == 0 {
		fmt.Printf("No data found\n")
		return nil
	}

	fmt.Printf("Fields:\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "FIELD\tVALUE\n")

	for field, value := range data {
		// Truncate long values
		displayValue := value
		if len(value) > 100 {
			displayValue = value[:100] + "..."
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", field, displayValue)
	}

	_ = w.Flush()

	return nil
}
