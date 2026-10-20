package handlers_test

import (
	"encoding/csv"
	"encoding/json"
	"strings"
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

func campaignReadPermissions(t *testing.T, app *handlers.App) []models.Permission {
	t.Helper()
	var perms []models.Permission
	for _, p := range testutil.GetOrCreateTestPermissions(t, app.DB) {
		if p.Resource == models.ResourceCampaigns && p.Action == models.ActionRead {
			perms = append(perms, p)
		}
	}
	require.NotEmpty(t, perms)
	return perms
}

// seedCampaign creates a campaign whose recipients went through every path a
// message can take, with counters set the way the worker and the status
// webhook set them: sent_count on a successful send, the others per webhook.
func seedCampaign(t *testing.T, app *handlers.App, orgID, templateID, userID uuid.UUID, account string, createdAt time.Time) *models.BulkMessageCampaign {
	t.Helper()
	c := &models.BulkMessageCampaign{
		BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: createdAt},
		OrganizationID:  orgID,
		Name:            "Campaign " + uuid.New().String()[:8],
		WhatsAppAccount: account,
		TemplateID:      templateID,
		Status:          models.CampaignStatusCompleted,
		CreatedBy:       userID,
		SentCount:       4, // r1 r2 r3 r5
		DeliveredCount:  2, // r1 r2
		ReadCount:       1, // r1
		FailedCount:     2, // r4 (at send) r5 (after send)
	}
	require.NoError(t, app.DB.Create(c).Error)

	now := time.Now()
	contact := testutil.CreateTestContact(t, app.DB, orgID)
	message := func(dir models.Direction, typ models.MessageType, waID, content string, replyTo *uuid.UUID) *models.Message {
		m := &models.Message{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    orgID,
			WhatsAppAccount:   account,
			ContactID:         contact.ID,
			Direction:         dir,
			MessageType:       typ,
			Content:           content,
			WhatsAppMessageID: waID,
			ReplyToMessageID:  replyTo,
		}
		require.NoError(t, app.DB.Create(m).Error)
		return m
	}
	add := func(phone string, status models.MessageStatus, sent, delivered, read bool) string {
		r := &models.BulkMessageRecipient{
			BaseModel:     models.BaseModel{ID: uuid.New()},
			CampaignID:    c.ID,
			PhoneNumber:   phone,
			RecipientName: "Recipient " + phone,
			Status:        status,
		}
		if sent {
			r.SentAt = &now
			r.WhatsAppMessageID = "wamid." + uuid.New().String()
		}
		if delivered {
			r.DeliveredAt = &now
		}
		if read {
			r.ReadAt = &now
		}
		require.NoError(t, app.DB.Create(r).Error)
		return r.WhatsAppMessageID
	}
	r1 := add("919000000001", models.MessageStatusRead, true, true, true)  // r1 sent, delivered, read
	r2 := add("919000000002", models.MessageStatusDelivered, true, true, false) // r2 sent, delivered
	add("919000000003", models.MessageStatusSent, true, false, false)     // r3 sent
	add("919000000004", models.MessageStatusFailed, false, false, false)  // r4 failed at send
	add("919000000005", models.MessageStatusFailed, true, false, false)   // r5 sent, then failed
	add("919000000006", models.MessageStatusPending, false, false, false) // r6 not sent yet

	// r1 taps a reply button on the campaign message: engaged.
	sent1 := message(models.DirectionOutgoing, models.MessageTypeTemplate, r1, "campaign", nil)
	message(models.DirectionIncoming, models.MessageType("button_reply"), "wamid.in."+uuid.New().String(), "Interested", &sent1.ID)
	// r2 writes in, but not as a reply to the campaign message: not engaged.
	message(models.DirectionOutgoing, models.MessageTypeTemplate, r2, "campaign", nil)
	message(models.DirectionIncoming, models.MessageTypeText, "wamid.in."+uuid.New().String(), "hello", nil)
	return c
}

func csvDataRows(t *testing.T, body []byte) [][]string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records, "expected at least a header row")
	return records[1:]
}

