package service

// Responses uses response.audio.transcript.delta + delta. Its
// response.audio.transcript.done and response.audio.done carry only type and
// sequence_number. Realtime response.output_audio_transcript.done instead
// carries transcript. Other underscore aliases and text are compatibility
// inputs, not canonical Responses schemas.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWSAudioFirstOutputContract_RealWireAndCompatibility(t *testing.T) {
	type audioCase struct {
		name, event, field, content string
		semantic, realtime          bool
	}
	cases := []audioCase{
		{name: "responses_transcript_done", event: "response.audio.transcript.done"},
		{name: "responses_audio_done", event: "response.audio.done"},
	}
	for _, content := range []string{"", " ", "payload"} {
		cases = append(cases,
			audioCase{name: "responses_transcript_delta/" + content, event: "response.audio.transcript.delta", field: "delta", content: content, semantic: content != ""},
			audioCase{name: "realtime_transcript_done/" + content, event: "response.output_audio_transcript.done", field: "transcript", content: content, semantic: content != "", realtime: true},
		)
	}
	for _, event := range []string{
		"response.audio_transcript.done", "response.output_audio_transcript.done",
		"response.audio.transcript.done", "response.audio.done", "response.output_audio.done",
	} {
		cases = append(cases, audioCase{name: event + "/missing", event: event})
		for _, field := range []string{"transcript", "text", "audio", "delta"} {
			for _, content := range []string{"", " ", "payload"} {
				label := "empty"
				if content == " " {
					label = "whitespace"
				}
				if content == "payload" {
					label = "content"
				}
				cases = append(cases, audioCase{
					name:  event + "/compat_" + field + "/" + label,
					event: event, field: field, content: content, semantic: content != "",
				})
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wire := map[string]any{"type": tc.event, "sequence_number": 1}
				if tc.realtime {
					wire = map[string]any{"type": tc.event, "event_id": "event_test", "response_id": "resp_contract",
						"item_id": "item_test", "output_index": 0, "content_index": 0}
				}
				if tc.field != "" {
					wire[tc.field] = tc.content
				}
				payload, err := json.Marshal(wire)
				require.NoError(t, err)
				svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
					[][]byte{payload, []byte(wsContractCompleted)}, []time.Duration{0, 2 * time.Second})
				defer svc.openaiWSPool.Close()
				c, rec := wsContractContext()
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
				if tc.semantic {
					require.NoError(t, err, "content-bearing completion must disarm the first-output window")
					require.NotNil(t, result)
					require.Contains(t, rec.Body.String(), string(payload))
				} else {
					wsContractRequireTimeout(t, err)
					require.Empty(t, rec.Body.String())
				}
				require.Len(t, conn.writes, 1)
			})
		})
	}
}

func TestWSAudioFirstOutputContract_TranscriptThenEOFNoReplay(t *testing.T) {
	for _, event := range []string{"response.audio_transcript.done", "response.output_audio_transcript.done", "response.audio.transcript.done"} {
		t.Run(event, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				payload := []byte(`{"type":"` + event + `","transcript":"already delivered"}`)
				const nextItem = `{"type":"response.output_item.added","item":{"type":"message","content":[]}}`
				svc, account, _, conn := newOpenAIWSV2FirstOutputHarness(t, 1,
					[][]byte{payload, []byte(nextItem)}, nil)
				defer svc.openaiWSPool.Close()
				c, rec := wsContractContext()
				_, err := svc.Forward(c.Request.Context(), c, account, []byte(wsContractRequest))
				require.Error(t, err)
				var fallback *openAIWSFallbackError
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &fallback))
				require.False(t, errors.As(err, &failover))
				require.Contains(t, rec.Body.String(), string(payload))
				require.Contains(t, rec.Body.String(), nextItem)
				require.Less(t, strings.Index(rec.Body.String(), string(payload)), strings.Index(rec.Body.String(), nextItem))
				require.Len(t, conn.writes, 1)
				require.True(t, conn.closed)
			})
		})
	}
}
