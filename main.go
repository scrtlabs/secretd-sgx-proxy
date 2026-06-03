package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "secretd-sgx-proxy",
		Short: "Subscription sidecar proxy for gating non-SGX trace endpoints",
		Long: `secretd-sgx-proxy is a lightweight sidecar proxy that sits in front of an SGX
validator's gRPC port. It gates trace endpoints behind block-consumption subscriptions.

Non-SGX operators send a MsgSend on-chain, register the tx hash via AddBalance,
and get access for the paid number of blocks. The sidecar verifies secp256k1 signatures
on every request, deduplicates requests per height, and transparently forwards authorized calls to the validator.`,
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the billing proxy server",
		RunE:  runServe,
	}

	// Flags
	serveCmd.Flags().String("listen", ":9191", "Address to listen on")
	serveCmd.Flags().String("backend", "localhost:9090", "Backend validator gRPC address")
	serveCmd.Flags().String("rpc", "http://localhost:26657", "Tendermint RPC URL for tx verification")
	serveCmd.Flags().String("operator", "", "Operator bech32 address (payment recipient)")
	serveCmd.Flags().Int64("price", 1000000, "Price per package in uscrt (default: 1 SCRT)")
	serveCmd.Flags().Int64("blocks", 100000, "Number of blocks granted per package (default: 100,000 blocks)")
	serveCmd.Flags().String("db-path", "./subscriptions.db", "Path to subscription LevelDB")

	serveCmd.MarkFlagRequired("operator")

	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(newClientCmd())

	if err := rootCmd.Execute(); err != nil {

		os.Exit(1)
	}
}

func runServe(cmd *cobra.Command, args []string) error {
	listen, _ := cmd.Flags().GetString("listen")
	backend, _ := cmd.Flags().GetString("backend")
	rpcURL, _ := cmd.Flags().GetString("rpc")
	operator, _ := cmd.Flags().GetString("operator")
	price, _ := cmd.Flags().GetInt64("price")
	blocksPerPackage, _ := cmd.Flags().GetInt64("blocks")
	dbPath, _ := cmd.Flags().GetString("db-path")

	log.Printf("[BILLING] Starting secretd-sgx-proxy")
	log.Printf("[BILLING]   Listen:   %s", listen)
	log.Printf("[BILLING]   Backend:  %s", backend)
	log.Printf("[BILLING]   RPC:      %s", rpcURL)
	log.Printf("[BILLING]   Operator: %s", operator)
	log.Printf("[BILLING]   Price:    %d uscrt per %d blocks", price, blocksPerPackage)
	log.Printf("[BILLING]   DB:       %s", dbPath)

	freeMode := price <= 0
	if freeMode {
		log.Printf("[BILLING]   Mode:     FREE (all endpoints open, no auth required)")
	} else {
		log.Printf("[BILLING]   Mode:     PAID")
	}

	// Open subscription store
	store, err := NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer store.Close()

	// Create billing service
	billing := NewBillingService(store, rpcURL, operator, price, blocksPerPackage)

	// Create interceptor
	interceptor := SubscriptionInterceptor(store)

	// Create proxy server
	proxy, err := NewProxyServer(backend, interceptor, billing, store, freeMode)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}
	defer proxy.Close()

	// Start listening
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", listen, err)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("[BILLING] Received signal %v, shutting down...", sig)
		proxy.Server().GracefulStop()
	}()

	log.Printf("[BILLING] Proxy server listening on %s → %s", listen, backend)
	return proxy.Server().Serve(lis)
}
