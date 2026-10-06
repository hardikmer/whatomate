package handlers

import (
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// campaignStatusRecipientCondition maps a message_status group to the
// recipients that make up its number. The widget sums the campaign counters,
// which are incremented as events happen, so they are cumulative: a message
// that was sent, then delivered, then read counts once in each of sent,
// delivered and read. The recipient's timestamps record the same events, so
// selecting on them (rather than on the current status) gives the same set.
var campaignStatusRecipientCondition = map[string]string{
	"sent":      "r.sent_at IS NOT NULL",
	"delivered": "r.delivered_at IS NOT NULL",
	"read":      "r.read_at IS NOT NULL",
	"engaged":   campaignEngagedCondition,
	"failed":    "r.status = 'failed'",
}

// campaignEngagedCondition: the recipient replied to, or tapped a reply button
// on, the campaign message they were sent. WhatsApp sends no "engaged" status,
// but it does tie such a reply to the message it answers, which we store as
// reply_to_message_id on the incoming message. Link clicks are not reported
// per recipient by WhatsApp, so they cannot count.
//
// Recipients are never linked to their Message row (message_id stays empty),
// so the join goes through the WhatsApp message id that both sides store.
const campaignEngagedCondition = `EXISTS (
		SELECT 1 FROM messages sent
		JOIN messages reply ON reply.reply_to_message_id = sent.id AND reply.direction = 'incoming'
		WHERE r.whats_app_message_id <> '' AND sent.whats_app_message_id = r.whats_app_message_id)`

// campaignScopeSQL selects the campaigns a message_status widget counts, built
// exactly as getCampaignMessageStatusData builds it. The widget filters
// reference campaign columns unqualified (status, name, ...), which would be
// ambiguous in a join with recipients, so callers use it as a subquery.
//
// No deleted_at filter, deliberately: the widget's raw query counts
// soft-deleted campaigns too, and recipients can only be deleted while a
// campaign is a draft, before anything is sent.
func campaignScopeSQL(orgID any, filters []FilterInput, start, end time.Time) (string, []any) {
	query := `SELECT id FROM bulk_message_campaigns
		WHERE organization_id = ? AND created_at >= ? AND created_at <= ?`
	return appendFilterSQL("campaigns", query, []any{orgID, start, end}, filters)
}

// campaignEngagedCount is the "engaged" figure for a message_status widget.
// Unlike the other four it is not a stored counter but counted live, so it
// stays correct whenever a reply arrives.
func (a *App) campaignEngagedCount(orgID any, filters []FilterInput, start, end time.Time) int64 {
	scope, args := campaignScopeSQL(orgID, filters, start, end)
	var n int64
	a.DB.Raw(`SELECT count(*) FROM bulk_message_recipients r
		WHERE `+campaignEngagedCondition+` AND r.campaign_id IN (`+scope+`)`, args...).Scan(&n)
	return n
}

// campaignEngagedByDay returns engaged recipients per campaign-creation day,
// keyed "2006-01-02", for the line-chart form of the widget.
func (a *App) campaignEngagedByDay(orgID any, filters []FilterInput, start, end time.Time) map[string]int64 {
	scope, args := campaignScopeSQL(orgID, filters, start, end)
	var rows []struct {
		Date time.Time
		N    int64
	}
	a.DB.Raw(`SELECT DATE_TRUNC('day', c.created_at) AS date, count(*) AS n
		FROM bulk_message_recipients r
		JOIN bulk_message_campaigns c ON c.id = r.campaign_id
		WHERE `+campaignEngagedCondition+` AND r.campaign_id IN (`+scope+`)
		GROUP BY 1`, args...).Scan(&rows)
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Date.Format("2006-01-02")] = row.N
	}
	return out
}

