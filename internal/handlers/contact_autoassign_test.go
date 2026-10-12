package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// A user whose role lets them add contacts but only see the ones assigned to
// them (contacts:write without contacts:read, e.g. a sales role) must be able
// to see a contact straight after adding it.
func TestApp_CreateContact_AssignsToCreatorWhoSeesOnlyTheirOwn(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	salesRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "sales", []string{"contacts:write", "chat:read", "chat:write"})
	sales := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("sales")), testutil.WithRoleID(&salesRole.ID))
	otherSales := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("sales-2")), testutil.WithRoleID(&salesRole.ID))
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("admin")), testutil.WithRoleID(&adminRole.ID))

	create := func(userID uuid.UUID, phone string) handlers.ContactResponse {
		req := testutil.NewJSONRequest(t, map[string]any{"phone_number": phone, "profile_name": "Lead " + phone})
		testutil.SetAuthContext(req, org.ID, userID)
		require.NoError(t, app.CreateContact(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
		var resp struct {
			Data handlers.ContactResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
		return resp.Data
	}
	visibleTo := func(userID uuid.UUID) []string {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		require.NoError(t, app.ListContacts(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var resp struct {
			Data struct {
				Contacts []handlers.ContactResponse `json:"contacts"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
		phones := []string{}
		for _, c := range resp.Data.Contacts {
			phones = append(phones, c.PhoneNumber)
		}
		return phones
	}
	assignee := func(phone string) *uuid.UUID {
		var c models.Contact
		require.NoError(t, app.DB.Where("organization_id = ? AND phone_number = ?", org.ID, phone).First(&c).Error)
		return c.AssignedUserID
	}

	t.Run("sales user sees the contact they add", func(t *testing.T) {
		created := create(sales.ID, "919000000101")
		require.NotNil(t, created.AssignedUserID)
		assert.Equal(t, sales.ID, *created.AssignedUserID)
		assert.Equal(t, []string{"919000000101"}, visibleTo(sales.ID))
		assert.Empty(t, visibleTo(otherSales.ID), "another sales user still does not see it")
	})

	t.Run("a contact added by someone who sees everything stays unassigned", func(t *testing.T) {
		created := create(admin.ID, "919000000102")
		assert.Nil(t, created.AssignedUserID)
		assert.Nil(t, assignee("919000000102"))
		assert.NotContains(t, visibleTo(sales.ID), "919000000102")
	})

	t.Run("re-adding a deleted, unassigned contact assigns it", func(t *testing.T) {
		old := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithPhoneNumber("919000000103"))
		require.NoError(t, app.DB.Delete(old).Error)
		create(sales.ID, "919000000103")
		got := assignee("919000000103")
		require.NotNil(t, got)
		assert.Equal(t, sales.ID, *got)
		assert.Contains(t, visibleTo(sales.ID), "919000000103")
	})

	t.Run("re-adding a deleted contact that belonged to another agent does not take it over", func(t *testing.T) {
		old := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithPhoneNumber("919000000104"))
		require.NoError(t, app.DB.Model(old).Update("assigned_user_id", otherSales.ID).Error)
		require.NoError(t, app.DB.Delete(old).Error)
		create(sales.ID, "919000000104")
		got := assignee("919000000104")
		require.NotNil(t, got)
		assert.Equal(t, otherSales.ID, *got)
	})
}
