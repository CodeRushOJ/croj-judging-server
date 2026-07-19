package external

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"
	"sync"
	"time"
)

const (
	defaultWebhookLeaseDuration     = time.Minute
	defaultWebhookRequestTimeout    = 30 * time.Second
	defaultWebhookDialTimeout       = 5 * time.Second
	defaultWebhookBaseRetryDelay    = 5 * time.Second
	defaultWebhookMaximumRetryDelay = 15 * time.Minute
	defaultWebhookMaximumAttempts   = 12
	defaultWebhookIdleDelay         = time.Second
	defaultWebhookTerminalRetention = 30 * 24 * time.Hour
	defaultWebhookAuthorityLimit    = 64
	maximumWebhookAuthorityLimit    = 1024
	webhookRetentionSweepInterval   = time.Hour
	webhookRetentionSweepBatch      = 100
	webhookRetentionSweepBudget     = 100
)

var ErrWebhookWorkerClosed = errors.New("webhook worker is closed")

const WebhookErrorDeliveryExpired = "delivery_expired"

// WebhookOutboxRepository is the durable boundary used by WebhookWorker. Its
// implementation owns database time, leasing, fencing, and state transitions.
type WebhookOutboxRepository interface {
	ClaimNextWebhook(context.Context, string, time.Duration) (WebhookClaim, error)
	SettleWebhook(context.Context, WebhookClaim, WebhookSettlement) error
	SweepTerminal(context.Context, time.Duration, int) (int64, error)
}

type webhookOutboxRepository = WebhookOutboxRepository

type callbackSecretDecryptor interface {
	Decrypt(string, string, string, EncryptedCallbackSecret) ([]byte, error)
}

type webhookOutcomeClient interface {
	DeliverOutcome(context.Context, WebhookDelivery, time.Duration) WebhookOutcome
}

type webhookDelivererFactory interface {
	New(string, uint16, time.Duration, time.Duration) (webhookOutcomeClient, io.Closer, error)
}

// WebhookWorkerConfig contains process-local delivery settings. The outbox
// repository remains authoritative for leases, deadlines, and attempt fencing.
type WebhookWorkerConfig struct {
	Repository         WebhookOutboxRepository
	CallbackCipher     *CallbackCipher
	WorkerID           string
	LeaseDuration      time.Duration
	RequestTimeout     time.Duration
	DialTimeout        time.Duration
	BaseRetryDelay     time.Duration
	MaximumRetryDelay  time.Duration
	MaximumAttempts    uint
	IdleDelay          time.Duration
	TerminalRetention  time.Duration
	MaximumAuthorities int
	Random             io.Reader
	Now                func() time.Time
}

// WebhookWorker claims, delivers, and fenced-settles durable webhook events.
type WebhookWorker struct {
	repository        webhookOutboxRepository
	decryptor         callbackSecretDecryptor
	factory           webhookDelivererFactory
	workerID          string
	leaseDuration     time.Duration
	requestTimeout    time.Duration
	dialTimeout       time.Duration
	baseRetryDelay    time.Duration
	maximumRetry      time.Duration
	maximumAttempts   uint
	idleDelay         time.Duration
	terminalRetention time.Duration
	random            io.Reader
	now               func() time.Time
	elapsedSince      func(time.Time) time.Duration
	cache             *webhookAuthorityCache
}

// NewWebhookWorker creates a worker using the production SSRF-safe transport.
func NewWebhookWorker(config WebhookWorkerConfig) (*WebhookWorker, error) {
	return newWebhookWorker(config, config.CallbackCipher, safeWebhookDelivererFactory{})
}

