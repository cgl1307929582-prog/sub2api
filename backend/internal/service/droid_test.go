//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func droidTestAccount() *Account {
	return &Account{ID: 801, Name: "factory-test", Platform: PlatformDroid, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "factory_test_secret"}}
}

func TestDroidRoundTripSanitizesNestedErrorsBeforeSharedExtraction(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
			require.NoError(t, err)
			body := droidNestedErrorBody(t, `factory_test_\u0073ecret`, 1)
			status, contentType := 401, "application/json"
			if stream {
				status, contentType = 200, "text/event-stream"
				body = append(append([]byte("event: error\ndata: "), body...), []byte("\n\n")...)
			}
			resp, err := droidRoundTrip(request, droidTestAccount(), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			redacted, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if stream {
				redacted = bytes.TrimSuffix(bytes.TrimPrefix(redacted, []byte("event: error\ndata: ")), []byte("\n\n"))
			}
			require.Equal(t, "invalid [REDACTED]", extractUpstreamErrorMessage(redacted))
		})
	}
}

func TestDroidGatewayProtocolMatrix(t *testing.T) {
	for _, model := range DefaultDroidModelIDs() {
		for _, ingress := range routingMatrixIngresses() {
			t.Run(model+"/"+ingress.name, func(t *testing.T) {
				upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				body := routingMatrixCase{ingress: ingress, model: model}.body()
				_ = ingress.forward(svc, adaptiveProtocolTestContext(ingress.path, body), droidTestAccount(), body)
				require.Len(t, upstream.requests, 1)
				request := upstream.requests[0]
				protocol, provider, path := droidModelRoute(model)
				if provider == "google" {
					path = droidGooglePath
				}
				require.Equal(t, "https://api.factory.ai"+path, request.URL.String())
				require.Equal(t, "Bearer factory_test_secret", request.Header.Get("Authorization"))
				require.Equal(t, provider, request.Header.Get("X-API-Provider"))
				require.Equal(t, "cli", request.Header.Get("X-Factory-Client"))
				require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
				require.True(t, HTTPUpstreamRedirectsDisabled(request.Context()))
				if provider == "google" {
					require.True(t, gjson.GetBytes(upstream.lastBody, "contents").Exists())
				} else if protocol == APIProtocolResponses {
					require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
				} else {
					require.True(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
				}
			})
		}
	}
}

func TestDroidAccountConnectionUsesSameTransport(t *testing.T) {
	for _, tc := range []struct {
		model    string
		response func() *http.Response
	}{
		{"claude-sonnet-4-6", adaptiveCNAnthropicTestResponse}, {"gpt-5.4", adaptiveCNResponsesTestResponse}, {"glm-5", adaptiveCNChatTestResponse},
	} {
		t.Run(tc.model, func(t *testing.T) {
			account := droidTestAccount()
			svc, upstream := adaptiveCNAccountTestService(account, tc.response())
			c, recorder := newTestContext()
			require.NoError(t, svc.TestAccountConnection(c, account.ID, tc.model, "hi", AccountTestModeDefault))
			require.Len(t, upstream.requests, 1)
			_, _, path := droidModelRoute(tc.model)
			require.Equal(t, path, upstream.requests[0].URL.Path)
			require.Equal(t, "Bearer factory_test_secret", upstream.requests[0].Header.Get("Authorization"))
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

func TestDroidAccountTestDoesNotInjectClaudeCodeIdentity(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			account := droidTestAccount()
			svc, upstream := adaptiveCNAccountTestService(account, adaptiveCNAnthropicTestResponse())
			c, recorder := newTestContext()
			require.NoError(t, svc.TestAccountConnection(c, account.ID, model, "hi", AccountTestModeDefault))
			// Factory rejects the generic probe's Claude Code identity with 403,
			// even when the shared transport has prepended the Droid identity.
			require.NotContains(t, string(upstream.lastBody), claudeCodeSystemPrompt)
			require.Equal(t, droidSystemPrompt, gjson.GetBytes(upstream.lastBody, "system").String())
			require.Equal(t, "hi", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.text").String())
			require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
			require.Contains(t, recorder.Body.String(), `"success":true`)
		})
	}
	// Other providers still use the original Claude Code probe payload.
	payload, err := createTestPayload("claude-opus-4-6")
	require.NoError(t, err)
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Contains(t, string(encoded), claudeCodeSystemPrompt)
}

func TestDroidNativeMessagesPreservesToolsThinkingAndUsage(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":2048,"stream":false,"system":"Keep my prompt","thinking":{"type":"enabled","budget_tokens":1024},"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"signature"},{"type":"tool_use","id":"tool_1","name":"lookup","input":{"query":"hello"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"found"}]}]}`)
	responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"tool_2","name":"lookup","input":{"query":"next"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":7,"cache_read_input_tokens":9,"cache_creation_input_tokens":5}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := newTestContext()
	result, err := svc.ForwardAsAnthropic(context.Background(), c, droidTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Equal(t, droidSystemPrompt+"\n\nKeep my prompt", gjson.GetBytes(upstream.lastBody, "system").String())
	for _, field := range []string{"thinking", "tools", "messages"} {
		require.JSONEq(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(upstream.lastBody, field).Raw, field)
	}
	require.JSONEq(t, responseBody, recorder.Body.String())
	// Shared billing representation includes cached tokens in total input.
	require.Equal(t, 34, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 9, result.Usage.CacheReadInputTokens)
}

func TestDroidNativeResponsesPreservesToolsReasoning(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"Keep instructions","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"found"}]}`)
	upstream := &httpUpstreamRecorder{err: errors.New("capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c := adaptiveProtocolTestContext("/v1/responses", body)
	_, _ = svc.Forward(context.Background(), c, droidTestAccount(), body)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, droidSystemPrompt+"\n\nKeep instructions", gjson.GetBytes(upstream.lastBody, "instructions").String())
	for _, field := range []string{"reasoning", "tools", "input"} {
		require.JSONEq(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(upstream.lastBody, field).Raw, field)
	}
}

