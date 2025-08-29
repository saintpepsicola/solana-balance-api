package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/assert"
	"golang.org/x/time/rate"
)

type MockSolanaClient struct {
	GetBalanceFunc func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error)
}

func (m *MockSolanaClient) GetBalance(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
	return m.GetBalanceFunc(ctx, account, commitment)
}

func setupTest() {
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
inflightRequests = make(map[string]*inflightRequest)
	inflightRequestsMutex.Unlock()

	ipLimitersMutex.Lock()
	ipLimiters = make(map[string]*rate.Limiter)
	ipLimitersMutex.Unlock()
}

func TestRateLimitMiddleware(t *testing.T) {
setupTest()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rateLimitedHandler := rateLimitMiddleware(testHandler)

	for i := 0; i < rateLimitBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		rateLimitedHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	rateLimitedHandler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
}

func TestCaching(t *testing.T) {
setupTest()

	callCount := 0
	solanaRPCClient = &MockSolanaClient{
		GetBalanceFunc: func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			callCount++
			return &rpc.GetBalanceResult{Value: 12345}, nil
		},
	}

	testWallet := "7xLk17EQQ5KLDLDe44wCmupJKJjTGd8hs3eSVVhCx932"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	req1 := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(reqBody))
	getBalanceHandler(httptest.NewRecorder(), req1)
	assert.Equal(t, 1, callCount, "RPC should be called once on the first request.")

	req2 := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(reqBody))
	getBalanceHandler(httptest.NewRecorder(), req2)
	assert.Equal(t, 1, callCount, "RPC should NOT be called again for a cached request.")

	time.Sleep(cacheTTL + 1*time.Second)

	req3 := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(reqBody))
	getBalanceHandler(httptest.NewRecorder(), req3)
	assert.Equal(t, 2, callCount, "RPC should be called again after the cache expires.")
}

func TestConcurrentRequests(t *testing.T) {
setupTest()

	callCount := 0
	solanaRPCClient = &MockSolanaClient{
		GetBalanceFunc: func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			time.Sleep(50 * time.Millisecond)
			callCount++
			return &rpc.GetBalanceResult{Value: 12345}, nil
		},
	}

	testWallet := "7xLk17EQQ5KLDLDe44wCmupJKJjTGd8hs3eSVVhCx932"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(reqBody))
			rr := httptest.NewRecorder()
			getBalanceHandler(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, callCount, "RPC should only be called ONCE for multiple concurrent requests.")
}