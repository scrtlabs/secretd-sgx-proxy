package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	pb "github.com/scrtlabs/secretd-sgx-proxy/proto/billing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func init() {
	// Override the default "proto" codec with our proxy codec.
	// This is the standard pattern from mwitkow/grpc-proxy:
	// check for *rawFrame (transparent proxy), delegate everything
	// else to the real proto codec. Since all billing message types
	// are now protoc-generated and implement proto.Message, the
	// fallback path is always safe.
	encoding.RegisterCodec(proxyCodec{})
}

// ProxyServer wraps a gRPC server that transparently proxies requests to a backend
type ProxyServer struct {
	backendConn *grpc.ClientConn
	server      *grpc.Server
	store       *Store // subscription store for auth checks in transparent proxy
	freeMode    bool   // when true, skip all auth/subscription gating (price=0)
}

// NewProxyServer creates a gRPC server that:
// 1. Registers the BillingService (AddBalance, CheckBalance, GetInfo) as a known service
// 2. For all other unregistered methods → transparent proxy to backend
// 3. Auth + subscription checks happen inside the transparent handler for gated methods
//
//	(grpc.UnaryInterceptor does NOT fire for UnknownServiceHandler — the check must be inline)
func NewProxyServer(backendAddr string, interceptor grpc.UnaryServerInterceptor, billing *BillingService, store *Store, freeMode ...bool) (*ProxyServer, error) {
	// Connect to backend (non-blocking, connection established on first RPC)
	conn, err := grpc.NewClient(
		backendAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(50*1024*1024)), // 50MB for large trace responses
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}

	free := len(freeMode) > 0 && freeMode[0]
	ps := &ProxyServer{backendConn: conn, store: store, freeMode: free}

	// Create gRPC server with interceptor and unknown service handler
	ps.server = grpc.NewServer(
		grpc.UnaryInterceptor(interceptor),
		grpc.UnknownServiceHandler(ps.transparentHandler),
		grpc.MaxRecvMsgSize(50*1024*1024), // 50MB
		grpc.MaxSendMsgSize(50*1024*1024),
	)

	// Register the billing service using the generated service descriptor
	pb.RegisterBillingServer(ps.server, billing)

	return ps, nil
}

// Server returns the underlying gRPC server for starting
func (ps *ProxyServer) Server() *grpc.Server {
	return ps.server
}

// Close cleans up the backend connection
func (ps *ProxyServer) Close() error {
	return ps.backendConn.Close()
}

// transparentHandler forwards unknown gRPC methods to the backend.
// This handles both unary and streaming calls.
// IMPORTANT: grpc.UnaryInterceptor does NOT fire for UnknownServiceHandler,
// so we must check auth + subscription inline here for gated methods.
func (ps *ProxyServer) transparentHandler(srv interface{}, serverStream grpc.ServerStream) error {
	// Extract the full method from the stream context
	fullMethod, ok := grpc.MethodFromServerStream(serverStream)
	if !ok {
		return status.Error(codes.Internal, "could not determine method")
	}

	// --- Subscription gate (inline, since interceptors don't fire here) ---
	var addr string
	if !ps.freeMode && gatedMethods[fullMethod] {
		var err error
		addr, err = VerifyRequest(serverStream.Context(), fullMethod)
		if err != nil {
			log.Printf("[BILLING] Auth failed for %s: %v", fullMethod, err)
			return status.Errorf(codes.Unauthenticated, "authentication failed: %v", err)
		}

		if !ps.store.HasBlocks(addr) {
			log.Printf("[BILLING] Access denied for %s: insufficient blocks", fullMethod)
			return status.Errorf(codes.PermissionDenied,
				"insufficient blocks — send payment and call AddBalance to renew")
		}

		log.Printf("[BILLING] Auth granted for %s → %s", addr, fullMethod)
	}
	// Forward incoming metadata to backend
	md, _ := metadata.FromIncomingContext(serverStream.Context())
	outCtx := metadata.NewOutgoingContext(context.Background(), md)

	// Use a fully generic stream descriptor to support both unary and streaming RPCs
	desc := &grpc.StreamDesc{
		ServerStreams: true,
		ClientStreams: true,
	}

	clientStream, err := ps.backendConn.NewStream(outCtx, desc, fullMethod)
	if err != nil {
		log.Printf("[PROXY] Failed to create backend stream for %s: %v", fullMethod, err)
		return err
	}

	// Forward requests from client to backend
	errChan := make(chan error, 1)
	go func() {
		isFirstFrame := true
		for {
			var frame rawFrame
			if err := serverStream.RecvMsg(&frame); err != nil {
				clientStream.CloseSend() // always signal EOF to backend
				if err == io.EOF {
					errChan <- nil
				} else {
					errChan <- err
				}
				return
			}

			// Perform block deduction on the first frame payload for gated methods
			if isFirstFrame && frame.payload != nil && !ps.freeMode && gatedMethods[fullMethod] {
				isFirstFrame = false
				if err := processBillingDeduction(ps.store, addr, fullMethod, frame.payload); err != nil {
					clientStream.CloseSend()
					errChan <- status.Errorf(codes.ResourceExhausted, "billing deduction failed: %v", err)
					if frame.payload != nil {
						putBuffer(frame.payload)
					}
					return
				}
			}

			if err := clientStream.SendMsg(&frame); err != nil {
				if frame.payload != nil {
					putBuffer(frame.payload)
				}
				errChan <- err
				return
			}
			if frame.payload != nil {
				putBuffer(frame.payload)
			}
		}
	}()

	// Forward responses from backend to client
	var forwardErr error
	headerSent := false
	for {
		var respFrame rawFrame
		if err := clientStream.RecvMsg(&respFrame); err != nil {
			if err == io.EOF {
				break
			}
			forwardErr = err
			break
		}

		if !headerSent {
			header, err := clientStream.Header()
			if err == nil && len(header) > 0 {
				serverStream.SetHeader(header)
			}
			headerSent = true
		}

		if err := serverStream.SendMsg(&respFrame); err != nil {
			if respFrame.payload != nil {
				putBuffer(respFrame.payload)
			}
			forwardErr = err
			break
		}
		if respFrame.payload != nil {
			putBuffer(respFrame.payload)
		}
	}

	// Wait for the client-to-backend copy to finish
	reqErr := <-errChan

	// Forward trailers
	serverStream.SetTrailer(clientStream.Trailer())

	if forwardErr != nil {
		return forwardErr
	}
	return reqErr
}

