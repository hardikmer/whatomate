package handlers_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func contactExportPermissions(t *testing.T, app *handlers.App) []models.Permission {
	t.Helper()
	var perms []models.Permission
	for _, p := range testutil.GetOrCreateTestPermissions(t, app.DB) {
		if p.Resource == models.ResourceContacts && p.Action == models.ActionExport {
			perms = append(perms, p)
		}
	}
	require.NotEmpty(t, perms)
	return perms
}

func TestApp_ExportConversations(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRole(t, app.DB, org.ID, "contact-exporter", contactExportPermissions(t, app))
	user := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("conv-export")),
		testutil.WithRoleID(&role.ID),
	)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("conv-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	now := time.Now()
	old := now.AddDate(0, -3, 0)
	msg := func(orgID, contactID uuid.UUID, dir models.Direction, content string, at time.Time, replyTo *uuid.UUID, meta models.JSONB) *models.Message {
		m := &models.Message{
			BaseModel:         models.BaseModel{ID: uuid.New(), CreatedAt: at},
			OrganizationID:    orgID,
			WhatsAppAccount:   account.Name,
			ContactID:         contactID,
			Direction:         dir,
			MessageType:       models.MessageTypeText,
			Content:           content,
			WhatsAppMessageID: "wamid." + uuid.New().String(),
			ReplyToMessageID:  replyTo,
			Metadata:          meta,
		}
		require.NoError(t, app.DB.Create(m).Error)
		return m
	}

	// Organic chat: never got a campaign, wrote in on their own.
	organic := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	msg(org.ID, organic.ID, models.DirectionIncoming, "hi", now.Add(-2*time.Hour), nil, nil)
	msg(org.ID, organic.ID, models.DirectionOutgoing, "hello, how can we help?", now.Add(-90*time.Minute), nil, nil)
	msg(org.ID, organic.ID, models.DirectionIncoming, "price please", now.Add(-time.Hour), nil, nil)
	// Older message outside the period must not be counted.
	msg(org.ID, organic.ID, models.DirectionIncoming, "old", old, nil, nil)

	// Campaign reply.
	campaign := &models.BulkMessageCampaign{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Diwali Offer", WhatsAppAccount: account.Name, TemplateID: template.ID,
		Status: models.CampaignStatusCompleted, CreatedBy: user.ID}
	require.NoError(t, app.DB.Create(campaign).Error)
	replier := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	sent := msg(org.ID, replier.ID, models.DirectionOutgoing, "campaign", now.Add(-3*time.Hour), nil,
		models.JSONB{"campaign_id": campaign.ID.String()})
	msg(org.ID, replier.ID, models.DirectionIncoming, "Interested", now.Add(-30*time.Minute), &sent.ID, nil)

	// Only messaged by us in the period: did not interact.
	silent := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	msg(org.ID, silent.ID, models.DirectionOutgoing, "campaign", now.Add(-time.Hour), nil, nil)
	// Wrote in, but only before the period.
	lapsed := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	msg(org.ID, lapsed.ID, models.DirectionIncoming, "old", old, nil, nil)
	// Another organisation.
	other := testutil.CreateTestOrganization(t, app.DB)
	otherContact := testutil.CreateTestContact(t, app.DB, other.ID)
	msg(other.ID, otherContact.ID, models.DirectionIncoming, "hi", now.Add(-time.Hour), nil, nil)

	call := func(userID uuid.UUID) (int, [][]string) {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		testutil.SetQueryParam(req, "from", now.AddDate(0, 0, -1).Format("2006-01-02"))
		testutil.SetQueryParam(req, "to", now.Format("2006-01-02"))
		require.NoError(t, app.ExportConversations(req))
		code := testutil.GetResponseStatusCode(req)
		if code != fasthttp.StatusOK {
			return code, nil
		}
		return code, csvDataRows(t, testutil.GetResponseBody(req))
	}

	code, rows := call(user.ID)
	require.Equal(t, fasthttp.StatusOK, code)
	require.Len(t, rows, 2, "only the two contacts who wrote in during the period")

	byPhone := map[string][]string{}
	for _, r := range rows {
		byPhone[r[1]] = r
	}
	o := byPhone[organic.PhoneNumber]
	require.NotNil(t, o)
	assert.Equal(t, "2", o[5], "messages from customer, in period only")
	assert.Equal(t, "1", o[6], "messages sent to customer")
	assert.Equal(t, "", o[7], "organic chat has no campaign")
	assert.Equal(t, "price please", o[10], "last customer message")

	c := byPhone[replier.PhoneNumber]
	require.NotNil(t, c)
	assert.Equal(t, "1", c[5])
	assert.Equal(t, "Diwali Offer", c[7], "campaign they replied to")
	assert.Equal(t, rows[0][1], replier.PhoneNumber, "most recent interaction first")

	noPermRole := testutil.CreateTestRole(t, app.DB, org.ID, "no-export", getAnalyticsPermissions(t, app))
	noPermUser := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("conv-noperm")), testutil.WithRoleID(&noPermRole.ID))
	code, _ = call(noPermUser.ID)
	assert.Equal(t, fasthttp.StatusForbidden, code, "needs contacts:export")
}
