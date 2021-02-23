/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package operation

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hyperledger/aries-framework-go/pkg/doc/util"
	"github.com/hyperledger/aries-framework-go/pkg/doc/verifiable"
	"github.com/trustbloc/edge-core/pkg/log"
	"github.com/trustbloc/edge-core/pkg/storage"
	"github.com/trustbloc/edge-core/pkg/zcapld"
	vaultclient "github.com/trustbloc/edge-service/pkg/client/vault"
	edgesvcops "github.com/trustbloc/edge-service/pkg/restapi/issuer/operation"
	"github.com/trustbloc/edge-service/pkg/restapi/vault"
	"github.com/trustbloc/edv/pkg/edvutils"

	"github.com/trustbloc/sandbox/pkg/internal/common/support"
)

const (
	// api paths
	register            = "/register"
	login               = "/login"
	logout              = "/logout"
	connect             = "/connect"
	disconnect          = "/disconnect"
	link                = "/link"
	accountLinkCallback = "/callback"
	consent             = "/consent"
	client              = "/client"
	getClient           = client + "/{id}"
	profile             = "/profile"
	getProfile          = profile + "/{id}"

	// store
	txnStoreName = "issuer_txn"

	// form param
	username   = "username"
	password   = "password"
	nationalID = "nationalID"

	// cookies
	actionCookie     = "action"
	idCookie         = "id"
	linkAction       = "link"
	sessionidCookie  = "sessionid"
	cookieExpiryTime = 5
	authExpiryTime   = 5

	vcsIssuerRequestTokenName = "vcs_issuer"

	// external paths
	issueCredentialURLFormat = "%s" + "/credentials/issueCredential"
	accountLinkURLFormat     = "%s/link?client_id=%s&callback=%s/callback&state=%s"

	// json-ld
	credentialContext = "https://www.w3.org/2018/credentials/v1"

	// TODO - remove this hard-coded val
	// clientID
	clientID = "uscis-profile"
)

var logger = log.New("acrp-restapi")

// nolint: gochecknoglobals
var cookieExpTime = time.Now().Add(cookieExpiryTime * time.Minute)

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type vaultClient interface {
	CreateVault() (*vault.CreatedVault, error)
	SaveDoc(vaultID, id string, content interface{}) (*vault.DocumentMetadata, error)
	CreateAuthorization(vaultID, requestingParty string,
		scope *vault.AuthorizationsScope) (*vault.CreatedAuthorization, error)
}

// Handler http handler for each controller API endpoint.
type Handler interface {
	Path() string
	Method() string
	Handle() http.HandlerFunc
}

// Operation defines handlers.
type Operation struct {
	store                storage.Store
	handlers             []Handler
	homePageHTML         string
	dashboardHTML        string
	consentHTML          string
	accountLinkedHTML    string
	accountNotLinkedHTML string
	httpClient           httpClient
	vcIssuerURL          string
	requestTokens        map[string]string
	accountLinkURL       string
	hostExternalURL      string
	vClient              vaultClient
}

// Config config.
type Config struct {
	StoreProvider        storage.Provider
	HomePageHTML         string
	DashboardHTML        string
	ConsentHTML          string
	AccountLinkedHTML    string
	AccountNotLinkedHTML string
	TLSConfig            *tls.Config
	VaultServerURL       string
	VCIssuerURL          string
	AccountLinkURL       string
	HostExternalURL      string
	RequestTokens        map[string]string
}

// New returns acrp operation instance.
func New(config *Config) (*Operation, error) {
	store, err := getTxnStore(config.StoreProvider)
	if err != nil {
		return nil, fmt.Errorf("acrp store provider : %w", err)
	}

	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: config.TLSConfig}}

	op := &Operation{
		store:                store,
		homePageHTML:         config.HomePageHTML,
		dashboardHTML:        config.DashboardHTML,
		consentHTML:          config.ConsentHTML,
		httpClient:           httpClient,
		accountLinkedHTML:    config.AccountLinkedHTML,
		accountNotLinkedHTML: config.AccountNotLinkedHTML,
		vcIssuerURL:          config.VCIssuerURL,
		accountLinkURL:       config.AccountLinkURL,
		hostExternalURL:      config.HostExternalURL,
		requestTokens:        config.RequestTokens,
		vClient:              vaultclient.New(config.VaultServerURL, vaultclient.WithHTTPClient(httpClient)),
	}

	op.registerHandler()

	return op, nil
}

