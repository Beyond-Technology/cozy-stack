package bitwarden

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	_ "github.com/cozy/cozy-stack/worker/mails"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
)

var ts *httptest.Server
var inst *instance.Instance
var token string
var orgaID, collID, folderID, cipherID string

func TestPrelogin(t *testing.T) {
	body := `{ "email": "me@bitwarden.example.net" }`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/accounts/prelogin", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]int
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, 0, result["Kdf"])
	assert.Equal(t, crypto.DefaultPBKDF2Iterations, result["KdfIterations"])
}

func TestConnect(t *testing.T) {
	testLogger := test.NewGlobal()
	setting, err := settings.Get(inst)
	assert.NoError(t, err)
	setting.EncryptedOrgKey = ""
	err = setting.Save(inst)
	assert.NoError(t, err)

	email := inst.PassphraseSalt()
	iter := crypto.DefaultPBKDF2Iterations
	pass, _ := crypto.HashPassWithPBKDF2([]byte("cozy"), email, iter)
	v := url.Values{
		"grant_type": {"password"},
		"username":   {string(email)},
		"password":   {string(pass)},
		"scope":      {"api offline_access"},
		"client_id":  {"browser"},
		"deviceType": {"3"},
	}
	res, err := http.PostForm(ts.URL+"/bitwarden/identity/connect/token", v)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	expiresIn := consts.AccessTokenValidityDuration.Seconds()
	assert.Equal(t, "Bearer", result["token_type"])
	assert.Equal(t, expiresIn, result["expires_in"])
	if assert.NotEmpty(t, result["access_token"]) {
		token = result["access_token"].(string)
	}
	assert.NotEmpty(t, result["refresh_token"])
	assert.NotEmpty(t, result["Key"])
	assert.NotEmpty(t, result["PrivateKey"])
	assert.NotEmpty(t, result["client_id"])
	assert.NotEmpty(t, result["registration_access_token"])
	assert.NotNil(t, result["Kdf"])
	assert.NotNil(t, result["KdfIterations"])

	assert.NotZero(t, len(testLogger.Entries))
	assert.Equal(t, "Organization key does not exist", testLogger.Entries[0].Message)

	setting, err = settings.Get(inst)
	assert.NoError(t, err)
	orgKey, err := setting.OrganizationKey()
	assert.NoError(t, err)
	assert.NotEmpty(t, orgKey)
}

func TestGetCozyOrg(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/organizations/cozy", nil)
	req.Header.Add("Authorization", "Bearer invalid-token")
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 401, res.StatusCode)

	req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/organizations/cozy", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]string
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	orgaID = result["organizationId"]
	assert.NotEmpty(t, orgaID)
	collID = result["collectionId"]
	assert.NotEmpty(t, collID)
	orgKey := result["organizationKey"]
	assert.NotEmpty(t, orgKey)
	_, err = base64.StdEncoding.DecodeString(orgKey)
	assert.NoError(t, err)
}

