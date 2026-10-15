package service

// WS bridge first-output contract (stream:true only; defaults remain zero):
// Known empty lifecycle, delta, done, item and part payloads remain staged.
// Real text/reasoning/refusal/tool/encrypted payloads commit, as do unknown
// event/item/part shapes whose replay safety cannot be established.
// One monotonic latch governs both staging and the absolute read deadline.
// First commitment flushes preamble in order; all later frames bypass staging.
// Terminal/error handling and TTFT remain independent. The read window begins
// after the main write, excluding acquisition, prewarm and write time.
// These tests consume Forward using synthetic WS frames and virtual time.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const wsContractCompleted = `{"type":"response.completed","response":{"id":"resp_contract","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`
const wsContractRequest = `{"model":"gpt-5.5","stream":true,"input":"synthetic"}`
const wsContractPreamble = `{"type":"response.created","response":{"id":"resp_contract"}}`

func wsContractContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func newOpenAIWSV2FirstOutputHarness(
	t *testing.T,
	timeoutSeconds int,
	events [][]byte,
	readDelays []time.Duration,
) (*OpenAIGatewayService, *Account, *gin.Context, *openAIWSCaptureConn) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := wsContractContext()
	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = timeoutSeconds
	conn := &openAIWSCaptureConn{events: events, readDelays: readDelays}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(), openaiWSPool: pool,
	}
	account := &Account{
		ID: 1, Name: "ws-first-output-test", Platform: PlatformOpenAI,
		Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	return svc, account, c, conn
}

func wsContractRequireTimeout(t *testing.T, err error) {
	t.Helper()
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusGatewayTimeout, failover.StatusCode)
	require.Contains(t, string(failover.ResponseBody), "first_output_timeout")
	require.True(t, failover.SafeToFailoverAfterWrite)
}

func TestWSFirstOutputContract_ScalarFamilies(t *testing.T) {
	// Underscore audio done + text are compatibility inputs, not canonical
	// Responses schemas; real audio wire shapes have a separate matrix.
	families := map[string]string{
		"response.output_text.delta":                   "delta",
		"response.reasoning_text.delta":                "delta",
		"response.reasoning_summary_text.delta":        "delta",
		"response.function_call_arguments.delta":       "delta",
		"response.custom_tool_call_input.delta":        "delta",
		"response.refusal.delta":                       "delta",
		"response.audio.delta":                         "delta",
		"response.output_audio.delta":                  "delta",
		"response.audio_transcript.delta":              "delta",
		"response.output_audio_transcript.delta":       "delta",
		"response.output_text.done":                    "text",
		"response.reasoning_text.done":                 "text",
		"response.reasoning_summary_text.done":         "text",
		"response.audio_transcript.done":               "text",
		"response.output_audio_transcript.done":        "text",
		"response.function_call_arguments.done":        "arguments",
		"response.custom_tool_call_input.done":         "input",
		"response.refusal.done":                        "refusal",
		"response.image_generation_call.partial_image": "partial_image_b64",
	}
	for eventType, field := range families {
		for _, content := range []string{"", " ", "payload"} {
			t.Run(eventType+"/"+fmt.Sprint(len(content)), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					payload, err := json.Marshal(map[string]string{"type": eventType, field: content})
					require.NoError(t, err)
					svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1, [][]byte{
						[]byte(wsContractPreamble), payload, []byte(wsContractCompleted),
					}, []time.Duration{0, 0, 2 * time.Second})
					defer svc.openaiWSPool.Close()
					c, rec := wsContractContext()
					result, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
					if content == "" {
						wsContractRequireTimeout(t, err)
						require.Empty(t, rec.Body.String(), "empty preamble must remain private")
						require.False(t, c.Writer.Written())
						require.True(t, conn.closed, "timed-out lease must be broken")
					} else {
						require.NoError(t, err)
						require.Equal(t, "resp_contract", result.ResponseID)
						require.Contains(t, rec.Body.String(), string(payload))
						require.Less(t, strings.Index(rec.Body.String(), "response.created"), strings.Index(rec.Body.String(), eventType))
						if eventType == "response.function_call_arguments.done" || eventType == "response.refusal.done" {
							require.Nil(t, result.FirstTokenMs, "commitment must not change TTFT classification")
						}
					}
					require.Len(t, conn.writes, 1, "never replay after output")
				})
			})
		}
	}
}