// registerHandler register handlers to be exposed from this service as REST API endpoints
func (o *Operation) registerHandler() {
	o.handlers = []Handler{
		support.NewHTTPHandler(register, http.MethodPost, o.register),
		support.NewHTTPHandler(login, http.MethodPost, o.login),
		support.NewHTTPHandler(logout, http.MethodGet, o.logout),
		support.NewHTTPHandler(connect, http.MethodGet, o.connect),
		support.NewHTTPHandler(disconnect, http.MethodGet, o.disconnect),
		support.NewHTTPHandler(link, http.MethodGet, o.link),
		support.NewHTTPHandler(accountLinkCallback, http.MethodGet, o.accountLinkCallback),
		support.NewHTTPHandler(consent, http.MethodGet, o.consent),
		support.NewHTTPHandler(client, http.MethodPost, o.createClient),
		support.NewHTTPHandler(getClient, http.MethodGet, o.getClient),
		support.NewHTTPHandler(profile, http.MethodPost, o.createProfile),
		support.NewHTTPHandler(getProfile, http.MethodGet, o.getProfile),
		support.NewHTTPHandler(getProfile, http.MethodDelete, o.deleteProfile),
	}
}

// GetRESTHandlers get all controller API handler available for this service
func (o *Operation) GetRESTHandlers() []Handler {
	return o.handlers
}

func (o *Operation) register(w http.ResponseWriter, r *http.Request) { // nolint: funlen
	err := r.ParseForm()
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("unable to parse form data: %s", err.Error()))

		return
	}

	pwd, err := o.store.Get(r.FormValue(username))
	if err != nil && !errors.Is(err, storage.ErrValueNotFound) {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("unable to get user data: %s", err.Error()))
		o.showDashboard(w, r.FormValue(username), "Oops User Data Not Found", "", false)

		return
	}

	if pwd != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, "username already exists")
		o.showDashboard(w, r.FormValue(username), "Username already exists", "", false)

		return
	}

	// create vault for the user
	vaultData, err := o.vClient.CreateVault()
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to create vault - err:%s", err.Error()))

		return
	}

	vaultID := vaultData.ID

	// wrap nationalID in a vc
	vcResp, err := o.createNationalIDCred(vaultID, r.FormValue(nationalID))
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to create vc: %s", err.Error()))
	}

	// save nationalID vc
	docID, err := o.saveNationalIDDoc(vaultID, vcResp)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to save doc - err:%s", err.Error()))

		return
	}

	uData := userData{
		Password:        r.FormValue(password),
		VaultID:         vaultID,
		NationalIDDocID: docID,
	}

	uDataBytes, err := json.Marshal(uData)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to unmarshal user data - err:%s", err.Error()))

		return
	}

	err = o.store.Put(r.FormValue(username), uDataBytes)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("unable to save user data: %s", err.Error()))

		return
	}

	o.showDashboard(w, r.FormValue(username), "", vaultID, false)
}

func (o *Operation) login(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("unable to parse form data: %s", err.Error()))

		return
	}

	uData, err := o.getUserData(r.FormValue(username))
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("unable to get user data: %s", err.Error()))
		o.showDashboard(w, r.FormValue(username), "Oops User Data Not Found", "", false)

		return
	}

	if r.FormValue(password) != uData.Password {
		o.writeErrorResponse(w, http.StatusBadRequest, "invalid password")
		o.showDashboard(w, r.FormValue(username), "Invalid Password", "", false)

		return
	}

	actionCookie, err := r.Cookie(actionCookie)
	if err != nil && !errors.Is(err, http.ErrNoCookie) {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get action cookie: %s", err.Error()))

		return
	}

	sessionID := uuid.New().String()

	err = o.store.Put(sessionID, []byte(r.FormValue(username)))
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to save session : %s", err.Error()))

		return
	}

	cookie := http.Cookie{Name: sessionidCookie, Value: sessionID, Expires: cookieExpTime}
	http.SetCookie(w, &cookie)

	if actionCookie != nil && actionCookie.Value == linkAction {
		o.loadHTML(w, o.consentHTML, map[string]interface{}{})

		return
	}

	o.showDashboard(w, r.FormValue(username), "", uData.VaultID, true)
}

func (o *Operation) logout(w http.ResponseWriter, r *http.Request) {
	clearCookies(w)

	o.loadHTML(w, o.homePageHTML, map[string]interface{}{})
}

