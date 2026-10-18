//go:build unit

package service

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// A pipe exposes buffering regressions: the producer cannot finish until the
// consumer receives its first complete event. Closing both ends bounds failures.
func TestDroidStreamingReaderDoesNotWaitForNextEvent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	request, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"messages":[]}`))
	require.NoError(t, err)
	response, err := droidRoundTrip(request, droidTestAccount(), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	})
	require.NoError(t, err)
	defer response.Body.Close()
	event := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n"
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, event); written <- err }()
	result := make(chan string, 1)
	go func() {
		buffered := bufio.NewReader(response.Body)
		var frame strings.Builder
		for {
			line, err := buffered.ReadString('\n')
			if err != nil {
				result <- "read error: " + err.Error()
				return
			}
			frame.WriteString(line)
			if line == "\n" {
				result <- frame.String()
				return
			}
		}
	}()
	select {
	case frame := <-result:
		require.Equal(t, event, frame)
	case <-time.After(time.Second):
		t.Fatal("first event buffered pending next event/EOF")
	}
	select {
	case err := <-written:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("producer did not finish first event")
	}
}

// Exercise actual HTTP flushing while the upstream pipe remains open. Metadata
// must commit immediately for Droid, but not change other platforms' staging.
func TestDroidResponsesFirstCompleteEventFlush(t *testing.T) {
	for _, platform := range []string{PlatformDroid, PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			upstream, producer := io.Pipe()
			defer upstream.Close()
			defer producer.Close()
			type outcome struct {
				result *openaiStreamingResult
				err    error
			}
			finished := make(chan outcome, 1)
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: upstream}
				result, err := svc.handleStreamingResponse(r.Context(), resp, c, &Account{ID: 1, Platform: platform}, time.Now(), "test-model", "test-model")
				finished <- outcome{result, err}
			}))
			defer front.Close()
			// Close the pipe before Server.Close even on a failed assertion.
			defer upstream.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			headers := make(chan struct{}, 1)
			firstEvent := make(chan string, 1)
			clientDone := make(chan error, 1)
			go func() {
				resp, err := client.Get(front.URL)
				if err != nil {
					clientDone <- err
					return
				}
				defer resp.Body.Close()
				headers <- struct{}{}
				reader := bufio.NewReader(resp.Body)
				var event strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						clientDone <- err
						return
					}
					event.WriteString(line)
					if line == "\n" {
						firstEvent <- event.String()
						break
					}
				}
				_, err = io.Copy(io.Discard, reader)
				clientDone <- err
			}()
			metadata := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\"}}\n"
			_, err := io.WriteString(producer, metadata)
			require.NoError(t, err)
			select {
			case <-headers:
				t.Fatal("incomplete event must not commit headers")
			case <-time.After(100 * time.Millisecond):
			}
			_, err = io.WriteString(producer, "\n")
			require.NoError(t, err)
			if platform == PlatformDroid {
				// Keep metadata committed without semantic output for a second
				// interval, so the final TTFT assertion distinguishes the two.
				time.Sleep(100 * time.Millisecond)
				select {
				case event := <-firstEvent:
					require.Contains(t, event, `"type":"response.created"`)
					require.Contains(t, event, `"id":"resp_first"`)
				case err := <-clientDone:
					t.Fatalf("client ended before first event: %v", err)
				case <-time.After(2 * time.Second):
					t.Fatal("complete Droid metadata event buffered pending next event/EOF")
				}
			} else {
				select {
				case <-headers:
					t.Fatal("non-Droid metadata must retain existing first-output staging")
				case <-time.After(100 * time.Millisecond):
				}
			}
			select {
			case <-finished:
				t.Fatal("upstream must still be blocked when first-event behavior is checked")
			default:
			}
			_, err = io.WriteString(producer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			require.NoError(t, err)
			require.NoError(t, producer.Close())
			select {
			case got := <-finished:
				require.NoError(t, got.err)
				require.NotNil(t, got.result)
				require.NotNil(t, got.result.firstTokenMs)
				// Metadata transport commit must not be reclassified as TTFT.
				require.GreaterOrEqual(t, *got.result.firstTokenMs, 180)
			case <-time.After(3 * time.Second):
				t.Fatal("stream did not finish")
			}
			require.NoError(t, <-clientDone)
		})
	}
}
