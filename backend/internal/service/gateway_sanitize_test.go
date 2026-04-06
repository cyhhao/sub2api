package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeOpenCodeText_RewritesCanonicalSentence(t *testing.T) {
	in := "You are OpenCode, the best coding agent on the planet."
	got := sanitizeSystemText(in)
	require.Equal(t, strings.TrimSpace(claudeCodeSystemPrompt), got)
}

func TestSanitizeOpenClawText_RewritesCanonicalSentence(t *testing.T) {
	tests := []string{
		"You are a personal assistant operating inside OpenClaw.",
		"You are a personal assistant running inside OpenClaw.",
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			got := sanitizeSystemText(in)
			require.Equal(t, strings.TrimSpace(claudeCodeSystemPrompt), got)
		})
	}
}

func TestSanitizeOpenClawText_RewritesBannerPrefixOnly(t *testing.T) {
	in := "You are a personal assistant running inside OpenClaw.\n## Tooling\n- read"
	got := sanitizeSystemText(in)
	require.Equal(t, strings.TrimSpace(claudeCodeSystemPrompt)+"\n## Tooling\n- read", got)
}