func (o *Operation) connect(w http.ResponseWriter, r *http.Request) {
	userName := r.URL.Query()["userName"]
	if len(userName) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing username")

		return
	}

	state := uuid.New().String()

	// TODO store state data

	endpoint := fmt.Sprintf(accountLinkURLFormat, o.accountLinkURL, clientID, o.hostExternalURL, state)

	http.Redirect(w, r, endpoint, http.StatusFound)
}

func (o *Operation) disconnect(w http.ResponseWriter, r *http.Request) {
	userName := r.URL.Query()["userName"]
	if len(userName) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing username")

		return
	}

	// TODO disconnect with other service / integrate trustbloc features

	o.showDashboard(w, userName[0], "", "", false)
}

func (o *Operation) accountLinkCallback(w http.ResponseWriter, r *http.Request) {
	auth := r.URL.Query()["auth"]
	if len(auth) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing authorization")
		o.loadHTML(w, o.accountNotLinkedHTML, nil)

		return
	}

	// TODO call vault-server /vaults/{vaultID}/authorizations  api

	// TODO call comparator-service /compare  api

	o.loadHTML(w, o.accountLinkedHTML, nil)
}

func (o *Operation) link(w http.ResponseWriter, r *http.Request) { // nolint: funlen
	// TODO use OIDC to link accounts
	clientID := r.URL.Query()["client_id"]
	if len(clientID) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing client_id")

		return
	}

	callback := r.URL.Query()["callback"]
	if len(callback) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing callback url")

		return
	}

	state := r.URL.Query()["state"]
	if len(state) == 0 {
		o.writeErrorResponse(w, http.StatusBadRequest, "missing state")

		return
	}

	logger.Infof("link : clientID=%s callback url= %s state=%s", clientID, callback, state)

	cDataBytes, err := o.store.Get(clientID[0])
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest,
			fmt.Sprintf("failed to client data: %s", err.Error()))

		return
	}

	var cData *clientData

	err = json.Unmarshal(cDataBytes, &cData)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to unmarshal client data: %s", err.Error()))

		return
	}

	data := sessionData{
		State:       state[0],
		CallbackURL: callback[0],
		DID:         cData.DID,
	}

	dataBytes, err := json.Marshal(data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to marshal session data: %s", err.Error()))

		return
	}

	sessionid := uuid.New().String()

	err = o.store.Put(sessionid, dataBytes)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to save session data: %s", err.Error()))

		return
	}

	// set cookies
	cookie := http.Cookie{Name: actionCookie, Value: linkAction, Expires: cookieExpTime}
	http.SetCookie(w, &cookie)

	cookie = http.Cookie{Name: idCookie, Value: sessionid, Expires: cookieExpTime}
	http.SetCookie(w, &cookie)

	http.Redirect(w, r, "/showlogin", http.StatusFound)
}

// nolint: funlen
func (o *Operation) consent(w http.ResponseWriter, r *http.Request) {
	// get the session id
	sessionidCookieData, err := r.Cookie(sessionidCookie)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get session cookie: %s", err.Error()))

		return
	}

	// get the session id
	idCookieData, err := r.Cookie(idCookie)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get id cookie: %s", err.Error()))

		return
	}

	username, err := o.store.Get(sessionidCookieData.Value)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get session data: %s", err.Error()))

		return
	}

	userData, err := o.getUserData(string(username))
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get user data: %s", err.Error()))

		return
	}

	// get the session data from db
	dataBytes, err := o.store.Get(idCookieData.Value)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get state data: %s", err.Error()))

		return
	}

	var data *sessionData

	err = json.Unmarshal(dataBytes, &data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to unmarshal state data: %s", err.Error()))

		return
	}

	logger.Infof("consent - createAuthorization : vaultID=%s rpDID=%s docID=%s",
		userData.VaultID, data.DID, userData.NationalIDDocID)

	// TODO https://github.com/trustbloc/sandbox/issues/799 update rp did ID, now using vaultID
	docAuth, err := o.vClient.CreateAuthorization(userData.VaultID, userData.VaultID, &vault.AuthorizationsScope{
		Target:  userData.NationalIDDocID,
		Actions: []string{"read"},
		Caveats: []vault.Caveat{{Type: zcapld.CaveatTypeExpiry, Duration: uint64(authExpiryTime * time.Second)}},
	})
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to create vault authorization: %s", err.Error()))

		return
	}

	if docAuth == nil || docAuth.Tokens == nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, "missing auth token from vault-server")

		return
	}

	logger.Infof("docAuthToken : edv=%s kms=%s", docAuth.Tokens.EDV, docAuth.Tokens.KMS)

	// TODO call comparator-service /authorization  api

	// TODO pass the zccap to the caller.
	auth := uuid.New().String()

	// invalid the cookies
	clearCookies(w)

	logger.Infof("consent : callback url= %s state=%s", data.CallbackURL, data.State)

	http.Redirect(w, r, fmt.Sprintf("%s?state=%s&auth=%s", data.CallbackURL, data.State, auth), http.StatusFound)
}