func TestCreateFolder(t *testing.T) {
	body := `
{
	"name": "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o="
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/folders", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]string
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o=", result["Name"])
	assert.Equal(t, "folder", result["Object"])
	assert.NotEmpty(t, result["RevisionDate"])
	assert.NotEmpty(t, result["Id"])
	folderID = result["Id"]
}

func TestListFolders(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/folders", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "list", result["Object"])
	data := result["Data"].([]interface{})
	assert.Len(t, data, 1)
	item := data[0].(map[string]interface{})
	assert.Equal(t, "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o=", item["Name"])
	assert.Equal(t, "folder", item["Object"])
	assert.Equal(t, folderID, item["Id"])
	assert.NotEmpty(t, item["RevisionDate"])
}

func TestGetFolder(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/folders/"+folderID, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o=", result["Name"])
	assert.Equal(t, "folder", result["Object"])
	assert.Equal(t, folderID, result["Id"])
	assert.NotEmpty(t, result["RevisionDate"])
}

func TestRenameFolder(t *testing.T) {
	body := `
{
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io="
}`
	req, _ := http.NewRequest("PUT", ts.URL+"/bitwarden/api/folders/"+folderID, bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]string
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=", result["Name"])
	assert.Equal(t, "folder", result["Object"])
	assert.NotEmpty(t, result["RevisionDate"])
	assert.Equal(t, folderID, result["Id"])
}

func TestDeleteFolder(t *testing.T) {
	body := `
{
	"name": "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o="
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/folders", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]string
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	id := result["Id"]

	body = `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": "` + id + `",
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"totp": null
	}
}`
	req, _ = http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result2 map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result2)
	assert.NoError(t, err)
	cID := result2["Id"].(string)

	req, _ = http.NewRequest("DELETE", ts.URL+"/bitwarden/api/folders/"+id, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	// Check that the cipher in this folder has been moved out
	req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers/"+cID, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result3 map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result3)
	assert.NoError(t, err)
	assert.Equal(t, "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=", result3["Name"])
	fID, ok := result3["FolderId"]
	assert.True(t, ok)
	assert.Empty(t, fID)

	req, _ = http.NewRequest("DELETE", ts.URL+"/bitwarden/api/ciphers/"+cID, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
}

func TestCreateNoType(t *testing.T) {
	body := `
{
	"name": "2.G38TIU3t1pGOfkzjCQE7OQ==|Xa1RupttU7zrWdzIT6oK+w==|J3C6qU1xDrfTgyJD+OrDri1GjgGhU2nmRK75FbZHXoI=",
	"organizationId": null
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 400, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.NotEmpty(t, result["error"])
}

func TestCreateLogin(t *testing.T) {
	body := `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": null,
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"passwordRevisionDate": "2019-09-13T12:26:42+02:00",
		"totp": null
	}
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assertCipherResponse(t, result)
	orgID, ok := result["OrganizationId"]
	assert.True(t, ok)
	assert.Empty(t, orgID)
	cipherID = result["Id"].(string)
}

func TestListCiphers(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "list", result["Object"])
	data := result["Data"].([]interface{})
	assert.Len(t, data, 1)
	item := data[0].(map[string]interface{})
	assertCipherResponse(t, item)
	orgID, ok := item["OrganizationId"]
	assert.True(t, ok)
	assert.Empty(t, orgID)
}

func TestGetCipher(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers/"+cipherID, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assertCipherResponse(t, result)
	orgID, ok := result["OrganizationId"]
	assert.True(t, ok)
	assert.Empty(t, orgID)
}

func assertCipherResponse(t *testing.T, result map[string]interface{}) {
	assert.Equal(t, "cipher", result["Object"])
	assert.NotEmpty(t, result["Id"])
	assert.Equal(t, float64(1), result["Type"])
	assert.Equal(t, false, result["Favorite"])
	assert.Equal(t, "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=", result["Name"])
	notes, ok := result["Notes"]
	assert.True(t, ok)
	assert.Empty(t, notes)
	fID, ok := result["FolderId"]
	assert.True(t, ok)
	assert.Empty(t, fID)
	login := result["Login"].(map[string]interface{})
	uris := login["Uris"].([]interface{})
	assert.Len(t, uris, 1)
	uri := uris[0].(map[string]interface{})
	assert.Equal(t, "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=", uri["Uri"])
	match, ok := uri["Match"]
	assert.True(t, ok)
	assert.Empty(t, match)
	assert.Equal(t, "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=", login["Username"])
	assert.Equal(t, "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=", login["Password"])
	assert.Equal(t, "2019-09-13T12:26:42+02:00", login["PasswordRevisionDate"])
	totp, ok := login["Totp"]
	assert.True(t, ok)
	assert.Empty(t, totp)
	fields, ok := result["Fields"]
	assert.True(t, ok)
	assert.Empty(t, fields)
	attachments, ok := result["Attachments"]
	assert.True(t, ok)
	assert.Empty(t, attachments)
	assert.NotEmpty(t, result["RevisionDate"])
	assert.Equal(t, true, result["Edit"])
	assert.Equal(t, false, result["OrganizationUseTotp"])
}

