//go:build unit

package handler

import (
	"bufio"
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type droidHandlerUpstream struct {
	service.HTTPUpstream
	status  int
	calls   []int64
	partial bool
}

func (u *droidHandlerUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	if id == 801 && u.status != 0 {
		return &http.Response{StatusCode: u.status, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"factory_test_secret"}}`))}, nil
	}
	var body string
	ct := "application/json"
	switch req.URL.Path {
	case "/api/llm/a/v1/messages":
		body = `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"healthy"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
		if u.partial {
			ct = "text/event-stream"
			body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial-output\"}}\n\n"
		}
	case "/api/llm/o/v1/responses":
		body = `{"id":"resp_ok","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"healthy"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
		if u.partial {
			ct = "text/event-stream"
			body = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.4\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial-output\"}\n\n"
		}
	case "/api/llm/o/v1/chat/completions":
		body = `{"id":"chat_ok","object":"chat.completion","model":"glm-5","choices":[{"index":0,"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
		if u.partial {
			ct = "text/event-stream"
			body = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial-output\"},\"finish_reason\":null}]}\n\n"
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

type droidUsageSpy struct {
	service.UsageLogRepository
	calls atomic.Int32
}

func (s *droidUsageSpy) Create(_ context.Context, _ *service.UsageLog) (bool, error) {
	s.calls.Add(1)
	return true, nil
}

type droidFixtureOptions struct {
	single       bool
	queuedCommit string
	usage        *droidUsageSpy
	admission    *droidAdmissionFixture
	parent       context.Context
	handlerDone  chan struct{}
	// streamIntervalSeconds sets gateway.stream_data_interval_timeout for the
	// Anthropic line pump in tests only (0 keeps the default: disabled).
	streamIntervalSeconds int
}

// One occupancy source is shared by scheduler probes, fast admission and polls.
type droidAdmissionFixture struct {
	occupied atomic.Bool
	// onlyID, when nonzero, limits occupancy to that one account.
	onlyID  int64
	denied  atomic.Int32
	waiting atomic.Int32
	blocked chan struct{}
	once    sync.Once
}

type droidAdmissionCache struct {
	*concurrencyCacheMock
	state *droidAdmissionFixture
}

func (m *droidAdmissionFixture) busy(id int64) bool {
	return m.occupied.Load() && (m.onlyID == 0 || m.onlyID == id)
}

func (m *droidAdmissionCache) AcquireAccountSlot(ctx context.Context, id int64, max int, requestID string) (bool, error) {
	if m.state.busy(id) {
		m.state.denied.Add(1)
		m.state.once.Do(func() { close(m.state.blocked) })
		return false, ctx.Err()
	}
	return m.concurrencyCacheMock.AcquireAccountSlot(ctx, id, max, requestID)
}

func (m *droidAdmissionCache) GetAccountConcurrency(_ context.Context, id int64) (int, error) {
	if m.state.busy(id) {
		return 1, nil
	}
	return 0, nil
}

func (m *droidAdmissionCache) GetAccountConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	result := make(map[int64]int, len(ids))
	for _, id := range ids {
		result[id], _ = m.GetAccountConcurrency(context.Background(), id)
	}
	return result, nil
}

func (m *droidAdmissionCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	result := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		count, _ := m.GetAccountConcurrency(context.Background(), account.ID)
		result[account.ID] = &service.AccountLoadInfo{AccountID: account.ID, CurrentConcurrency: count, LoadRate: count * 100}
	}
	return result, nil
}

func (m *droidAdmissionCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	m.state.waiting.Add(1)
	return true, nil
}

func (m *droidAdmissionCache) DecrementAccountWaitCount(context.Context, int64) error {
	m.state.waiting.Add(-1)
	return nil
}

type droidFixtureContextKey struct{}

func newDroidHandler(t *testing.T, upstream service.HTTPUpstream) (*gin.Engine, *grokCredentialHandlerRepo) {
	return newDroidHandlerWithOptions(t, upstream, droidFixtureOptions{})
}

func newDroidHandlerWithOptions(t *testing.T, upstream service.HTTPUpstream, options droidFixtureOptions) (*gin.Engine, *grokCredentialHandlerRepo) {
	t.Helper()
	accounts := []service.Account{}
	for i := int64(801); i <= 802; i++ {
		accounts = append(accounts, service.Account{ID: i, Platform: service.PlatformDroid, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: int(i - 800), Credentials: map[string]any{"api_key": "factory_test_secret"}})
		if options.single {
			break
		}
	}
	repo := &grokCredentialHandlerRepo{accounts: accounts}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 30 * time.Second
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 100
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.StreamDataIntervalTimeout = options.streamIntervalSeconds
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	var usage service.UsageLogRepository
	if options.usage != nil {
		usage = options.usage
	}
	var slotCalls atomic.Int32
	cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }, acquireAccountSlotFn: func(ctx context.Context, _ int64, _ int, _ string) (bool, error) {
		if options.queuedCommit != "" && slotCalls.Add(1) == 1 {
			c := ctx.Value(droidFixtureContextKey{}).(*gin.Context)
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			if options.queuedCommit == "heartbeat" {
				_, _ = c.Writer.WriteString(": queued\n\n")
			}
			c.Writer.Flush()
			return false, nil
		}
		return true, nil
	}}
	concurrency := service.NewConcurrencyService(cache)
	if options.admission != nil {
		concurrency = service.NewConcurrencyService(&droidAdmissionCache{concurrencyCacheMock: cache, state: options.admission})
	}
	var schedulerConcurrency *service.ConcurrencyService
	if options.queuedCommit != "" || options.admission != nil {
		schedulerConcurrency = concurrency
	}
	gateway := service.NewOpenAIGatewayService(repo, usage, nil, nil, nil, nil, nil, cfg, nil, schedulerConcurrency, service.NewBillingService(cfg, nil), service.NewRateLimitService(repo, nil, cfg, nil, nil), billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	groupID := int64(901)
	key := &service.APIKey{ID: 902, GroupID: &groupID, User: &service.User{ID: 903, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformDroid, Status: service.StatusActive}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if options.handlerDone != nil {
			defer close(options.handlerDone)
		}
		if options.parent != nil {
			ctx, cancel := context.WithCancel(c.Request.Context())
			stop := context.AfterFunc(options.parent, cancel)
			defer stop()
			defer cancel()
			if deadline, ok := options.parent.Deadline(); ok {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
				defer deadlineCancel()
			}
			c.Request = c.Request.WithContext(ctx)
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), droidFixtureContextKey{}, c))
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/v1/responses", h.Responses)
	router.POST("/v1/messages", h.Messages)
	router.POST("/v1/chat/completions", h.ChatCompletions)
	return router, repo
}

func TestDroidHandlerAccountFailoverAndPartialStream(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"messages", "/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"gpt-5.4","input":"hi"}`},
		{"chat", "/v1/chat/completions", `{"model":"glm-5","messages":[{"role":"user","content":"hi"}]}`},
	} {
		for _, status := range []int{401, 429} {
			t.Run(tc.name+http.StatusText(status), func(t *testing.T) {
				upstream := &droidHandlerUpstream{status: status}
				router, repo := newDroidHandler(t, upstream)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
				require.Equal(t, 200, recorder.Code, recorder.Body.String())
				require.Contains(t, recorder.Body.String(), "healthy")
				require.NotContains(t, recorder.Body.String(), "factory_test_secret")
				require.Equal(t, []int64{801, 802}, upstream.calls)
				if status == 401 {
					require.Equal(t, []int64{801}, repo.setErrorIDs)
				} else {
					require.Equal(t, []int64{801}, repo.rateLimitIDs)
				}
			})
		}
		t.Run(tc.name+"PartialStream", func(t *testing.T) {
			upstream := &droidHandlerUpstream{partial: true}
			router, _ := newDroidHandler(t, upstream)
			recorder := httptest.NewRecorder()
			body := `{"stream":true,` + tc.body[1:]
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)))
			require.Contains(t, recorder.Body.String(), "partial-output")
			require.Equal(t, []int64{801}, upstream.calls, "partial output must not replay on another account")
		})
	}
}

