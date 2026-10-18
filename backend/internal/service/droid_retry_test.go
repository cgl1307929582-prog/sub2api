//go:build unit

package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"net/http/httptest"

	"github.com/stretchr/testify/require"
)

func TestDroidRetryTransportClassificationPreserved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		persistent bool
	}{
		{"refused", syscall.ECONNREFUSED, true},
		{"dns", &net.DNSError{Err: "no such host factory_test_secret", Name: "secret", IsNotFound: true}, true},
		{"proxy_auth", errors.New("proxy authentication required factory_test_secret"), true},
		{"reset", syscall.ECONNRESET, false},
		{"eof", io.EOF, false},
		{"timeout", &net.DNSError{Err: "i/o timeout factory_test_secret", IsTimeout: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			safe := safeDroidError(tc.err)
			require.Equal(t, tc.persistent, classifyUpstreamTransportError(safe).Persistent)
			require.NotContains(t, safe.Error(), "factory_test_secret")
		})
	}
	require.ErrorIs(t, safeDroidError(context.Canceled), context.Canceled)
	require.ErrorIs(t, safeDroidError(context.DeadlineExceeded), context.DeadlineExceeded)
}

type droidRetryCloseSpy struct {
	io.Reader
	closed atomic.Int32
}

func (s *droidRetryCloseSpy) Close() error { s.closed.Add(1); return nil }

func TestDroidRetryRoundTripHasNoInternalReplayAndClosesFailure(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(map[bool]string{false: "http_failure", true: "transport_failure"}[transportError], func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
			require.NoError(t, err)
			source := &droidRetryCloseSpy{Reader: strings.NewReader(`{"error":{"message":"factory_test_secret"}}`)}
			calls := 0
			response, err := droidRoundTrip(request, droidTestAccount(), func(*http.Request) (*http.Response, error) {
				calls++
				response := &http.Response{StatusCode: 502, Header: make(http.Header), Body: source}
				if transportError {
					return response, syscall.ECONNRESET
				}
				return response, nil
			})
			require.Equal(t, 1, calls, "only the handler may own replay")
			require.Equal(t, int32(1), source.closed.Load())
			if transportError {
				require.Error(t, err)
				require.Nil(t, response)
			} else {
				require.NoError(t, err)
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NotContains(t, string(body), "factory_test_secret")
			}
		})
	}
}

func TestDroidRetryRequestPreparationIndependent(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/api/llm/a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`},
		{"/api/llm/o/v1/responses", `{"model":"gpt-5.4","input":"hi"}`},
		{"/api/llm/o/v1/chat/completions", `{"model":"glm-5","messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "https://api.factory.ai"+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			var first string
			for attempt := 0; attempt < 3; attempt++ {
				prepared, err := prepareDroidRequest(request, droidTestAccount())
				require.NoError(t, err)
				body, err := io.ReadAll(prepared.Body)
				require.NoError(t, err)
				require.NoError(t, prepared.Body.Close())
				if attempt == 0 {
					first = string(body)
				} else {
					require.Equal(t, first, string(body))
				}
				require.Equal(t, 1, strings.Count(string(body), droidSystemPrompt))
				require.Len(t, prepared.Header.Values("Authorization"), 1)
				require.Len(t, prepared.Header.Values("X-Session-ID"), 1)
			}
			original, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(original))
		})
	}
}

func TestDroidRetryHTTPMetadata(t *testing.T) {
	s := &OpenAIGatewayService{}
	for _, status := range []int{500, 502, 503, 504, 401, 400, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			headers := http.Header{"Retry-After": {"1"}}
			failure := s.newOpenAIAccountFailoverError(droidTestAccount(), status, headers, []byte(`{"error":{"message":"upstream failed"}}`), "upstream failed", status == 429, false)
			require.Equal(t, status >= 500 || status == 429, failure.RetryableOnSameAccount)
			require.Equal(t, headers, failure.ResponseHeaders)
			require.True(t, failure.SameAccountRetryDeadline.IsZero(), "the handler owns the request budget")
			if failure.RetryableOnSameAccount {
				want := 2
				if status == 429 {
					want = 1
				}
				require.Equal(t, want, failure.SameAccountRetryMax)
			}
		})
	}
}

func TestDroidRetryTransportMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"reset", syscall.ECONNRESET, true}, {"eof", io.EOF, true},
		{"independent_timeout", context.DeadlineExceeded, true},
		{"dial_timeout", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, true},
		{"refused", syscall.ECONNREFUSED, false},
		{"dns", &net.DNSError{Err: "no such host", IsNotFound: true}, false},
		{"proxy_auth", errors.New("proxy authentication required"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			s := &OpenAIGatewayService{}
			err := s.handleOpenAIUpstreamTransportError(context.Background(), c, droidTestAccount(), safeDroidError(tc.err), false)
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, tc.retry, failure.RetryableOnSameAccount)
			if tc.retry {
				require.Equal(t, 2, failure.SameAccountRetryMax)
				require.Equal(t, 300*time.Millisecond, failure.SameAccountRetryDelay)
				require.True(t, failure.SameAccountRetryDeadline.IsZero())
			}
		})
	}
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if expired {
			var end context.CancelFunc
			ctx, end = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer end()
		}
		cancel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		err := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(ctx, c, droidTestAccount(), safeDroidError(io.EOF), false)
		var failure *UpstreamFailoverError
		require.False(t, errors.As(err, &failure))
		require.ErrorIs(t, err, ctx.Err())
	}
}

func TestDroidRetryMetadataExclusionsAndNonDroid(t *testing.T) {
	s := &OpenAIGatewayService{}
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		disabled bool
	}{
		{"auth", 401, `{}`, false},
		{"request", 400, `{}`, false},
		{"forbidden", 403, `{}`, false},
		{"disabled", 502, `{}`, true},
		{"cyber", 503, `{"error":{"code":"cyber_policy","message":"blocked"}}`, false},
		{"access_state", 503, `{"error":{"code":"account_deactivated"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := s.newOpenAIAccountFailoverError(droidTestAccount(), tc.status, nil, []byte(tc.body), "", tc.disabled, true)
			require.False(t, failure.RetryableOnSameAccount)
		})
	}
	for _, value := range []string{"", "invalid", "30", "0"} {
		failure := s.newOpenAIAccountFailoverError(droidTestAccount(), 429, http.Header{"Retry-After": {value}}, nil, "", true, true)
		require.False(t, failure.RetryableOnSameAccount, value)
		require.Equal(t, value, failure.ResponseHeaders.Get("Retry-After"))
	}
	account := *droidTestAccount()
	account.Platform = PlatformOpenAI
	failure := s.newOpenAIAccountFailoverError(&account, 502, nil, nil, "", false, false)
	require.False(t, failure.RetryableOnSameAccount)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	err := s.handleOpenAIUpstreamTransportError(context.Background(), c, &account, syscall.ECONNRESET, false)
	require.ErrorAs(t, err, &failure)
	require.False(t, failure.RetryableOnSameAccount)
}

func TestDroidRetryFixedAccountAdmission(t *testing.T) {
	a := *droidTestAccount()
	b := a
	b.ID++
	s := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{a, b}}}
	single, err := s.IsSingleDroidRetryAccount(context.Background(), nil, a.ID, "gpt-5.4", false, "")
	require.NoError(t, err)
	require.False(t, single)
	selection, err := s.SelectDroidRetryAccount(context.Background(), nil, b.ID, "gpt-5.4", false, "")
	require.NoError(t, err)
	require.Equal(t, b.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	reset := time.Now().Add(time.Minute)
	b.RateLimitResetAt = &reset
	s.accountRepo = stubOpenAIAccountRepo{accounts: []Account{a, b}}
	_, err = s.SelectDroidRetryAccount(context.Background(), nil, b.ID, "gpt-5.4", false, "")
	require.Error(t, err, "must not bypass target cooldown or select the other account")
	single, err = s.IsSingleDroidRetryAccount(context.Background(), nil, a.ID, "gpt-5.4", false, "")
	require.NoError(t, err)
	require.True(t, single)
}

func TestDroidRetryHTTPPipelineKeepsAccountState(t *testing.T) {
	for _, status := range []int{401, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			cfg := &config.Config{}
			s := &OpenAIGatewayService{cfg: cfg, rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil)}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			response := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"1"}}}
			failure := s.failoverOpenAIUpstreamHTTPError(context.Background(), c, droidTestAccount(), response, []byte(`{"error":{"message":"upstream failed"}}`), "upstream failed", "gpt-5.4")
			require.NotNil(t, failure)
			require.Equal(t, status != 401, failure.RetryableOnSameAccount)
			require.Equal(t, "1", failure.ResponseHeaders.Get("Retry-After"))
			if status == 429 {
				require.Equal(t, 1, repo.rateLimitedCalls)
				require.Equal(t, int64(801), repo.lastRateLimitedID)
				require.True(t, repo.lastRateLimitedAt.After(time.Now()))
			}
			if status == 401 {
				require.Equal(t, 1, repo.setErrorCalls)
				require.Zero(t, repo.updateCredentialsCalls)
			}
		})
	}
}

func TestDroidRetryCanceledRoundTripNeverSends(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.factory.ai/api/llm/o/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	require.NoError(t, err)
	calls := 0
	_, err = droidRoundTrip(request, droidTestAccount(), func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF })
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls)
}

func TestDroidBodyReadRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"eof", io.EOF, true}, {"unexpected_eof", io.ErrUnexpectedEOF, true},
		{"reset", fmt.Errorf("secret-key: %w", syscall.ECONNRESET), true},
		{"timeout", &net.OpError{Op: "read", Err: context.DeadlineExceeded}, true},
		{"canceled", context.Canceled, false}, {"too_large", ErrUpstreamResponseBodyTooLarge, false},
		{"scanner_limit", bufio.ErrTooLong, false}, {"guarded_limit", errOpenAIFirstOutputScannerLimit, false},
		{"refused", syscall.ECONNREFUSED, false}, {"arbitrary", errors.New("secret-key"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, safeDroidError(tc.err), safeDroidError(safeDroidError(tc.err))} {
				require.Equal(t, tc.retry, isDroidRecoverableBodyReadError(err))
			}
			require.NotContains(t, safeDroidError(tc.err).Error(), "secret-key")
		})
	}
}

func TestDroidBodyReadRetryCommitAndParentGuards(t *testing.T) {
	for _, mode := range []string{"retry", "child_timeout", "cancel", "deadline", "header_flush", "write", "non_droid", "too_large"} {
		t.Run(mode, func(t *testing.T) {
			parent := context.Background()
			if mode == "cancel" {
				ctx, cancel := context.WithCancel(parent)
				cancel()
				parent = ctx
			}
			if mode == "deadline" {
				ctx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer cancel()
				parent = ctx
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(parent)
			ctx := parent
			if mode == "child_timeout" {
				child, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer cancel()
				ctx = child
			}
			if mode == "header_flush" {
				c.Writer.Flush()
			}
			if mode == "write" {
				_, _ = c.Writer.WriteString("data: {}\n\n")
			}
			a := *droidTestAccount()
			if mode == "non_droid" {
				a.Platform = PlatformOpenAI
			}
			err := error(context.DeadlineExceeded)
			if mode == "too_large" {
				err = ErrUpstreamResponseBodyTooLarge
			}
			body := &droidRetryCloseSpy{Reader: strings.NewReader("")}
			failure := droidBodyReadFailover(ctx, c, &a, &http.Response{Body: body}, err)
			if mode != "retry" && mode != "child_timeout" {
				require.Nil(t, failure)
				require.Zero(t, body.closed.Load())
				return
			}
			require.NotNil(t, failure)
			require.True(t, failure.RetryableOnSameAccount)
			require.Equal(t, 2, failure.SameAccountRetryMax)
			require.Equal(t, 300*time.Millisecond, failure.SameAccountRetryDelay)
			require.True(t, failure.SameAccountRetryDeadline.IsZero())
			require.Equal(t, int32(1), body.closed.Load())
			require.False(t, c.Writer.Written())
		})
	}
}

type droidBodyReadErrorReader struct{ err error }

func (r droidBodyReadErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestDroidBodyReadFailuresReturnNoUsage(t *testing.T) {
	s := &OpenAIGatewayService{}
	for _, endpoint := range []string{"responses", "messages", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, readErr := range []error{io.ErrUnexpectedEOF, context.DeadlineExceeded, io.EOF} {
				if !stream && readErr == io.EOF {
					continue
				} // io.ReadAll treats EOF as successful completion.
				t.Run(fmt.Sprintf("%s/stream=%v/%v", endpoint, stream, readErr), func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil)
					body := &droidRetryCloseSpy{Reader: droidBodyReadErrorReader{safeDroidError(readErr)}}
					resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}
					var err error
					switch endpoint {
					case "responses":
						if stream {
							r, e := s.handleStreamingResponseWithReasoning(context.Background(), resp, c, droidTestAccount(), time.Now(), "gpt-5.4", "gpt-5.4", "")
							require.Nil(t, r)
							err = e
						} else {
							r, e := s.handleNonStreamingResponse(context.Background(), resp, c, droidTestAccount(), "gpt-5.4", "gpt-5.4")
							require.Nil(t, r)
							err = e
						}
					case "messages":
						var r *OpenAIForwardResult
						if stream {
							r, err = s.handleNativeAnthropicStreamingResponse(context.Background(), resp, c, droidTestAccount(), "claude", "claude", "claude", nil, time.Now())
						} else {
							r, err = s.handleNativeAnthropicBufferedResponse(context.Background(), resp, c, droidTestAccount(), "claude", "claude", "claude", nil, time.Now())
						}
						require.Nil(t, r)
					case "chat":
						var r *OpenAIForwardResult
						if stream {
							r, err = s.streamRawChatCompletions(c, resp, droidTestAccount(), "glm-5", "glm-5", "glm-5", nil, nil, time.Now(), 10)
						} else {
							r, err = s.bufferRawChatCompletions(c, resp, droidTestAccount(), "glm-5", "glm-5", "glm-5", nil, nil, time.Now())
						}
						require.Nil(t, r)
					}
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.True(t, failure.RetryableOnSameAccount)
					require.False(t, c.Writer.Written())
					require.Positive(t, body.closed.Load())
				})
			}
		}
	}
}

// Exercise both native-Anthropic cross-protocol converters, not their same-protocol siblings.
func callDroidAnthropicConverter(s *OpenAIGatewayService, entry string, stream bool, resp *http.Response, c *gin.Context, account *Account) (*OpenAIForwardResult, error) {
	if entry == "responses" {
		if stream {
			return s.handleResponsesStreamingFromNativeAnthropic(resp, c, "claude", "claude", "claude", nil, time.Now(), apicompat.ResponsesClientToolMapping{}, true, account)
		}
		return s.handleResponsesBufferedFromNativeAnthropic(resp, c, "claude", "claude", "claude", nil, time.Now(), apicompat.ResponsesClientToolMapping{}, true, account)
	}
	if stream {
		return s.handleCCStreamingFromNativeAnthropic(resp, c, "claude", "claude", "claude", nil, time.Now(), true, account)
	}
	return s.handleCCBufferedFromNativeAnthropic(resp, c, "claude", "claude", "claude", nil, time.Now(), true, account)
}

func TestDroidAnthropicConvertersBodyReadRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name string
				err  error
			}{
				{"eof", io.EOF},
				{"unexpected_eof", io.ErrUnexpectedEOF},
				{"reset", syscall.ECONNRESET},
				{"timeout", context.DeadlineExceeded},
				{"net_timeout", &net.DNSError{IsTimeout: true}},
			} {
				// Both pump.next sites must preserve and classify the actual read error.
				for _, prefix := range []string{"", "event: ping\n"} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/after_event=%t", entry, stream, tc.name, prefix != ""), func(t *testing.T) {
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
						body := &droidRetryCloseSpy{Reader: io.MultiReader(strings.NewReader(prefix), droidBodyReadErrorReader{tc.err})}
						resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}
						result, err := callDroidAnthropicConverter(&OpenAIGatewayService{}, entry, stream, resp, c, droidTestAccount())
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.True(t, failover.RetryableOnSameAccount)
						require.Equal(t, 2, failover.SameAccountRetryMax)
						require.Equal(t, 300*time.Millisecond, failover.SameAccountRetryDelay)
						require.Nil(t, result, "failed attempts must not return billable usage")
						require.False(t, c.Writer.Written(), "retry classification must precede any fallback response")
						require.Equal(t, int32(1), body.closed.Load())
					})
				}
			}
		}
	}
}

func TestDroidAnthropicConvertersRetryExclusions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, exclusion := range []string{"non_droid", "canceled", "deadline", "header_flush", "bytes_written", "scan_limit", "malformed", "protocol_error", "body_limit", "arbitrary_error", "terminal"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", entry, stream, exclusion), func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx := context.Background()
					account := droidTestAccount()
					var reader io.Reader = droidBodyReadErrorReader{io.ErrUnexpectedEOF}
					s := &OpenAIGatewayService{}
					switch exclusion {
					case "non_droid":
						account = &Account{Platform: PlatformOpenAI}
					case "canceled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					case "deadline":
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
						defer cancel()
					case "header_flush":
						c.Writer.Flush()
					case "bytes_written":
						_, _ = c.Writer.Write([]byte("committed"))
					case "scan_limit":
						reader = strings.NewReader(strings.Repeat("x", 128*1024))
						s.cfg = &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 64 * 1024}}
					case "malformed":
						reader = strings.NewReader("event: message_start\ndata: {invalid}\n\n")
					case "protocol_error":
						reader = strings.NewReader("event: error\ndata: {\"type\":\"error\"}\n\n")
					case "body_limit":
						reader = droidBodyReadErrorReader{ErrUpstreamResponseBodyTooLarge}
					case "arbitrary_error":
						reader = droidBodyReadErrorReader{errors.New("invalid upstream payload")}
					case "terminal":
						reader = io.MultiReader(strings.NewReader(miniAnthropicSSEStream()), droidBodyReadErrorReader{io.ErrUnexpectedEOF})
					}
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
					body := &droidRetryCloseSpy{Reader: reader}
					defer body.Close() // production forwarder owns cleanup of non-retry results
					result, err := callDroidAnthropicConverter(s, entry, stream, &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, c, account)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "must not turn protocol, parent or post-commit failures into replay")
					require.Zero(t, body.closed.Load(), "classifier must leave excluded reads to existing cleanup")
					if exclusion == "terminal" {
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Positive(t, result.Usage.InputTokens)
					} else {
						require.Error(t, err)
					}
				})
			}
		}
	}
}

func TestDroidAnthropicConvertersIdleBeforeCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, prefix := range []string{"", "event: ping\n"} {
				t.Run(fmt.Sprintf("%s/stream=%t/after_event=%t", entry, stream, prefix != ""), func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
					pr, pw := io.Pipe()
					defer pw.Close()
					defer pr.Close()
					body := &droidRetryCloseSpy{Reader: pr}
					// Close must unblock the scanner as a real HTTP response body does.
					wrapped := &droidAnthropicPipeBody{droidRetryCloseSpy: body, pipe: pr}
					if prefix != "" {
						go func() { _, _ = io.WriteString(pw, prefix) }()
					}
					result, err := callDroidAnthropicConverter(newNativeAnthropicHangTestService(1), entry, stream, &http.Response{StatusCode: 200, Header: make(http.Header), Body: wrapped}, c, droidTestAccount())
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.RetryableOnSameAccount)
					require.Nil(t, result)
					require.False(t, c.Writer.Written())
					require.Equal(t, int32(1), body.closed.Load())
				})
			}
		}
	}
}

type droidAnthropicPipeBody struct {
	*droidRetryCloseSpy
	pipe *io.PipeReader
}

func (b *droidAnthropicPipeBody) Close() error {
	_ = b.droidRetryCloseSpy.Close()
	return b.pipe.Close()
}

func TestDroidAnthropicConvertersPartialUsageAndCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	partial := strings.Split(miniAnthropicSSEStream(), "event: content_block_start")[0]
	for _, entry := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", entry, stream), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				body := &droidRetryCloseSpy{Reader: io.MultiReader(strings.NewReader(partial), droidBodyReadErrorReader{io.ErrUnexpectedEOF})}
				defer body.Close()
				result, err := callDroidAnthropicConverter(&OpenAIGatewayService{}, entry, stream, &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, c, droidTestAccount())
				var failover *UpstreamFailoverError
				if stream {
					require.True(t, c.Writer.Written(), "converted first event must commit immediately")
					require.False(t, errors.As(err, &failover))
					require.Error(t, err)
					require.NotNil(t, result)
					require.Equal(t, 10, result.Usage.InputTokens, "post-commit partial usage must be retained")
				} else {
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result, "even accumulated failed-attempt usage must be discarded before replay")
					require.False(t, c.Writer.Written())
					require.Equal(t, int32(1), body.closed.Load())
				}
			})
		}
	}
}