// --- Buffer Pool for zero-allocation proxying ---

var framePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

func getBuffer(size int) []byte {
	bPtr := framePool.Get().(*[]byte)
	if cap(*bPtr) < size {
		newB := make([]byte, size)
		return newB
	}
	return (*bPtr)[:size]
}

func putBuffer(b []byte) {
	if cap(b) <= 1024*1024 { // Don't pool huge buffers > 1MB
		framePool.Put(&b)
	}
}

// --- Codec ---

// rawFrame is a raw byte frame that bypasses protobuf marshaling.
// This allows transparent proxying without knowing the proto schema.
type rawFrame struct {
	payload []byte
}

// proxyCodec implements grpc/encoding.Codec.
// It handles *rawFrame as raw bytes for transparent proxying,
// and delegates everything else to the standard proto codec.
// This is the established pattern from mwitkow/grpc-proxy.
type proxyCodec struct{}

func (proxyCodec) Name() string {
	return "proto"
}

func (proxyCodec) Marshal(v interface{}) ([]byte, error) {
	if f, ok := v.(*rawFrame); ok {
		return f.payload, nil
	}
	return proto.Marshal(v.(proto.Message))
}

func (proxyCodec) Unmarshal(data []byte, v interface{}) error {
	if f, ok := v.(*rawFrame); ok {
		f.payload = getBuffer(len(data))
		copy(f.payload, data)
		return nil
	}
	return proto.Unmarshal(data, v.(proto.Message))
}

// recvReqFromStream reads the incoming gRPC request as raw bytes.
// Returns an owned copy safe to use after this call.
func recvReqFromStream(stream grpc.ServerStream) ([]byte, error) {
	var frame rawFrame
	if err := stream.RecvMsg(&frame); err != nil {
		return nil, err
	}
	data := make([]byte, len(frame.payload))
	copy(data, frame.payload)
	if frame.payload != nil {
		putBuffer(frame.payload)
	}
	return data, nil
}

// processBillingDeduction extracts the height from the payload and deducts block credits.
func processBillingDeduction(store *Store, addr, fullMethod string, payload []byte) error {
	switch fullMethod {
	case "/secret.compute.v1beta1.Query/BlockTraces",
		"/secret.compute.v1beta1.Query/EcallRecord",
		"/secret.compute.v1beta1.Query/NetworkPubkey",
		"/secret.compute.v1beta1.Query/BlockCreateResults",
		"/secret.compute.v1beta1.Query/MachineIDProof":
		height, err := extractHeight(payload, 1)
		if err != nil {
			return fmt.Errorf("failed to extract height for %s: %w", fullMethod, err)
		}
		_, err = store.ConsumeHeight(addr, height)
		return err

	case "/secret.compute.v1beta1.Query/EncryptedSeed":
		height, err := extractHeight(payload, 2)
		if err != nil {
			return fmt.Errorf("failed to extract height for %s: %w", fullMethod, err)
		}
		_, err = store.ConsumeHeight(addr, height)
		return err

	case "/secret.compute.v1beta1.Query/EcallRecords":
		start, end, err := extractHeightRange(payload)
		if err != nil {
			return fmt.Errorf("failed to extract height range for %s: %w", fullMethod, err)
		}
		_, err = store.ConsumeHeightRange(addr, start, end)
		return err

	case "/secret.compute.v1beta1.Query/AnalyzeCode":
		// No block height, just deduct 1 generic block
		return store.ConsumeGeneric(addr)

	default:
		// Fallback for any other gated method
		return store.ConsumeGeneric(addr)
	}
}

// extractHeight parses a simple protobuf message to extract an int64 field.
func extractHeight(payload []byte, fieldNum protowire.Number) (int64, error) {
	b := payload
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, fmt.Errorf("invalid tag")
		}
		b = b[n:]
		
		if num == fieldNum && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, fmt.Errorf("invalid varint")
			}
			return int64(v), nil
		}
		
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0, fmt.Errorf("invalid field value")
		}
		b = b[m:]
	}
	return 0, fmt.Errorf("height field not found in payload")
}

// extractHeightRange parses start_height (field 1) and end_height (field 2).
func extractHeightRange(payload []byte) (start int64, end int64, err error) {
	b := payload
	foundStart, foundEnd := false, false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, 0, fmt.Errorf("invalid tag")
		}
		b = b[n:]
		
		if num == 1 && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, 0, fmt.Errorf("invalid varint")
			}
			start = int64(v)
			foundStart = true
			b = b[n:]
			continue
		}
		if num == 2 && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, 0, fmt.Errorf("invalid varint")
			}
			end = int64(v)
			foundEnd = true
			b = b[n:]
			continue
		}
		
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0, 0, fmt.Errorf("invalid field value")
		}
		b = b[m:]
	}
	
	if !foundStart || !foundEnd {
		return 0, 0, fmt.Errorf("missing start or end height")
	}
	return start, end, nil
}