func TestDroidValidationAndNoSecretInErrors(t *testing.T) {
	for _, tc := range []struct{ name, key, kind string }{{"empty", "", AccountTypeAPIKey}, {"bearer", "Bearer factory_test_secret", AccountTypeAPIKey}, {"newline", "factory_test_secret\n", AccountTypeAPIKey}, {"oauth", "factory_test_secret", AccountTypeOAuth}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformDroid, Type: tc.kind, Credentials: map[string]any{"api_key": tc.key}}, map[string]any{})
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.NotContains(t, err.Error(), "factory_test_secret")
		})
	}
	require.NoError(t, validateDroidAccount(droidTestAccount()))
	require.Contains(t, DefaultDroidModelIDs(), "gpt-5.4")
	require.Equal(t, DefaultDroidModelIDs(), defaultModelsListCandidateIDs(PlatformDroid))
	require.True(t, upstreamBillingProbeTargetIsOfficialAPI("https://api.factory.ai/api/llm/o/v1"))
	_, err := (&AccountTestService{}).buildUpstreamModelsRequest(context.Background(), droidTestAccount())
	require.Error(t, err)
}

func TestDroidPreparationIsolatesIdentityAndRejectsInvalidRoutes(t *testing.T) {
	account := droidTestAccount()
	req, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer client_key")
	req.Header.Set("Cookie", "client-session")
	req.Header.Set("X-Session-ID", "client-session")
	first, err := prepareDroidRequest(req, account)
	require.NoError(t, err)
	second, err := prepareDroidRequest(req, account)
	require.NoError(t, err)
	require.NotEqual(t, first.Header.Get("X-Session-ID"), second.Header.Get("X-Session-ID"))
	require.NotEqual(t, first.Header.Get("X-Assistant-Message-ID"), second.Header.Get("X-Assistant-Message-ID"))
	require.Empty(t, first.Header.Get("Cookie"))
	require.Equal(t, "placeholder", first.Header.Get("X-API-Key"))
	require.Equal(t, "Bearer client_key", req.Header.Get("Authorization"))
	for _, path := range []string{"/api/llm/o/v1/responses", "/api/llm/a/v1/messages?secret=anything"} {
		bad, _ := http.NewRequest(http.MethodPost, "https://api.factory.ai"+path, strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
		_, err = prepareDroidRequest(bad, account)
		require.Error(t, err)
	}
	unknown, _ := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/o/v1/chat/completions", strings.NewReader(`{"model":"unknown-family"}`))
	_, err = prepareDroidRequest(unknown, account)
	require.Error(t, err)
}

type droidChunkReader struct {
	data  []byte
	chunk int
}

func (r *droidChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.chunk)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
func (r *droidChunkReader) Close() error { return nil }

func TestDroidRedactionAcrossEveryChunkBoundary(t *testing.T) {
	for _, secret := range []string{"factory_test_secret", "aaaa", "ababab"} {
		input := "data: " + secret + secret + " done\n\n" + secret[:len(secret)-1]
		want := strings.ReplaceAll(input, secret, "[REDACTED]")
		for chunk := 1; chunk <= len(secret)+2; chunk++ {
			r := &droidSecretReader{source: &droidChunkReader{data: []byte(input), chunk: chunk}, secret: []byte(secret)}
			out, err := io.ReadAll(r)
			require.NoError(t, err)
			require.Equal(t, want, string(out), fmt.Sprint(secret, chunk))
		}
	}
	// An ordinary SSE frame must not wait for the next frame or EOF.
	r := &droidSecretReader{source: &droidChunkReader{data: []byte("data: hello\n\n"), chunk: 13}, secret: []byte("factory_test_secret")}
	buf := make([]byte, 100)
	n, err := r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "data: hello\n\n", string(buf[:n]))
}