func TestWSFirstOutputContract_ItemPartPairs(t *testing.T) {
	for _, tc := range []struct{ name, family, field, empty, content string }{
		{"message_text", "output_item", "item", `{"type":"message","content":[]}`, `{"type":"message","content":[{"type":"output_text","text":"ok"}]}`},
		{"message_refusal", "output_item", "item", `{"type":"message","content":[{"type":"refusal","refusal":""}]}`, `{"type":"message","content":[{"type":"refusal","refusal":"no"}]}`},
		{"reasoning_summary", "output_item", "item", `{"type":"reasoning","summary":[]}`, `{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}]}`},
		{"encrypted_reasoning", "output_item", "item", `{"type":"reasoning","encrypted_content":""}`, `{"type":"reasoning","encrypted_content":"opaque"}`},
		{"compaction", "output_item", "item", `{"type":"compaction","encrypted_content":""}`, `{"type":"compaction","encrypted_content":"opaque"}`},
		{"function", "output_item", "item", `{"type":"function_call","arguments":""}`, `{"type":"function_call","arguments":"{}"}`},
		{"custom", "output_item", "item", `{"type":"custom_tool_call","input":""}`, `{"type":"custom_tool_call","input":"pwd"}`},
		{"text_part", "content_part", "part", `{"type":"output_text","text":""}`, `{"type":"output_text","text":"ok"}`},
		{"refusal_part", "content_part", "part", `{"type":"refusal","refusal":""}`, `{"type":"refusal","refusal":"no"}`},
		{"summary_part", "reasoning_summary_part", "part", `{"type":"summary_text","text":""}`, `{"type":"summary_text","text":"reason"}`},
	} {
		for _, phase := range []string{"added", "done"} {
			for _, present := range []bool{false, true} {
				t.Run(tc.name+"/"+phase+"/"+fmt.Sprint(present), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						inner := tc.empty
						if present {
							inner = tc.content
						}
						payload := fmt.Sprintf(`{"type":"response.%s.%s","%s":%s}`, tc.family, phase, tc.field, inner)
						svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
							[][]byte{[]byte(payload), []byte(wsContractCompleted)}, []time.Duration{0, 2 * time.Second})
						defer svc.openaiWSPool.Close()
						c, rec := wsContractContext()
						result, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
						if present {
							require.NoError(t, err)
							require.NotNil(t, result)
							require.Contains(t, rec.Body.String(), payload)
						} else {
							wsContractRequireTimeout(t, err)
							require.Empty(t, rec.Body.String())
						}
						require.Len(t, conn.writes, 1)
					})
				})
			}
		}
	}
}

