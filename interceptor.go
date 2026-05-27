package main

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Gated trace method paths — these require an active subscription
var gatedMethods = map[string]bool{
	"/secret.compute.v1beta1.Query/BlockTraces":        true,
	"/secret.compute.v1beta1.Query/EcallRecord":        true,
	"/secret.compute.v1beta1.Query/EcallRecords":       true,
	"/secret.compute.v1beta1.Query/EncryptedSeed":      true,
	"/secret.compute.v1beta1.Query/MachineIDProof":     true,
	"/secret.compute.v1beta1.Query/NetworkPubkey":      true,
	"/secret.compute.v1beta1.Query/BlockCreateResults": true,
	"/secret.compute.v1beta1.Query/AnalyzeCode":        true,
}

// SubscriptionInterceptor returns a gRPC unary server interceptor that checks
// subscription status for gated methods.
func SubscriptionInterceptor(store *Store) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Skip check for non-gated methods (public queries, billing RPCs)
		if !gatedMethods[info.FullMethod] {
			return handler(ctx, req)
		}

		// Verify signature and extract caller address
		addr, err := VerifyRequest(ctx, info.FullMethod)
		if err != nil {
			log.Printf("[BILLING] Auth failed for %s: %v", info.FullMethod, err)
			return nil, status.Errorf(codes.Unauthenticated,
				"authentication failed: %v", err)
		}

		// Check subscription
		if !store.IsActive(addr) {
			expiry, found := store.GetExpiry(addr)
			msg := fmt.Sprintf("subscription expired or not found for %s", addr)
			if found {
				msg = fmt.Sprintf("subscription expired for %s (expired at unix %d)", addr, expiry)
			}
			log.Printf("[BILLING] Access denied for %s: %s", info.FullMethod, msg)
			return nil, status.Errorf(codes.PermissionDenied,
				"%s — send payment and call AddBalance to renew", msg)
		}

		log.Printf("[BILLING] Access granted for %s → %s", addr, info.FullMethod)
		return handler(ctx, req)
	}
}
