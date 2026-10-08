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

	// Got the campaign, never replied: campaign, not engaged.
	silent := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	msg(org.ID, silent.ID, models.DirectionOutgoing, "campaign", now.Add(-4*time.Hour), nil,
		models.JSONB{"campaign_id": campaign.ID.String()})
	// An agent wrote first (not a campaign), no reply: team, not engaged.
	teamOnly := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	tmImg := msg(org.ID, teamOnly.ID, models.DirectionOutgoing, "", now.Add(-5*time.Hour), nil, nil)
	require.NoError(t, app.DB.Model(tmImg).Update("message_type", models.MessageTypeImage).Error)
	// The campaign send failed: never reached, must not appear at all.
	failed := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	fm := msg(org.ID, failed.ID, models.DirectionOutgoing, "campaign", now.Add(-time.Hour), nil,
		models.JSONB{"campaign_id": campaign.ID.String()})
	require.NoError(t, app.DB.Model(fm).Update("status", models.MessageStatusFailed).Error)
	// Wrote in, but only before the period.
	lapsed := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	msg(org.ID, lapsed.ID, models.DirectionIncoming, "old", old, nil, nil)
	// Another organisation.
	other := testutil.CreateTestOrganization(t, app.DB)
	otherContact := testutil.CreateTestContact(t, app.DB, other.ID)
	msg(other.ID, otherContact.ID, models.DirectionIncoming, "hi", now.Add(-time.Hour), nil, nil)

	call := func(userID uuid.UUID, filters map[string]string) (int, [][]string) {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		testutil.SetQueryParam(req, "from", now.AddDate(0, 0, -1).Format("2006-01-02"))
		testutil.SetQueryParam(req, "to", now.Format("2006-01-02"))
		for k, v := range filters {
			testutil.SetQueryParam(req, k, v)
		}
		require.NoError(t, app.ExportConversations(req))
		code := testutil.GetResponseStatusCode(req)
		if code != fasthttp.StatusOK {
			return code, nil
		}
		return code, csvDataRows(t, testutil.GetResponseBody(req))
	}
	phones := func(rows [][]string) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r[1])
		}
		return out
	}

	code, rows := call(user.ID, nil)
	require.Equal(t, fasthttp.StatusOK, code)
	require.Equal(t, []string{replier.PhoneNumber, organic.PhoneNumber, silent.PhoneNumber, teamOnly.PhoneNumber}, phones(rows),
		"everyone reached in the period, most recent activity first; failed, lapsed and other-org excluded")

	byPhone := map[string][]string{}
	for _, r := range rows {
		byPhone[r[1]] = r
	}
	// Columns: 3 started by, 4 engaged, 8 from customer, 9 to customer, 10 campaign,
	// 13 first message, 14 first customer message, 15 last customer message.
	o := byPhone[organic.PhoneNumber]
	assert.Equal(t, []string{"Customer", "Yes", "2", "1", "", "hi", "hi", "price please"},
		[]string{o[3], o[4], o[8], o[9], o[10], o[13], o[14], o[15]}, "the message before the period is not the first")
	c := byPhone[replier.PhoneNumber]
	assert.Equal(t, []string{"Campaign", "Yes", "1", "1", "Diwali Offer", "campaign", "Interested", "Interested"},
		[]string{c[3], c[4], c[8], c[9], c[10], c[13], c[14], c[15]})
	sl := byPhone[silent.PhoneNumber]
	assert.Equal(t, []string{"Campaign", "No", "0", "1", "", "campaign", ""},
		[]string{sl[3], sl[4], sl[8], sl[9], sl[5], sl[13], sl[14]})
	tm := byPhone[teamOnly.PhoneNumber]
	assert.Equal(t, []string{"Team", "No", "[image]"}, []string{tm[3], tm[4], tm[13]}, "media without text shows its type")

	for name, tc := range map[string]struct {
		filters map[string]string
		want    []string
	}{
		"engaged":                      {map[string]string{"engagement": "engaged"}, []string{replier.PhoneNumber, organic.PhoneNumber}},
		"not engaged":                  {map[string]string{"engagement": "not_engaged"}, []string{silent.PhoneNumber, teamOnly.PhoneNumber}},
		"campaign":                     {map[string]string{"started_by": "campaign"}, []string{replier.PhoneNumber, silent.PhoneNumber}},
		"customer":                     {map[string]string{"started_by": "customer"}, []string{organic.PhoneNumber}},
		"team":                         {map[string]string{"started_by": "team"}, []string{teamOnly.PhoneNumber}},
		"campaign, engaged":            {map[string]string{"started_by": "campaign", "engagement": "engaged"}, []string{replier.PhoneNumber}},
		"campaign, no reply":           {map[string]string{"started_by": "campaign", "engagement": "not_engaged"}, []string{silent.PhoneNumber}},
		"all is the same as no filter": {map[string]string{"started_by": "all", "engagement": "all"}, phones(rows)},
	} {
		t.Run(name, func(t *testing.T) {
			code, got := call(user.ID, tc.filters)
			require.Equal(t, fasthttp.StatusOK, code)
			assert.Equal(t, tc.want, phones(got))
		})
	}

	code, _ = call(user.ID, map[string]string{"engagement": "maybe"})
	assert.Equal(t, fasthttp.StatusBadRequest, code, "unknown filter value")

	noPermRole := testutil.CreateTestRole(t, app.DB, org.ID, "no-export", getAnalyticsPermissions(t, app))
	noPermUser := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("conv-noperm")), testutil.WithRoleID(&noPermRole.ID))
	code, _ = call(noPermUser.ID, nil)
	assert.Equal(t, fasthttp.StatusForbidden, code, "needs contacts:export")
}
