package sandbox

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type sandboxTestServer struct {
	sandboxpb.UnimplementedSandboxServiceServer
	execute func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error)
}

func (s *sandboxTestServer) Execute(ctx context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	return s.execute(ctx, request)
}

func TestClientExecutesOverGRPCAndReusesConnection(t *testing.T) {
	var dials atomic.Int32
	client, stop := newBufconnClient(t, 500*time.Millisecond, &dials, func(_ context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		return &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: request.SourceCode}, nil
	})
	defer stop()

	for range 2 {
		response, err := client.Execute(context.Background(), "sandbox.test:50051", &sandboxpb.ExecuteRequest{SourceCode: "package main"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if response.Status != "Accepted" || response.Stdout != "package main" {
			t.Fatalf("unexpected response: %+v", response)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
}

func TestClientAppliesRPCDeadline(t *testing.T) {
	client, stop := newBufconnClient(t, 20*time.Millisecond, nil, func(ctx context.Context, _ *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		<-ctx.Done()
		return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
	})
	defer stop()

	_, err := client.Execute(context.Background(), "sandbox.test:50051", &sandboxpb.ExecuteRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Execute error = %v, want DeadlineExceeded", err)
	}
}

func TestClientPreservesRetryableGRPCFailureCode(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.ResourceExhausted} {
		t.Run(code.String(), func(t *testing.T) {
			client, stop := newBufconnClient(t, time.Second, nil, func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
				return nil, status.Error(code, "retry later")
			})
			defer stop()

			_, err := client.Execute(context.Background(), "sandbox.test:50051", &sandboxpb.ExecuteRequest{})
			if status.Code(err) != code {
				t.Fatalf("Execute error = %v, want %s", err, code)
			}
		})
	}
}

func TestClientCloseRejectsNewExecutions(t *testing.T) {
	client, stop := newBufconnClient(t, time.Second, nil, func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		return &sandboxpb.ExecuteResponse{Status: "Accepted"}, nil
	})
	defer stop()

	if _, err := client.Execute(context.Background(), "sandbox.test:50051", &sandboxpb.ExecuteRequest{}); err != nil {
		t.Fatalf("initial Execute: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := client.Execute(context.Background(), "sandbox.test:50051", &sandboxpb.ExecuteRequest{}); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Execute after Close error = %v, want ErrClientClosed", err)
	}
}

func TestClientBoundsAndExpiresEndpointConnections(t *testing.T) {
	const idleTTL = time.Hour
	client, stop := newBufconnClientWithLimits(t, time.Second, 2, idleTTL, nil, func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		return &sandboxpb.ExecuteResponse{Status: "Accepted"}, nil
	})
	defer stop()
	for _, address := range []string{"10.0.0.1:50051", "10.0.0.2:50051", "10.0.0.3:50051"} {
		if _, err := client.Execute(context.Background(), address, &sandboxpb.ExecuteRequest{}); err != nil {
			t.Fatalf("Execute %s: %v", address, err)
		}
	}
	client.mu.Lock()
	if got := len(client.conns); got != 2 {
		client.mu.Unlock()
		t.Fatalf("connection cache length = %d, want 2", got)
	}
	client.mu.Unlock()

	client.mu.Lock()
	for _, entry := range client.conns {
		entry.lastUsed = time.Now().Add(-idleTTL)
	}
	client.mu.Unlock()
	if _, err := client.Execute(context.Background(), "10.0.0.4:50051", &sandboxpb.ExecuteRequest{}); err != nil {
		t.Fatalf("Execute after idle expiry: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := len(client.conns); got != 1 {
		t.Fatalf("connection cache length after expiry = %d, want 1", got)
	}
}

func newBufconnClient(
	t *testing.T,
	timeout time.Duration,
	dials *atomic.Int32,
	execute func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error),
) (*Client, func()) {
	return newBufconnClientWithLimits(t, timeout, 128, 5*time.Minute, dials, execute)
}

func newBufconnClientWithLimits(
	t *testing.T,
	timeout time.Duration,
	maxConnections int,
	idleTTL time.Duration,
	dials *atomic.Int32,
	execute func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error),
) (*Client, func()) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	sandboxpb.RegisterSandboxServiceServer(server, &sandboxTestServer{execute: execute})
	go func() {
		_ = server.Serve(listener)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		if dials != nil {
			dials.Add(1)
		}
		return listener.Dial()
	}
	client := NewClientWithCache(timeout, maxConnections, idleTTL, grpc.WithContextDialer(dialer))
	return client, func() {
		_ = client.Close()
		server.Stop()
		_ = listener.Close()
	}
}
