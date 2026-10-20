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

// ExportConversations downloads, as CSV, every contact who sent at least one
// message in the period - organic chats as well as campaign replies - with
// one row per contact. It is the "who interacted with us" report: the
// campaign engaged count only sees replies to campaign messages, while most
// conversations start on their own or inside chatbot flows.
//
// GET /api/conversations/export?from=YYYY-MM-DD&to=YYYY-MM-DD
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

	type row struct {
		ProfileName     string
		PhoneNumber     string
		WhatsAppAccount string
		FirstIn         *time.Time
		LastIn          *time.Time
		FromCustomer    int64
		ToCustomer      int64
		Campaigns       string
		AssignedTo      string
		Tags            string
		LastMessage     string
	}

	// One row per contact with at least one incoming message in the period.
	// Every message count, the last message and the campaign attribution are
	// all limited to the same period, so the row describes that window only.
	var rows []row
	if err := a.DB.Raw(`
		SELECT c.profile_name, c.phone_number, c.whats_app_account,
		       min(m.created_at) FILTER (WHERE m.direction = 'incoming') AS first_in,
		       max(m.created_at) FILTER (WHERE m.direction = 'incoming') AS last_in,
		       count(*) FILTER (WHERE m.direction = 'incoming') AS from_customer,
		       count(*) FILTER (WHERE m.direction = 'outgoing') AS to_customer,
		       COALESCE((
		           SELECT string_agg(DISTINCT bc.name, ', ')
		           FROM messages rp
		           JOIN messages cm ON cm.id = rp.reply_to_message_id
		           JOIN bulk_message_campaigns bc ON bc.id::text = cm.metadata->>'campaign_id'
		           WHERE rp.contact_id = c.id AND rp.direction = 'incoming'
		             AND rp.created_at >= ? AND rp.created_at <= ?
		       ), '') AS campaigns,
		       COALESCE(u.full_name, '') AS assigned_to,
		       COALESCE(c.tags::text, '[]') AS tags,
		       COALESCE((
		           SELECT lm.content FROM messages lm
		           WHERE lm.contact_id = c.id AND lm.direction = 'incoming'
		             AND lm.created_at >= ? AND lm.created_at <= ?
		           ORDER BY lm.created_at DESC LIMIT 1
		       ), '') AS last_message
		FROM contacts c
		JOIN messages m ON m.contact_id = c.id AND m.created_at >= ? AND m.created_at <= ?
		LEFT JOIN users u ON u.id = c.assigned_user_id
		WHERE c.organization_id = ? AND c.deleted_at IS NULL
		GROUP BY c.id, u.full_name
		HAVING count(*) FILTER (WHERE m.direction = 'incoming') > 0
		ORDER BY last_in DESC`,
		start, end, start, end, start, end, orgID).Scan(&rows).Error; err != nil {
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
	_ = w.Write([]string{"Name", "Phone", "WhatsApp number", "First message (UTC)", "Last message (UTC)",
		"Messages from customer", "Messages sent to customer", "Replied to campaign", "Assigned to", "Tags", "Last customer message"})
	for _, rw := range rows {
		name, phone := rw.ProfileName, rw.PhoneNumber
		if mask {
			phone = utils.MaskPhoneNumber(phone)
			name = utils.MaskIfPhoneNumber(name)
		}
		var tags []string
		_ = json.Unmarshal([]byte(rw.Tags), &tags)
		record := []string{name, phone, rw.WhatsAppAccount, stamp(rw.FirstIn), stamp(rw.LastIn),
			strconv.FormatInt(rw.FromCustomer, 10), strconv.FormatInt(rw.ToCustomer, 10),
			rw.Campaigns, rw.AssignedTo, strings.Join(tags, ", "), rw.LastMessage}
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