func TestApp_ExportWidgetCampaignRecipients_RowsMatchWidgetNumbers(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRole(t, app.DB, org.ID, "campaign-viewer", campaignReadPermissions(t, app))
	user := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-export")),
		testutil.WithRoleID(&role.ID),
	)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("export-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	// In the period: counted.
	seedCampaign(t, app, org.ID, template.ID, user.ID, account.Name, time.Now())
	// Outside the period: must not appear in the numbers or the export.
	seedCampaign(t, app, org.ID, template.ID, user.ID, account.Name, time.Now().AddDate(0, -3, 0))
	// Another organisation: must never appear.
	other := testutil.CreateTestOrganization(t, app.DB)
	otherUser := testutil.CreateTestUser(t, app.DB, other.ID, testutil.WithEmail(testutil.UniqueEmail("other-org")))
	otherAccount := testutil.CreateTestWhatsAppAccountWith(t, app.DB, other.ID, testutil.WithAccountName("other-account"))
	otherTemplate := testutil.CreateTestTemplate(t, app.DB, other.ID, otherAccount.Name)
	seedCampaign(t, app, other.ID, otherTemplate.ID, otherUser.ID, otherAccount.Name, time.Now())

	widget := &models.Widget{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		UserID:         &user.ID,
		Name:           "Campaign Performance",
		DataSource:     "campaigns",
		Metric:         "count",
		DisplayType:    "table",
		GroupByField:   "message_status",
	}
	require.NoError(t, app.DB.Create(widget).Error)

	from := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	to := time.Now().Format("2006-01-02")

	// What the dashboard shows.
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())
	testutil.SetQueryParam(req, "from", from)
	testutil.SetQueryParam(req, "to", to)
	require.NoError(t, app.GetWidgetData(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var resp struct {
		Data struct {
			DataPoints []struct {
				Label string  `json:"label"`
				Value float64 `json:"value"`
			} `json:"data_points"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	shown := map[string]int{}
	for _, dp := range resp.Data.DataPoints {
		shown[dp.Label] = int(dp.Value)
	}
	require.Equal(t, map[string]int{"sent": 4, "delivered": 2, "read": 1, "engaged": 1, "failed": 2}, shown)

	// What each download contains must equal that number.
	for status, want := range shown {
		t.Run(status, func(t *testing.T) {
			req := testutil.NewGETRequest(t)
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", widget.ID.String())
			testutil.SetQueryParam(req, "status", status)
			testutil.SetQueryParam(req, "from", from)
			testutil.SetQueryParam(req, "to", to)
			require.NoError(t, app.ExportWidgetCampaignRecipients(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			rows := csvDataRows(t, testutil.GetResponseBody(req))
			assert.Equal(t, want, len(rows), "CSV rows for %q must equal the number on the widget", status)
			if status == "engaged" && len(rows) == 1 {
				assert.Equal(t, "Interested", rows[0][7], "Reply column carries what the customer tapped")
			}
		})
	}
}

func TestApp_ExportWidgetCampaignRecipients_Guards(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRole(t, app.DB, org.ID, "campaign-viewer", campaignReadPermissions(t, app))
	user := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("export-guards")),
		testutil.WithRoleID(&role.ID),
	)
	noPermRole := testutil.CreateTestRole(t, app.DB, org.ID, "no-campaigns", getAnalyticsPermissions(t, app))
	noPermUser := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("export-noperm")),
		testutil.WithRoleID(&noPermRole.ID),
	)

	grouped := &models.Widget{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, UserID: &user.ID,
		Name: "Campaign Performance", DataSource: "campaigns", Metric: "count", DisplayType: "table", GroupByField: "message_status", IsShared: true}
	plain := &models.Widget{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, UserID: &user.ID,
		Name: "Total Messages", DataSource: "messages", Metric: "count", DisplayType: "number"}
	require.NoError(t, app.DB.Create(grouped).Error)
	require.NoError(t, app.DB.Create(plain).Error)

	call := func(userID uuid.UUID, widgetID uuid.UUID, status string) int {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		testutil.SetPathParam(req, "id", widgetID.String())
		testutil.SetQueryParam(req, "status", status)
		require.NoError(t, app.ExportWidgetCampaignRecipients(req))
		return testutil.GetResponseStatusCode(req)
	}

	assert.Equal(t, fasthttp.StatusBadRequest, call(user.ID, grouped.ID, "pending"), "unknown status")
	assert.Equal(t, fasthttp.StatusBadRequest, call(user.ID, plain.ID, "sent"), "widget not grouped by message_status")
	assert.Equal(t, fasthttp.StatusNotFound, call(user.ID, uuid.New(), "sent"), "unknown widget")
	assert.Equal(t, fasthttp.StatusForbidden, call(noPermUser.ID, grouped.ID, "sent"), "no campaigns:read")
}