func TestDroidRedactsHTTPAndSSEErrorsBeforeCoreHandlers(t *testing.T) {
	for _, status := range []int{200, 401, 429, 500} {
		account := droidTestAccount()
		req, _ := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/o/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
		resp, err := droidRoundTrip(req, account, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": {"factory_test_secret"}, "Set-Cookie": {"factory_test_secret"}, "Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(`data: {"error":{"message":"factory_test_secret"}}`))}, nil
		})
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NotContains(t, string(b), "factory_test_secret")
		require.Contains(t, string(b), "[REDACTED]")
		require.Empty(t, resp.Header.Get("Set-Cookie"))
		require.Equal(t, "[REDACTED]", resp.Header.Get("X-Request-Id"))
		require.Equal(t, "30", resp.Header.Get("Retry-After"))
	}
}

func TestDroidHTTPFailuresRemainEligibleForAccountFailover(t *testing.T) {
	for _, status := range []int{401, 429} {
		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"factory_test_secret"}}`))}}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		c, recorder := newTestContext()
		_, err := svc.ForwardAsAnthropic(context.Background(), c, droidTestAccount(), body, "", "")
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.Equal(t, status, failover.StatusCode)
		require.NotContains(t, string(failover.ResponseBody), "factory_test_secret")
		require.Empty(t, recorder.Body.String())
		require.Len(t, upstream.requests, 1)
	}
}

func TestDroidNativeStreamUsageAndNoReplayAfterPartialOutput(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicStreamResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := newTestContext()
	result, err := svc.ForwardAsAnthropic(context.Background(), c, droidTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Equal(t, 16, result.Usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), "message_stop")
	response := nativeAnthropicStreamResponse()
	all, _ := io.ReadAll(response.Body)
	partial := bytes.Split(all, []byte("event: message_delta"))[0]
	response.Body = io.NopCloser(bytes.NewReader(partial))
	upstream = &httpUpstreamRecorder{resp: response}
	svc.httpUpstream = upstream
	c, recorder = newTestContext()
	_, err = svc.ForwardAsAnthropic(context.Background(), c, droidTestAccount(), body, "", "")
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "pong")
	require.Len(t, upstream.requests, 1)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "partial output must not request another account replay")
}

func TestDroidCancellationPropagatesToTransport(t *testing.T) {
	for _, stream := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		upstreamCtx, release := detachAccountUpstreamContext(ctx, droidTestAccount(), stream)
		release()
		cancel()
		require.ErrorIs(t, upstreamCtx.Err(), context.Canceled)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/o/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	_, err := droidRoundTrip(req, droidTestAccount(), func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("factory_test_secret: %w", context.Canceled)
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "factory_test_secret")
}

func TestDroidLocalRejectionDoesNotFailOver(t *testing.T) {
	for _, ingress := range routingMatrixIngresses() {
		t.Run(ingress.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: errors.New("must not send")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			body := routingMatrixCase{ingress: ingress, model: "unknown-family"}.body()
			c := adaptiveProtocolTestContext(ingress.path, body)
			err := ingress.forward(svc, c, droidTestAccount(), body)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			require.Empty(t, upstream.requests)
		})
	}
	require.True(t, shouldEstimateOpenAIInputTokensLocally(droidTestAccount()))
}

func TestDroidSanitizedNetworkFailuresKeepCooldown(t *testing.T) {
	for _, raw := range []error{&net.DNSError{Err: "factory_test_secret", IsNotFound: true}, errors.New("proxy authentication required factory_test_secret"), errors.New("connection refused factory_test_secret")} {
		err := safeDroidError(raw)
		require.NotContains(t, err.Error(), "factory_test_secret")
		repo := &openaiTransportAccountRepoStub{}
		svc := &OpenAIGatewayService{accountRepo: repo}
		c, _ := newTestContext()
		var failover *UpstreamFailoverError
		require.ErrorAs(t, svc.handleOpenAIUpstreamTransportError(context.Background(), c, droidTestAccount(), err, false), &failover)
		require.Len(t, repo.tempUnschedCalls, 1)
		require.NotContains(t, repo.tempUnschedCalls[0].reason, "factory_test_secret")
	}
	require.False(t, classifyUpstreamTransportError(safeDroidError(errors.New("temporary reset factory_test_secret"))).Persistent)
}

func TestDroidRateLimitAndAuthState(t *testing.T) {
	for _, retry := range []string{"60", time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat), "invalid", "-2", ""} {
		t.Run(retry, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			before := time.Now()
			svc.HandleUpstreamError(context.Background(), droidTestAccount(), 429, http.Header{"Retry-After": {retry}}, []byte(`{"error":{"message":"rate limited"}}`))
			require.Equal(t, 1, repo.rateLimitedCalls)
			require.Equal(t, int64(801), repo.lastRateLimitedID)
			require.True(t, repo.lastRateLimitedAt.After(before))
			if retry == "60" {
				require.InDelta(t, 60, repo.lastRateLimitedAt.Sub(before).Seconds(), 1)
			}
			require.Zero(t, repo.setErrorCalls)
		})
	}
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	require.True(t, svc.HandleUpstreamError(context.Background(), droidTestAccount(), 401, http.Header{}, []byte(`{"error":{"message":"invalid API key"}}`)))
	require.Equal(t, 1, repo.setErrorCalls)
	require.Zero(t, repo.updateCredentialsCalls)
}

type droidCancelUpstream struct {
	HTTPUpstream
	started chan struct{}
}

func (u *droidCancelUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	close(u.started)
	<-req.Context().Done()
	return nil, req.Context().Err()
}
func TestDroidForwardCancellationStopsActualRequest(t *testing.T) {
	for _, ingress := range routingMatrixIngresses() {
		t.Run(ingress.name, func(t *testing.T) {
			upstream := &droidCancelUpstream{started: make(chan struct{})}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			body := routingMatrixCase{ingress: ingress, model: "claude-sonnet-4-6"}.body()
			c := adaptiveProtocolTestContext(ingress.path, body)
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			done := make(chan error, 1)
			go func() {
				var err error
				switch ingress.name {
				case "responses":
					_, err = svc.Forward(ctx, c, droidTestAccount(), body)
				case "messages":
					_, err = svc.ForwardAsAnthropic(ctx, c, droidTestAccount(), body, "", "")
				default:
					_, err = svc.ForwardAsChatCompletions(ctx, c, droidTestAccount(), body, "", "")
				}
				done <- err
			}()
			select {
			case <-upstream.started:
			case <-time.After(2 * time.Second):
				t.Fatal("request never started")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(2 * time.Second):
				t.Fatal("client cancellation did not reach Factory request")
			}
		})
	}
}

func TestDroidCrossProtocolToolRoundTrip(t *testing.T) {
	for _, responseShape := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/stream=%v", responseShape, stream), func(t *testing.T) {
				var body []byte
				if responseShape {
					body = []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"input":[{"type":"function_call","call_id":"toolu_old","name":"lookup","arguments":"{\"query\":\"old\"}"},{"type":"function_call_output","call_id":"toolu_old","output":"found"}]}`, stream))
				} else {
					body = []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}],"messages":[{"role":"assistant","tool_calls":[{"id":"toolu_old","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"old\"}"}}]},{"role":"tool","tool_call_id":"toolu_old","content":"found"}]}`, stream))
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(toolAnthropicSSEStream()))}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				c, rec := newTestContext()
				var result *OpenAIForwardResult
				var err error
				if responseShape {
					result, err = svc.Forward(context.Background(), c, droidTestAccount(), body)
				} else {
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), body, "", "")
				}
				require.NoError(t, err)
				require.Len(t, upstream.requests, 1)
				require.Contains(t, string(upstream.lastBody), `"tool_use"`)
				require.Contains(t, string(upstream.lastBody), `"tool_result"`)
				require.Contains(t, string(upstream.lastBody), `"tool_use_id":"toolu_old"`)
				require.Equal(t, "lookup", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
				require.Contains(t, rec.Body.String(), "lookup")
				require.Equal(t, 10, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				if !stream {
					path := "choices.0.message.tool_calls.0.function.arguments"
					if responseShape {
						path = "output.0.arguments"
					}
					require.JSONEq(t, `{"query":"status"}`, gjson.GetBytes(rec.Body.Bytes(), path).String())
				}
			})
		}
	}
}

func TestDroidEscapedCredentialRedaction(t *testing.T) {
	for _, key := range []string{"factory_test_secret", "factory/key+test="} {
		encoded := ""
		for _, r := range key {
			encoded += fmt.Sprintf(`\u%04x`, r)
		}
		for _, payload := range []string{`{"error":{"message":"` + encoded + `"}}`, `{"error":{"message":"` + strings.ReplaceAll(key, "/", `\/`) + `"}}`} {
			for chunk := 1; chunk <= 8; chunk++ {
				account := droidTestAccount()
				account.Credentials["api_key"] = key
				req, _ := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
				resp, err := droidRoundTrip(req, account, func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 401, Header: http.Header{}, Body: &droidChunkReader{data: []byte(payload), chunk: chunk}}, nil
				})
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, "[REDACTED]", gjson.GetBytes(body, "error.message").String())
			}
		}
	}
	// An escaped backslash is literal user content, not a Unicode escape.
	literal := `{"text":"literal \\u0073, slash \/"}`
	reader := &droidJSONEscapeReader{source: io.NopCloser(strings.NewReader(literal))}
	normalized, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.JSONEq(t, literal, string(normalized))
}

func TestDroidCrossProtocolTruncationAndErrorNeverCompletes(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, explicitError := range []bool{false, true} {
				t.Run(fmt.Sprintf("responses=%v/stream=%v/error=%v", responses, stream, explicitError), func(t *testing.T) {
					partial := strings.Split(toolAnthropicSSEStream(), "event: message_delta")[0]
					if explicitError {
						partial += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"factory_test_secret\"}}\n\n"
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(partial))}}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					c, rec := newTestContext()
					var result *OpenAIForwardResult
					var err error
					if responses {
						result, err = svc.Forward(context.Background(), c, droidTestAccount(), []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"input":"hi"}`, stream)))
					} else {
						result, err = svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)), "", "")
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					// The handler retries uncommitted buffered truncation on the same account;
					// explicit protocol errors and committed streams are not retried.
					if !stream && !explicitError {
						require.ErrorAs(t, err, &failover)
						require.Equal(t, 502, failover.StatusCode)
						require.True(t, failover.RetryableOnSameAccount)
						require.Equal(t, 2, failover.SameAccountRetryMax)
						require.Equal(t, 300*time.Millisecond, failover.SameAccountRetryDelay)
						require.Nil(t, result)
						require.False(t, c.Writer.Written())
						require.Equal(t, 0, rec.Body.Len())
					} else {
						require.NotErrorAs(t, err, &failover)
						require.NotNil(t, result)
						require.Equal(t, 10, result.Usage.InputTokens)
						require.Contains(t, rec.Body.String(), "error")
					}
					require.NotContains(t, rec.Body.String(), "response.completed")
					require.NotContains(t, rec.Body.String(), "[DONE]")
					require.NotContains(t, rec.Body.String(), "factory_test_secret")
					require.Len(t, upstream.requests, 1)
					if responses && stream {
						require.Contains(t, rec.Body.String(), "event: response.failed\n")
						var createdID, failedID string
						var lastSequence int64 = -1
						for _, line := range strings.Split(rec.Body.String(), "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							payload := strings.TrimPrefix(line, "data: ")
							sequence := gjson.Get(payload, "sequence_number").Int()
							require.Greater(t, sequence, lastSequence)
							lastSequence = sequence
							switch gjson.Get(payload, "type").String() {
							case "response.created":
								createdID = gjson.Get(payload, "response.id").String()
							case "response.failed":
								failedID = gjson.Get(payload, "response.id").String()
								require.Equal(t, "failed", gjson.Get(payload, "response.status").String())
								require.Positive(t, gjson.Get(payload, "response.created_at").Int())
							}
						}
						require.NotEmpty(t, createdID)
						require.Equal(t, createdID, failedID)
					}
					if !stream && explicitError {
						require.Equal(t, 502, rec.Code)
					}
				})
			}
		}
	}
}