func TestUpdateCipher(t *testing.T) {
	body := `
{
	"type": 2,
	"favorite": true,
	"name": "2.G38TIU3t1pGOfkzjCQE7OQ==|Xa1RupttU7zrWdzIT6oK+w==|J3C6qU1xDrfTgyJD+OrDri1GjgGhU2nmRK75FbZHXoI=",
	"folderId": "` + folderID + `",
	"organizationId": null,
	"notes": "2.rSw0uVQEFgUCEmOQx0JnDg==|MKqHLD25aqaXYHeYJPH/mor7l3EeSQKsI7A/R+0bFTI=|ODcUScISzKaZWHlUe4MRGuTT2S7jpyDmbOHl7d+6HiM=",
	"secureNote": {
		"type": 0
	}
}`
	req, _ := http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/"+cipherID, bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assertUpdatedCipherResponse(t, result)
	orgID, ok := result["OrganizationId"]
	assert.True(t, ok)
	assert.Empty(t, orgID)
}

func assertUpdatedCipherResponse(t *testing.T, result map[string]interface{}) {
	assert.Equal(t, "cipher", result["Object"])
	assert.Equal(t, cipherID, result["Id"])
	assert.Equal(t, float64(2), result["Type"])
	assert.Equal(t, true, result["Favorite"])
	assert.Equal(t, "2.G38TIU3t1pGOfkzjCQE7OQ==|Xa1RupttU7zrWdzIT6oK+w==|J3C6qU1xDrfTgyJD+OrDri1GjgGhU2nmRK75FbZHXoI=", result["Name"])
	assert.Equal(t, folderID, result["FolderId"])
	assert.Equal(t, "2.rSw0uVQEFgUCEmOQx0JnDg==|MKqHLD25aqaXYHeYJPH/mor7l3EeSQKsI7A/R+0bFTI=|ODcUScISzKaZWHlUe4MRGuTT2S7jpyDmbOHl7d+6HiM=", result["Notes"])
	secure := result["SecureNote"].(map[string]interface{})
	assert.Equal(t, float64(0), secure["Type"])
	_, ok := result["Login"]
	assert.False(t, ok)
	fields, ok := result["Fields"]
	assert.True(t, ok)
	assert.Empty(t, fields)
	attachments, ok := result["Attachments"]
	assert.True(t, ok)
	assert.Empty(t, attachments)
	assert.NotEmpty(t, result["RevisionDate"])
	assert.Equal(t, true, result["Edit"])
	assert.Equal(t, false, result["OrganizationUseTotp"])
}

func TestDeleteCipher(t *testing.T) {
	body := `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": null,
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"totp": null
	}
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	id := result["Id"].(string)

	req, _ = http.NewRequest("DELETE", ts.URL+"/bitwarden/api/ciphers/"+id, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
}

func TestSoftDeleteCipher(t *testing.T) {
	body := `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": null,
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"totp": null
	}
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	id := result["Id"].(string)

	req, _ = http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/"+id+"/delete", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers/"+id, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.NotEmpty(t, result["DeletedDate"])
}

