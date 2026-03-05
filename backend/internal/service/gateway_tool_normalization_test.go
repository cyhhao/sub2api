package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToSnakeCase_CamelCaseKeysDoNotTruncate(t *testing.T) {
	t.Skip("tool parameter normalization is disabled")
}

func TestNormalizeClaudeOAuthRequestBody_ToolSchemaKeepsOriginalParamNames(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"tools":[
			{
				"name":"edit",
				"input_schema":{
					"type":"object",
					"properties":{
						"filePath":{"type":"string"},
						"oldString":{"type":"string"},
						"newString":{"type":"string"},
						"replaceAll":{"type":"boolean"}
					},
					"required":["filePath","oldString","newString"]
				}
			}
		],
		"messages":[]
	}`)

	newBody, _, toolNameMap := normalizeClaudeOAuthRequestBody(body, "claude-sonnet-4-5", claudeOAuthNormalizeOptions{})

	var req map[string]any
	require.NoError(t, json.Unmarshal(newBody, &req))

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)

	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Edit", tool["name"])

	schema, ok := tool["input_schema"].(map[string]any)
	require.True(t, ok)

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, props, "filePath")
	require.Contains(t, props, "oldString")
	require.Contains(t, props, "newString")
	require.Contains(t, props, "replaceAll")
	require.NotContains(t, props, "file_path")
	require.NotContains(t, props, "old_string")
	require.NotContains(t, props, "new_string")
	require.NotContains(t, props, "replace_all")
	require.NotContains(t, props, "filpath")
	require.NotContains(t, props, "olstring")
	require.NotContains(t, props, "nestring")
	require.NotContains(t, props, "replacall")

	requiredList, ok := schema["required"].([]any)
	require.True(t, ok)
	require.Equal(t, []any{"filePath", "oldString", "newString"}, requiredList)

	// Tool name mapping is still expected.
	require.Equal(t, "edit", toolNameMap["Edit"])
	// Parameter key mapping is intentionally disabled.
	require.NotContains(t, toolNameMap, "file_path")
	require.NotContains(t, toolNameMap, "old_string")
	require.NotContains(t, toolNameMap, "new_string")
	require.NotContains(t, toolNameMap, "replace_all")
}
