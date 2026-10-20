package handlers

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestExtractAIHandoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       string
		want     string
		wantFlag bool
	}{
		{"no marker", "Our next batch starts soon.", "Our next batch starts soon.", false},
		{"marker at end", "Let me check with the team. [HANDOFF]", "Let me check with the team.", true},
		{"marker on own line", "Let me check with the team.\n[HANDOFF]", "Let me check with the team.", true},
		{"lower case", "One moment [handoff]", "One moment", true},
		{"marker only", "[HANDOFF]", "", true},
		{"repeated", "[HANDOFF] Sure. [HANDOFF]", "Sure.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, flag := extractAIHandoff(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantFlag, flag)
		})
	}
}

func TestTrimCurrentMessage(t *testing.T) {
	t.Parallel()

	in := func(m string) models.ChatbotSessionMessage {
		return models.ChatbotSessionMessage{Direction: models.DirectionIncoming, Message: m}
	}
	out := func(m string) models.ChatbotSessionMessage {
		return models.ChatbotSessionMessage{Direction: models.DirectionOutgoing, Message: m}
	}

	// The just-logged incoming message is dropped; the limit still applies.
	got := trimCurrentMessage([]models.ChatbotSessionMessage{in("hi"), out("hello"), in("price?")}, "price?", 4)
	assert.Equal(t, []models.ChatbotSessionMessage{in("hi"), out("hello")}, got)

	got = trimCurrentMessage([]models.ChatbotSessionMessage{in("a"), out("b"), in("c"), out("d"), in("now")}, "now", 2)
	assert.Equal(t, []models.ChatbotSessionMessage{in("c"), out("d")}, got)

	// Nothing to drop when the last entry is not the current message.
	got = trimCurrentMessage([]models.ChatbotSessionMessage{in("hi"), out("hello")}, "price?", 4)
	assert.Equal(t, []models.ChatbotSessionMessage{in("hi"), out("hello")}, got)
}

func TestMaskPhoneKeepCountryCode(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "+9172******89", maskPhoneKeepCountryCode("917202999989"))
	assert.Equal(t, "+1631*****81", maskPhoneKeepCountryCode("+16315551181"))
	assert.Equal(t, "+*****", maskPhoneKeepCountryCode("12345"))
}
