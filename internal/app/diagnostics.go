package app

import (
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type diagnostic struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
}

func (a *App) diagnosticsHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		st := a.registrar.Status()
		cfg := a.cfg.Get()
		steps := []diagnostic{}
		add := func(name string, good bool, detail, action string) {
			state := "attention"
			if good {
				state = "ok"
			}
			steps = append(steps, diagnostic{name, state, detail, action})
		}
		add("Plex account", st.Claimed, "The server must be claimed by the owner account.", "Sign in and claim this server in Plex & remote access.")
		add("Server discovery", st.Listed && st.LastPublishError == "", "Plex must list Ether and its connection addresses.", "Check the published address and refresh registration.")
		if st.Claimed {
			switch {
			case st.FriendSyncError != "":
				add("Shared users", false, "Shared Plex users failed to sync: "+st.FriendSyncError, "Run checks again or choose Sync Plex users once the database or network settles.")
			case !st.LastFriendSync.IsZero():
				noun := "users"
				if st.Friends == 1 {
					noun = "user"
				}
				add("Shared users", true, fmt.Sprintf("%d shared Plex %s synced.", st.Friends, noun), "")
			default:
				steps = append(steps, diagnostic{"Shared users", "unknown", "Shared Plex users have not been checked yet.", "Run checks again to sync shared users now."})
			}
		}
		add("Remote access enabled", cfg.Plex.RemoteAccess, "The configured public port must forward to Ether.", "Enable remote access and set the router port forward.")
		add("Port mapping", st.UPnP.LastError == "" && st.UPnP.Active, "Automatic mapping is optional when a manual port forward exists.", "Use a manual forward if your router does not support UPnP/NAT-PMP.")
		switch wan := st.WANCheck(); {
		case wan.Reason == "outside_client":
			add("External reachability", true, "An authenticated Plex client connected from outside this network within the last five minutes.", "")
		case wan.Reason == "self_check":
			add("External reachability", true, "Ether reached itself at "+wan.Address+". Checked from this server, so devices away from home may differ.", "")
		case wan.Reason == "wrong_identity":
			add("External reachability", false, wan.Address+" is answered by a different server.", "Check where the public host name and your reverse proxy point.")
		case wan.Reason == "self_check_failed":
			steps = append(steps, diagnostic{"External reachability", "unknown", "Ether could not reach " + wan.Address + " from this server. Some routers block this check, so outside devices may still connect.", "Check the public host name, your reverse proxy and its certificate."})
		case wan.Reason == "plex_tv":
			add("External reachability", wan.State == "reachable", "plex.tv tested inbound connectivity to your published public port from outside your network.", "Check router port forwarding, double NAT/CGNAT, and the published public port.")
		default:
			steps = append(steps, diagnostic{"External reachability", "unknown", "Plex has not supplied an external connectivity result yet.", "Refresh registration and allow the connectivity check to complete."})
		}
		add("Secure connection", st.Cert.Present && !st.Cert.SelfSigned && st.CertError == "", "Official clients prefer a valid plex.direct or custom certificate.", "Check certificate renewal, local DNS rebinding protection, and the device clock.")
		key, err := os.Stat(filepath.Join(a.opts.DataDir, config.CredentialKeyFile))
		private := err == nil && key.Mode().Perm()&0077 == 0
		add("Credential storage", private, "Configuration secrets use authenticated encryption with a private local key.", "Keep the data directory and complete backup bundles private.")
		steps = append(steps, diagnostic{"Compatibility version", "ok", plextv.PMSVersion(cfg), "Ether reports the PMS version its responses were verified against unless you override it or opt into tracking the latest public release in Plex settings."})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"checkedAt": time.Now(), "steps": steps, "remote": st, "compatibilityVersion": plextv.PMSVersion(cfg), "latestPublicVersion": cfg.Plex.LatestCompatibilityVersion, "limits": "Account settings and paid app features follow Plex's account authorization and entitlements."})
	})
	m.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		a.registrar.Trigger()
		a.registrar.RequestReachabilityCheck()
		if a.frontends != nil {
			a.frontends.refreshLAN()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]bool{"scheduled": true})
	})
	return m
}