func TestDroidReasoningBudgetAndChatGuard(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "high", "xhigh"} {
		for _, cap := range []int{0, 2048, 512} {
			t.Run(fmt.Sprintf("%s/%d", effort, cap), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{err: errors.New("capture")}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","reasoning":{"effort":%q},"max_output_tokens":%d,"input":"hi"}`, effort, cap))
				c, rec := newTestContext()
				_, _ = svc.Forward(context.Background(), c, droidTestAccount(), body)
				if cap == 512 && (effort == "high" || effort == "xhigh") {
					require.Empty(t, upstream.requests)
					require.Equal(t, 400, rec.Code)
					return
				}
				require.Len(t, upstream.requests, 1)
				max := gjson.GetBytes(upstream.lastBody, "max_tokens").Int()
				budget := gjson.GetBytes(upstream.lastBody, "thinking.budget_tokens").Int()
				require.Less(t, budget, max)
				if cap > 0 {
					require.EqualValues(t, cap, max)
				}
				if effort == "none" || effort == "minimal" {
					require.Zero(t, budget)
				}
			})
		}
	}
	upstream := &httpUpstreamRecorder{err: errors.New("must not send")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, rec := newTestContext()
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), []byte(`{"model":"claude-sonnet-4-6","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`), "", "")
	require.Error(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "Messages or Responses")
}