func (o *Operation) createClient(w http.ResponseWriter, r *http.Request) {
	req := &clientReq{}

	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest,
			fmt.Sprintf("failed to decode request: %s", err.Error()))

		return
	}

	// TODO integrate with OIDC provider

	data := clientData{
		ClientID: uuid.New().String(),
		DID:      req.DID,
		Callback: req.Callback,
	}

	dataBytes, err := json.Marshal(data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to marshal client data: %s", err.Error()))

		return
	}

	err = o.store.Put(data.ClientID, dataBytes)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to save client data: %s", err.Error()))

		return
	}

	o.writeResponse(w, http.StatusCreated, clientResp{
		ClientID:     uuid.New().String(),
		ClientSecret: uuid.New().String(),
		DID:          req.DID,
		Callback:     req.Callback,
	})
}

func (o *Operation) getClient(w http.ResponseWriter, r *http.Request) {
	var data *clientData

	o.getData(w, strings.Split(r.URL.Path, "/")[2], data)
}

func (o *Operation) createProfile(w http.ResponseWriter, r *http.Request) {
	data := &profileData{}

	err := json.NewDecoder(r.Body).Decode(data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusBadRequest,
			fmt.Sprintf("failed to decode request: %s", err.Error()))

		return
	}

	dataBytes, err := json.Marshal(data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to marshal client data: %s", err.Error()))

		return
	}

	err = o.store.Put(data.ID, dataBytes)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("failed to save client data: %s", err.Error()))

		return
	}

	o.writeResponse(w, http.StatusCreated, data)
}

func (o *Operation) getProfile(w http.ResponseWriter, r *http.Request) {
	var data *profileData

	o.getData(w, strings.Split(r.URL.Path, "/")[2], data)
}

func (o *Operation) getData(w http.ResponseWriter, id string, data interface{}) {
	dataBytes, err := o.store.Get(id)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to get data : id=%s - %s", id, err.Error()))

		return
	}

	err = json.Unmarshal(dataBytes, &data)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to unmarshal data : id=%s - %s", id, err.Error()))

		return
	}

	o.writeResponse(w, http.StatusOK, data)
}

func (o *Operation) deleteProfile(w http.ResponseWriter, r *http.Request) {
	profileID := strings.Split(r.URL.Path, "/")[2]

	err := o.store.Delete(profileID)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to delete data : id=%s - %s", profileID, err.Error()))

		return
	}

	o.writeResponse(w, http.StatusOK, nil)
}

func (o *Operation) showDashboard(w http.ResponseWriter, userName, msg string, vaultID string, serviceLinked bool) {
	endpoint := fmt.Sprintf("/connect?userName=%s", userName)
	if serviceLinked {
		endpoint = fmt.Sprintf("/disconnect?userName=%s", userName)
	}

	if msg == "" {
		o.loadHTML(w, o.dashboardHTML, map[string]interface{}{
			"UserName":      userName,
			"ServiceLinked": serviceLinked,
			"URL":           endpoint,
			"VaultID":       vaultID,
			"ErrMsg":        "",
		})
	} else {
		o.loadHTML(w, o.homePageHTML, map[string]interface{}{
			"UserName":      userName,
			"ServiceLinked": serviceLinked,
			"URL":           endpoint,
			"ErrMsg":        msg,
		})
	}

}

func (o *Operation) loadHTML(w http.ResponseWriter, htmlFileName string, data map[string]interface{}) {
	t, err := template.ParseFiles(htmlFileName)
	if err != nil {
		o.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("unable to load html: %s", err.Error()))

		return
	}

	if err := t.Execute(w, data); err != nil {
		logger.Errorf("failed execute %s html template: %s", htmlFileName, err.Error())
	}
}

