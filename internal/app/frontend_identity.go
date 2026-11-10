package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

type surfaceIdentity struct {
	settings    func() frontend.Settings
	surface     frontend.Protocol
	users       core.Auth
	resolver    *identity.Resolver
	local       *identity.Local
	grants      *identity.Grants
	quick       *identity.QuickConnect
	times       *identity.ViewerTimes
	login, poll *identity.LoginLimiter
}

func (s surfaceIdentity) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if s.resolver == nil {
		return nil, core.ErrUnauthorized
	}
	return s.resolver.Resolve(ctx, string(s.surface), credential)
}

func (s surfaceIdentity) Accept(ctx context.Context, token string) (*core.Viewer, frontend.CredentialAcceptance, error) {
	if s.surface != "plex" || core.IsGrantCredential(token) {
		return nil, frontend.CredentialAcceptance{}, core.ErrUnauthorized
	}
	if acc, ok := s.users.(frontend.CredentialAcceptor); ok {
		return acc.Accept(ctx, token)
	}
	u, err := s.Resolve(ctx, token)
	return u, frontend.CredentialAcceptance{}, err
}

func (s surfaceIdentity) ReceiptCurrent(ctx context.Context, r frontend.CredentialAcceptance) error {
	if s.surface != "plex" {
		return core.ErrUnauthorized
	}
	if acc, ok := s.users.(frontend.CredentialAcceptor); ok {
		return acc.ReceiptCurrent(ctx, r)
	}
	return core.ErrUnauthorized
}

func (s surfaceIdentity) IssueDelegation(ctx context.Context, userID int64) (string, error) {
	if s.surface != "plex" {
		return "", core.ErrUnauthorized
	}
	if del, ok := s.users.(frontend.DelegationAuth); ok {
		return del.IssueDelegation(ctx, userID)
	}
	return "", core.ErrUnauthorized
}

func (s surfaceIdentity) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	if act, ok := s.users.(frontend.ActiveUserSource); ok {
		return act.ActiveUser(ctx, id)
	}
	return nil, core.ErrUnauthorized
}

func (s surfaceIdentity) Users(ctx context.Context) ([]core.User, error) {
	if s.users == nil {
		return nil, nil
	}
	return s.users.Users(ctx)
}

func (s surfaceIdentity) ViewerTimes(ctx context.Context, credential string, target int64) (frontend.ViewerTimes, error) {
	if s.times == nil || target <= 0 {
		return frontend.ViewerTimes{}, core.ErrUnauthorized
	}
	viewer, err := s.Resolve(ctx, credential)
	if err != nil {
		return frontend.ViewerTimes{}, err
	}
	if viewer == nil {
		return frontend.ViewerTimes{}, core.ErrUnauthorized
	}
	return s.times.Get(ctx, viewer.ID, target)
}

// ResolveDevice binds validation to the stored surface and device without writes.
func (s surfaceIdentity) ResolveDevice(ctx context.Context, credential string, device core.Device) (*core.Viewer, time.Time, error) {
	if s.surface == "" || s.surface == "plex" || s.grants == nil {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	return s.grants.ResolveDevice(ctx, string(s.surface), credential, device)
}

func (s surfaceIdentity) ResolveGrantDevice(ctx context.Context, credential, claim string) (*core.Viewer, core.Device, time.Time, error) {
	if s.surface == "" || s.surface == "plex" || s.grants == nil {
		return nil, core.Device{}, time.Time{}, core.ErrUnauthorized
	}
	return s.grants.ResolveGrantDevice(ctx, string(s.surface), credential, claim)
}

func (s surfaceIdentity) allow(ctx context.Context, limiter *identity.LoginLimiter, key string) error {
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return core.ErrUnauthorized
	}
	allowed, err := limiter.Allow(ctx, key, ip)
	if err != nil {
		return err
	}
	if !allowed {
		return frontend.ErrRateLimited
	}
	return nil
}