func TestWSFirstOutputContract_CommitThenMetadataEOF(t *testing.T) {
	for name, payload := range map[string]string{
		"delta":          `{"type":"response.output_text.delta","delta":"ok"}`,
		"refusal_done":   `{"type":"response.refusal.done","refusal":"no"}`,
		"unknown_event":  `{"type":"response.future_result","opaque":{"value":1}}`,
		"unknown_item":   `{"type":"response.output_item.done","item":{"type":"future_tool","opaque":{"value":1}}}`,
		"unknown_part":   `{"type":"response.content_part.done","part":{"type":"future_audio","opaque":"data"}}`,
		"builtin_search": `{"type":"response.output_item.done","item":{"type":"web_search_call","action":{"type":"search","query":"synthetic"}}}`,
		"image":          `{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"aW1hZ2U="}}`,
		"audio_alias":    `{"type":"response.output_audio.delta","audio":"YXVkaW8="}`,
		"untyped":        `{"opaque_output":"possibly delivered content"}`,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const metadata = `{"type":"response.output_item.added","item":{"type":"message","id":"next","content":[]}}`
				svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
					[][]byte{[]byte(wsContractPreamble), []byte(payload), []byte(metadata)}, nil)
				defer svc.openaiWSPool.Close()
				c, rec := wsContractContext()
				_, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
				require.Error(t, err)
				var fallback *openAIWSFallbackError
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &fallback))
				require.False(t, errors.As(err, &failover))
				require.Contains(t, rec.Body.String(), payload)
				require.Contains(t, rec.Body.String(), metadata)
				require.Less(t, strings.Index(rec.Body.String(), payload), strings.Index(rec.Body.String(), metadata))
				require.Len(t, conn.writes, 1)
				require.True(t, conn.closed)
				recorder, ok := svc.httpUpstream.(*httpUpstreamRecorder)
				require.True(t, ok)
				require.Nil(t, recorder.lastReq, "no HTTP fallback after commitment")
			})
		})
	}
}