func (o *Operation) writeErrorResponse(rw http.ResponseWriter, status int, msg string) {
	logger.Errorf(msg)

	rw.WriteHeader(status)

	write := rw.Write
	if _, err := write([]byte(msg)); err != nil {
		logger.Errorf("Unable to send error message, %s", err)
	}
}

func (o *Operation) writeResponse(rw http.ResponseWriter, status int, v interface{}) {
	rw.WriteHeader(status)

	err := json.NewEncoder(rw).Encode(v)
	if err != nil {
		logger.Errorf("Unable to send response, %s", err)
	}
}

func getTxnStore(prov storage.Provider) (storage.Store, error) {
	err := prov.CreateStore(txnStoreName)
	if err != nil && !errors.Is(err, storage.ErrDuplicateStore) {
		return nil, err
	}

	txnStore, err := prov.OpenStore(txnStoreName)
	if err != nil {
		return nil, err
	}

	return txnStore, nil
}

func (o *Operation) getUserData(username string) (*userData, error) {
	uDataBytes, err := o.store.Get(username)
	if err != nil {
		return nil, fmt.Errorf("get user data: %w", err)
	}

	var uData *userData

	err = json.Unmarshal(uDataBytes, &uData)
	if err != nil {
		return nil, fmt.Errorf("unmarshal user data: %w", err)
	}

	return uData, nil
}

func (o *Operation) createNationalIDCred(sub, nationalID string) ([]byte, error) {
	if nationalID == "" {
		return nil, errors.New("nationalID is mandatory")
	}

	cred := verifiable.Credential{}
	cred.ID = uuid.New().URN()
	cred.Context = []string{credentialContext}
	cred.Types = []string{"VerifiableCredential"}
	// issuerID will be overwritten in the issuer
	cred.Issuer = verifiable.Issuer{ID: uuid.New().URN()}
	cred.Issued = util.NewTime(time.Now().UTC())

	credentialSubject := make(map[string]interface{})
	credentialSubject["id"] = sub
	credentialSubject[nationalID] = nationalID

	cred.Subject = credentialSubject

	credBytes, err := cred.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal credential: %w", err)
	}

	vcReq, err := json.Marshal(edgesvcops.IssueCredentialRequest{
		Credential: credBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal vc request credential: %w", err)
	}

	endpoint := fmt.Sprintf(issueCredentialURLFormat, o.vcIssuerURL)

	vcResp, err := o.sendHTTPRequest(http.MethodPost, endpoint, vcReq, http.StatusCreated,
		o.requestTokens[vcsIssuerRequestTokenName])
	if err != nil {
		return nil, fmt.Errorf("failed to create vc - url:%s err: %w", endpoint, err)
	}

	return vcResp, nil
}

func (o *Operation) saveNationalIDDoc(vaultID string, vcResp []byte) (string, error) {
	docID, err := edvutils.GenerateEDVCompatibleID()
	if err != nil {
		return "", fmt.Errorf("create edv doc id : %w", err)
	}

	_, err = o.vClient.SaveDoc(vaultID, docID, vcResp)
	if err != nil {
		return "", fmt.Errorf("failed to save doc : %w", err)
	}

	return docID, nil
}

func (o *Operation) sendHTTPRequest(method, endpoint string, reqBody []byte, status int,
	httpToken string) ([]byte, error) {
	logger.Errorf("sendHTTPRequest: method=%s url=%s reqBody=%s", method, endpoint, string(reqBody))

	req, err := http.NewRequest(method, endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	if httpToken != "" {
		req.Header.Add("Authorization", "Bearer "+httpToken)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		err = resp.Body.Close()
		if err != nil {
			logger.Warnf("failed to close response body")
		}
	}()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Warnf("failed to read response body for status: %d", resp.StatusCode)
	}

	logger.Errorf("httpResponse: status=%d respBody=%s", resp.StatusCode, string(respBody))

	if resp.StatusCode != status {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(respBody))
	}

	return respBody, nil
}

func clearCookies(w http.ResponseWriter) {
	cookie := http.Cookie{Name: actionCookie, Value: "", MaxAge: -1}
	http.SetCookie(w, &cookie)

	cookie = http.Cookie{Name: sessionidCookie, Value: "", MaxAge: -1}
	http.SetCookie(w, &cookie)

	cookie = http.Cookie{Name: idCookie, Value: "", MaxAge: -1}
	http.SetCookie(w, &cookie)
}