func TestRestoreCipher(t *testing.T) {
	body := `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": null,
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"totp": null
	}
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	id := result["Id"].(string)

	req, _ = http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/"+id+"/delete", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	req, _ = http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/"+id+"/restore", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers/"+id, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Empty(t, result["DeletedDate"])
}

func TestSync(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/sync", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "sync", result["Object"])

	profile := result["Profile"].(map[string]interface{})
	assert.NotEmpty(t, profile["Id"])
	assert.Equal(t, "Pierre", profile["Name"])
	assert.Equal(t, "me@bitwarden.example.net", profile["Email"])
	assert.Equal(t, false, profile["EmailVerified"])
	assert.Equal(t, true, profile["Premium"])
	assert.Equal(t, nil, profile["MasterPasswordHint"])
	assert.Equal(t, "en", profile["Culture"])
	assert.Equal(t, false, profile["TwoFactorEnabled"])
	assert.NotEmpty(t, profile["Key"])
	assert.NotEmpty(t, profile["PrivateKey"])
	assert.NotEmpty(t, profile["SecurityStamp"])
	assert.Equal(t, "profile", profile["Object"])

	ciphers := result["Ciphers"].([]interface{})
	assert.Len(t, ciphers, 3)
	c := ciphers[0].(map[string]interface{})
	assertUpdatedCipherResponse(t, c)

	folders := result["Folders"].([]interface{})
	assert.Len(t, folders, 1)
	f := folders[0].(map[string]interface{})
	assert.Equal(t, "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=", f["Name"])
	assert.Equal(t, "folder", f["Object"])
	assert.NotEmpty(t, f["RevisionDate"])
	assert.Equal(t, folderID, f["Id"])

	domains := result["Domains"].(map[string]interface{})
	ed, ok := domains["EquivalentDomains"]
	assert.True(t, ok)
	assert.Empty(t, ed)
	ged, ok := domains["GlobalEquivalentDomains"]
	assert.True(t, ok)
	assert.NotEmpty(t, ged)
	assert.Equal(t, "domains", domains["Object"])
}

func TestBulkDeleteCiphers(t *testing.T) {
	// Setup
	nbCiphersToDelete := 5
	nbCiphers, err := couchdb.CountAllDocs(inst, consts.BitwardenCiphers)
	assert.NoError(t, err)

	var ids []string
	for i := 0; i < nbCiphersToDelete; i++ {
		body := `
{
	"type": 1,
	"favorite": false,
	"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
	"notes": null,
	"folderId": null,
	"organizationId": null,
	"login": {
		"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
		"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
		"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
		"totp": null
	}
}`
		req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers", bytes.NewBufferString(body))
		req.Header.Add("Content-Type", "application/json")
		req.Header.Add("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		assert.NoError(t, err)
		assert.Equal(t, 200, res.StatusCode)
		var result map[string]interface{}
		err = json.NewDecoder(res.Body).Decode(&result)
		assert.NoError(t, err)
		ids = append(ids, result["Id"].(string))
	}

	nb, err := couchdb.CountAllDocs(inst, consts.BitwardenCiphers)
	assert.NoError(t, err)
	assert.Equal(t, nbCiphers+nbCiphersToDelete, nb)

	// Test soft delete in bulk
	body, _ := json.Marshal(map[string][]string{
		"ids": ids,
	})
	buf := bytes.NewBuffer(body)
	req, _ := http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/delete", buf)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	for _, id := range ids {
		req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/api/ciphers/"+id, nil)
		req.Header.Add("Authorization", "Bearer "+token)
		res, err = http.DefaultClient.Do(req)
		assert.NoError(t, err)
		assert.Equal(t, 200, res.StatusCode)
		var result map[string]interface{}
		err = json.NewDecoder(res.Body).Decode(&result)
		assert.NoError(t, err)
		assert.NotEmpty(t, result["DeletedDate"])
	}

	// Test restore in bulk
	buf = bytes.NewBuffer(body)
	req, _ = http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/restore", buf)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "list", result["Object"])
	data := result["Data"].([]interface{})
	assert.Len(t, data, nbCiphersToDelete)

	for i := range data {
		item := data[i].(map[string]interface{})
		assert.Equal(t, ids[i], item["Id"])
		assert.Empty(t, item["DeletedDate"])
	}

	// Test delete in bulk
	buf = bytes.NewBuffer(body)
	req, _ = http.NewRequest("DELETE", ts.URL+"/bitwarden/api/ciphers", buf)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	nb, err = couchdb.CountAllDocs(inst, consts.BitwardenCiphers)
	assert.NoError(t, err)
	assert.Equal(t, nbCiphers, nb)
}

func TestSharedCipher(t *testing.T) {
	body := `
{
	"cipher": {
		"type": 1,
		"favorite": false,
		"name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
		"notes": null,
		"folderId": null,
		"organizationId": "` + orgaID + `",
		"login": {
			"uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
			"username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
			"password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
			"passwordRevisionDate": "2019-09-13T12:26:42+02:00",
			"totp": null
		}
	},
	"collectionIds": ["` + collID + `"]
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers/create", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assertCipherResponse(t, result)
	orgID, ok := result["OrganizationId"]
	assert.True(t, ok)
	assert.Equal(t, orgID, orgaID)
	cipherID = result["Id"].(string)

	body = `
{
	"type": 2,
	"favorite": true,
	"name": "2.G38TIU3t1pGOfkzjCQE7OQ==|Xa1RupttU7zrWdzIT6oK+w==|J3C6qU1xDrfTgyJD+OrDri1GjgGhU2nmRK75FbZHXoI=",
	"folderId": "` + folderID + `",
	"organizationId": "` + orgaID + `",
	"notes": "2.rSw0uVQEFgUCEmOQx0JnDg==|MKqHLD25aqaXYHeYJPH/mor7l3EeSQKsI7A/R+0bFTI=|ODcUScISzKaZWHlUe4MRGuTT2S7jpyDmbOHl7d+6HiM=",
	"secureNote": {
		"type": 0
	}
}`
	req, _ = http.NewRequest("PUT", ts.URL+"/bitwarden/api/ciphers/"+cipherID, bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result2 map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result2)
	assert.NoError(t, err)
	assertUpdatedCipherResponse(t, result2)
	orgID, ok = result2["OrganizationId"]
	assert.True(t, ok)
	assert.Equal(t, orgID, orgaID)
}

func TestSetKeyPair(t *testing.T) {
	body, _ := json.Marshal(map[string]string{
		"encryptedPrivateKey": "2.demXNYbv8o47sG+fhYYvhg==|jXpxet7AApeIzrC3Yr752LwmjBdCZn6HJl6SjEOVP3rrOpGu5qV2rN0dBH5yXXWHusfxM7IvXkdC/fzBUAmFFOU5ubTp9kHFBqIn51tiJG6BRs5aTm7kF6TYSHVDIP5kUdX4O7DcmD23dqtq/8211DSAFR/DK1QDm5Da77Clh7NHxQE9Z9RTW1PBGV56DfzrY3N06H6vI+V6fTZ6HJRD2pdPczR2ZNC0ziQP7qCUYNlSjEv70O4VoYMSUsdb4UUE1YetcSdZ+dIAy+V2KHfoHmTFYI4DtMCW6WpDzp0ufPvszFjt1EwaMr78hujMrQr1gFWxgN8kOLJyYCrd1F5aIxWXHghBH/t+QU31gyQOxCdj18f10ssfuY/y7vocSJQ9pTRRPNh4beGAijV1AETaXWLK1L6oMnkbdhr9ZA2I6cZaHNCaHIynHQH7NUqKKQUJL/FyZ8rBv4YNnxCMRi9p88IoTb0oPsUCoNCaIZ2cvzXz+0VpU6zxj4ke7H6Bu7H46MSB1P+YHzGLtFNzZJVsUBEkz7dotUDeTeqlYKnq7oldWJ4HlqODevzCev+FRnYgrYpoXmYC/dxa1R5IlKCu6rEmP05A7Nw4h9cymnTwRMEoZRSppJ2O5FlSx/Go9Jz12g2Tfiaf+RvO7nkIb2qKiz7Jo2aJgakL5lMOlEdBA2+dsYSyX4Tvu8Ua4p0GcYaGOgSjXH27lQ73ZpHSicf4Q1kAooVl+6zTOPAqgMXOnyyVSRqBPse28HuDwGtmD8BAeVDIfkMW+a+PlWa+yoEWKfDHRduoxNod7Pc9xlNFt6eOeGoBQTEIiF7ccBDtNiSU1yfvqBZEgI8QF0QiGUo9eP7+59so5eu9/DuzjdqFMmGPtG3zHifMxuMzO5+E9UxTyHuCwvxuH93F4vmPC8zzXXn8/ErhEeqmYl1lxZbfJDm1qcjTkJibNKJ9+CXUeP0hq8yi07SEN1xJSZpupf90EUjrdFd3impz3gZKummEjTvzr3J1JX4gC/wD0mGkROHQwb0jCTDJNC18cX4usPYtNr3FxLZmxCGgPmZhkzFjF0qppN1aXTxQskdorEejQUwLL5EnWJySd9/W2P6PmjkJTAwKYUNHsmVUAfbMA7y7QBIjVFFWS4xYy0GJcc8NaLKkFMkGv/oluw552prWAJZ4aM2asoNgyv/JARrAF+JbOPSpax+CtUMO+LCFlBITHopbkHz0TwI1UMj/vIOh9wxDiMqe3YBiviymudX+B7awCaUPTLubWW1jwC4kBnXmRGAKyyIvzgOvwkdcKfQRxoTxq7JFTL/hWk7x4HlQqviSWGY166CLIp6SydCT+cqHMf3MHhe8AQZVC+nIDVNQZWfpFbOFb3nNDwlT+laWrtsiuX7hHiL0VLaCU4xzup5m4zvi59/Qxj0+d8n6M/3GP3/Tvp/bKY9m7CHoeimtGF9Ai2QFJFMOEQw3S1SUBL62ZsezKgBap6y1RqmMzdz/h3f5mhHxRMoQ0kgzZwMNWJvi2acGoIttcmBU7Cn6fqxYNi11dg17M7cFJAQCMicvd4pEwl8IBrm7uFrzbLvuLeolyiDx8GX3jfIo//Ceqa6P/RIqN8jKzH3nTSePuVqkXYiIdxhlAeF//EYW0CwOjd3GEoc=|aUt6NKqrLW4HeprkbwjuBzSQbR84imTujhUPxK17eX4=",
		"publicKey":           "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAmAYrTtY4FBJL/TeTGqr1uHCoMCzUDgwvgq7gBGiNrk24gPbb3xreM+HxubBvkzTlgoS6m1KKKKtD4tWrLU33Xc+PevbKSZDLvBfUe+golGU1XKFxUcIkgINtB0i8LmCVCShiCrlhn2VorcAbekR/1RXtoJqpqq1urhI+RdGVXy8HBBoULA7BoV7wC8dBdkRtnQMNuvGyHclV7yjgealKGqgxz4aNcgsfybquKvYg6PUj8dAxUy7KlmMR7klPyO8nahYqyhpQ/t0xle0WyCkdx5YuYhRSA67Tok+E8fCW5WXOPfIdPZDXS+6/wW1NhcQEa5j6EW11PF/Xq0awBUFwnwIDAQAB",
	})
	buf := bytes.NewBuffer(body)
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/accounts/keys", buf)
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)

	setting, err := settings.Get(inst)
	assert.NoError(t, err)
	orgKey, err := setting.OrganizationKey()
	assert.NoError(t, err)
	assert.NotEmpty(t, orgKey)
}