func TestDroidLocalHTTPTransportIntegration(t *testing.T) {
	t.Run("headers_and_redaction", func(t *testing.T) {
		headers := make(chan http.Header, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Clone()
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"factory_test_secret"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
		}))
		defer server.Close()
		router, repo := newDroidHandler(t, repository.NewHTTPUpstream(nil))
		repo.accounts[0].Credentials["api_base_urls"] = map[string]any{"anthropic": server.URL + "/api/llm/a"}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "factory_test_secret")
		require.Contains(t, rec.Body.String(), "[REDACTED]")
		select {
		case hdr := <-headers:
			require.Equal(t, "Bearer factory_test_secret", hdr.Get("Authorization"))
			require.Equal(t, "anthropic", hdr.Get("X-API-Provider"))
			require.NotEmpty(t, hdr.Get("X-Session-ID"))
		default:
			t.Fatal("local HTTP server did not receive request")
		}
	})
	t.Run("no_redirect_credential_forwarding", func(t *testing.T) {
		var targetCalls atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
		defer target.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer redirector.Close()
		router, repo := newDroidHandler(t, repository.NewHTTPUpstream(nil))
		for i := range repo.accounts {
			repo.accounts[i].Credentials["api_base_urls"] = map[string]any{"responses": redirector.URL + "/api/llm/o/v1"}
		}
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`)))
		require.Zero(t, targetCalls.Load())
	})
	t.Run("cancellation_closes_upstream_socket", func(t *testing.T) {
		started, closed := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":3}}}\n\n")
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			close(closed)
		}))
		defer server.Close()
		router, repo := newDroidHandler(t, repository.NewHTTPUpstream(nil))
		repo.accounts[0].Credentials["api_base_urls"] = map[string]any{"anthropic": server.URL + "/api/llm/a"}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		done := make(chan struct{})
		go func() { router.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not start")
		}
		cancel()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream socket was not canceled")
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not terminate")
		}
	})
}

var droidRegressionEntrances = []struct{ name, path, body string }{
	{"messages", "/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
	{"chat", "/v1/chat/completions", `{"model":"glm-5","messages":[{"role":"user","content":"hi"}]}`},
	{"responses", "/v1/responses", `{"model":"gpt-5.4","input":"hi"}`},
}

// Records the scheduler's actual account ID but always sends a real HTTP request.
type droidLocalUpstream struct {
	service.HTTPUpstream
	client  *http.Client
	mu      sync.Mutex
	ids     []int64
	times   []time.Time
	bodies  []string
	headers []http.Header
}

func (u *droidLocalUpstream) Do(r *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	u.mu.Lock()
	u.ids = append(u.ids, id)
	u.times = append(u.times, time.Now())
	u.bodies = append(u.bodies, string(b))
	u.headers = append(u.headers, r.Header.Clone())
	u.mu.Unlock()
	return u.client.Do(r)
}

func (u *droidLocalUpstream) snapshot() ([]int64, []time.Time, []string, []http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.ids...), append([]time.Time(nil), u.times...), append([]string(nil), u.bodies...), append([]http.Header(nil), u.headers...)
}

func droidLocalFixture(t *testing.T, upstream http.HandlerFunc, options droidFixtureOptions, timeout time.Duration) (*httptest.Server, *droidLocalUpstream) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	u := &droidLocalUpstream{client: &http.Client{Transport: transport, Timeout: timeout}}
	router, repo := newDroidHandlerWithOptions(t, u, options)
	for i := range repo.accounts {
		repo.accounts[i].Credentials["api_base_urls"] = map[string]any{service.APIProtocolAnthropic: server.URL + "/api/llm/a", service.APIProtocolResponses: server.URL + "/api/llm/o/v1", service.APIProtocolChatCompletions: server.URL + "/api/llm/o/v1"}
	}
	front := httptest.NewServer(router)
	t.Cleanup(front.Close)
	return front, u
}

func droidHealthyHTTP(w http.ResponseWriter, r *http.Request) {
	response, _ := (&droidHandlerUpstream{}).Do(r, "", 801, 1)
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	_, _ = io.Copy(w, response.Body)
	_ = response.Body.Close()
}

func droidPost(t *testing.T, front *httptest.Server, path, body string) (int, http.Header, string) {
	t.Helper()
	client := &http.Client{Timeout: 8 * time.Second}
	r, err := client.Post(front.URL+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return r.StatusCode, r.Header, string(b)
}

func TestDroidSingleAccountTransientRecovery(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		for _, failure := range []string{"eof", "timeout", "500", "502", "503", "504"} {
			t.Run(entrance.name+"/"+failure, func(t *testing.T) {
				var calls atomic.Int32
				spy := &droidUsageSpy{}
				front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					if calls.Add(1) == 1 {
						switch failure {
						case "eof":
							conn, _, err := w.(http.Hijacker).Hijack()
							if err == nil {
								_ = conn.Close()
							}
						case "timeout":
							<-r.Context().Done()
						default:
							var status int
							_, _ = fmt.Sscan(failure, &status)
							w.WriteHeader(status)
							_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
						}
						return
					}
					if spy.calls.Load() != 0 {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					droidHealthyHTTP(w, r)
				}, droidFixtureOptions{single: true, usage: spy}, 120*time.Millisecond)
				status, _, body := droidPost(t, front, entrance.path, entrance.body)
				ids, times, bodies, headers := u.snapshot()
				require.Equal(t, 200, status, body)
				require.Contains(t, body, "healthy")
				require.Equal(t, []int64{801, 801}, ids, "must pin the same non-pool account")
				require.Equal(t, int32(2), calls.Load())
				require.Eventually(t, func() bool { return spy.calls.Load() == 1 }, time.Second, 10*time.Millisecond)
				require.GreaterOrEqual(t, times[1].Sub(times[0]), 240*time.Millisecond)
				require.Less(t, times[1].Sub(times[0]), 3*time.Second)
				require.Equal(t, bodies[0], bodies[1], "replay must be complete and identity must not accumulate")
				for _, hdr := range headers {
					require.Len(t, hdr.Values("Authorization"), 1)
					require.Len(t, hdr.Values("X-Session-Id"), 1)
				}
				require.NotContains(t, body, "factory_test_secret")
			})
		}
	}
}

func TestDroidSingleAccountRetryExhaustion(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
			}, droidFixtureOptions{single: true}, time.Second)
			start := time.Now()
			code, _, body := droidPost(t, front, droidRegressionEntrances[0].path, droidRegressionEntrances[0].body)
			ids, times, _, _ := u.snapshot()
			require.Equal(t, []int64{801, 801, 801}, ids, "503 must not stack a second recovery loop")
			require.GreaterOrEqual(t, code, 400)
			require.NotContains(t, body, "factory_test_secret")
			require.Less(t, time.Since(start), 3*time.Second)
			require.GreaterOrEqual(t, times[1].Sub(times[0]), 240*time.Millisecond)
			require.GreaterOrEqual(t, times[2].Sub(times[1]), 640*time.Millisecond)
		})
	}
}

func TestDroidQueuedCommitNeverReplays(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		for _, commit := range []string{"heartbeat", "header-only"} {
			t.Run(entrance.name+"/"+commit, func(t *testing.T) {
				front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(502)
					_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
				}, droidFixtureOptions{queuedCommit: commit}, time.Second)
				_, _, body := droidPost(t, front, entrance.path, `{"stream":true,`+entrance.body[1:])
				ids, _, _, _ := u.snapshot()
				require.Equal(t, []int64{801}, ids, "commit while waiting for a concurrency slot must close the retry window")
				require.NotContains(t, body, "factory_test_secret")
				require.Contains(t, body, "error")
			})
		}
	}
}

func TestDroidSingleAccountRetryAfter(t *testing.T) {
	validDate := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	for _, retryAfter := range []string{"1", "60", validDate, "", "invalid", strings.Repeat("9", 129), "604801", time.Now().Add(8 * 24 * time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(retryAfter, func(t *testing.T) {
			var calls atomic.Int32
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if retryAfter != "" {
						w.Header().Set("Retry-After", retryAfter)
					}
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
					return
				}
				droidHealthyHTTP(w, r)
			}, droidFixtureOptions{single: true}, time.Second)
			code, headers, body := droidPost(t, front, droidRegressionEntrances[0].path, droidRegressionEntrances[0].body)
			ids, times, _, _ := u.snapshot()
			if retryAfter == "1" {
				require.Equal(t, 200, code, body)
				require.Equal(t, []int64{801, 801}, ids)
				require.GreaterOrEqual(t, times[1].Sub(times[0]), time.Second)
			} else {
				require.Equal(t, 429, code, body)
				require.Equal(t, []int64{801}, ids)
				expected := ""
				if retryAfter == "60" || retryAfter == validDate {
					expected = retryAfter
				}
				require.Equal(t, expected, headers.Get("Retry-After"))
				require.NotContains(t, body, "factory_test_secret")
			}
		})
	}
}

func TestDroidStreamFirstEventBeforeUpstreamCompletion(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				response, _ := (&droidHandlerUpstream{partial: true}).Do(r, "", 801, 1)
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				// Send exactly one complete event, not the multi-event fixture.
				first, rest, _ := strings.Cut(string(payload), "\n\n")
				_, _ = io.WriteString(w, first+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, rest)
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}, droidFixtureOptions{single: true}, 4*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+entrance.path, strings.NewReader(`{"stream":true,`+entrance.body[1:]))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "headers and first event must arrive before release")
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
			reader := bufio.NewReader(resp.Body)
			var event strings.Builder
			for {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				event.WriteString(line)
				if line == "\n" && strings.Contains(event.String(), "data:") {
					break
				}
			}
			// The upstream's first complete event arrives while upstream is still
			// blocked: no ping/comment substitute, no later event merged into it.
			require.NotContains(t, event.String(), "ping")
			require.False(t, strings.HasPrefix(event.String(), ":"), "comment/keepalive must not stand in for the first event")
			select {
			case <-release:
				t.Fatal("upstream released before the first event reached the client")
			default:
			}
			switch entrance.name {
			case "messages":
				require.Contains(t, event.String(), `"type":"message_start"`)
				require.Contains(t, event.String(), `"id":"msg_1"`)
				require.NotContains(t, event.String(), "partial-output")
			case "chat":
				require.Contains(t, event.String(), `"id":"chat_1"`)
				require.Contains(t, event.String(), `"content":"partial-output"`)
			case "responses":
				require.Contains(t, event.String(), `"type":"response.created"`)
				require.Contains(t, event.String(), `"id":"resp_1"`)
				require.NotContains(t, event.String(), "response.output_text.delta")
			}
			unblock()
			_, _ = io.ReadAll(reader)
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids, "truncated stream must never replay")
		})
	}
}

func TestDroidSingleAccountBackoffCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			failed := make(chan struct{}, 1)
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(502)
				_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
				select {
				case failed <- struct{}{}:
				default:
				}
			}, droidFixtureOptions{single: true}, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
			}
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+droidRegressionEntrances[0].path, strings.NewReader(droidRegressionEntrances[0].body))
			require.NoError(t, err)
			done := make(chan struct{})
			go func() {
				defer close(done)
				resp, _ := http.DefaultClient.Do(req)
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}()
			select {
			case <-failed:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not reached")
			}
			if !deadline {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt retry wait")
			}
			time.Sleep(400 * time.Millisecond)
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids)
		})
	}
}

func TestDroidFailedAttemptsNeverRecordUsage(t *testing.T) {
	spy := &droidUsageSpy{}
	front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
	}, droidFixtureOptions{single: true, usage: spy}, time.Second)
	_, _, _ = droidPost(t, front, droidRegressionEntrances[0].path, droidRegressionEntrances[0].body)
	ids, _, _, _ := u.snapshot()
	require.Equal(t, []int64{801, 801, 801}, ids)
	require.Zero(t, spy.calls.Load())
}

func TestDroidSingleAccountPermanentHTTPFailureNeverRetries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
			}, droidFixtureOptions{single: true}, time.Second)
			code, _, body := droidPost(t, front, droidRegressionEntrances[0].path, droidRegressionEntrances[0].body)
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids)
			require.GreaterOrEqual(t, code, 400)
			require.NotContains(t, body, "factory_test_secret")
		})
	}
}

func TestDroidSuccessfulRequestRecordsUsageOnce(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			spy := &droidUsageSpy{}
			front, u := droidLocalFixture(t, droidHealthyHTTP, droidFixtureOptions{single: true, usage: spy}, time.Second)
			code, _, body := droidPost(t, front, entrance.path, entrance.body)
			require.Equal(t, 200, code, body)
			require.Eventually(t, func() bool { return spy.calls.Load() == 1 }, time.Second, 10*time.Millisecond)
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids)
		})
	}
}

func TestDroidReplayBarrierRequestCommit(t *testing.T) {
	for _, platform := range []string{service.PlatformDroid, service.PlatformOpenAI} {
		for _, commit := range []string{"none", "header", "heartbeat", "terminal"} {
			t.Run(platform+"/"+commit, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				account := &service.Account{Platform: platform}
				switch commit {
				case "header":
					c.Writer.Flush()
				case "heartbeat":
					_, err := c.Writer.WriteString(": heartbeat\n\n")
					require.NoError(t, err)
				case "terminal":
					service.MarkResponseCommitted(c)
				}
				require.Equal(t, platform == service.PlatformDroid && commit != "none", droidResponseCommitted(c, account))
				// Merely flushing while queued never blocks the first send.
				var barrier droidReplayBarrier
				require.False(t, barrier.stop(nil, c, false))
			})
		}
	}
}

func TestDroidReplayBarrierRechecksBeforeSend(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := &service.Account{Platform: service.PlatformDroid}
	failure := &service.UpstreamFailoverError{StatusCode: 502, SafeToFailoverAfterWrite: true}
	var barrier droidReplayBarrier
	barrier.remember(account, failure)
	h := &OpenAIGatewayHandler{}
	require.False(t, barrier.stop(h, c, false))
	// Admission/backoff commits only headers after the first replay check.
	c.Writer.Flush()
	require.True(t, openAIForwardMayFailover(c, c.Writer.Size(), failure), "legacy exemption remains unchanged")
	require.True(t, barrier.stop(h, c, false), "Droid overrides SafeToFailoverAfterWrite and header-only exemption")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "factory_test_secret")

	// A service-written terminal event must not get a second terminal error.
	service.MarkResponseCommitted(c)
	before := rec.Body.String()
	require.True(t, barrier.stop(h, c, false))
	require.Equal(t, before, rec.Body.String())
}

func TestDroidReplayBarrierCompactKeepalive(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	service.MarkOpenAICompactClientStream(c)
	stop := service.StartOpenAICompactSSEKeepalive(c, time.Millisecond)
	defer stop()
	// Stop synchronizes with the writer before inspecting it, including under race.
	time.Sleep(50 * time.Millisecond)
	require.True(t, service.StopOpenAICompactSSEKeepaliveCommitted(c))
	failure := &service.UpstreamFailoverError{StatusCode: 502}
	require.True(t, openAIForwardMayFailover(c, -1, failure), "non-Droid compact keepalive exemption is preserved")
	require.True(t, droidResponseCommitted(c, &service.Account{Platform: service.PlatformDroid}))
	require.False(t, droidResponseCommitted(c, &service.Account{Platform: service.PlatformOpenAI}))
}

func TestDroidRetryPinsAccountWithTwoCandidates(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			var calls atomic.Int32
			front, upstream := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= 2 {
					w.WriteHeader(502)
					_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
					return
				}
				droidHealthyHTTP(w, r)
			}, droidFixtureOptions{}, time.Second)
			status, _, body := droidPost(t, front, entrance.path, entrance.body)
			require.Equal(t, 200, status, body)
			ids, times, _, _ := upstream.snapshot()
			require.Equal(t, []int64{801, 801, 801}, ids)
			require.GreaterOrEqual(t, times[2].Sub(times[1]), 640*time.Millisecond)
		})
	}
}

func TestDroidRetryBudgetAndParentDeadline(t *testing.T) {
	account := &service.Account{ID: 801, Platform: service.PlatformDroid}
	for _, mode := range []string{"budget", "deadline", "committed", "non-droid"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
			failure := &service.UpstreamFailoverError{StatusCode: 502, RetryableOnSameAccount: true}
			barrier := droidReplayBarrier{deadline: time.Now().Add(10 * time.Millisecond), retries: map[int64]int{}, rateRetries: map[int64]int{}}
			barrier.remember(account, failure)
			target := account
			if mode == "deadline" {
				barrier.deadline = time.Now().Add(service.DroidRetryBudget)
			}
			if mode == "committed" {
				c.Writer.Flush()
			}
			if mode == "non-droid" {
				target = &service.Account{ID: 801, Platform: service.PlatformOpenAI}
			}
			start := time.Now()
			replay, terminal := barrier.retry(&OpenAIGatewayHandler{}, c, target, failure, false, false)
			require.False(t, replay)
			require.Equal(t, mode == "deadline" || mode == "committed", terminal)
			require.Zero(t, barrier.pending)
			require.Less(t, time.Since(start), 200*time.Millisecond)
		})
	}
}

func TestDroidMixedTransientAndRateLimitRetryBudget(t *testing.T) {
	for _, sequence := range [][]int{{502, 429, 502}, {429, 502, 429}} {
		t.Run(fmt.Sprint(sequence), func(t *testing.T) {
			var calls atomic.Int32
			front, upstream := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(sequence) {
					droidHealthyHTTP(w, r)
					return
				}
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(sequence[index])
				_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
			}, droidFixtureOptions{single: true}, time.Second)
			start := time.Now()
			status, _, body := droidPost(t, front, droidRegressionEntrances[0].path, droidRegressionEntrances[0].body)
			ids, _, _, _ := upstream.snapshot()
			require.Equal(t, []int64{801, 801, 801}, ids)
			require.GreaterOrEqual(t, status, 400)
			require.NotContains(t, body, "factory_test_secret")
			require.Less(t, time.Since(start), service.DroidRetryBudget)
		})
	}
}

func TestDroidRetryAdmissionBudgetHTTP(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		for _, mode := range []string{"budget", "cancel", "deadline"} {
			t.Run(entrance.name+"/"+mode, func(t *testing.T) {
				state := &droidAdmissionFixture{blocked: make(chan struct{})}
				parent, cancel := context.WithCancel(context.Background())
				if mode == "deadline" {
					cancel()
					parent, cancel = context.WithTimeout(context.Background(), 900*time.Millisecond)
				}
				defer cancel()
				handlerDone := make(chan struct{})
				failed := make(chan time.Time, 1)
				front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
					// The first attempt owns a slot already. Occupy its replacement
					// before returning the failure, deterministically before backoff.
					state.occupied.Store(true)
					select {
					case failed <- time.Now():
					default:
					}
					w.WriteHeader(502)
					_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
				}, droidFixtureOptions{single: true, admission: state, parent: parent, handlerDone: handlerDone}, time.Second)
				ctx, clientCancel := context.WithCancel(context.Background())
				defer clientCancel()
				// Cancel the SERVER parent before fixture Close, including on the old
				// 30-second wait path. The client has no timeout masquerading as success.
				t.Cleanup(cancel)
				type result struct {
					status int
					body   string
					err    error
				}
				resultCh := make(chan result, 1)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+entrance.path, strings.NewReader(entrance.body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				go func() {
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						resultCh <- result{err: err}
						return
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					resultCh <- result{status: resp.StatusCode, body: string(body), err: err}
				}()
				var start time.Time
				select {
				case start = <-failed:
				case <-time.After(2 * time.Second):
					t.Fatal("first upstream request missing")
				}
				select {
				case <-state.blocked:
				case <-time.After(2 * time.Second):
					t.Fatal("pinned retry never attempted admission")
				}
				if mode == "cancel" {
					require.Eventually(t, func() bool { return state.waiting.Load() == 1 && state.denied.Load() >= 3 }, 500*time.Millisecond, time.Millisecond, "retry must enter the occupied queue before cancellation")
					cancel()
				}
				limit := time.Second
				if mode == "budget" {
					limit = service.DroidRetryBudget + 600*time.Millisecond - time.Since(start)
				}
				select {
				case <-handlerDone:
				case <-time.After(limit):
					t.Fatal("server retry admission exceeded request budget; default queue timeout leaked")
				}
				select {
				case got := <-resultCh:
					require.NoError(t, got.err)
					require.NotContains(t, got.body, "factory_test_secret")
					if mode == "budget" {
						require.GreaterOrEqual(t, got.status, 400, got.body)
						require.Contains(t, got.body, "error")
						require.GreaterOrEqual(t, time.Since(start), service.DroidRetryBudget-200*time.Millisecond)
					}
				case <-time.After(time.Second):
					t.Fatal("client did not observe server completion")
				}
				require.GreaterOrEqual(t, state.denied.Load(), int32(3), "scheduler, fast acquire and polling must all observe occupancy")
				require.Zero(t, state.waiting.Load(), "wait counter leaked")
				ids, _, _, _ := u.snapshot()
				require.Equal(t, []int64{801}, ids, "budget expiry or parent cancellation must not replay")
			})
		}
	}
}

func TestDroidRetryAfterTerminalBranches(t *testing.T) {
	validDate := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	for _, value := range []string{"60", validDate, "invalid", strings.Repeat("9", 129), "604801", time.Now().Add(8 * 24 * time.Hour).UTC().Format(http.TimeFormat), "1\r\nX-Injected: yes"} {
		for _, pinned := range []bool{false, true} {
			for _, anthropic := range []bool{false, true} {
				t.Run(fmt.Sprintf("%q/pinned=%v/anthropic=%v", value, pinned, anthropic), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
					account := &service.Account{ID: 801, Platform: service.PlatformDroid}
					failure := &service.UpstreamFailoverError{StatusCode: 429, ResponseHeaders: http.Header{"Retry-After": {value}}, ResponseBody: []byte(`{"error":{"message":"factory_test_secret"}}`)}
					barrier := droidReplayBarrier{single: map[int64]bool{801: true}}
					barrier.remember(account, failure)
					if pinned {
						barrier.pending = 801
						require.True(t, barrier.selectionFailed(&OpenAIGatewayHandler{}, c, anthropic, false))
					} else {
						replay, terminal := barrier.retry(&OpenAIGatewayHandler{}, c, account, failure, anthropic, false)
						require.False(t, replay)
						require.True(t, terminal)
					}
					expected := ""
					if value == "60" || value == validDate {
						expected = value
					}
					require.Equal(t, 429, rec.Code)
					require.Equal(t, expected, rec.Header().Get("Retry-After"))
					require.NotContains(t, rec.Body.String(), "factory_test_secret")
				})
			}
		}
	}
}

// Complete, terminal SSE streams for a successful streaming retry.
func droidHealthySSE(path string) string {
	switch path {
	case "/api/llm/a/v1/messages":
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"healthy\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case "/api/llm/o/v1/responses":
		return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"gpt-5.4\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"healthy\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"healthy\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	default:
		return "data: {\"id\":\"chat_ok\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"healthy\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chat_ok\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
			"data: [DONE]\n\n"
	}
}

func droidHealthyStreamHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, droidHealthySSE(r.URL.Path))
}

func droidStreamTerminal(name string) string {
	switch name {
	case "messages":
		return "message_stop"
	case "responses":
		return "response.completed"
	}
	return "[DONE]"
}

// droidBodyReadFailure commits upstream 200 headers with Flush, then either
// truncates the body and drops the socket or blocks until the upstream
// client's own timeout closes it. The parent (inbound) context stays valid.
func droidBodyReadFailure(w http.ResponseWriter, r *http.Request, stream bool, mode string) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
	}
	w.WriteHeader(http.StatusOK)
	if mode == "truncate" {
		// An incomplete first event/JSON document: no SSE event boundary.
		if stream {
			_, _ = io.WriteString(w, "data: {\"type\":")
		} else {
			_, _ = io.WriteString(w, `{"id":"partial`)
		}
	}
	w.(http.Flusher).Flush()
	if mode == "truncate" {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func TestDroidBodyReadFailureSameAccountRetry(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"truncate", "timeout"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", entrance.name, stream, mode), func(t *testing.T) {
					var calls atomic.Int32
					spy := &droidUsageSpy{}
					failedDone := make(chan struct{})
					front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.ReadAll(r.Body)
						if calls.Add(1) == 1 {
							defer close(failedDone)
							droidBodyReadFailure(w, r, stream, mode)
							return
						}
						if spy.calls.Load() != 0 {
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						if stream {
							droidHealthyStreamHTTP(w, r)
						} else {
							droidHealthyHTTP(w, r)
						}
					}, droidFixtureOptions{usage: spy}, 400*time.Millisecond)
					body := entrance.body
					if stream {
						body = `{"stream":true,` + body[1:]
					}
					status, _, got := droidPost(t, front, entrance.path, body)
					select {
					case <-failedDone:
					case <-time.After(2 * time.Second):
						t.Fatal("failed upstream attempt was never closed")
					}
					ids, times, bodies, headers := u.snapshot()
					require.Equal(t, 200, status, got)
					require.Contains(t, got, "healthy")
					require.NotContains(t, got, "partial")
					if stream {
						require.Contains(t, got, droidStreamTerminal(entrance.name))
					}
					require.Equal(t, []int64{801, 801}, ids, "body-read failure must retry the pinned account, not switch")
					require.Equal(t, int32(2), calls.Load())
					require.Equal(t, bodies[0], bodies[1], "replayed upstream request body must be identical")
					require.NotEmpty(t, bodies[0])
					require.GreaterOrEqual(t, times[1].Sub(times[0]), 240*time.Millisecond)
					for _, hdr := range headers {
						require.Len(t, hdr.Values("Authorization"), 1)
					}
					require.Eventually(t, func() bool { return spy.calls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
					require.Never(t, func() bool { return spy.calls.Load() != 1 }, 200*time.Millisecond, 10*time.Millisecond, "failed attempt must not record usage")
				})
			}
		}
	}
}