// PublicViewers returns only active local sign-in names when the owner enables the picker.
func (s surfaceIdentity) PublicViewers(ctx context.Context) ([]frontend.PublicViewer, error) {
	if s.surface == "plex" || s.settings == nil || s.local == nil {
		return []frontend.PublicViewer{}, nil
	}
	enabled, _ := s.settings()["publicViewerPicker"].(bool)
	if !enabled {
		return []frontend.PublicViewer{}, nil
	}
	if err := s.allow(ctx, s.login, "public-viewers"); err != nil {
		return nil, err
	}
	return s.local.PublicViewers(ctx)
}
func (s surfaceIdentity) SignIn(ctx context.Context, username, password string, device core.Device) (string, error) {
	if s.surface == "plex" {
		return "", core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.login, username); err != nil {
		return "", err
	}
	if core.IsGrantCredential(password) {
		if s.grants == nil {
			return "", core.ErrUnauthorized
		}
		return s.grants.SignInWithGrant(ctx, string(s.surface), username, password, device)
	}
	viewer, link, err := s.local.Authenticate(ctx, username, password)
	if err != nil {
		return "", err
	}
	// The grant is the sign-in. Login timestamps are recorded after the
	// response so a large catalogue database cannot hold the client open.
	return s.grants.Issue(ctx, viewer, &link, string(s.surface), device)
}
func (s surfaceIdentity) IssueRefresh(ctx context.Context, access string, device core.Device) (string, error) {
	return s.grants.IssueRefresh(ctx, string(s.surface), access, device)
}
func (s surfaceIdentity) Refresh(ctx context.Context, token string, device core.Device) (string, string, error) {
	if err := s.allow(ctx, s.poll, "refresh"); err != nil {
		return "", "", err
	}
	access, refresh, err := s.grants.Refresh(ctx, string(s.surface), token, device)
	if err == nil {
		_, _ = s.Resolve(ctx, access)
	}
	return access, refresh, err
}
func (s surfaceIdentity) QuickConnectInitiate(ctx context.Context, device core.Device) (string, string, error) {
	if s.surface == "plex" {
		return "", "", core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.login, "quickconnect-init"); err != nil {
		return "", "", err
	}
	return s.quick.Initiate(ctx, string(s.surface), device)
}
func (s surfaceIdentity) QuickConnectPoll(ctx context.Context, secret string) (string, error) {
	if s.surface == "plex" {
		return "", core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return "", err
	}
	grant, err := s.quick.PollSurface(ctx, string(s.surface), secret)
	if errors.Is(err, identity.ErrQuickConnectPending) {
		return "", frontend.ErrQuickConnectPending
	}
	return grant, err
}

// QuickConnectPollDevice redeems or replays a secret for one device.
func (s surfaceIdentity) QuickConnectPollDevice(ctx context.Context, secret string, device core.Device) (string, error) {
	if s.surface == "plex" {
		return "", core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return "", err
	}
	grant, err := s.quick.PollSurfaceDevice(ctx, string(s.surface), secret, device)
	if errors.Is(err, identity.ErrQuickConnectPending) {
		return "", frontend.ErrQuickConnectPending
	}
	return grant, err
}

// NoteSignIn records a sign-in after the client has the token.
func (s surfaceIdentity) NoteSignIn(ctx context.Context, viewerID int64) error {
	if s.times == nil {
		return nil
	}
	return s.times.RecordSignIn(ctx, viewerID)
}

func (s surfaceIdentity) QuickConnectStatus(ctx context.Context, secret string) (bool, error) {
	if s.surface == "plex" {
		return false, core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return false, err
	}
	return s.quick.StatusSurface(ctx, string(s.surface), secret)
}

func (s surfaceIdentity) QuickConnectDetails(ctx context.Context, secret string) (frontend.QuickConnectDetails, error) {
	if s.surface == "plex" {
		return frontend.QuickConnectDetails{}, core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return frontend.QuickConnectDetails{}, err
	}
	details, err := s.quick.DetailsSurface(ctx, string(s.surface), secret)
	if err != nil {
		return frontend.QuickConnectDetails{}, err
	}
	return frontend.QuickConnectDetails{Approved: details.Approved, Code: details.Code, Secret: secret, Device: details.Device, CreatedAt: details.CreatedAt, ExpiresAt: details.ExpiresAt}, nil
}
func (s surfaceIdentity) QuickConnectApprove(ctx context.Context, viewer *core.Viewer, code string) error {
	if s.surface == "plex" || viewer == nil || viewer.ID <= 0 {
		return core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.login, "quickconnect-approve"); err != nil {
		return err
	}
	return s.quick.ApproveSurface(ctx, string(s.surface), code, viewer.ID)
}
func (s surfaceIdentity) Revoke(ctx context.Context, credential string) error {
	if s.surface == "plex" {
		return core.ErrUnauthorized
	}
	viewer, err := s.Resolve(ctx, credential)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(credential))
	return s.grants.RevokeGrant(ctx, viewer.ID, hex.EncodeToString(hash[:]))
}
func (surfaceIdentity) SetupComplete() bool { return identity.ProtocolSetupComplete() }

var _ frontend.SurfaceIdentity = surfaceIdentity{}

var _ frontend.DeviceIdentity = surfaceIdentity{}

// AuthenticateAppPassword never accepts a household password or issues a new grant.
func (s surfaceIdentity) AuthenticateAppPassword(ctx context.Context, username, password string) (*core.Viewer, error) {
	if s.surface != "xtream" || s.login == nil {
		return nil, core.ErrUnauthorized
	}
	if ip, ok := frontend.ClientIP(ctx); !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, core.ErrUnauthorized
	}
	// Valid app passwords authenticate each API/Range request. Charging those
	// against the interactive sign-in budget would lock out a normal startup.
	if core.IsGrantCredential(password) && username == frontend.XtreamUsername(password) {
		viewer, err := s.Resolve(ctx, password)
		if err == nil && viewer != nil {
			return viewer, nil
		}
	}
	if err := s.allow(ctx, s.login, "xtream"); err != nil {
		return nil, err
	}
	return nil, core.ErrUnauthorized
}
