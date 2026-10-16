//go:build unit

package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newDroidHandler(t *testing.T, upstream service.HTTPUpstream) (*gin.Engine, *grokCredentialHandlerRepo) {
	t.Helper()
	accounts := []service.Account{}
	for i := int64(801); i <= 802; i++ {
		accounts = append(accounts, service.Account{ID: i, Platform: service.PlatformDroid, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: int(i - 800), Credentials: map[string]any{"api_key": "factory_test_secret"}})
	}
	repo := &grokCredentialHandlerRepo{accounts: accounts}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), service.NewRateLimitService(repo, nil, cfg, nil, nil), billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }, acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	groupID := int64(901)
	key := &service.APIKey{ID: 902, GroupID: &groupID, User: &service.User{ID: 903, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformDroid, Status: service.StatusActive}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
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