func TestDroidBodyReadFailurePersistentIsBounded(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"truncate", "timeout"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", entrance.name, stream, mode), func(t *testing.T) {
					spy := &droidUsageSpy{}
					var active sync.WaitGroup
					front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
						active.Add(1)
						defer active.Done()
						_, _ = io.ReadAll(r.Body)
						droidBodyReadFailure(w, r, stream, mode)
					}, droidFixtureOptions{single: true, usage: spy}, 300*time.Millisecond)
					body := entrance.body
					if stream {
						body = `{"stream":true,` + body[1:]
					}
					start := time.Now()
					status, _, got := droidPost(t, front, entrance.path, body)
					require.Less(t, time.Since(start), service.DroidRetryBudget+time.Second)
					ids, _, bodies, _ := u.snapshot()
					require.Equal(t, []int64{801, 801, 801}, ids, "same-account body-read retries must be bounded")
					require.Equal(t, bodies[0], bodies[1])
					require.Equal(t, bodies[0], bodies[2])
					require.GreaterOrEqual(t, status, 400, got)
					require.Contains(t, got, "error")
					require.NotContains(t, got, "factory_test_secret")
					waited := make(chan struct{})
					go func() { active.Wait(); close(waited) }()
					select {
					case <-waited:
					case <-time.After(2 * time.Second):
						t.Fatal("upstream attempts were not closed")
					}
					require.Never(t, func() bool { return spy.calls.Load() != 0 }, 200*time.Millisecond, 10*time.Millisecond, "failed attempts must not record usage")
				})
			}
		}
	}
}

