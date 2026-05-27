package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockQueryServer is the interface required by grpc.ServiceDesc.HandlerType
type mockQueryServer interface{}

// mockBackend implements mockQueryServer
type mockBackend struct{}

func startMockBackend(t *testing.T) (string, func()) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen for mock backend: %v", err)
	}

	server := grpc.NewServer()

	// Register a mock service for secret.compute.v1beta1.Query
	sd := grpc.ServiceDesc{
		ServiceName: "secret.compute.v1beta1.Query",
		HandlerType: (*mockQueryServer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "BlockTraces", // Gated
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					return &rawFrame{payload: []byte("traces-from-backend")}, nil
				},
			},
			{
				MethodName: "PublicQuery", // Not Gated
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					return &rawFrame{payload: []byte("public-from-backend")}, nil
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "test.proto",
	}

	server.RegisterService(&sd, &mockBackend{})

	go func() {
		_ = server.Serve(lis)
	}()

	return lis.Addr().String(), func() {
		server.Stop()
		_ = lis.Close()
	}
}

func TestProxyServer_GatingAndRouting(t *testing.T) {
	// 1. Start mock backend validator
	backendAddr, stopBackend := startMockBackend(t)
	defer stopBackend()

	// 2. Setup database store
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_proxy.db")
	store, _ := NewStore(dbPath)
	defer store.Close()

	// Mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	// 3. Create billing service and proxy server
	pricePerPeriod := int64(1000000)
	periodSeconds := int64(3600)
	billing := NewBillingService(store, "http://localhost:26657", "operator_addr", pricePerPeriod, periodSeconds)
	interceptor := SubscriptionInterceptor(store)

	proxy, err := NewProxyServer(backendAddr, interceptor, billing, store)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	// Start proxy listening on a random port
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen for proxy: %v", err)
	}
	defer proxyLis.Close()

	go func() {
		_ = proxy.Server().Serve(proxyLis)
	}()
	defer proxy.Server().Stop()

	// 4. Setup client connection to proxy
	conn, err := grpc.Dial(proxyLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	// Generate client credentials
	privKey, _ := secp256k1.GeneratePrivateKey()
	privKeyBytes := privKey.Serialize()
	callerAddr, _ := PubKeyToBech32(privKey.PubKey())

	// ---- Test Case 1: Call non-gated method (should succeed without credentials) ----
	req := &rawFrame{payload: []byte("public-req")}
	resp := &rawFrame{}
	err = conn.Invoke(context.Background(), "/secret.compute.v1beta1.Query/PublicQuery", req, resp)
	if err != nil {
		t.Errorf("public query failed: %v", err)
	}
	if string(resp.payload) != "public-from-backend" {
		t.Errorf("expected 'public-from-backend', got %s", string(resp.payload))
	}

	// ---- Test Case 2: Call gated method without credentials (should fail with Unauthenticated) ----
	err = conn.Invoke(context.Background(), "/secret.compute.v1beta1.Query/BlockTraces", req, resp)
	if err == nil {
		t.Error("expected gated query without signature metadata to fail, but it succeeded")
	} else {
		st, _ := status.FromError(err)
		if st.Code() != codes.Unauthenticated {
			t.Errorf("expected code Unauthenticated, got %v", st.Code())
		}
	}

	// ---- Test Case 3: Call gated method with credentials but no active subscription (should fail with PermissionDenied) ----
	md, _ := SignRequest(privKeyBytes, "/secret.compute.v1beta1.Query/BlockTraces")
	ctxWithAuth := metadata.NewOutgoingContext(context.Background(), md)

	err = conn.Invoke(ctxWithAuth, "/secret.compute.v1beta1.Query/BlockTraces", req, resp)
	if err == nil {
		t.Error("expected gated query with inactive subscription to fail, but it succeeded")
	} else {
		st, _ := status.FromError(err)
		if st.Code() != codes.PermissionDenied {
			t.Errorf("expected code PermissionDenied, got %v", st.Code())
		}
	}

	// ---- Test Case 4: Call gated method with valid subscription (should succeed and forward) ----
	// Direct injection of balance into store
	_ = store.AddTime(callerAddr, 1000, "TX_HASH")

	err = conn.Invoke(ctxWithAuth, "/secret.compute.v1beta1.Query/BlockTraces", req, resp)
	if err != nil {
		t.Errorf("gated query with active subscription failed: %v", err)
	}
	if string(resp.payload) != "traces-from-backend" {
		t.Errorf("expected 'traces-from-backend', got %s", string(resp.payload))
	}
}
