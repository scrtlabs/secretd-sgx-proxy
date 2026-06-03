package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/scrtlabs/secretd-sgx-proxy/proto/billing"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
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

	fmt.Printf("\nSuccess! Added %d blocks to your subscription.\n", resp.GetBlocksAdded())
	fmt.Printf("Amount Processed: %d uscrt\n", resp.GetAmountReceived())
	fmt.Printf("Total Blocks Remaining: %d\n", resp.GetBlocksRemaining())

	return nil
}

func runCheckBalance(cmd *cobra.Command, args []string) error {
	privKeyBytes, err := loadPrivateKey(cmd)
	if err != nil {
		return err
	}

	privKey := secp256k1.PrivKeyFromBytes(privKeyBytes)
	pubKey := privKey.PubKey()
	addr, err := PubKeyToBech32(pubKey)
	if err != nil {
		return fmt.Errorf("failed to derive address: %w", err)
	}

	url, _ := cmd.Flags().GetString("url")
	conn, err := grpc.NewClient(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to proxy: %w", err)
	}
	defer conn.Close()

	client := pb.NewBillingClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	md, err := SignRequest(privKeyBytes, "/billing.Billing/CheckBalance")
	if err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}
	ctx = metadata.NewOutgoingContext(ctx, md)

	resp, err := client.CheckBalance(ctx, &pb.CheckBalanceRequest{})
	if err != nil {
		return fmt.Errorf("check balance failed: %w", err)
	}

	fmt.Printf("Address:         %s\n", addr)
	fmt.Printf("Blocks Remaining:%d\n", resp.GetBlocksRemaining())
	if resp.GetBlocksRemaining() <= 0 {
		fmt.Printf("Status:          EXHAUSTED (buy more blocks)\n")
	} else {
		fmt.Printf("Status:          ACTIVE\n")
	}

	return nil
}

func runGetInfo(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	conn, err := grpc.NewClient(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to proxy: %w", err)
	}
	defer conn.Close()

	client := pb.NewBillingClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.GetInfo(ctx, &pb.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("get info failed: %w", err)
	}

	fmt.Printf("Operator Address:   %s\n", resp.GetOperatorAddr())
	fmt.Printf("Price Per Package:  %d uscrt\n", resp.GetPricePerPackage())
	fmt.Printf("Blocks Per Package: %d blocks\n", resp.GetBlocksPerPackage())

	return nil
}
