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

func newBufconnClient(
	t *testing.T,
	timeout time.Duration,
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
	client := NewClient(timeout, grpc.WithContextDialer(dialer))
	return client, func() {
		_ = client.Close()
		server.Stop()
		_ = listener.Close()
	}
}