func TestWSFirstOutputContract_PolicyAndLease(t *testing.T) {
	for _, tc := range []struct {
		name, effort       string
		standard, override int
		stream, timeout    bool
	}{
		{"disabled", "xhigh", 0, 0, true, false},
		{"override_does_not_enable", "high", 0, 1, true, false},
		{"nonstream", "low", 1, 0, false, false},
		{"standard", "low", 1, 4, true, true},
		{"high_override", "high", 1, 4, true, false},
		{"xhigh_override", "xhigh", 1, 4, true, false},
		{"max_override", "max", 1, 4, true, false},
		{"override_fallback", "high", 1, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, account, c, conn := newOpenAIWSV2FirstOutputHarness(t, tc.standard,
					[][]byte{[]byte(wsContractCompleted)}, []time.Duration{2 * time.Second})
				defer svc.openaiWSPool.Close()
				svc.cfg.Gateway.OpenAIHighEffortFirstOutputTimeoutSeconds = tc.override
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"reasoning":{"effort":%q},"input":"synthetic"}`, tc.stream, tc.effort))
				_, err := svc.Forward(c.Request.Context(), c, account, body)
				if !tc.timeout {
					require.NoError(t, err)
					return
				}
				wsContractRequireTimeout(t, err)
				require.True(t, conn.closed)
				fresh := &openAIWSCaptureConn{events: [][]byte{[]byte(wsContractCompleted)}}
				dialer := &openAIWSCaptureDialer{conn: fresh}
				svc.openaiWSPool.setClientDialerForTest(dialer)
				next, _ := wsContractContext()
				_, err = svc.Forward(next.Request.Context(), next, account, body)
				require.NoError(t, err)
				require.Equal(t, 1, dialer.DialCount(), "broken lease cannot be reused")
			})
		})
	}
}

// A delayed successful write is independent of the first-output read window.
type wsContractDelayedWriter struct {
	*openAIWSCaptureConn
	writeDelay  time.Duration
	lastWriteAt time.Time
}

func (c *wsContractDelayedWriter) WriteJSON(ctx context.Context, value any) error {
	select {
	case <-time.After(c.writeDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.openAIWSCaptureConn.WriteJSON(ctx, value); err != nil {
		return err
	}
	c.lastWriteAt = time.Now()
	return nil
}

type wsContractDialer struct {
	conn  openAIWSClientConn
	delay time.Duration
}

func (d wsContractDialer) Dial(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, 0, nil, ctx.Err()
		}
	}
	return d.conn, 0, http.Header{}, nil
}

func TestWSFirstOutputContract_ReadWindowOrigin(t *testing.T) {
	for _, prewarm := range []bool{false, true} {
		for _, silent := range []bool{false, true} {
			t.Run(fmt.Sprintf("prewarm=%t/silent=%t", prewarm, silent), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					events := [][]byte{}
					delays := []time.Duration{}
					if prewarm {
						events = append(events, []byte(wsContractCompleted))
						delays = append(delays, 2*time.Second)
					}
					events = append(events, []byte(wsContractCompleted))
					delay := 500 * time.Millisecond
					if silent {
						delay = 2 * time.Second
					}
					delays = append(delays, delay)
					svc, account, c, conn := newOpenAIWSV2FirstOutputHarness(t, 1, events, delays)
					defer svc.openaiWSPool.Close()
					svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
					writer := &wsContractDelayedWriter{openAIWSCaptureConn: conn, writeDelay: 2 * time.Second}
					svc.openaiWSPool.setClientDialerForTest(wsContractDialer{conn: writer, delay: 2 * time.Second})
					_, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
					wantWrites := 1
					if prewarm {
						wantWrites++
					}
					require.Len(t, conn.writes, wantWrites)
					if !silent {
						require.NoError(t, err)
						require.Equal(t, 500*time.Millisecond, time.Since(writer.lastWriteAt))
						return
					}
					wsContractRequireTimeout(t, err)
					require.Equal(t, time.Second, time.Since(writer.lastWriteAt))
					raw, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok)
					opsEvents, ok := raw.([]*OpsUpstreamErrorEvent)
					require.True(t, ok)
					require.Len(t, opsEvents, 1)
					require.Contains(t, opsEvents[0].Detail, "elapsed_ms=1000 timeout_ms=1000")
					require.Contains(t, opsEvents[0].Detail, "phase=websocket_first_semantic_output_read")
				})
			})
		}
	}
}

func TestWSFirstOutputContract_MetadataAbsoluteBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, account, c, _ := newOpenAIWSV2FirstOutputHarness(t, 1, [][]byte{
			[]byte(wsContractPreamble), []byte(`{"type":"response.in_progress"}`),
			[]byte(`{"type":"keepalive"}`), []byte(wsContractCompleted),
		}, []time.Duration{0, 400 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond})
		defer svc.openaiWSPool.Close()
		start := time.Now()
		_, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
		wsContractRequireTimeout(t, err)
		require.Equal(t, time.Second, time.Since(start))
	})
}

func TestWSFirstOutputContract_ShorterReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, account, c, _ := newOpenAIWSV2FirstOutputHarness(t, 4,
			[][]byte{[]byte(wsContractCompleted)}, []time.Duration{2 * time.Second})
		defer svc.openaiWSPool.Close()
		svc.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
		_, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
		require.Error(t, err)
		if raw, ok := c.Get(OpsUpstreamErrorsKey); ok {
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			for _, event := range events {
				require.NotEqual(t, "first_output_timeout", event.Kind)
			}
		}
	})
}

func TestWSFirstOutputContract_CancellationAndTerminal(t *testing.T) {
	t.Run("caller_cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, account, c, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
				[][]byte{[]byte(wsContractCompleted)}, []time.Duration{2 * time.Second})
			defer svc.openaiWSPool.Close()
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := svc.Forward(ctx, c, account, []byte(wsContractRequest))
				done <- err
			}()
			synctest.Wait()
			cancel()
			err := <-done
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			_, recorded := c.Get(OpsUpstreamErrorsKey)
			require.False(t, recorded, "caller cancellation is not a timeout attempt")
			require.Len(t, conn.writes, 1)
		})
	})
	t.Run("terminal_without_output", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
				[][]byte{[]byte(wsContractPreamble), []byte(wsContractCompleted)}, nil)
			defer svc.openaiWSPool.Close()
			c, rec := wsContractContext()
			result, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
			require.NoError(t, err, "a terminal must finish without reading EOF or retrying")
			require.Nil(t, result.FirstTokenMs)
			require.Equal(t, "data: "+wsContractPreamble+"\n\ndata: "+wsContractCompleted+"\n\n", rec.Body.String())
			require.Len(t, conn.writes, 1)
		})
	})
}
