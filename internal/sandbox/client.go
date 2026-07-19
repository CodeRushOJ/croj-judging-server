package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var ErrClientClosed = errors.New("sandbox client is closed")

const maxBatchMessageBytesV1 = 64 << 20

// Client owns one reusable gRPC connection per ready sandbox endpoint.
type Client struct {
	timeout     time.Duration
	dialOptions []grpc.DialOption
	maxConns    int
	idleTTL     time.Duration

	mu     sync.Mutex
	conns  map[string]*connectionEntry
	closed bool
}

type connectionEntry struct {
	connection *grpc.ClientConn
	lastUsed   time.Time
	inUse      int
}

func NewClient(timeout time.Duration, dialOptions ...grpc.DialOption) *Client {
	return NewClientWithCache(timeout, 128, 5*time.Minute, dialOptions...)
}

func NewClientWithCache(timeout time.Duration, maxConnections int, idleTTL time.Duration, dialOptions ...grpc.DialOption) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxConnections <= 0 {
		maxConnections = 128
	}
	if idleTTL <= 0 {
		idleTTL = 5 * time.Minute
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	options = append(options, dialOptions...)
	return &Client{
		timeout:     timeout,
		dialOptions: options,
		maxConns:    maxConnections,
		idleTTL:     idleTTL,
		conns:       make(map[string]*connectionEntry),
	}
}

func (c *Client) Execute(ctx context.Context, address string, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	if address == "" {
		return nil, fmt.Errorf("sandbox address is required")
	}
	if request == nil {
		return nil, fmt.Errorf("sandbox execute request is required")
	}

	entry, err := c.acquire(address)
	if err != nil {
		return nil, err
	}
	defer c.release(address, entry)
	rpcContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := sandboxpb.NewSandboxServiceClient(entry.connection).Execute(rpcContext, request)
	if err != nil {
		return nil, fmt.Errorf("execute on sandbox %s: %w", address, err)
	}
	return response, nil
}

func batchRPCTimeout(base time.Duration, request *sandboxpb.ExecuteBatchV1Request) time.Duration {
	if request == nil || len(request.Cases) <= 1 {
		return base
	}
	caseTimeoutSeconds := request.Timeout
	if caseTimeoutSeconds <= 0 {
		caseTimeoutSeconds = 1
	}
	if caseTimeoutSeconds > 30 {
		caseTimeoutSeconds = 30
	}
	return base + time.Duration(len(request.Cases)-1)*time.Duration(caseTimeoutSeconds)*time.Second
}

// ExecuteBatch runs one compile-once stream and returns events only after a clean EOF.
// Partial streams are discarded so callers can safely retry the complete batch.
func (c *Client) ExecuteBatch(
	ctx context.Context,
	address string,
	request *sandboxpb.ExecuteBatchV1Request,
) ([]*sandboxpb.ExecuteBatchV1Event, error) {
	if address == "" {
		return nil, fmt.Errorf("sandbox address is required")
	}
	if request == nil {
		return nil, fmt.Errorf("sandbox batch request is required")
	}
	entry, err := c.acquire(address)
	if err != nil {
		return nil, err
	}
	defer c.release(address, entry)
	rpcContext, cancel := context.WithTimeout(ctx, batchRPCTimeout(c.timeout, request))
	defer cancel()
	stream, err := sandboxpb.NewSandboxServiceClient(entry.connection).ExecuteBatchV1(
		rpcContext,
		request,
		grpc.MaxCallSendMsgSize(maxBatchMessageBytesV1),
	)
	if err != nil {
		return nil, fmt.Errorf("start batch on sandbox %s: %w", address, err)
	}
	events := make([]*sandboxpb.ExecuteBatchV1Event, 0, len(request.Cases)+1)
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("receive batch from sandbox %s: %w", address, err)
		}
		events = append(events, event)
	}
}

func (c *Client) acquire(address string) (*connectionEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClientClosed
	}
	now := time.Now()
	c.pruneIdle(now)
	if entry := c.conns[address]; entry != nil {
		entry.inUse++
		entry.lastUsed = now
		return entry, nil
	}
	if len(c.conns) >= c.maxConns {
		c.evictOldestIdle()
	}
	if len(c.conns) >= c.maxConns {
		return nil, fmt.Errorf("sandbox connection cache is full")
	}
	// EndpointSlice returns an already-resolved Pod address. Passthrough avoids
	// sending that address through gRPC's default DNS resolver a second time.
	connection, err := grpc.NewClient("passthrough:///"+address, c.dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("create gRPC client for sandbox %s: %w", address, err)
	}
	entry := &connectionEntry{connection: connection, lastUsed: now, inUse: 1}
	c.conns[address] = entry
	return entry, nil
}

func (c *Client) release(address string, entry *connectionEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns[address] != entry {
		return
	}
	if entry.inUse > 0 {
		entry.inUse--
	}
	entry.lastUsed = time.Now()
	if !c.closed {
		c.pruneIdle(entry.lastUsed)
	}
}

func (c *Client) pruneIdle(now time.Time) {
	for address, entry := range c.conns {
		if entry.inUse == 0 && now.Sub(entry.lastUsed) >= c.idleTTL {
			delete(c.conns, address)
			_ = entry.connection.Close()
		}
	}
}

func (c *Client) evictOldestIdle() {
	var oldestAddress string
	var oldest *connectionEntry
	for address, entry := range c.conns {
		if entry.inUse != 0 {
			continue
		}
		if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
			oldestAddress, oldest = address, entry
		}
	}
	if oldest != nil {
		delete(c.conns, oldestAddress)
		_ = oldest.connection.Close()
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	connections := make([]*grpc.ClientConn, 0, len(c.conns))
	for _, entry := range c.conns {
		connections = append(connections, entry.connection)
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
