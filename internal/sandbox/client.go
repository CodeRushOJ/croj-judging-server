package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var ErrClientClosed = errors.New("sandbox client is closed")

// Client owns one reusable gRPC connection per ready sandbox endpoint.
type Client struct {
	timeout     time.Duration
	dialOptions []grpc.DialOption

	mu     sync.Mutex
	conns  map[string]*grpc.ClientConn
	closed bool
}

func NewClient(timeout time.Duration, dialOptions ...grpc.DialOption) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	options = append(options, dialOptions...)
	return &Client{
		timeout:     timeout,
		dialOptions: options,
		conns:       make(map[string]*grpc.ClientConn),
	}
}

func (c *Client) Execute(ctx context.Context, address string, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	if address == "" {
		return nil, fmt.Errorf("sandbox address is required")
	}
	if request == nil {
		return nil, fmt.Errorf("sandbox execute request is required")
	}

	connection, err := c.connection(address)
	if err != nil {
		return nil, err
	}
	rpcContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := sandboxpb.NewSandboxServiceClient(connection).Execute(rpcContext, request)
	if err != nil {
		return nil, fmt.Errorf("execute on sandbox %s: %w", address, err)
	}
	return response, nil
}

func (c *Client) connection(address string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClientClosed
	}
	if connection := c.conns[address]; connection != nil {
		return connection, nil
	}
	// EndpointSlice returns an already-resolved Pod address. Passthrough avoids
	// sending that address through gRPC's default DNS resolver a second time.
	connection, err := grpc.NewClient("passthrough:///"+address, c.dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("create gRPC client for sandbox %s: %w", address, err)
	}
	c.conns[address] = connection
	return connection, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	connections := make([]*grpc.ClientConn, 0, len(c.conns))
	for _, connection := range c.conns {
		connections = append(connections, connection)
	}
	c.conns = nil
	c.mu.Unlock()

	var closeErrors []error
	for _, connection := range connections {
		if err := connection.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}