func newWebhookWorker(config WebhookWorkerConfig, decryptor callbackSecretDecryptor, factory webhookDelivererFactory) (*WebhookWorker, error) {
	applyWebhookWorkerDefaults(&config)
	if config.Repository == nil || decryptor == nil || factory == nil || !validWorkerID(config.WorkerID) {
		return nil, fmt.Errorf("webhook repository, callback cipher, deliverer factory, and worker ID are required")
	}
	if config.RequestTimeout <= 0 || config.LeaseDuration <= config.RequestTimeout || config.LeaseDuration > maximumWebhookRetryAfter ||
		config.DialTimeout <= 0 || config.DialTimeout > config.RequestTimeout {
		return nil, fmt.Errorf("webhook lease must exceed positive HTTP and dial timeouts and be at most 15 minutes")
	}
	if config.BaseRetryDelay <= 0 || config.MaximumRetryDelay < config.BaseRetryDelay || config.MaximumRetryDelay > maximumWebhookRetryAfter {
		return nil, fmt.Errorf("webhook retry delays must be positive, ordered, and at most 15 minutes")
	}
	if config.MaximumAttempts == 0 || config.MaximumAttempts > 100 || config.IdleDelay <= 0 || config.IdleDelay > time.Minute {
		return nil, fmt.Errorf("webhook attempts and idle delay are invalid")
	}
	if config.TerminalRetention <= 0 || config.TerminalRetention > 365*24*time.Hour || config.MaximumAuthorities < 1 || config.MaximumAuthorities > maximumWebhookAuthorityLimit {
		return nil, fmt.Errorf("webhook retention or authority cache limit is invalid")
	}
	if config.Random == nil || config.Now == nil {
		return nil, fmt.Errorf("webhook random source and clock are required")
	}
	return &WebhookWorker{
		repository: config.Repository, decryptor: decryptor, factory: factory,
		workerID: config.WorkerID, leaseDuration: config.LeaseDuration,
		requestTimeout: config.RequestTimeout, dialTimeout: config.DialTimeout,
		baseRetryDelay: config.BaseRetryDelay, maximumRetry: config.MaximumRetryDelay,
		maximumAttempts: config.MaximumAttempts, idleDelay: config.IdleDelay,
		terminalRetention: config.TerminalRetention, random: config.Random, now: config.Now,
		elapsedSince: time.Since, cache: newWebhookAuthorityCache(config.MaximumAuthorities),
	}, nil
}