func TestDroidBodyReadFailureAfterFirstEventNeverReplays(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			read := make(chan struct{})
			var once sync.Once
			signal := func() { once.Do(func() { close(read) }) }
			defer signal()
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				first, _, _ := strings.Cut(droidHealthySSE(r.URL.Path), "\n\n")
				_, _ = io.WriteString(w, first+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-read:
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Second):
					return
				}
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			}, droidFixtureOptions{single: true}, 4*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+entrance.path, strings.NewReader(`{"stream":true,`+entrance.body[1:]))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			reader := bufio.NewReader(resp.Body)
			var event strings.Builder
			for {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				event.WriteString(line)
				if line == "\n" && strings.Contains(event.String(), "data:") {
					break
				}
			}
			marker := map[string]string{"messages": `"type":"message_start"`, "responses": `"type":"response.created"`, "chat": `"content":"healthy"`}[entrance.name]
			require.Contains(t, event.String(), marker)
			signal()
			rest, _ := io.ReadAll(reader)
			require.NotContains(t, string(rest), "factory_test_secret")
			require.NotContains(t, string(rest), marker, "a replayed attempt would resend the first event")
			require.NotContains(t, string(rest), droidStreamTerminal(entrance.name), "a truncated committed stream must not gain a success terminal")
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids, "client-visible first event closes the replay window")
		})
	}
}

