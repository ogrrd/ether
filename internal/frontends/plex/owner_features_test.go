package plex

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOwnerEntitlementsReachSharedClients(t *testing.T) {
	active := true
	features := []string{
		"76ddd91e-8321-4916-94b6-ded8e3727a64", // remote-watch-pass
		"56cd352b-0d47-436d-aced-f20db3508de5", // remote-watch-pass-pms-validation
		"c757f4d0-2ce6-42d8-ab73-b4808b97cc81", // legacy mobile
		"82999dd3-a2be-482e-9f44-357879b4f603", // pass
		"pass", "remote-watch-pass",
	}
	f := newFixture(func(d *Deps) {
		setDepsOwnerSubscription(d, func() (bool, []string) { return active, features })
	})
	defer f.srv.Close()
	check := func(wantActive, wantFeatures string) {
		t.Helper()
		for _, path := range []string{"/", "/media/providers", "/myplex/account"} {
			rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": friendToken})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d", path, rec.Code)
			}
			activeAttr, featuresAttr := "myPlexSubscription", "ownerFeatures"
			if path == "/myplex/account" {
				activeAttr, featuresAttr = "subscriptionActive", "subscriptionFeatures"
			}
			for _, attr := range []string{activeAttr + `="` + wantActive + `"`, featuresAttr + `="` + wantFeatures + `"`} {
				// The account endpoint omits the empty feature list.
				if wantFeatures == "" && strings.HasPrefix(attr, featuresAttr) && !strings.Contains(rec.Body.String(), featuresAttr+`="`) {
					continue
				}
				if !strings.Contains(rec.Body.String(), attr) {
					t.Fatalf("%s missing %s: %s", path, attr, rec.Body.String())
				}
			}
			for _, product := range []string{"Plex for iOS", "Plex for Apple TV"} {
				jsonRec := f.request(t, http.MethodGet, path, map[string]string{
					"X-Plex-Token": friendToken, "Accept": "application/json",
					"X-Plex-Product": product, "X-Plex-Version": "2026.20.0",
				})
				var doc map[string]map[string]any
				if jsonRec.Code != http.StatusOK || json.Unmarshal(jsonRec.Body.Bytes(), &doc) != nil {
					t.Fatalf("%s %s: invalid JSON response %d", product, path, jsonRec.Code)
				}
				fields := doc["MediaContainer"]
				if path == "/myplex/account" {
					fields = doc["MyPlex"]
				}
				if fields[activeAttr] != (wantActive == "1") || (fields[featuresAttr] != wantFeatures && !(wantFeatures == "" && fields[featuresAttr] == nil)) {
					t.Fatalf("%s %s: active=%v features=%v", product, path, fields[activeAttr], fields[featuresAttr])
				}
			}
		}
	}
	check("1", strings.Join(features, ","))
	// Changes after startup, including revocation, must reach clients.
	active, features = false, nil
	check("0", "")
}