func TestDroidCrossProtocolIdleTimeoutPreservesUsageAndFails(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/stream=%v", responses, stream), func(t *testing.T) {
				pr, pw := io.Pipe()
				defer pr.Close()
				defer pw.Close()
				go func() { _, _ = pw.Write([]byte(strings.Split(toolAnthropicSSEStream(), "event: message_delta")[0])) }()
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}}
				cfg := rawChatCompletionsTestConfig()
				cfg.Gateway.StreamDataIntervalTimeout = 1
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				c, rec := newTestContext()
				var result *OpenAIForwardResult
				var err error
				start := time.Now()
				if responses {
					result, err = svc.Forward(context.Background(), c, droidTestAccount(), []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"input":"hi"}`, stream)))
				} else {
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)), "", "")
				}
				elapsed := time.Since(start)
				var failover *UpstreamFailoverError
				if !stream {
					// An uncommitted buffered idle timeout now asks the handler to retry the same account.
					require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
					require.Less(t, elapsed, 5*time.Second)
					require.Error(t, err)
					require.ErrorAs(t, err, &failover)
					require.Equal(t, 502, failover.StatusCode)
					require.True(t, failover.RetryableOnSameAccount)
					require.Equal(t, 2, failover.SameAccountRetryMax)
					require.Equal(t, 300*time.Millisecond, failover.SameAccountRetryDelay)
					require.Nil(t, result)
					require.False(t, c.Writer.Written())
					require.Equal(t, 0, rec.Body.Len())
				} else {
					require.ErrorContains(t, err, "timeout")
					require.NotErrorAs(t, err, &failover)
					require.NotNil(t, result)
					require.Equal(t, 10, result.Usage.InputTokens)
					require.Contains(t, rec.Body.String(), "error")
				}
				require.Len(t, upstream.requests, 1)
				require.NotContains(t, rec.Body.String(), "response.completed")
				require.NotContains(t, rec.Body.String(), "[DONE]")
				if stream && responses {
					require.Contains(t, rec.Body.String(), "event: response.failed\n")
				}
			})
		}
	}
}

func TestDroidPreflightAfterHeartbeatWritesSSEError(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		c, rec := newTestContext()
		c.Request.URL.Path = path
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString(": ping\n\n")
		c.Writer.Flush()
		err := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(context.Background(), c, droidTestAccount(), &droidRequestError{message: "unsupported Factory model"}, false)
		require.Error(t, err)
		var failover *UpstreamFailoverError
		require.NotErrorAs(t, err, &failover)
		require.Contains(t, rec.Body.String(), "data: {")
		require.NotContains(t, rec.Body.String(), "\n\n{\"error\"")
		if path == "/v1/responses" {
			require.Contains(t, rec.Body.String(), "event: response.failed\n")
		}
	}
}

func TestDroidConversionRejectionAfterHeartbeatWritesSSEError(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(fmt.Sprint(responses), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: errors.New("must not send")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, rec := newTestContext()
			c.Request.URL.Path = "/v1/chat/completions"
			if responses {
				c.Request.URL.Path = "/v1/responses"
			}
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(": ping\n\n")
			c.Writer.Flush()
			var err error
			if responses {
				_, err = svc.Forward(context.Background(), c, droidTestAccount(), []byte(`{"model":"claude-sonnet-4-6","stream":true,"reasoning":{"effort":"high"},"max_output_tokens":512,"input":"hi"}`))
			} else {
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), []byte(`{"model":"claude-sonnet-4-6","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`), "", "")
			}
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Empty(t, upstream.requests)
			require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), "data: {")
			require.NotContains(t, rec.Body.String(), "\n\n{\"error\"")
			if responses {
				require.Contains(t, rec.Body.String(), "event: response.failed\n")
			}
		})
	}
}

func TestDroidResponsesThinkingSignatureRoundTrip(t *testing.T) {
	sse := toolAnthropicSSEStream()
	sse = strings.ReplaceAll(sse, `"index":0`, `"index":1`)
	split := strings.Index(sse, "event: content_block_start")
	thinking := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reason\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque-signature\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	sse = sse[:split] + thinking + sse[split:]
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, rec := newTestContext()
			_, err := svc.Forward(context.Background(), c, droidTestAccount(), []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%v,"reasoning":{"effort":"high"},"input":"hi"}`, stream)))
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), `"encrypted_content":"anthropic-thinking-v1:`)
			if stream {
				return
			}
			next := fmt.Sprintf(`{"model":"claude-sonnet-4-6","reasoning":{"effort":"high"},"input":%s}`, gjson.GetBytes(rec.Body.Bytes(), "output").Raw)
			upstream.resp = nil
			upstream.err = errors.New("capture follow-up")
			c, _ = newTestContext()
			_, _ = svc.Forward(context.Background(), c, droidTestAccount(), []byte(next))
			require.Contains(t, string(upstream.lastBody), `"signature":"opaque-signature"`)
			require.Contains(t, string(upstream.lastBody), `"thinking":"reason"`)
		})
	}
}
