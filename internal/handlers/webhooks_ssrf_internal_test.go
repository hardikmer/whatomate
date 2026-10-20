package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateWebhookURL_AllowedInternalHost(t *testing.T) {
	SetAllowedInternalHosts("")
	assert.NoError(t, validateWebhookURL("http://n8n:5678/webhook/x"),
		"a bare docker hostname is not an IP literal, so structural validation passes")
	assert.Error(t, validateWebhookURL("http://127.0.0.1:5678/webhook/x"))

	SetAllowedInternalHosts(" N8N , other ")
	defer SetAllowedInternalHosts("")
	assert.True(t, isAllowedInternalHost("n8n"))
	assert.True(t, isAllowedInternalHost("other"))
	assert.False(t, isAllowedInternalHost("localhost"))
	assert.NoError(t, validateWebhookURL("http://n8n:5678/webhook/x"))
	assert.Error(t, validateWebhookURL("http://localhost:5678/webhook/x"))
}
