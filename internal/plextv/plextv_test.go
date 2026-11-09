package plextv

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/remote"
)

const testMachineID = "0123456789abcdef0123456789abcdef01234567"

func testConfig() config.Config {
	cfg := config.Defaults(os.TempDir())
	cfg.Server.MachineIdentifier = testMachineID
	cfg.Server.FriendlyName = "Living Room"
	cfg.Plex.ClientIdentifier = "app-client-id"
	return cfg
}

func newServerClient(t *testing.T, f *fakePlexTV) *Client {
	c, err := New(Options{BaseURL: f.srv.URL, Identity: ServerIdentity(testConfig(), ""), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func owner() Account {
	return Account{ID: 42, UUID: "uuid-42", Username: "owner", Title: "The Owner", Email: "o@example.com",
		Subscription: Subscription{Active: true, Status: "Active", Plan: "lifetime"}}
}

func TestIdentities(t *testing.T) {
	cfg := testConfig()
	s := ServerIdentity(cfg, "")
	if s.Product != "Plex Media Server" || s.ClientIdentifier != testMachineID || s.Provides != "server" ||
		s.Version != DefaultServerVersion || s.DeviceName != "Living Room" {
		t.Fatalf("%+v", s)
	}
	a := AppIdentity(cfg, "")
	if a.Product != "Ether" || a.ClientIdentifier != "app-client-id" || a.Provides != "" {
		t.Fatalf("%+v", a)
	}
}

func TestPINFlow(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	c := newServerClient(t, f).WithIdentity(AppIdentity(testConfig(), "1.0"))
	ctx := context.Background()
	pin, err := c.CreatePIN(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pin.ID == 0 || pin.Code == "" || pin.ExpiresAt.Before(time.Now()) || pin.ClientIdentifier != "app-client-id" {
		t.Fatalf("%+v", pin)
	}
	h := f.header("/api/v2/pins")
	if h.Get("X-Plex-Product") != "Ether" || h.Get("X-Plex-Client-Identifier") != "app-client-id" || h.Get("Accept") != "application/json" {
		t.Fatalf("headers %v", h)
	}

	authURL := c.AuthURL(pin, "http://localhost:32400/setup")
	frag := authURL[strings.Index(authURL, "#?")+2:]
	q, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authURL, DefaultAuthAppURL+"#?") || q.Get("clientID") != "app-client-id" || q.Get("code") != pin.Code ||
		q.Get("context[device][product]") != "Ether" || q.Get("forwardUrl") != "http://localhost:32400/setup" {
		t.Fatalf("auth url %s", authURL)
	}

	p, err := c.CheckPIN(ctx, pin.ID, pin.Code)
	if err != nil || p.AuthToken != "" {
		t.Fatal(p, err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.approvePIN(pin.ID, "user-token")
	}()
	tok, err := c.WaitPIN(ctx, pin, 10*time.Millisecond)
	if err != nil || tok != "user-token" {
		t.Fatal(tok, err)
	}
	acct, err := c.User(ctx, tok)
	if err != nil || acct.ID != 42 || acct.DisplayName() != "The Owner" || !acct.Subscription.Active {
		t.Fatal(acct, err)
	}
}

func TestPINExpiredAndCancel(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	c := newServerClient(t, f).WithIdentity(AppIdentity(testConfig(), ""))
	_, err := c.WaitPIN(context.Background(), &PIN{ID: 1, Code: "gone"}, time.Millisecond)
	if !errors.Is(err, ErrPINExpired) {
		t.Fatalf("got %v", err)
	}
	pin, err := c.CreatePIN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.WaitPIN(ctx, pin, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestUserUnauthorized(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	c := newServerClient(t, f)
	_, err := c.User(context.Background(), "bogus")
	if !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 1001 || ae.Message != "User could not be authenticated" {
		t.Fatalf("%+v", ae)
	}
	if _, err := c.User(context.Background(), ""); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestLiveXMLAccountAndSubscriptionShapes(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.userXML = `<user id="42" uuid="owner-uuid" username="owner" title="The Owner" email="owner@example.invalid" friendlyName="Owner" home="1" homeAdmin="1"><subscription active="1" status="Active" plan="lifetime"><features><feature id="remoteWatchPass"/></features></subscription></user>`
	f.usersXML = `<users><user id="42" uuid="owner-uuid" username="owner" title="The Owner" email="owner@example.invalid" friendlyName="Owner"><providers/></user></users>`
	c := newServerClient(t, f)
	a, err := c.User(context.Background(), "server-token-42")
	if err != nil || a.ID != 42 || !a.Subscription.Active || a.Subscription.Plan != "lifetime" || !a.HomeAdmin {
		t.Fatalf("account=%+v err=%v", a, err)
	}
	users, err := c.Users(context.Background(), "server-token-42")
	if err != nil || len(users) != 1 || users[0].PlexID != 42 || users[0].Username != "owner" {
		t.Fatalf("users=%+v err=%v", users, err)
	}
	subs, err := c.UserSubscriptions(context.Background(), "server-token-42")
	if err != nil || len(subs) != 1 || subs[0].PlexPassKnown {
		t.Fatalf("subscriptions=%+v err=%v", subs, err)
	}
	services, err := c.UserServices(context.Background(), "server-token-42")
	if err != nil || len(services) != 1 || services[0].Identifier != "example" {
		t.Fatalf("services=%+v err=%v", services, err)
	}
}

func TestAccessTokensObservedDeviceTokenShape(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = `<tokens><token type="device" token="device-token" tokenDisabled="0" owned="1" createdAt="2026-01-02 03:04:05 UTC"/></tokens>`
	c := newServerClient(t, f)
	tokens, err := c.AccessTokens(context.Background(), "server-token-42")
	if err != nil || len(tokens) != 1 || tokens[0].Token != "device-token" || !tokens[0].Owned || tokens[0].PlexID != 0 {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
}

func TestClaimExchange(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	c := newServerClient(t, f)
	res, err := c.Claim(context.Background(), "user-token")
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerToken != "server-token-42" || res.Owner.ID != 42 {
		t.Fatalf("%+v", res)
	}
	h := f.header("/api/claim/exchange")
	if h.Get("X-Plex-Product") != "Plex Media Server" || h.Get("X-Plex-Provides") != "server" || h.Get("X-Plex-Client-Identifier") != testMachineID {
		t.Fatalf("exchange headers %v", h)
	}
	// The exchange needs the server identity.
	if _, err := c.WithIdentity(AppIdentity(testConfig(), "")).ExchangeClaim(context.Background(), "claim-x"); err == nil {
		t.Fatal("exchange with app identity")
	}
}

func TestClaimExchangeFailureStoresNoServerToken(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	f.exchangeErr = 422
	c := newServerClient(t, f)
	if res, err := c.Claim(context.Background(), "user-token"); !errors.Is(err, ErrClaimExchangeFailed) {
		t.Fatalf("res=%+v err=%v, want ErrClaimExchangeFailed", res, err)
	} else {
		var ce *ClaimExchangeError
		if !errors.As(err, &ce) {
			t.Fatalf("err=%T, want *ClaimExchangeError", err)
		}
	}
	store := &memConfig{cfg: testConfig()}
	users := &fakeUsers{}
	r, err := NewRegistrar(RegistrarOptions{
		Config: store, Client: c, Users: users,
		WAN: &fakeWAN{ip: "203.0.113.7"}, Mapper: &fakeMapper{},
		LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(context.Background(), "user-token"); !errors.Is(err, ErrClaimExchangeFailed) {
		t.Fatalf("registrar Claim err=%v, want ErrClaimExchangeFailed", err)
	}
	if got := store.Get().Plex; got.Claimed || got.Token != "" {
		t.Fatalf("failed claim stored server token: %+v", got)
	}
	if users.token != "" || users.owner.ID != 0 {
		t.Fatalf("failed claim updated UserSync: %+v", users)
	}
	if _, err := c.Claim(context.Background(), "bad"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestExtractAuthToken(t *testing.T) {
	cases := map[string]string{
		`<user><authentication-token> abc </authentication-token></user>`: "abc",
		`<user authenticationToken="def"/>`:                               "def",
		`{"authToken":"ghi"}`:                                             "ghi",
		`{"user":{"authToken":"jkl"}}`:                                    "jkl",
		`<user/>`:                                                         "",
	}
	for in, want := range cases {
		if got := extractAuthToken([]byte(in)); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestPublishAndResources(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	c := newServerClient(t, f)
	ctx := context.Background()
	uris := []string{"http://192.168.1.5:32400", "http://203.0.113.7:32400"}
	if err := c.PublishConnections(ctx, "server-token-42", uris); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.published, ",") != strings.Join(uris, ",") {
		t.Fatal(f.published)
	}
	h := f.header("/devices/" + testMachineID)
	if h.Get("X-Plex-Token") != "server-token-42" || h.Get("X-Plex-Provides") != "server" {
		t.Fatal(h)
	}
	if err := c.PublishConnections(ctx, "server-token-42", nil); err != nil || len(f.published) != 1 || f.published[0] != "" {
		t.Fatalf("empty publish: %v %v", f.published, err)
	}
	if err := c.PublishConnections(ctx, "server-token-42", uris); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterServer(ctx, "server-token-42", ServerRegistration{Name: "Ether", Address: "203.0.113.7", Port: 32401,
		PrivateAddress: "192.168.1.5", PrivatePort: 32400, Version: "1.42", LocalAddresses: []string{"192.168.1.5", "fd00::5"}, MappingState: "mapped"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.registered[0], `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("registration body lacks the XML declaration: %s", f.registered[0])
	}
	var reg struct {
		XMLName xml.Name `xml:"MediaContainer"`
		Size    int      `xml:"size,attr"`
		Server  struct {
			Name              string     `xml:"name,attr"`
			Address           string     `xml:"address,attr"`
			Port              int        `xml:"port,attr"`
			Version           string     `xml:"version,attr"`
			MachineIdentifier string     `xml:"machineIdentifier,attr"`
			LocalAddresses    string     `xml:"localAddresses,attr"`
			Date              int64      `xml:"date,attr"`
			Relay             string     `xml:"relay,attr"`
			Attrs             []xml.Attr `xml:",any,attr"`
		}
	}
	if err := xml.Unmarshal([]byte(f.registered[0]), &reg); err != nil || reg.Size != 1 || reg.Server.Name != "Ether" || reg.Server.Address != "203.0.113.7" ||
		reg.Server.Port != 32401 || reg.Server.Version != "1.42" || reg.Server.MachineIdentifier != testMachineID ||
		reg.Server.LocalAddresses != "192.168.1.5,[fd00::5]" || reg.Server.Relay != "0" || reg.Server.Date <= 0 || len(reg.Server.Attrs) != 0 {
		t.Fatalf("registration=%+v err=%v", reg, err)
	}
	if h := f.header("/servers.xml"); h.Get("X-Plex-Token") != "" || h.Get("X-Plex-Client-Identifier") != testMachineID {
		t.Fatalf("servers.xml headers: %v", h)
	}
	// Without a public port the listener port is registered: plex.tv rejects
	// a body with no port at all (422).
	if err := c.RegisterServer(ctx, "server-token-42", ServerRegistration{Name: "Ether", PrivatePort: 32400, Version: "1.42"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.registered[1], `port="32400"`) || strings.Contains(f.registered[1], "address=") {
		t.Fatalf("registration without remote access: %s", f.registered[1])
	}
	asyncID, err := c.ReportConnectivity(ctx, "server-token-42")
	if err != nil || len(asyncID) != 36 || f.connectivity != 1 {
		t.Fatalf("connectivity id=%q requests=%d err=%v", asyncID, f.connectivity, err)
	}
	res, err := c.FindServer(ctx, "server-token-42", strings.ToUpper(testMachineID))
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProvidesServer() || !res.Owned || len(res.Connections) != 4 || res.Connections[0].URI != "https://192-168-1-5."+fakeHash+".plex.direct:32400" {
		t.Fatalf("%+v", res)
	}
	f.addUser("stranger", Account{ID: 7, Username: "stranger"})
	if _, err := c.FindServer(ctx, "stranger", testMachineID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestResourcesNumericBooleanFlags(t *testing.T) {
	var r Resource
	err := json.Unmarshal([]byte(`{"clientIdentifier":"fixture","publicAddressMatches":1,"owned":1,"connections":[{"protocol":"https","address":"127.0.0.1","port":32401,"uri":"https://example.invalid","local":1,"relay":0,"IPv6":0}]}`), &r)
	if err != nil || !r.PublicAddressMatches || !r.Owned || len(r.Connections) != 1 || !r.Connections[0].Local || r.Connections[0].Relay || r.Connections[0].IPv6 {
		t.Fatalf("resource=%+v err=%v", r, err)
	}
}

const accessTokensXML = `<?xml version="1.0" encoding="UTF-8"?>
<tokens>
  <token token="owner-access" owned="1" username="owner" title="The Owner" id="42"/>
  <token token="friend-a" owned="0" username="alice" title="Alice" id="100" home="0" all_libraries="0">
    <sections><section key="1"/><section key="/library/sections/2"/></sections>
  </token>
  <token token="friend-b" owned="0" username="bob" id="101" all_libraries="1"/>
  <token owned="0" username="notoken" id="102"/>
</tokens>`

const usersXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer friendlyName="myPlex" identifier="com.plexapp.plugins.myplex" machineIdentifier="x" totalSize="2" size="2">
  <User id="100" title="Alice" username="alice" email="alice@example.com" thumb="https://plex.tv/users/a/avatar" home="0" restricted="0">
    <Server id="1" serverId="9" machineIdentifier="` + testMachineID + `" name="Ether" lastSeenAt="1" numLibraries="2" allLibraries="0" owned="0" pending="0"/>
  </User>
  <User id="101" title="Bob B" username="bob" email="bob@example.com" thumb="" home="1" restricted="1"/>
</MediaContainer>`

const sharedServersXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer friendlyName="myPlex" identifier="com.plexapp.plugins.myplex" machineIdentifier="` + testMachineID + `" size="2">
  <SharedServer id="1" username="carol" email="carol@example.com" userID="200" accessToken="friend-c" name="Ether" acceptedAt="1700000000" invitedAt="1690000000" allowSync="1" owned="0">
    <Section id="10" key="1" title="Movies" type="movie" shared="1"/>
    <Section id="11" key="2" title="TV Shows" type="show" shared="0"/>
  </SharedServer>
  <SharedServer id="2" username="dave" email="dave@example.com" userID="201" accessToken="friend-d" name="Ether" acceptedAt="" invitedAt="1690000000" owned="0"/>
</MediaContainer>`

func TestFriendsAccessTokens(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	f.accessFail = 1 // first attempt 500s; the client retries
	c := newServerClient(t, f)
	friends, err := c.Friends(context.Background(), "server-token-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(friends) != 3 {
		t.Fatalf("%+v", friends)
	}
	if !friends[0].Owned || friends[0].Token != "owner-access" {
		t.Fatalf("%+v", friends[0])
	}
	a := friends[1]
	if a.PlexID != 100 || a.Token != "friend-a" || a.Email != "alice@example.com" || a.Thumb == "" ||
		strings.Join(a.Sections, ",") != "1,/library/sections/2" || a.AllLibs {
		t.Fatalf("%+v", a)
	}
	b := friends[2]
	if b.Title != "Bob B" || !b.Home || !b.Restricted || !b.AllLibs {
		t.Fatalf("%+v", b)
	}
	if f.count("GET /api/v2/server/access_tokens") != 2 {
		t.Fatalf("expected one retry, requests %d", f.count("GET /servers/"))
	}
}

func TestFriendsSharedServersFallback(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessFail = 10
	f.sharedXML = sharedServersXML
	c := newServerClient(t, f)
	friends, err := c.Friends(context.Background(), "server-token-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(friends) != 2 {
		t.Fatalf("%+v", friends)
	}
	if friends[0].PlexID != 200 || friends[0].Token != "friend-c" || friends[0].Pending || strings.Join(friends[0].Sections, ",") != "1" {
		t.Fatalf("%+v", friends[0])
	}
	if !friends[1].Pending {
		t.Fatalf("dave should be pending: %+v", friends[1])
	}
}

func TestRequestPlexDirectCertificate(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.pendingDL = 2
	c := newServerClient(t, f)
	b, err := c.RequestPlexDirectCertificate(context.Background(), "server-token-42", CertRequest{PollInterval: 5 * time.Millisecond, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if b.Hash != fakeHash || b.Subject != "*."+fakeHash+".plex.direct" {
		t.Fatalf("%+v", b)
	}
	if err := b.Leaf.VerifyHostname("192-168-1-5." + fakeHash + ".plex.direct"); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(pair.Certificate) != 2 {
		t.Fatalf("chain length %d", len(pair.Certificate))
	}
	if _, err := keyFromPEM(b.KeyPEM); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /api/v2/devices/"+testMachineID+"/certificate/download") != 3 {
		t.Fatal("expected two 202 polls before success")
	}
	// The CSR carries plex.tv's DER subject verbatim, no extensions, an
	// RSA-2048 key and a SHA-256 signature (the fake enforces subject and
	// multipart shape).
	if csr := f.lastCSR; csr == nil || csr.SignatureAlgorithm != x509.SHA256WithRSA ||
		csr.PublicKey.(*rsa.PublicKey).N.BitLen() != 2048 || len(csr.DNSNames) != 0 {
		t.Fatalf("csr %+v", f.lastCSR)
	}
	if ct := f.header("/api/v2/devices/" + testMachineID + "/certificate/csr").Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Fatalf("csr content type %q", ct)
	}

	// Without a decodable DER subject the CSR falls back to CN only.
	f.mu.Lock()
	f.subjectAttr = "not base64 DER"
	f.mu.Unlock()
	if _, err := c.RequestPlexDirectCertificate(context.Background(), "server-token-42", CertRequest{PollInterval: 5 * time.Millisecond, Timeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.lastCSR.Subject.Names); n != 1 {
		t.Fatalf("fallback subject has %d attributes", n)
	}

	// A 5xx upload is not retried within one attempt (rate limits).
	f.mu.Lock()
	f.csrStatus = http.StatusBadGateway
	before := f.csrUploads
	f.mu.Unlock()
	if _, err := c.RequestPlexDirectCertificate(context.Background(), "server-token-42", CertRequest{}); err == nil || f.count("PUT ") < 1 {
		t.Fatal("expected upload failure")
	}
	f.mu.Lock()
	uploads := f.csrUploads - before
	f.csrStatus = 0
	f.mu.Unlock()
	if uploads != 0 { // the fake rejects with csrStatus before counting
		t.Fatalf("uploads %d", uploads)
	}
	if n := f.count("PUT /api/v2/devices/" + testMachineID + "/certificate/csr"); n != 3 {
		t.Fatalf("csr PUTs %d, want 3 (one per attempt)", n)
	}

	// A device plex.tv will not certify.
	f.mu.Lock()
	f.machineID = "other"
	f.mu.Unlock()
	if _, err := c.RequestPlexDirectCertificate(context.Background(), "server-token-42", CertRequest{}); !errors.Is(err, ErrCertUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestCertificateDownloadTimeout(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.pendingDL = 1 << 30
	c := newServerClient(t, f)
	_, err := c.RequestPlexDirectCertificate(context.Background(), "server-token-42", CertRequest{PollInterval: 5 * time.Millisecond, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "not issued within") {
		t.Fatalf("got %v", err)
	}
}

func TestCertManager(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	c := newServerClient(t, f)
	dir := t.TempDir()
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	newMgr := func() *CertManager {
		m, err := NewCertManager(CertManagerOptions{Dir: dir, Client: c, Now: clock,
			Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := newMgr()
	ctx := context.Background()

	// Unclaimed: self-signed fallback.
	info, err := m.Ensure(ctx, "", []string{"192.168.1.5"}, false)
	if err == nil || !info.Present || !info.SelfSigned {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if _, err := m.GetCertificate(nil); err != nil {
		t.Fatal(err)
	}

	// Rate limited: keeps the self-signed certificate and backs off.
	f.mu.Lock()
	f.csrStatus = 429
	f.mu.Unlock()
	info, err = m.Ensure(ctx, "server-token-42", nil, false)
	if !errors.Is(err, ErrRateLimited) || !info.SelfSigned || info.LastError == "" || info.NextAttempt.IsZero() {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	n := f.count("PUT /api/v2/devices/")
	if _, err := m.Ensure(ctx, "server-token-42", nil, false); err == nil || f.count("PUT /api/v2/devices/") != n {
		t.Fatal("expected back-off without a request")
	}
	// The back-off survives a restart.
	m = newMgr()
	if _, err := m.Ensure(ctx, "server-token-42", nil, false); err == nil || f.count("PUT /api/v2/devices/") != n {
		t.Fatal("back-off lost on restart")
	}

	// After the back-off, a plex.direct certificate replaces it.
	f.mu.Lock()
	f.csrStatus = 0
	f.mu.Unlock()
	advance(25 * time.Hour)
	info, err = m.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || info.SelfSigned || info.Hash != fakeHash || info.LastError != "" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if m.NeedsRenewal() {
		t.Fatal("fresh certificate needs renewal")
	}
	fp := info.Fingerprint

	// Reload from disk.
	m = newMgr()
	if got := m.Info(); got.Fingerprint != fp || got.Hash != fakeHash {
		t.Fatalf("reloaded %+v", got)
	}
	// Close to expiry it renews with reason=expiring.
	advance(80 * 24 * time.Hour)
	if !m.NeedsRenewal() {
		t.Fatal("should need renewal")
	}
	info, err = m.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || info.Fingerprint == fp {
		t.Fatalf("renewal: %+v %v", info, err)
	}
	if q := f.header("/api/v2/devices/" + testMachineID + "/certificate/csr"); q == nil {
		t.Fatal("no csr request recorded")
	}
	// TLS handshake with the served certificate.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	pool := x509.NewCertPool()
	pool.AddCert(f.caCert)
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "192-168-1-5." + fakeHash + ".plex.direct"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	conn.Close()
}

func TestCertManagerCustom(t *testing.T) {
	b, err := GenerateSelfSigned([]string{"plex.example.com"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "custom.crt"), filepath.Join(dir, "custom.key")
	os.WriteFile(cf, b.CertPEM, 0o600)
	os.WriteFile(kf, b.KeyPEM, 0o600)
	m, err := NewCertManager(CertManagerOptions{Dir: filepath.Join(dir, "managed"), CustomCertFile: cf, CustomKeyFile: kf})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.Ensure(context.Background(), "tok", nil, true)
	if err != nil || !info.Custom || info.CertFile != cf || m.NeedsRenewal() {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestCertManagerRetriesClientErrorsSoonerThanRateLimits(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.mu.Lock()
	f.csrStatus = http.StatusUnprocessableEntity
	f.mu.Unlock()
	c := newServerClient(t, f)
	now := time.Now()
	m, err := NewCertManager(CertManagerOptions{Dir: t.TempDir(), Client: c,
		Now: func() time.Time { return now }, ClientErrorRetryAfter: 2 * time.Minute,
		Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.Ensure(context.Background(), "server-token-42", nil, false)
	if err == nil || info.RetryAfter != 2*time.Minute || !info.NextAttempt.Equal(info.LastAttempt.Add(2*time.Minute)) {
		t.Fatalf("client error retry state=%+v err=%v", info, err)
	}
	requests := f.count("PUT /api/v2/devices/")
	if _, err := m.Ensure(context.Background(), "server-token-42", nil, false); err == nil || f.count("PUT /api/v2/devices/") != requests {
		t.Fatal("expected 4xx request to be deferred")
	}
}

func TestGenerateSelfSigned(t *testing.T) {
	b, err := GenerateSelfSigned([]string{"192.168.1.5", "ether.lan", "192.168.1.5"}, "ABC", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Hash != "ABC" && b.Hash != "abc" {
		t.Fatal(b.Hash)
	}
	for _, h := range []string{"192.168.1.5", "ether.lan", "localhost", "1-2-3-4.abc.plex.direct"} {
		if err := b.Leaf.VerifyHostname(h); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	if !isSelfSigned(b.Leaf) {
		t.Fatal("not self-signed")
	}
}

func TestParseCertificates(t *testing.T) {
	b, _ := GenerateSelfSigned(nil, "", time.Hour)
	der := b.Leaf.Raw
	for name, in := range map[string][]byte{
		"pem":  b.CertPEM,
		"der":  der,
		"json": []byte(`{"certificate":` + quoteJSON(string(b.CertPEM)) + `}`),
	} {
		cs, err := parseCertificates(in)
		if err != nil || len(cs) != 1 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parseCertificates([]byte("nope")); err == nil {
		t.Fatal("parsed garbage")
	}
}

func quoteJSON(s string) string {
	r := strings.NewReplacer("\n", `\n`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// --- registrar ---

type memConfig struct {
	mu  sync.Mutex
	cfg config.Config
}

func (m *memConfig) Get() config.Config { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg }
func (m *memConfig) Update(fn func(*config.Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.cfg
	if err := fn(&next); err != nil {
		return err
	}
	m.cfg = next
	return nil
}

type fakeUsers struct {
	mu        sync.Mutex
	owner     Account
	token     string
	friends   []Friend
	syncErr   error
	syncCalls int
}

func (u *fakeUsers) SetOwner(ctx context.Context, a Account, tok string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.owner, u.token = a, tok
	return nil
}

func (u *fakeUsers) SyncFriends(ctx context.Context, fs []Friend) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.syncCalls++
	if u.syncErr != nil {
		return u.syncErr
	}
	u.friends = fs
	return nil
}

type fakeWAN struct {
	mu sync.Mutex
	ip string
}

func (w *fakeWAN) Detect(ctx context.Context) (net.IP, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ip == "" {
		return nil, remote.ErrNoWANIP
	}
	return net.ParseIP(w.ip), nil
}

type fakeMapper struct {
	mu      sync.Mutex
	st      remote.MappingStatus
	ensures int
	closed  int
}

func (m *fakeMapper) Ensure(ctx context.Context, internal, external int) (remote.MappingStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensures++
	m.st = remote.MappingStatus{Active: true, Method: remote.MethodUPnP, ExternalPort: external, InternalPort: internal, ExternalIP: "203.0.113.7"}
	return m.st, nil
}
func (m *fakeMapper) Status() remote.MappingStatus { m.mu.Lock(); defer m.mu.Unlock(); return m.st }
func (m *fakeMapper) Close(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed++
	m.st = remote.MappingStatus{}
}

func TestRegistrar(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	cfg := testConfig()
	cfg.Plex.RemoteAccess = true
	cfg.Plex.UseUPnP = true
	cfg.Plex.PublicPort = 32401
	cfg.Plex.CustomURLs = []string{"https://plex.example.com"}
	store := &memConfig{cfg: cfg}
	users := &fakeUsers{}
	wan := &fakeWAN{ip: "198.51.100.1"}
	mapper := &fakeMapper{}
	var lanMu sync.Mutex
	lan := []net.IP{net.ParseIP("192.168.1.5")}
	certs, err := NewCertManager(CertManagerOptions{Dir: t.TempDir(), Client: newServerClient(t, f),
		Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistrar(RegistrarOptions{
		Config: store, Client: newServerClient(t, f), Users: users, WAN: wan, Mapper: mapper, Certs: certs,
		Reach: &remote.ReachabilityChecker{Client: &http.Client{}, Timeout: 50 * time.Millisecond},
		LAN:   func() ([]net.IP, error) { lanMu.Lock(); defer lanMu.Unlock(); return lan, nil },
		Tick:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if st := r.Status(); st.Claimed {
		t.Fatal("claimed before claim")
	}
	if r.SignInClient().Identity().Product != "Ether" {
		t.Fatal("sign-in identity")
	}

	// Unclaimed loop: mapping and certificate (self-signed) run, no plex.tv publishing.
	r.runDue(ctx)
	if f.count("PUT /devices/") != 0 {
		t.Fatal("published while unclaimed")
	}
	if st := r.Status(); !st.Cert.SelfSigned || st.PublicIP != "203.0.113.7" || !st.UPnP.Active {
		t.Fatalf("%+v", st)
	}

	res, err := r.Claim(ctx, "user-token")
	if err != nil {
		t.Fatal(err)
	}
	got := store.Get().Plex
	if !got.Claimed || got.Token != res.ServerToken || got.OwnerPlexID != 42 || got.OwnerUsername != "owner" {
		t.Fatalf("config not persisted: %+v", got)
	}
	if users.owner.ID != 42 || users.token != "server-token-42" {
		t.Fatalf("users sink: %+v", users)
	}

	r.runDue(ctx)
	st := r.Status()
	if !st.Claimed || st.LastPublishError != "" || !st.Listed || !st.PlexPass {
		t.Fatalf("%+v", st)
	}
	r.mu.Lock()
	r.st.PlexPass = false
	r.mu.Unlock()
	if err := r.RefreshOwner(ctx); err != nil || !r.Status().PlexPass {
		t.Fatalf("owner subscription refresh status=%+v err=%v", r.Status(), err)
	}
	// plex.direct certificate obtained after claiming; http URIs published
	// (LAN, WAN from the UPnP mapping, custom).
	if st.Cert.SelfSigned || st.Cert.Hash != fakeHash || store.Get().Plex.CertHash != fakeHash {
		t.Fatalf("cert %+v", st.Cert)
	}
	want := "http://192.168.1.5:32400,http://203.0.113.7:32401,https://plex.example.com"
	if strings.Join(st.PublishedURIs, ",") != want {
		t.Fatalf("published %v", st.PublishedURIs)
	}
	var plexDirect bool
	for _, c := range st.Connections {
		if c.URI == "https://192-168-1-5."+fakeHash+".plex.direct:32400" {
			plexDirect = true
		}
	}
	if !plexDirect {
		t.Fatalf("connections %+v", st.Connections)
	}
	if st.Reachability == nil || st.Reachability.Reachable || len(st.Reachability.Results) == 0 {
		t.Fatalf("reachability %+v", st.Reachability)
	}
	if st.Reachability.Results[0].URI != "https://203-0-113-7."+fakeHash+".plex.direct:32401" {
		t.Fatalf("reachability should probe mapped public port: %+v", st.Reachability.Results)
	}
	if f.connectivity == 0 {
		t.Fatal("connectivity report not sent")
	}
	if len(f.registered) == 0 || st.Friends != 2 || len(users.friends) != 3 {
		t.Fatalf("registered=%d friends=%d synced=%d", len(f.registered), st.Friends, len(users.friends))
	}

	// Nothing changed: no republish on the next pass.
	puts := f.count("PUT /devices/")
	r.runDue(ctx)
	if f.count("PUT /devices/") != puts {
		t.Fatal("republished without change")
	}

	// LAN address change triggers a republish through the running loop.
	lanMu.Lock()
	lan = []net.IP{net.ParseIP("192.168.1.9")}
	lanMu.Unlock()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err == nil {
		t.Fatal("double start")
	}
	r.Trigger()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(strings.Join(r.Status().PublishedURIs, ","), "192.168.1.9") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no republish: %+v", r.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.Stop()
	r.Stop()
	if mapper.closed == 0 {
		t.Fatal("mapping not removed on stop")
	}

	// Turning remote access off publishes LAN + custom only.
	store.Update(func(c *config.Config) error { c.Plex.RemoteAccess = false; return nil })
	if err := r.PublishNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Status().PublishedURIs, ","); got != "http://192.168.1.9:32400,https://plex.example.com" {
		t.Fatalf("lan-only publish %s", got)
	}

	if err := r.Unclaim(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Claimed || store.Get().Plex.Token != "" {
		t.Fatalf("unclaim: %+v", st)
	}
}

func TestRegistrarBYODomainHTTPSWithoutPlexDirectCert(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	cfg.Plex.RemoteAccess = true
	cfg.Plex.UseUPnP = false
	cfg.Plex.PublicHost = "plex.example.com"
	cfg.Plex.PublicPort = 443
	cfg.Plex.SecureConnections = "required"
	store := &memConfig{cfg: cfg}
	r, err := NewRegistrar(RegistrarOptions{
		Config: store,
		Client: newServerClient(t, f),
		WAN:    &fakeWAN{},
		Mapper: &fakeMapper{},
		LAN:    func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	if got := strings.Join(st.PublishedURIs, ","); got != "https://plex.example.com" {
		t.Fatalf("published URIs = %q, want https://plex.example.com", got)
	}
}

func TestRegistrarPublishBackoff(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	cfg := testConfig()
	cfg.Plex.Token = "revoked"
	cfg.Plex.Claimed = true
	now := time.Now()
	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg}, Client: newServerClient(t, f), WAN: &fakeWAN{}, Mapper: &fakeMapper{},
		LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.runDue(ctx)
	st := r.Status()
	if st.LastPublishError == "" || st.FriendSyncError == "" {
		t.Fatalf("%+v", st)
	}
	puts := f.count("PUT /devices/")
	r.runDue(ctx) // within back-off
	if f.count("PUT /devices/") != puts {
		t.Fatal("retried inside back-off")
	}
	now = now.Add(2 * time.Minute)
	r.runDue(ctx)
	if f.count("PUT /devices/") == puts {
		t.Fatal("no retry after back-off")
	}
}

func TestRegistrarPublishesClusterUnion(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	cfg.Plex.RemoteAccess = true
	var mu sync.Mutex
	peers := []string{"http://192.168.1.6:32400", "http://198.51.100.1:32400", "http://192.168.1.5:32400"}
	lanCalls := 0
	now := time.Now()
	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg}, Client: newServerClient(t, f), WAN: &fakeWAN{ip: "198.51.100.1"}, Mapper: &fakeMapper{},
		// Reachability is unrelated to membership publication. Never let
		// TEST-NET address probes block the loop on the host's real network.
		Reach: &remote.ReachabilityChecker{Client: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("offline reachability fixture")
			},
		}}},
		LAN: func() ([]net.IP, error) {
			mu.Lock()
			defer mu.Unlock()
			lanCalls++
			return []net.IP{net.ParseIP("192.168.1.5")}, nil
		},
		PeerURIs: func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(peers) },
		Now:      func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Tick:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A node that is not running the loop still reports its own URIs,
	// refreshing addresses only when stale.
	own := r.NodeURIs(ctx)
	if strings.Join(own, ",") != "http://192.168.1.5:32400,http://198.51.100.1:32400" {
		t.Fatalf("node URIs %v", own)
	}
	r.NodeURIs(ctx)
	if lanCalls != 1 {
		t.Fatalf("fresh addresses re-enumerated: %d", lanCalls)
	}
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	r.NodeURIs(ctx)
	if lanCalls != 2 {
		t.Fatalf("stale addresses not refreshed: %d", lanCalls)
	}

	// The leader publishes its own URIs first, then the peers', deduplicated.
	if err := r.PublishNow(ctx); err != nil {
		t.Fatal(err)
	}
	want := "http://192.168.1.5:32400,http://198.51.100.1:32400,http://192.168.1.6:32400"
	f.mu.Lock()
	got := strings.Join(f.published, ",")
	f.mu.Unlock()
	if got != want {
		t.Fatalf("published %s, want %s", got, want)
	}
	if got = strings.Join(r.Status().PublishedURIs, ","); got != want {
		t.Fatalf("status %s", got)
	}

	// Leadership can be lost and regained: the loop restarts and a
	// membership change republishes through RequestPublish.
	for i := 0; i < 2; i++ {
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		peers = []string{fmt.Sprintf("http://192.168.1.%d:32400", 20+i)}
		mu.Unlock()
		r.RequestPublish()
		wantPeer := fmt.Sprintf("192.168.1.%d", 20+i)
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(strings.Join(r.Status().PublishedURIs, ","), wantPeer) {
			if time.Now().After(deadline) {
				t.Fatalf("no republish for membership change: %v", r.Status().PublishedURIs)
			}
			time.Sleep(10 * time.Millisecond)
		}
		r.Stop()
		if r.Status().Presence.Connected {
			t.Fatal("presence still connected after stop")
		}
	}
}

func TestRegistrationDateEncoding(t *testing.T) {
	// PMS: v = µs &^ 0xf | flags; v ^= (v>>3)&0x38; v ^= (v>>3)&7.
	if got := registrationDate(time.UnixMicro(0x1C3)); got != 0x1FF {
		t.Fatalf("date %#x", got)
	}
	now := time.Now()
	if d := registrationDate(now) - now.UnixMicro(); d < -64 || d > 64 {
		t.Fatalf("date drifted by %d µs", d)
	}
}

func TestParseCertificateSubject(t *testing.T) {
	der, _ := asn1.Marshal(pkix.Name{CommonName: "*.abc123.plex.direct"}.ToRDNSequence())
	s, err := parseCertificateSubject([]byte(`<subject commonName="*.ABC123.plex.direct" subject="` + base64.StdEncoding.EncodeToString(der) + `"/>`))
	if err != nil || s.hash != "abc123" || s.cn != "*.abc123.plex.direct" || !bytes.Equal(s.rawSubject, der) {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := parseCertificateSubject([]byte(`<subject commonName="*.abc123.plex.direct"/>`)); err != nil || s.rawSubject != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := parseCertificateSubject([]byte(`<subject/>`)); !errors.Is(err, ErrCertUnavailable) {
		t.Fatal(err)
	}
}

func TestCSRUploadBody(t *testing.T) {
	body, ct, err := csrUploadBody([]byte("-----BEGIN NEW CERTIFICATE REQUEST-----\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "multipart/form-data; boundary=") ||
		!bytes.Contains(body, []byte("Content-Disposition: form-data; name=\"file\"\r\n")) || bytes.Contains(body, []byte("filename")) ||
		!bytes.Contains(body, []byte("Content-Type: application/x-pem-file\r\n\r\n-----BEGIN NEW CERTIFICATE REQUEST-----\n")) {
		t.Fatalf("%s\n%s", ct, body)
	}
}

func TestRegistrarFriendSyncWriteContentionRetry(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	users := &fakeUsers{syncErr: errors.New("database is locked (waited 10s for this process's other writer)")}
	now := time.Now()
	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg},
		Client: newServerClient(t, f),
		Users:  users,
		WAN:    &fakeWAN{},
		Mapper: &fakeMapper{},
		LAN:    func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Attempt 1: transient SQLite write-gate contention suppresses the
	// dashboard warning and schedules a 15s retry instead of waiting 30m.
	r.runDue(ctx)
	if st := r.Status(); st.FriendSyncError != "" {
		t.Fatalf("attempt 1 surfaced FriendSyncError = %q on transient contention", st.FriendSyncError)
	}
	now = now.Add(5 * time.Second)
	r.runDue(ctx)
	if users.syncCalls != 1 {
		t.Fatalf("syncCalls inside 15s window = %d, want 1", users.syncCalls)
	}

	// Attempt 2 at +15s: still contended, still suppressed.
	now = now.Add(11 * time.Second)
	r.runDue(ctx)
	if users.syncCalls != 2 {
		t.Fatalf("syncCalls after 15s = %d, want 2", users.syncCalls)
	}
	if st := r.Status(); st.FriendSyncError != "" {
		t.Fatalf("attempt 2 surfaced FriendSyncError = %q", st.FriendSyncError)
	}

	// Attempt 3 at +30s: persistent contention surfaces the error while
	// continuing 15s retries.
	now = now.Add(15 * time.Second)
	r.runDue(ctx)
	if users.syncCalls != 3 {
		t.Fatalf("syncCalls at attempt 3 = %d, want 3", users.syncCalls)
	}
	if st := r.Status(); st.FriendSyncError == "" {
		t.Fatal("persistent contention did not surface FriendSyncError at threshold")
	}

	// Once the writer finishes, the next 15s retry clears FriendSyncError
	// without waiting for the 30m FriendSyncInterval.
	users.mu.Lock()
	users.syncErr = nil
	users.mu.Unlock()
	now = now.Add(15 * time.Second)
	r.runDue(ctx)
	st := r.Status()
	if st.FriendSyncError != "" || st.Friends != 2 {
		t.Fatalf("after contention cleared: err=%q friends=%d, want empty err and 2 friends", st.FriendSyncError, st.Friends)
	}
}

func TestRegistrarFriendSyncBackoffAndManualRecheck(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	users := &fakeUsers{syncErr: errors.New("upstream 502")}
	now := time.Now()
	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg},
		Client: newServerClient(t, f),
		Users:  users,
		WAN:    &fakeWAN{},
		Mapper: &fakeMapper{},
		LAN:    func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	r.runDue(ctx)
	if st := r.Status(); st.FriendSyncError != "upstream 502" {
		t.Fatalf("FriendSyncError = %q, want upstream 502", st.FriendSyncError)
	}
	// Retries in 1m instead of waiting the full 30m FriendSyncInterval.
	now = now.Add(30 * time.Second)
	r.runDue(ctx)
	if users.syncCalls != 1 {
		t.Fatalf("retried inside 1m backoff: calls=%d", users.syncCalls)
	}
	now = now.Add(31 * time.Second)
	r.runDue(ctx)
	if users.syncCalls != 2 {
		t.Fatalf("did not retry after 1m backoff: calls=%d", users.syncCalls)
	}

	// Manual "Run checks again" (Trigger + RequestReachabilityCheck)
	// immediately re-runs friend sync and clears the error.
	users.mu.Lock()
	users.syncErr = nil
	users.mu.Unlock()
	r.Trigger()
	r.RequestReachabilityCheck()
	r.runDue(ctx)
	if st := r.Status(); st.FriendSyncError != "" || st.Friends != 2 {
		t.Fatalf("after manual recheck: err=%q friends=%d", st.FriendSyncError, st.Friends)
	}
}
