package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTransport records the outgoing request and returns a canned response,
// so the Chat Completions client can be tested without a network or database.
type stubTransport struct {
	status int
	body   string
	req    *http.Request
	sent   map[string]any
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.req = r
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &s.sent)
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(bytes.NewBufferString(s.body)),
		Header:     make(http.Header),
	}, nil
}

func openRouterSettings(model string) *models.ChatbotSettings {
	return &models.ChatbotSettings{AI: models.AIConfig{
		Enabled:      true,
		Provider:     models.AIProviderOpenRouter,
		APIKey:       "sk-or-test",
		Model:        model,
		MaxTokens:    300,
		Temperature:  0.4,
		SystemPrompt: "You are a helpful assistant.",
	}}
}

func TestChatCompletions_OpenRouterRequest(t *testing.T) {
	stub := &stubTransport{status: 200, body: `{"choices":[{"message":{"content":"  Hello from OpenRouter  "}}]}`}
	app := &App{HTTPClient: &http.Client{Transport: stub}}

	// A vendor-prefixed OpenRouter model ID must be passed through untouched.
	out, err := app.generateChatCompletionsResponse(openRouterSettings("anthropic/claude-sonnet-5"),
		nil, "hi", "Batch starts 16 Aug.", openRouterChatURL, "OpenRouter")
	require.NoError(t, err)

	assert.Equal(t, "Hello from OpenRouter", out)
	assert.Equal(t, "https://openrouter.ai/api/v1/chat/completions", stub.req.URL.String())
	assert.Equal(t, "Bearer sk-or-test", stub.req.Header.Get("Authorization"))
	assert.Equal(t, "anthropic/claude-sonnet-5", stub.sent["model"])
	assert.EqualValues(t, 300, stub.sent["max_tokens"])

	msgs := stub.sent["messages"].([]any)
	require.Len(t, msgs, 2)
	sys := msgs[0].(map[string]any)
	assert.Equal(t, "system", sys["role"])
	// AI context snippets are appended to the configured system prompt.
	assert.Equal(t, "You are a helpful assistant.\n\nBatch starts 16 Aug.", sys["content"])
	assert.Equal(t, "hi", msgs[1].(map[string]any)["content"])
}

func TestChatCompletions_OpenAIUnchanged(t *testing.T) {
	stub := &stubTransport{status: 200, body: `{"choices":[{"message":{"content":"ok"}}]}`}
	app := &App{HTTPClient: &http.Client{Transport: stub}}
	s := openRouterSettings("gpt-4o-mini")
	s.AI.Provider = models.AIProviderOpenAI

	_, err := app.generateOpenAIResponse(s, nil, "hi", "")
	require.NoError(t, err)
	assert.Equal(t, "https://api.openai.com/v1/chat/completions", stub.req.URL.String())
}

func TestChatCompletions_ErrorNamesProvider(t *testing.T) {
	stub := &stubTransport{status: 402, body: `{"error":{"message":"Insufficient credits"}}`}
	app := &App{HTTPClient: &http.Client{Transport: stub}}

	_, err := app.generateChatCompletionsResponse(openRouterSettings("openai/gpt-5.6-terra"),
		nil, "hi", "", openRouterChatURL, "OpenRouter")
	require.Error(t, err)
	// Operators need to see which provider failed and why (402 = out of credit).
	assert.Contains(t, err.Error(), "OpenRouter API error (HTTP 402): Insufficient credits")
}
