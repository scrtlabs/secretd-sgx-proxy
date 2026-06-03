package main

import (
	"context"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Gated trace method paths — these require an active subscription.
var gatedMethods = map[string]bool{
	"/secret.compute.v1beta1.Query/BlockTraces":        true, // execution traces from the enclave
	"/secret.compute.v1beta1.Query/EcallRecord":        true, // per-block random seed + validator evidence
	"/secret.compute.v1beta1.Query/EcallRecords":       true, // batch range of ecall records
	"/secret.compute.v1beta1.Query/EncryptedSeed":      true, // node-specific encrypted bootstrap seed
	"/secret.compute.v1beta1.Query/MachineIDProof":     true, // SGX machine attestation proof (v1.25+)
	"/secret.compute.v1beta1.Query/NetworkPubkey":      true, // IO + node public keys at a given height
	"/secret.compute.v1beta1.Query/BlockCreateResults": true, // MsgStoreCode results (wasm/code hashes)
	"/secret.compute.v1beta1.Query/AnalyzeCode":        true, // contract feature analysis (proxied to secretd)
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
		if !store.HasBlocks(addr) {
			log.Printf("[BILLING] Access denied for %s: insufficient blocks", info.FullMethod)
			return nil, status.Errorf(codes.PermissionDenied,
				"insufficient blocks — send payment and call AddBalance to renew")
		}

		log.Printf("[BILLING] Access granted for %s → %s", addr, info.FullMethod)
		return handler(ctx, req)
	}
}