func applyWebhookWorkerDefaults(config *WebhookWorkerConfig) {
	if config.LeaseDuration == 0 {
		config.LeaseDuration = defaultWebhookLeaseDuration
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultWebhookRequestTimeout
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = defaultWebhookDialTimeout
	}
	if config.BaseRetryDelay == 0 {
		config.BaseRetryDelay = defaultWebhookBaseRetryDelay
	}
	if config.MaximumRetryDelay == 0 {
		config.MaximumRetryDelay = defaultWebhookMaximumRetryDelay
	}
	if config.MaximumAttempts == 0 {
		config.MaximumAttempts = defaultWebhookMaximumAttempts
	}
	if config.IdleDelay == 0 {
		config.IdleDelay = defaultWebhookIdleDelay
	}
	if config.TerminalRetention == 0 {
		config.TerminalRetention = defaultWebhookTerminalRetention
	}
	if config.MaximumAuthorities == 0 {
		config.MaximumAuthorities = defaultWebhookAuthorityLimit
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
}

// Run delivers until the context is cancelled or repository authority fails.
func (worker *WebhookWorker) Run(ctx context.Context) error {
	if worker == nil {
		return fmt.Errorf("webhook worker is not configured")
	}
	defer worker.Close()
	nextSweep := time.Time{}
	for {
		if now := worker.now().UTC(); !now.Before(nextSweep) {
			drained, err := worker.sweepTerminal(ctx)
			if err != nil {
				return err
			}
			if drained {
				nextSweep = now.Add(webhookRetentionSweepInterval)
			} else {
				nextSweep = now.Add(worker.idleDelay)
			}
		}
		err := worker.processNext(ctx)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrWebhookNotAvailable) {
			return err
		}
		timer := time.NewTimer(worker.idleDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func (worker *WebhookWorker) sweepTerminal(ctx context.Context) (bool, error) {
	for batch := 0; batch < webhookRetentionSweepBudget; batch++ {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		deleted, err := worker.repository.SweepTerminal(ctx, worker.terminalRetention, webhookRetentionSweepBatch)
		if err != nil {
			return false, err
		}
		if deleted < webhookRetentionSweepBatch {
			return true, nil
		}
	}
	return false, nil
}

func (worker *WebhookWorker) processNext(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	localClaimStart := worker.now()
	claim, err := worker.repository.ClaimNextWebhook(ctx, worker.workerID, worker.leaseDuration)
	if err != nil {
		return err
	}
	localClaimTime := worker.now()
	databaseClaimTime := claim.LeaseUntil.Add(-worker.leaseDuration)
	claimElapsed := worker.elapsedSince(localClaimStart)
	if claimElapsed < 0 {
		claimElapsed = 0
	}
	databaseReceiveTime := databaseClaimTime.Add(claimElapsed)
	if !claim.LeaseUntil.After(databaseClaimTime) {
		return nil
	}
	if !claim.ExpiresAt.After(databaseClaimTime) {
		return worker.settle(ctx, claim, WebhookSettlement{Disposition: WebhookPermanentFailure, ErrorCode: WebhookErrorDeliveryExpired})
	}
	if _, err := validateCallbackDestination(claim.DestinationURL, claim.AllowedHost, claim.AllowedPort); err != nil {
		return worker.settle(ctx, claim, WebhookSettlement{Disposition: WebhookPermanentFailure, ErrorCode: WebhookErrorUnsafeDestination})
	}
	encrypted := claim.EncryptedSecret()
	secret, err := worker.decryptor.Decrypt(claim.TenantID, claim.CallbackID, claim.DestinationURL, encrypted)
	defer clear(secret)
	clear(encrypted.Ciphertext)
	clear(encrypted.Nonce)
	clear(claim.secret.Ciphertext)
	clear(claim.secret.Nonce)
	if err != nil {
		return worker.settle(ctx, claim, WebhookSettlement{Disposition: WebhookPermanentFailure, ErrorCode: WebhookErrorCallbackDecrypt})
	}
	deliverer, err := worker.cachedDeliverer(claim.AllowedHost, claim.AllowedPort)
	if err != nil {
		return worker.settle(ctx, claim, WebhookSettlement{Disposition: WebhookPermanentFailure, ErrorCode: WebhookErrorConfiguration})
	}
	databaseBeforeDelivery := estimatedWebhookDatabaseTime(databaseReceiveTime, worker.elapsedSince(localClaimTime))
	remainingLease := claim.LeaseUntil.Sub(databaseBeforeDelivery)
	if remainingLease <= 0 {
		return nil
	}
	// LeaseUntil is a database wall-clock value. A duration-based context keeps
	// host clock offset from cancelling a live database lease prematurely.
	deliveryContext, cancel := context.WithTimeout(ctx, remainingLease)
	outcome := deliverer.DeliverOutcome(deliveryContext, WebhookDelivery{
		EventID: claim.EventID, DestinationURL: claim.DestinationURL, Secret: secret, Body: claim.Body,
	}, worker.maximumRetry)
	cancel()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	databaseNow := estimatedWebhookDatabaseTime(databaseReceiveTime, worker.elapsedSince(localClaimTime))
	if !claim.LeaseUntil.After(databaseNow) {
		return nil
	}
	settlement := WebhookSettlement{Disposition: outcome.Disposition, HTTPStatus: outcome.HTTPStatus, ErrorCode: outcome.ErrorCode}
	if outcome.Disposition == WebhookRetry {
		delay := worker.baseRetryDelay / 2
		if claim.AttemptCount < worker.maximumAttempts {
			delay, err = webhookBackoff(worker.baseRetryDelay, worker.maximumRetry, claim.AttemptCount, outcome.RetryAfter, worker.random)
			if err != nil {
				return err
			}
		}
		settlement.RetryDelay = delay
	}
	return worker.settle(ctx, claim, settlement)
}

func estimatedWebhookDatabaseTime(databaseClaimTime time.Time, elapsed time.Duration) time.Time {
	if elapsed < 0 {
		elapsed = 0
	}
	return databaseClaimTime.Add(elapsed)
}

func (worker *WebhookWorker) settle(ctx context.Context, claim WebhookClaim, settlement WebhookSettlement) error {
	err := worker.repository.SettleWebhook(ctx, claim, settlement)
	if errors.Is(err, ErrWebhookLeaseLost) {
		return nil
	}
	return err
}

func (worker *WebhookWorker) cachedDeliverer(host string, port uint16) (webhookOutcomeClient, error) {
	key := strings.ToLower(host) + ":" + fmt.Sprint(port)
	if deliverer, ok := worker.cache.Get(key); ok {
		return deliverer, nil
	}
	deliverer, closer, err := worker.factory.New(host, port, worker.requestTimeout, worker.dialTimeout)
	if err != nil {
		return nil, err
	}
	if closer == nil {
		closer = noopCloser{}
	}
	authoritative, err := worker.cache.Add(key, deliverer, closer)
	if err != nil {
		_ = closer.Close()
		return nil, err
	}
	return authoritative, nil
}

// Close releases cached transports and their idle connections.
func (worker *WebhookWorker) Close() error {
	if worker == nil || worker.cache == nil {
		return nil
	}
	return worker.cache.Close()
}

func webhookBackoff(base, maximum time.Duration, attempt uint, retryAfter time.Duration, random io.Reader) (time.Duration, error) {
	if base <= 0 || maximum < base || maximum > maximumWebhookRetryAfter || attempt == 0 || random == nil {
		return 0, fmt.Errorf("webhook backoff input is invalid")
	}
	delay := base
	for remaining := attempt - 1; remaining > 0 && delay < maximum; remaining-- {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	var entropy [8]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return 0, fmt.Errorf("read webhook retry jitter: %w", err)
	}
	// Mul64 returns high then low; use high as floor(random*delay/2^64).
	high, _ := bits.Mul64(binary.BigEndian.Uint64(entropy[:]), uint64(delay))
	jittered := delay/2 + time.Duration(high)
	if jittered > maximum {
		jittered = maximum
	}
	if retryAfter < 0 {
		retryAfter = 0
	}
	if retryAfter > maximum {
		retryAfter = maximum
	}
	if retryAfter > jittered {
		jittered = retryAfter
	}
	if jittered < 0 {
		return 0, nil
	}
	return jittered, nil
}

type safeWebhookDelivererFactory struct{}

func (safeWebhookDelivererFactory) New(host string, port uint16, requestTimeout, dialTimeout time.Duration) (webhookOutcomeClient, io.Closer, error) {
	deliverer, err := NewSafeWebhookDeliverer(host, port, requestTimeout, dialTimeout)
	if err != nil {
		return nil, nil, err
	}
	return deliverer, webhookIdleCloser{deliverer: deliverer}, nil
}

type webhookIdleCloser struct{ deliverer *WebhookDeliverer }

func (closer webhookIdleCloser) Close() error {
	if closer.deliverer == nil {
		return nil
	}
	if transport, ok := closer.deliverer.transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	} else if transport, ok := closer.deliverer.transport.(*safeCallbackTransport); ok && transport.transport != nil {
		transport.transport.CloseIdleConnections()
	}
	return nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

type webhookAuthorityEntry struct {
	key       string
	deliverer webhookOutcomeClient
	closer    io.Closer
}

type webhookAuthorityCache struct {
	mu      sync.Mutex
	limit   int
	closed  bool
	entries map[string]*list.Element
	lru     *list.List
}

func newWebhookAuthorityCache(limit int) *webhookAuthorityCache {
	return &webhookAuthorityCache{limit: limit, entries: make(map[string]*list.Element), lru: list.New()}
}

func (cache *webhookAuthorityCache) Get(key string) (webhookOutcomeClient, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return nil, false
	}
	element, ok := cache.entries[key]
	if !ok {
		return nil, false
	}
	cache.lru.MoveToFront(element)
	return element.Value.(webhookAuthorityEntry).deliverer, true
}

func (cache *webhookAuthorityCache) Add(key string, deliverer webhookOutcomeClient, closer io.Closer) (webhookOutcomeClient, error) {
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return nil, ErrWebhookWorkerClosed
	}
	if existing, ok := cache.entries[key]; ok {
		cache.lru.MoveToFront(existing)
		authoritative := existing.Value.(webhookAuthorityEntry).deliverer
		cache.mu.Unlock()
		_ = closer.Close()
		return authoritative, nil
	}
	element := cache.lru.PushFront(webhookAuthorityEntry{key: key, deliverer: deliverer, closer: closer})
	cache.entries[key] = element
	var evictedCloser io.Closer
	if cache.lru.Len() > cache.limit {
		evicted := cache.lru.Back()
		entry := evicted.Value.(webhookAuthorityEntry)
		delete(cache.entries, entry.key)
		cache.lru.Remove(evicted)
		evictedCloser = entry.closer
	}
	cache.mu.Unlock()
	if evictedCloser != nil {
		_ = evictedCloser.Close()
	}
	return deliverer, nil
}

func (cache *webhookAuthorityCache) Close() error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return nil
	}
	cache.closed = true
	var result error
	for element := cache.lru.Front(); element != nil; element = element.Next() {
		result = errors.Join(result, element.Value.(webhookAuthorityEntry).closer.Close())
	}
	cache.entries = nil
	cache.lru.Init()
	return result
}