// ExportWidgetCampaignRecipients downloads, as CSV, the campaign recipients
// behind one row of a campaigns widget grouped by message_status - the people
// a "sent 1,204" row is counting. It scopes campaigns exactly as the widget
// does: same organisation, same created_at period, same widget filters.
func (a *App) ExportWidgetCampaignRecipients(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Phone numbers leave the system here, so require campaign access on top
	// of the widget being visible to this user.
	if !a.HasPermission(userID, models.ResourceCampaigns, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	id, err := parsePathUUID(r, "id", "widget")
	if err != nil {
		return nil
	}

	status := string(r.RequestCtx.QueryArgs().Peek("status"))
	condition, ok := campaignStatusRecipientCondition[status]
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "status must be one of sent, delivered, read, engaged, failed", nil, "")
	}

	var widget models.Widget
	if err := a.DB.Where(
		"id = ? AND organization_id = ? AND (user_id = ? OR is_shared = true)",
		id, orgID, userID,
	).First(&widget).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Widget not found", nil, "")
	}
	if widget.DataSource != "campaigns" || widget.GroupByField != "message_status" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Export is only available for campaign widgets grouped by message status", nil, "")
	}

	// Same period rules as executeWidgetQuery, so the export matches the number.
	fromStr := string(r.RequestCtx.QueryArgs().Peek("from"))
	toStr := string(r.RequestCtx.QueryArgs().Peek("to"))
	now := time.Now()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := now
	if fromStr != "" && toStr != "" {
		if start, end, errMsg := parseDateRange(fromStr, toStr); errMsg == "" {
			periodStart, periodEnd = start, end
		}
	}

	filters := make([]FilterInput, 0, len(widget.Filters))
	for _, f := range widget.Filters {
		if filterMap, ok := f.(map[string]any); ok {
			filters = append(filters, FilterInput{
				Field:    widgetGetString(filterMap, "field"),
				Operator: widgetGetString(filterMap, "operator"),
				Value:    widgetGetString(filterMap, "value"),
			})
		}
	}

	// Same campaign scope as the widget's own numbers, so rows equal the count.
	campaignQuery, args := campaignScopeSQL(orgID, filters, periodStart, periodEnd)

	type exportRow struct {
		CampaignName  string
		RecipientName string
		PhoneNumber   string
		Status        string
		ErrorMessage  string
		SentAt        *time.Time
		DeliveredAt   *time.Time
		ReadAt        *time.Time
		Reply         string
	}

	// reply = the first message the recipient sent in reply to this campaign
	// message (a button tap's title, or their text); empty if they never did.
	var rows []exportRow
	if err := a.DB.Raw(`
		SELECT c.name AS campaign_name, r.recipient_name, r.phone_number, r.status,
		       r.error_message, r.sent_at, r.delivered_at, r.read_at,
		       COALESCE(fr.content, '') AS reply
		FROM bulk_message_recipients r
		JOIN bulk_message_campaigns c ON c.id = r.campaign_id
		LEFT JOIN LATERAL (
			SELECT reply.content FROM messages sent
			JOIN messages reply ON reply.reply_to_message_id = sent.id AND reply.direction = 'incoming'
			WHERE r.whats_app_message_id <> '' AND sent.whats_app_message_id = r.whats_app_message_id
			ORDER BY reply.created_at LIMIT 1
		) fr ON true
		WHERE `+condition+`
		  AND r.campaign_id IN (`+campaignQuery+`)
		ORDER BY c.created_at, r.created_at`, args...).Scan(&rows).Error; err != nil {
		a.Log.Error("Failed to export widget recipients", "error", err, "widget_id", id)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to export recipients", nil, "")
	}

	mask := a.ShouldMaskPhoneNumbers(orgID)
	stamp := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05")
	}

	var buf strings.Builder
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Campaign", "Recipient", "Phone", "Current status", "Sent at (UTC)", "Delivered at (UTC)", "Read at (UTC)", "Reply", "Error"})
	for _, row := range rows {
		name, phone := row.RecipientName, row.PhoneNumber
		if mask {
			phone = utils.MaskPhoneNumber(phone)
			name = utils.MaskIfPhoneNumber(name)
		}
		record := []string{row.CampaignName, name, phone, row.Status, stamp(row.SentAt), stamp(row.DeliveredAt), stamp(row.ReadAt), row.Reply, row.ErrorMessage}
		// Same CSV-injection guard as the other exports: '=' and '@' start a
		// formula in spreadsheets. '+' and '-' are left alone (phone numbers).
		for i, cell := range record {
			if len(cell) > 0 && (cell[0] == '=' || cell[0] == '@') {
				record[i] = "'" + cell
			}
		}
		_ = w.Write(record)
	}
	w.Flush()

	filename := fmt.Sprintf("campaign_%s_%s_to_%s.csv", status, periodStart.Format("20060102"), periodEnd.Format("20060102"))
	r.RequestCtx.Response.Header.Set("Content-Type", "text/csv")
	r.RequestCtx.Response.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	r.RequestCtx.SetBody([]byte(buf.String()))
	return nil
}
