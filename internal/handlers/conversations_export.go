package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// conversationEngagementFilter and conversationStartedByFilter are the report's
// optional filters, mapped to conditions on the per-contact rows below.
var conversationEngagementFilter = map[string]string{
	"engaged":     "from_customer > 0",
	"not_engaged": "from_customer = 0",
}

// messageText is a message's text for the report, or its type in brackets
// ("[image]") when it has none, so media messages don't show as blank cells.
const messageText = "COALESCE(NULLIF(fm.content, ''), '[' || fm.message_type || ']')"

var conversationStartedByFilter = map[string]string{
	"campaign": "started_by = 'Campaign'",
	"customer": "started_by = 'Customer'",
	"team":     "started_by = 'Team'",
}

// ExportConversations downloads, as CSV, every contact we were in a
// conversation with during the period, one row per contact: those who wrote
// to us (engaged) and those we reached who never replied (not engaged).
// Outgoing messages that failed to send don't count as reaching anyone.
//
// "Started by" says how the conversation began:
//   - Campaign: they were sent a campaign message in the period, or replied
//     to one (even one sent before the period)
//   - Customer: no campaign, and the first message in the period was theirs
//   - Team: no campaign, and an agent wrote first (e.g. a manual template)
//
// GET /api/conversations/export?from=YYYY-MM-DD&to=YYYY-MM-DD&engagement=engaged|not_engaged&started_by=campaign|customer|team
func (a *App) ExportConversations(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionExport, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := now
	fromStr := string(r.RequestCtx.QueryArgs().Peek("from"))
	toStr := string(r.RequestCtx.QueryArgs().Peek("to"))
	if fromStr != "" && toStr != "" {
		s, e, errMsg := parseDateRange(fromStr, toStr)
		if errMsg != "" {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
		}
		start, end = s, e
	}

	where := []string{"true"}
	for param, conditions := range map[string]map[string]string{
		"engagement": conversationEngagementFilter,
		"started_by": conversationStartedByFilter,
	} {
		value := string(r.RequestCtx.QueryArgs().Peek(param))
		if value == "" || value == "all" {
			continue
		}
		condition, ok := conditions[value]
		if !ok {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+param, nil, "")
		}
		where = append(where, condition)
	}

	type row struct {
		ProfileName          string
		PhoneNumber          string
		WhatsAppAccount      string
		StartedBy            string
		FirstIn              *time.Time
		LastIn               *time.Time
		LastOut              *time.Time
		FromCustomer         int64
		ToCustomer           int64
		Campaigns            string
		AssignedTo           string
		Tags                 string
		FirstMessage         string
		FirstCustomerMessage string
		LastMessage          string
	}

	// per: one row per contact with a message in the period, failed sends
	// excluded. Every count, the last message and the campaign attribution
	// are limited to the same period, so the row describes that window only.
	var rows []row
	if err := a.DB.Raw(`
		WITH per AS (
			SELECT c.id, c.profile_name, c.phone_number, c.whats_app_account, c.tags, c.assigned_user_id,
			       min(m.created_at) FILTER (WHERE m.direction = 'incoming') AS first_in,
			       max(m.created_at) FILTER (WHERE m.direction = 'incoming') AS last_in,
			       max(m.created_at) FILTER (WHERE m.direction = 'outgoing') AS last_out,
			       count(*) FILTER (WHERE m.direction = 'incoming') AS from_customer,
			       count(*) FILTER (WHERE m.direction = 'outgoing') AS to_customer,
			       bool_or(m.direction = 'outgoing' AND COALESCE(m.metadata->>'campaign_id', '') <> '') AS got_campaign,
			       (array_agg(m.direction ORDER BY m.created_at))[1] AS first_direction
			FROM contacts c
			JOIN messages m ON m.contact_id = c.id AND m.created_at >= ? AND m.created_at <= ?
			     AND NOT (m.direction = 'outgoing' AND m.status = 'failed')
			WHERE c.organization_id = ? AND c.deleted_at IS NULL
			GROUP BY c.id
		), attributed AS (
			SELECT per.*,
			       COALESCE((
			           SELECT string_agg(DISTINCT bc.name, ', ')
			           FROM messages rp
			           JOIN messages cm ON cm.id = rp.reply_to_message_id
			           JOIN bulk_message_campaigns bc ON bc.id::text = cm.metadata->>'campaign_id'
			           WHERE rp.contact_id = per.id AND rp.direction = 'incoming'
			             AND rp.created_at >= ? AND rp.created_at <= ?
			       ), '') AS campaigns
			FROM per
		), classified AS (
			SELECT attributed.*,
			       CASE WHEN got_campaign OR campaigns <> '' THEN 'Campaign'
			            WHEN first_direction = 'incoming' THEN 'Customer'
			            ELSE 'Team' END AS started_by
			FROM attributed
		)
		SELECT k.profile_name, k.phone_number, k.whats_app_account, k.started_by,
		       k.first_in, k.last_in, k.last_out, k.from_customer, k.to_customer, k.campaigns,
		       COALESCE(u.full_name, '') AS assigned_to,
		       COALESCE(k.tags::text, '[]') AS tags,
		       COALESCE((
		           SELECT `+messageText+` FROM messages fm
		           WHERE fm.contact_id = k.id AND fm.created_at >= ? AND fm.created_at <= ?
		             AND NOT (fm.direction = 'outgoing' AND fm.status = 'failed')
		           ORDER BY fm.created_at LIMIT 1
		       ), '') AS first_message,
		       COALESCE((
		           SELECT `+messageText+` FROM messages fm
		           WHERE fm.contact_id = k.id AND fm.direction = 'incoming'
		             AND fm.created_at >= ? AND fm.created_at <= ?
		           ORDER BY fm.created_at LIMIT 1
		       ), '') AS first_customer_message,
		       COALESCE((
		           SELECT `+messageText+` FROM messages fm
		           WHERE fm.contact_id = k.id AND fm.direction = 'incoming'
		             AND fm.created_at >= ? AND fm.created_at <= ?
		           ORDER BY fm.created_at DESC LIMIT 1
		       ), '') AS last_message
		FROM classified k
		LEFT JOIN users u ON u.id = k.assigned_user_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY GREATEST(k.last_in, k.last_out) DESC`,
		start, end, orgID, start, end, start, end, start, end, start, end).Scan(&rows).Error; err != nil {
		a.Log.Error("Failed to export conversations", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to export conversations", nil, "")
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
	_ = w.Write([]string{"Name", "Phone", "WhatsApp number", "Started by", "Engaged",
		"First customer message (UTC)", "Last customer message (UTC)", "Last message to customer (UTC)",
		"Messages from customer", "Messages sent to customer", "Replied to campaign", "Assigned to", "Tags", "First message", "First customer message", "Last customer message"})
	for _, rw := range rows {
		name, phone := rw.ProfileName, rw.PhoneNumber
		if mask {
			phone = utils.MaskPhoneNumber(phone)
			name = utils.MaskIfPhoneNumber(name)
		}
		var tags []string
		_ = json.Unmarshal([]byte(rw.Tags), &tags)
		engaged := "No"
		if rw.FromCustomer > 0 {
			engaged = "Yes"
		}
		record := []string{name, phone, rw.WhatsAppAccount, rw.StartedBy, engaged,
			stamp(rw.FirstIn), stamp(rw.LastIn), stamp(rw.LastOut),
			strconv.FormatInt(rw.FromCustomer, 10), strconv.FormatInt(rw.ToCustomer, 10),
			rw.Campaigns, rw.AssignedTo, strings.Join(tags, ", "),
			rw.FirstMessage, rw.FirstCustomerMessage, rw.LastMessage}
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

	filename := fmt.Sprintf("conversations_%s_to_%s.csv", start.Format("20060102"), end.Format("20060102"))
	r.RequestCtx.Response.Header.Set("Content-Type", "text/csv")
	r.RequestCtx.Response.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	r.RequestCtx.SetBody([]byte(buf.String()))
	return nil
}
