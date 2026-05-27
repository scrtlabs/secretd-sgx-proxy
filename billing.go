package main

import (
	"context"
	"log"
	"math"

	pb "github.com/scrtlabs/secretd-billing/proto/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BillingService handles AddBalance, CheckBalance, and GetInfo RPCs.
// Embeds UnimplementedBillingServer for forward compatibility.
type BillingService struct {
	pb.UnimplementedBillingServer

	store          *Store
	rpcURL         string // Tendermint RPC URL for tx verification
	operatorAddr   string // Bech32 address that receives payments
	pricePerPeriod int64  // uscrt per period
	periodSeconds  int64  // how many seconds one period lasts
}

// NewBillingService creates a new billing service
func NewBillingService(store *Store, rpcURL, operatorAddr string, pricePerPeriod, periodSeconds int64) *BillingService {
	return &BillingService{
		store:          store,
		rpcURL:         rpcURL,
		operatorAddr:   operatorAddr,
		pricePerPeriod: pricePerPeriod,
		periodSeconds:  periodSeconds,
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

	durationSec := SecondsFromAmount(amount, bs.pricePerPeriod, bs.periodSeconds)
	if durationSec <= 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"payment of %d uscrt is too small (need at least 1 second of access)", amount)
	}

	if err := bs.store.AddTime(callerAddr, durationSec, req.GetTxHash()); err != nil {
		log.Printf("[BILLING] Failed to add time for %s: %v", callerAddr, err)
		return nil, status.Errorf(codes.AlreadyExists, "failed to add balance: %v", err)
	}

	info := bs.store.GetBalance(callerAddr)
	expiryUnix := int64(0)
	if info != nil {
		expiryUnix = info.ExpiryUnix
	}

	log.Printf("[BILLING] Added %d seconds for %s (amount=%d uscrt, expiry=%d)", durationSec, callerAddr, amount, expiryUnix)

	return &pb.AddBalanceResponse{
		Active:         true,
		ExpiryUnix:     expiryUnix,
		SecondsAdded:   durationSec,
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
			Active:           false,
			ExpiryUnix:       0,
			RemainingSeconds: 0,
		}, nil
	}

	now := currentUnix()
	remaining := info.ExpiryUnix - now
	active := remaining > 0
	if remaining < 0 {
		remaining = 0
	}

	return &pb.CheckBalanceResponse{
		Active:           active,
		ExpiryUnix:       info.ExpiryUnix,
		RemainingSeconds: remaining,
	}, nil
}

func currentUnix() int64 {
	return timeNow().Unix()
}

// SecondsFromAmount calculates how many seconds of access a payment buys.
// Formula: (amount * periodSeconds) / pricePerPeriod — proportional.
func SecondsFromAmount(amount, pricePerPeriod, periodSeconds int64) int64 {
	if pricePerPeriod <= 0 {
		return math.MaxInt64
	}
	
	// Prevent int64 overflow by calculating full periods first
	fullPeriods := amount / pricePerPeriod
	remainder := amount % pricePerPeriod
	
	return (fullPeriods * periodSeconds) + ((remainder * periodSeconds) / pricePerPeriod)
}

// GetInfo returns operator info, pricing, and pre-calculated cost tiers.
// No authentication required.
func (bs *BillingService) GetInfo(ctx context.Context, req *pb.GetInfoRequest) (*pb.GetInfoResponse, error) {
	// Standard periods
	type period struct {
		label   string
		seconds int64
	}
	periods := []period{
		{"1 day", 86400},
		{"1 week", 604800},
		{"1 month", 2592000}, // 30 days
	}

	var tiers []*pb.PriceTier
	for _, p := range periods {
		cost := int64(0)
		if bs.pricePerPeriod > 0 && bs.periodSeconds > 0 {
			cost = (p.seconds * bs.pricePerPeriod) / bs.periodSeconds
		}
		tiers = append(tiers, &pb.PriceTier{
			Label:      p.label,
			Seconds:    p.seconds,
			PriceUscrt: cost,
		})
	}

	return &pb.GetInfoResponse{
		OperatorAddr:   bs.operatorAddr,
		PricePerPeriod: bs.pricePerPeriod,
		PeriodSeconds:  bs.periodSeconds,
		Tiers:          tiers,
	}, nil
}
