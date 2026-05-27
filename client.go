package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/scrtlabs/secretd-billing/proto/billing"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func newClientCmd() *cobra.Command {
	clientCmd := &cobra.Command{
		Use:   "client",
		Short: "Client commands for interacting with the billing sidecar",
	}

	addBalanceCmd := &cobra.Command{
		Use:   "add-balance",
		Short: "Submit a payment transaction hash to the billing sidecar",
		RunE:  runAddBalance,
	}
	addBalanceCmd.Flags().String("url", "localhost:9191", "gRPC URL of the billing sidecar")
	addBalanceCmd.Flags().String("key", "", "Hex-encoded secp256k1 private key (use --key-file instead for security)")
	addBalanceCmd.Flags().String("key-file", "", "Path to file containing hex-encoded private key")
	addBalanceCmd.Flags().String("tx-hash", "", "Transaction hash of the MsgSend payment")
	addBalanceCmd.MarkFlagRequired("tx-hash")

	checkBalanceCmd := &cobra.Command{
		Use:   "check-balance",
		Short: "Check the subscription balance and expiry",
		RunE:  runCheckBalance,
	}
	checkBalanceCmd.Flags().String("url", "localhost:9191", "gRPC URL of the billing sidecar")
	checkBalanceCmd.Flags().String("key", "", "Hex-encoded secp256k1 private key (use --key-file instead for security)")
	checkBalanceCmd.Flags().String("key-file", "", "Path to file containing hex-encoded private key")

	getInfoCmd := &cobra.Command{
		Use:   "get-info",
		Short: "Get subscription pricing info from the billing sidecar (no auth required)",
		RunE:  runGetInfo,
	}
	getInfoCmd.Flags().String("url", "localhost:9191", "gRPC URL of the billing sidecar")

	clientCmd.AddCommand(addBalanceCmd)
	clientCmd.AddCommand(checkBalanceCmd)
	clientCmd.AddCommand(getInfoCmd)

	return clientCmd
}

// loadPrivateKey resolves the private key from --key or --key-file flags.
func loadPrivateKey(cmd *cobra.Command) ([]byte, error) {
	keyHex, _ := cmd.Flags().GetString("key")
	keyFile, _ := cmd.Flags().GetString("key-file")

	if keyHex == "" && keyFile == "" {
		return nil, fmt.Errorf("either --key or --key-file is required")
	}

	if keyFile != "" {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read key file %s: %w", keyFile, err)
		}
		keyHex = strings.TrimSpace(string(data))
	}

	privKeyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key hex: %w", err)
	}

	return privKeyBytes, nil
}

func runAddBalance(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	txHash, _ := cmd.Flags().GetString("tx-hash")

	privKeyBytes, err := loadPrivateKey(cmd)
	if err != nil {
		return err
	}

	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", url, err)
	}
	defer conn.Close()

	// Generate signature metadata
	md, err := SignRequest(privKeyBytes, "/billing.Billing/AddBalance")
	if err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outCtx := metadata.NewOutgoingContext(ctx, md)

	client := pb.NewBillingClient(conn)
	fmt.Printf("Submitting tx hash %s to %s...\n", txHash, url)

	resp, err := client.AddBalance(outCtx, &pb.AddBalanceRequest{TxHash: txHash})
	if err != nil {
		return fmt.Errorf("AddBalance failed: %w", err)
	}

	fmt.Printf("\nSuccess! Added %d seconds (%.1f minutes) to your subscription.\n", resp.GetSecondsAdded(), float64(resp.GetSecondsAdded())/60.0)
	fmt.Printf("Amount Processed: %d uscrt\n", resp.GetAmountReceived())
	fmt.Printf("New Expiry:       %s (Unix: %d)\n", time.Unix(resp.GetExpiryUnix(), 0).Format(time.RFC1123), resp.GetExpiryUnix())

	return nil
}

func runCheckBalance(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")

	privKeyBytes, err := loadPrivateKey(cmd)
	if err != nil {
		return err
	}

	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", url, err)
	}
	defer conn.Close()

	// Generate signature metadata
	md, err := SignRequest(privKeyBytes, "/billing.Billing/CheckBalance")
	if err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outCtx := metadata.NewOutgoingContext(ctx, md)

	client := pb.NewBillingClient(conn)
	resp, err := client.CheckBalance(outCtx, &pb.CheckBalanceRequest{})
	if err != nil {
		return fmt.Errorf("CheckBalance failed: %w", err)
	}

	if resp.GetActive() {
		fmt.Printf("Status:     ACTIVE\n")
		fmt.Printf("Expiry:     %s\n", time.Unix(resp.GetExpiryUnix(), 0).Format(time.RFC1123))
		minutes := float64(resp.GetRemainingSeconds()) / 60.0
		fmt.Printf("Remaining:  %.1f minutes (%d seconds)\n", minutes, resp.GetRemainingSeconds())
	} else {
		fmt.Printf("Status:     EXPIRED / INACTIVE\n")
		if resp.GetExpiryUnix() > 0 {
			fmt.Printf("Expired at: %s\n", time.Unix(resp.GetExpiryUnix(), 0).Format(time.RFC1123))
		}
	}

	return nil
}

func runGetInfo(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")

	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", url, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pb.NewBillingClient(conn)
	resp, err := client.GetInfo(ctx, &pb.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("GetInfo failed: %w", err)
	}

	fmt.Printf("Operator:   %s\n", resp.GetOperatorAddr())
	fmt.Printf("Base Price: %d uscrt per %d seconds\n", resp.GetPricePerPeriod(), resp.GetPeriodSeconds())

	if resp.GetPricePerPeriod() == 0 {
		fmt.Printf("\nMode: FREE — all endpoints open, no payment required\n")
	} else {
		fmt.Printf("\nPricing Tiers:\n")
		for _, tier := range resp.GetTiers() {
			scrt := float64(tier.GetPriceUscrt()) / 1_000_000.0
			fmt.Printf("  %-10s  %d uscrt (%.2f SCRT)\n", tier.GetLabel(), tier.GetPriceUscrt(), scrt)
		}
	}

	return nil
}