func TestSettingsDomains(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"equivalentDomains": [][]string{
			{"stackoverflow.com", "serverfault.com", "superuser.com"},
		},
		"globalEquivalentDomains": []int{42, 69},
	})
	buf := bytes.NewBuffer(body)
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/settings/domains", buf)
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assertDomainsReponse(t, res)

	req, _ = http.NewRequest("GET", ts.URL+"/bitwarden/api/settings/domains", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assertDomainsReponse(t, res)
}

func assertDomainsReponse(t *testing.T, res *http.Response) {
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err := json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, result["Object"], "domains")
	equivalent, ok := result["EquivalentDomains"].([]interface{})
	assert.True(t, ok)
	assert.Len(t, equivalent, 1)
	domains, ok := equivalent[0].([]interface{})
	assert.True(t, ok)
	assert.Len(t, domains, 3)
	assert.Equal(t, domains[0], "stackoverflow.com")
	assert.Equal(t, domains[1], "serverfault.com")
	assert.Equal(t, domains[2], "superuser.com")
	global, ok := result["GlobalEquivalentDomains"].([]interface{})
	assert.True(t, ok)
	assert.Len(t, global, len(bitwarden.GlobalDomains))
	for i := range global {
		item := global[i].(map[string]interface{})
		k := int(item["Type"].(float64))
		excluded := (k == 42) || (k == 69)
		assert.Equal(t, item["Excluded"], excluded)
		assert.True(t, len(item["Domains"].([]interface{})) > 0)
	}
}

