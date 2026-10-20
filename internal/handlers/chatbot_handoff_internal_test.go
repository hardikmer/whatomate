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

func TestExtractAIActions(t *testing.T) {
	t.Parallel()

	in := "Done, Aarav's seat is booked for Sunday 11 AM.\n[[ACTION {\"type\":\"demo_booked\",\"child_name\":\"Aarav\",\"demo_datetime\":\"2026-10-25T11:00:00+05:30\"}]]"
	got, actions := extractAIActions(in)
	assert.Equal(t, "Done, Aarav's seat is booked for Sunday 11 AM.", got)
	if assert.Len(t, actions, 1) {
		assert.Equal(t, "demo_booked", actions[0]["type"])
		assert.Equal(t, "Aarav", actions[0]["child_name"])
	}

	// Several blocks, plus the handoff marker, survive in order.
	got, actions = extractAIActions("Thanks! [[ACTION {\"type\":\"wants_to_pay\"}]]\n[[ACTION {\"type\":\"demo_interested\"}]]\n[HANDOFF]")
	assert.Contains(t, got, "Thanks!")
	assert.Contains(t, got, "[HANDOFF]")
	assert.NotContains(t, got, "ACTION")
	assert.Len(t, actions, 2)
	got, handoff := extractAIHandoff(got)
	assert.Equal(t, "Thanks!", got)
	assert.True(t, handoff)

	// Broken JSON or a missing type is stripped but not dispatched.
	got, actions = extractAIActions("Hi [[ACTION {not json}]] there [[ACTION {\"child\":\"x\"}]]")
	assert.Equal(t, "Hi  there", got)
	assert.Empty(t, actions)

	// No markers: unchanged.
	got, actions = extractAIActions("Plain reply")
	assert.Equal(t, "Plain reply", got)
	assert.Nil(t, actions)
}
