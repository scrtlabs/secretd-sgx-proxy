package main

import (
	"context"
	"log"
	"math"

	pb "github.com/scrtlabs/secretd-sgx-proxy/proto/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BillingService handles AddBalance, CheckBalance, and GetInfo RPCs.
// Embeds UnimplementedBillingServer for forward compatibility.
type BillingService struct {
	pb.UnimplementedBillingServer

	store            *Store
	rpcURL           string // Tendermint RPC URL for tx verification
	operatorAddr     string // Bech32 address that receives payments
	pricePerPackage  int64  // uscrt per package
	blocksPerPackage int64  // how many blocks in one package
}

// NewBillingService creates a new billing service
func NewBillingService(store *Store, rpcURL, operatorAddr string, pricePerPackage, blocksPerPackage int64) *BillingService {
	return &BillingService{
		store:            store,
		rpcURL:           rpcURL,
		operatorAddr:     operatorAddr,
		pricePerPackage:  pricePerPackage,
		blocksPerPackage: blocksPerPackage,
	}
}

// AddBalance verifies a payment tx hash and extends the caller's subscription
func (bs *BillingService) AddBalance(ctx context.Context, req *pb.AddBalanceRequest) (*pb.AddBalanceResponse, error) {
	callerAddr, err := VerifyRequest(ctx, "/billing.Billing/AddBalance")
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "auth failed: %v", err)
	}

	if req.GetTxHash() == "" {
		return nil, status.Error(codes.InvalidArgument, "tx_hash is required")
	}

	log.Printf("[BILLING] AddBalance request from %s, txHash=%s", callerAddr, req.GetTxHash())

	// Verify payment on-chain
	amount, sender, err := verifyPayment(bs.rpcURL, req.GetTxHash(), bs.operatorAddr, 0)
	if err != nil {
		log.Printf("[BILLING] Payment verification failed for %s: %v", req.GetTxHash(), err)
		return nil, status.Errorf(codes.FailedPrecondition, "payment verification failed: %v", err)
	}

	if sender != callerAddr {
		return nil, status.Errorf(codes.PermissionDenied,
			"payment sender %s does not match authenticated caller %s", sender, callerAddr)
	}

	blocksGranted := BlocksFromAmount(amount, bs.pricePerPackage, bs.blocksPerPackage)
	if blocksGranted <= 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"payment of %d uscrt is too small (need at least 1 block of access)", amount)
	}

	if err := bs.store.AddBlocks(callerAddr, blocksGranted, req.GetTxHash()); err != nil {
		log.Printf("[BILLING] Failed to add blocks for %s: %v", callerAddr, err)
		return nil, status.Errorf(codes.AlreadyExists, "failed to add balance: %v", err)
	}

	info := bs.store.GetBalance(callerAddr)
	blocksRemaining := int64(0)
	if info != nil {
		blocksRemaining = info.BlocksRemaining
	}

	log.Printf("[BILLING] Added %d blocks for %s (amount=%d uscrt, total_remaining=%d)", blocksGranted, callerAddr, amount, blocksRemaining)

	return &pb.AddBalanceResponse{
		Active:         blocksRemaining > 0,
		BlocksRemaining: blocksRemaining,
		BlocksAdded:    blocksGranted,
		AmountReceived: amount,
	}, nil
}

// CheckBalance returns the subscription status for the authenticated caller
func (bs *BillingService) CheckBalance(ctx context.Context, req *pb.CheckBalanceRequest) (*pb.CheckBalanceResponse, error) {
	callerAddr, err := VerifyRequest(ctx, "/billing.Billing/CheckBalance")
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "auth failed: %v", err)
	}

	info := bs.store.GetBalance(callerAddr)
	if info == nil {
		return &pb.CheckBalanceResponse{
			Active:          false,
			BlocksRemaining: 0,
		}, nil
	}

	return &pb.CheckBalanceResponse{
		Active:          info.BlocksRemaining > 0,
		BlocksRemaining: info.BlocksRemaining,
	}, nil
}

// BlocksFromAmount calculates how many blocks of access a payment buys.
// Formula: (amount * blocksPerPackage) / pricePerPackage
func BlocksFromAmount(amount, pricePerPackage, blocksPerPackage int64) int64 {
	if pricePerPackage <= 0 {
		return math.MaxInt64
	}
	
	// Prevent int64 overflow by calculating full packages first
	fullPackages := amount / pricePerPackage
	remainder := amount % pricePerPackage
	
	return (fullPackages * blocksPerPackage) + ((remainder * blocksPerPackage) / pricePerPackage)
}

// GetInfo returns operator info, pricing, and pre-calculated cost tiers.
// No authentication required.
func (bs *BillingService) GetInfo(ctx context.Context, req *pb.GetInfoRequest) (*pb.GetInfoResponse, error) {
	// Duration-based tiers (~6 seconds per block)
	type pkg struct {
		label  string
		blocks int64
	}
	packages := []pkg{
		{"1 day (~14,400 blocks)", 14400},
		{"1 week (~100,800 blocks)", 100800},
		{"1 month (~432,000 blocks)", 432000},
	}

	var tiers []*pb.PriceTier
	for _, p := range packages {
		cost := int64(0)
		if bs.pricePerPackage > 0 && bs.blocksPerPackage > 0 {
			cost = (p.blocks * bs.pricePerPackage) / bs.blocksPerPackage
		}
		tiers = append(tiers, &pb.PriceTier{
			Label:      p.label,
			Blocks:     p.blocks,
			PriceUscrt: cost,
		})
	}

	return &pb.GetInfoResponse{
		OperatorAddr:     bs.operatorAddr,
		PricePerPackage:  bs.pricePerPackage,
		BlocksPerPackage: bs.blocksPerPackage,
		Tiers:            tiers,
	}, nil
}