func TestImportCiphers(t *testing.T) {
	nbCiphers, err := couchdb.CountAllDocs(inst, consts.BitwardenCiphers)
	assert.NoError(t, err)
	nbFolders, err := couchdb.CountAllDocs(inst, consts.BitwardenFolders)
	assert.NoError(t, err)
	body := `
{
  "ciphers": [{
    "type": 2,
    "favorite": true,
    "name": "2.G38TIU3t1pGOfkzjCQE7OQ==|Xa1RupttU7zrWdzIT6oK+w==|J3C6qU1xDrfTgyJD+OrDri1GjgGhU2nmRK75FbZHXoI=",
    "folderId": null,
    "organizationId": null,
    "notes": "2.rSw0uVQEFgUCEmOQx0JnDg==|MKqHLD25aqaXYHeYJPH/mor7l3EeSQKsI7A/R+0bFTI=|ODcUScISzKaZWHlUe4MRGuTT2S7jpyDmbOHl7d+6HiM=",
    "secureNote": {
      "type": 0
    }
  }, {
    "type": 1,
    "favorite": false,
    "name": "2.d7MttWzJTSSKx1qXjHUxlQ==|01Ath5UqFZHk7csk5DVtkQ==|EMLoLREgCUP5Cu4HqIhcLqhiZHn+NsUDp8dAg1Xu0Io=",
    "folderId": null,
    "organizationId": null,
    "notes": null,
    "login": {
      "uri": "2.T57BwAuV8ubIn/sZPbQC+A==|EhUSSpJWSzSYOdJ/AQzfXuUXxwzcs/6C4tOXqhWAqcM=|OWV2VIqLfoWPs9DiouXGUOtTEkVeklbtJQHkQFIXkC8=",
      "username": "2.JbFkAEZPnuMm70cdP44wtA==|fsN6nbT+udGmOWv8K4otgw==|JbtwmNQa7/48KszT2hAdxpmJ6DRPZst0EDEZx5GzesI=",
      "password": "2.e83hIsk6IRevSr/H1lvZhg==|48KNkSCoTacopXRmIZsbWg==|CIcWgNbaIN2ix2Fx1Gar6rWQeVeboehp4bioAwngr0o=",
      "totp": null
    }
  }],
  "folders": [{
    "name": "2.FQAwIBaDbczEGnEJw4g4hw==|7KreXaC0duAj0ulzZJ8ncA==|nu2sEvotjd4zusvGF8YZJPnS9SiJPDqc1VIfCrfve/o="
  }],
  "folderRelationships": [
    {"key": 1, "value": 0}
  ]
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/ciphers/import", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	nb, err := couchdb.CountAllDocs(inst, consts.BitwardenCiphers)
	assert.NoError(t, err)
	assert.Equal(t, nbCiphers+2, nb)
	nb, err = couchdb.CountAllDocs(inst, consts.BitwardenFolders)
	assert.NoError(t, err)
	assert.Equal(t, nbFolders+1, nb)
}

func TestCreateOrganization(t *testing.T) {
	body := `
{
	"name": "Family Organization",
	"key": "bmFjbF53D9mrdGbVqQzMB54uIg678EIpU/uHFYjynSPSA6vIv5/6nUy4Uk22SjIuDB3pZ679wLE3o7R/Imzn47OjfT6IrJ8HaysEhsZA25Dn8zwEtTMtgNepUtH084wAMgNeIcElW24U/MfRscjAk8cDUIm5xnzyi2vtJfe9PcHTmzRXyng=",
	"collectionName": "2.rrpSDDODsWZqL7EhLVsu/Q==|OSuh+MmmR89ppdb/A7KxBg==|kofpAocL2G4a3P1C2R1U+i9hWbhfKfsPKM6kfoyCg/M="
}`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/organizations", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "Family Organization", result["Name"])
	assert.Equal(t, "profileOrganization", result["Object"])
	assert.Equal(t, true, result["Enabled"])
	assert.EqualValues(t, 2, result["Status"])
	assert.EqualValues(t, 0, result["Type"])
	orgaID, _ = result["Id"].(string)
	assert.NotEmpty(t, orgaID)
	assert.NotEmpty(t, result["Key"])
}

func TestGetOrganization(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/organizations/"+orgaID, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "Family Organization", result["Name"])
	assert.Equal(t, "profileOrganization", result["Object"])
	assert.Equal(t, true, result["Enabled"])
	assert.EqualValues(t, 2, result["Status"])
	assert.EqualValues(t, 0, result["Type"])
	assert.Equal(t, orgaID, result["Id"])
	assert.NotEmpty(t, result["Key"])
}

func TestListCollections(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/organizations/"+orgaID+"/collections", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "list", result["Object"])
	data := result["Data"].([]interface{})
	assert.Len(t, data, 1)
	coll := data[0].(map[string]interface{})
	assert.NotEmpty(t, coll["Id"])
	assert.Equal(t, "2.rrpSDDODsWZqL7EhLVsu/Q==|OSuh+MmmR89ppdb/A7KxBg==|kofpAocL2G4a3P1C2R1U+i9hWbhfKfsPKM6kfoyCg/M=", coll["Name"])
	assert.Equal(t, "collection", coll["Object"])
	assert.Equal(t, orgaID, coll["OrganizationId"])
	assert.Equal(t, false, coll["ReadOnly"])
}

func TestSyncOrganizationAndCollection(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/sync", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
	var result map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&result)
	assert.NoError(t, err)
	assert.Equal(t, "sync", result["Object"])

	profile := result["Profile"].(map[string]interface{})
	orgs := profile["Organizations"].([]interface{})
	for i := range orgs {
		org := orgs[i].(map[string]interface{})
		if org["Id"] == orgaID {
			assert.Equal(t, "Family Organization", org["Name"])
		} else {
			assert.Equal(t, "Cozy", org["Name"])
		}
		assert.NotEmpty(t, org["Key"])
		assert.Equal(t, "profileOrganization", org["Object"])
	}
	assert.Len(t, orgs, 2)

	colls := result["Collections"].([]interface{})
	for i := range colls {
		coll := colls[i].(map[string]interface{})
		if coll["Id"] != collID {
			assert.Equal(t, coll["OrganizationId"], orgaID)
			assert.Equal(t, coll["Name"], "2.rrpSDDODsWZqL7EhLVsu/Q==|OSuh+MmmR89ppdb/A7KxBg==|kofpAocL2G4a3P1C2R1U+i9hWbhfKfsPKM6kfoyCg/M=")
		}
		assert.Equal(t, "collection", coll["Object"])
	}
	assert.Len(t, colls, 2)
}

func TestDeleteOrganization(t *testing.T) {
	email := inst.PassphraseSalt()
	iter := crypto.DefaultPBKDF2Iterations
	pass, _ := crypto.HashPassWithPBKDF2([]byte("cozy"), email, iter)
	body := fmt.Sprintf(`{"masterPasswordHash": "%s"}`, pass)
	req, _ := http.NewRequest("DELETE", ts.URL+"/bitwarden/api/organizations/"+orgaID, bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
}

func TestChangeSecurityStamp(t *testing.T) {
	email := inst.PassphraseSalt()
	iter := crypto.DefaultPBKDF2Iterations
	pass, _ := crypto.HashPassWithPBKDF2([]byte("cozy"), email, iter)
	body, _ := json.Marshal(map[string]string{
		"masterPasswordHash": string(pass),
	})
	buf := bytes.NewBuffer(body)
	res, err := http.Post(ts.URL+"/bitwarden/api/accounts/security-stamp", "application/json", buf)
	assert.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode)

	// Check that token is no longer valid
	req, _ := http.NewRequest("GET", ts.URL+"/bitwarden/api/folders", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 401, res.StatusCode)
}

func TestSendHint(t *testing.T) {
	body := `{ "email": "me@bitwarden.example.net" }`
	req, _ := http.NewRequest("POST", ts.URL+"/bitwarden/api/accounts/password-hint", bytes.NewBufferString(body))
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode)
}

func TestMain(m *testing.M) {
	config.UseTestFile()
	testutils.NeedCouchdb()
	setup := testutils.NewSetup(m, "bitwarden_test")
	inst = setup.GetTestInstance(&lifecycle.Options{
		Domain:     "bitwarden.example.net",
		Passphrase: "cozy",
		PublicName: "Pierre",
		Email:      "pierre@cozy.localhost",
	})

	ts = setup.GetTestServer("/bitwarden", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	os.Exit(setup.Run())
}