// Responses/Chat requests for claude-* models are converted to Factory's
// Anthropic endpoint; the upstream is always an Anthropic SSE stream.
var droidAnthropicConvertEntrances = []struct{ name, path, body string }{
	{"responses", "/v1/responses", `{"model":"claude-sonnet-4-6","input":"hi"}`},
	{"chat", "/v1/chat/completions", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`},
}

const droidAnthropicPath = "/api/llm/a/v1/messages"

// droidAnthropicReadFailure commits 200 + Flush, then fails before any
// convertible event is complete. The parent context stays valid throughout.
func droidAnthropicReadFailure(w http.ResponseWriter, r *http.Request, mode string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if mode == "truncate" || mode == "eof" {
		// An event line whose data line never arrives: no complete event.
		_, _ = io.WriteString(w, "event: message_start\n")
	}
	w.(http.Flusher).Flush()
	switch mode {
	case "truncate":
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	case "eof":
		return // clean end of body without message_stop
	}
	// timeout / idle: block until the upstream client gives up and closes.
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func droidRequireConvertedClient(t *testing.T, entrance string, stream bool, got string) {
	t.Helper()
	require.Contains(t, got, "healthy")
	require.NotContains(t, got, "factory_test_secret")
	require.NotContains(t, got, "message_start", "client must receive converted protocol, not raw Anthropic SSE")
	switch {
	case entrance == "responses" && stream:
		require.Contains(t, got, `"type":"response.created"`)
		require.Contains(t, got, `"type":"response.completed"`)
	case entrance == "responses":
		require.Contains(t, got, `"object":"response"`)
		require.NotContains(t, got, "data:")
	case stream:
		require.Contains(t, got, `"object":"chat.completion.chunk"`)
		require.Contains(t, got, "data: [DONE]")
	default:
		require.Contains(t, got, `"object":"chat.completion"`)
		require.NotContains(t, got, "data:")
	}
}

func droidClientBody(body string, stream bool) string {
	if stream {
		return `{"stream":true,` + body[1:]
	}
	return body
}

func TestDroidAnthropicConvertBodyReadFailureSameAccountRetry(t *testing.T) {
	for _, entrance := range droidAnthropicConvertEntrances {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"truncate", "eof", "timeout", "idle"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", entrance.name, stream, mode), func(t *testing.T) {
					var calls atomic.Int32
					var pathMu sync.Mutex
					var paths []string
					spy := &droidUsageSpy{}
					failedDone := make(chan struct{})
					options := droidFixtureOptions{usage: spy}
					clientTimeout := 400 * time.Millisecond
					if mode == "idle" {
						// Line-pump idle sentinel fires first; HTTP client timeout is later.
						options.streamIntervalSeconds = 1
						clientTimeout = 2500 * time.Millisecond
					}
					front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.ReadAll(r.Body)
						pathMu.Lock()
						paths = append(paths, r.URL.Path)
						pathMu.Unlock()
						if calls.Add(1) == 1 {
							defer close(failedDone)
							droidAnthropicReadFailure(w, r, mode)
							return
						}
						if spy.calls.Load() != 0 {
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						droidHealthyStreamHTTP(w, r)
					}, options, clientTimeout)
					start := time.Now()
					status, headers, got := droidPost(t, front, entrance.path, droidClientBody(entrance.body, stream))
					require.Less(t, time.Since(start), service.DroidRetryBudget+time.Second)
					select {
					case <-failedDone:
					case <-time.After(2 * time.Second):
						t.Fatal("failed upstream attempt was never closed")
					}
					require.Equal(t, 200, status, got)
					if stream {
						require.Contains(t, headers.Get("Content-Type"), "text/event-stream")
					} else {
						require.Contains(t, headers.Get("Content-Type"), "application/json")
					}
					droidRequireConvertedClient(t, entrance.name, stream, got)
					ids, times, bodies, hdrs := u.snapshot()
					require.Equal(t, []int64{801, 801}, ids, "body-read failure must retry the pinned account")
					require.Equal(t, int32(2), calls.Load())
					pathMu.Lock()
					require.Equal(t, []string{droidAnthropicPath, droidAnthropicPath}, paths)
					pathMu.Unlock()
					require.NotEmpty(t, bodies[0])
					require.Equal(t, bodies[0], bodies[1], "replayed upstream request body must be identical")
					require.Contains(t, bodies[0], `"stream":true`, "conversion always requests an upstream stream")
					require.GreaterOrEqual(t, times[1].Sub(times[0]), 240*time.Millisecond)
					for _, hdr := range hdrs {
						require.Len(t, hdr.Values("Authorization"), 1)
					}
					require.Eventually(t, func() bool { return spy.calls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
					require.Never(t, func() bool { return spy.calls.Load() != 1 }, 200*time.Millisecond, 10*time.Millisecond, "failed attempt must not record usage")
				})
			}
		}
	}
}

func TestDroidAnthropicConvertBodyReadFailurePersistentIsBounded(t *testing.T) {
	for _, entrance := range droidAnthropicConvertEntrances {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"truncate", "timeout"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", entrance.name, stream, mode), func(t *testing.T) {
					spy := &droidUsageSpy{}
					var active sync.WaitGroup
					var calls atomic.Int32
					front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
						active.Add(1)
						defer active.Done()
						calls.Add(1)
						_, _ = io.ReadAll(r.Body)
						droidAnthropicReadFailure(w, r, mode)
					}, droidFixtureOptions{single: true, usage: spy}, 300*time.Millisecond)
					start := time.Now()
					status, _, got := droidPost(t, front, entrance.path, droidClientBody(entrance.body, stream))
					require.Less(t, time.Since(start), service.DroidRetryBudget+time.Second)
					ids, _, bodies, _ := u.snapshot()
					require.Equal(t, []int64{801, 801, 801}, ids, "original attempt plus at most two same-account retries")
					require.Equal(t, int32(3), calls.Load())
					require.Equal(t, bodies[0], bodies[1])
					require.Equal(t, bodies[0], bodies[2])
					require.GreaterOrEqual(t, status, 400, got)
					require.Contains(t, got, "error")
					require.NotContains(t, got, "factory_test_secret")
					waited := make(chan struct{})
					go func() { active.Wait(); close(waited) }()
					select {
					case <-waited:
					case <-time.After(2 * time.Second):
						t.Fatal("upstream attempts were not closed")
					}
					require.Never(t, func() bool { return spy.calls.Load() != 0 }, 200*time.Millisecond, 10*time.Millisecond, "failed attempts must not record usage")
				})
			}
		}
	}
}

func TestDroidAnthropicConvertAfterFirstEventNeverReplays(t *testing.T) {
	healthy := droidHealthySSE(droidAnthropicPath)
	events := strings.SplitAfter(healthy, "\n\n")
	for _, entrance := range droidAnthropicConvertEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			read := make(chan struct{})
			var once sync.Once
			signal := func() { once.Do(func() { close(read) }) }
			defer signal()
			upstreamDone := make(chan struct{})
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamDone)
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				// Responses: message_start alone converts to response.created.
				// Chat: text only becomes a chunk once a content delta arrives.
				prefix := events[0]
				if entrance.name == "chat" {
					prefix = events[0] + events[1] + events[2]
				}
				_, _ = io.WriteString(w, prefix)
				w.(http.Flusher).Flush()
				select {
				case <-read:
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Second):
					return
				}
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			}, droidFixtureOptions{single: true}, 4*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+entrance.path, strings.NewReader(droidClientBody(entrance.body, true)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			reader := bufio.NewReader(resp.Body)
			var event strings.Builder
			for {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				event.WriteString(line)
				if line == "\n" && strings.Contains(event.String(), "data:") {
					break
				}
			}
			first := event.String()
			require.False(t, strings.HasPrefix(first, ":"), "comment/keepalive must not stand in for the first event")
			require.NotContains(t, first, "ping")
			var marker string
			if entrance.name == "responses" {
				marker = `"type":"response.created"`
				require.Contains(t, first, marker)
				require.NotContains(t, first, "response.completed")
			} else {
				marker = `"object":"chat.completion.chunk"`
				require.Contains(t, first, marker)
				require.NotContains(t, first, "[DONE]")
			}
			signal()
			rest, _ := io.ReadAll(reader)
			require.NotContains(t, string(rest), "factory_test_secret")
			require.NotContains(t, string(rest), `"type":"response.created"`, "a replayed attempt would resend the first event")
			require.NotContains(t, string(rest), "response.completed", "a truncated committed stream must not gain a success terminal")
			require.NotContains(t, string(rest), "[DONE]", "a truncated committed stream must not gain a success terminal")
			select {
			case <-upstreamDone:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream handler was not released")
			}
			ids, _, _, _ := u.snapshot()
			require.Equal(t, []int64{801}, ids, "client-visible converted event closes the replay window")
		})
	}
}

// After same-account retries exhaust part of the request budget, an ordinary
// switch to another (occupied) account must not wait the scheduler's full
// fallback queue timeout (30s here): admission stays within the one budget.
func TestDroidRetryBudgetBoundsCrossAccountAdmission(t *testing.T) {
	for _, entrance := range droidRegressionEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			state := &droidAdmissionFixture{blocked: make(chan struct{}), onlyID: 802}
			state.occupied.Store(true)
			parent, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			handlerDone := make(chan struct{})
			front, u := droidLocalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(502)
				_, _ = io.WriteString(w, `{"error":{"message":"factory_test_secret"}}`)
			}, droidFixtureOptions{admission: state, parent: parent, handlerDone: handlerDone}, time.Second)
			type result struct {
				status int
				body   string
				err    error
			}
			resultCh := make(chan result, 1)
			start := time.Now()
			go func() {
				resp, err := http.Post(front.URL+entrance.path, "application/json", strings.NewReader(entrance.body))
				if err != nil {
					resultCh <- result{err: err}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				resultCh <- result{status: resp.StatusCode, body: string(body), err: err}
			}()
			select {
			case <-handlerDone:
			case <-time.After(service.DroidRetryBudget + 1500*time.Millisecond):
				t.Fatal("cross-account admission exceeded the request retry budget; fallback queue timeout leaked")
			}
			got := <-resultCh
			require.NoError(t, got.err)
			require.GreaterOrEqual(t, got.status, 400, got.body)
			require.NotContains(t, got.body, "factory_test_secret")
			require.Less(t, time.Since(start), service.DroidRetryBudget+1500*time.Millisecond)
			require.Zero(t, state.waiting.Load(), "wait counter leaked")
			ids, _, _, _ := u.snapshot()
			for _, id := range ids {
				require.Equal(t, int64(801), id, "occupied account must never be sent to")
			}
			require.NotEmpty(t, ids)
		})
	}
}
