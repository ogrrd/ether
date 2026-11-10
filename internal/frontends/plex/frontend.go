package plex

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Frontend adapts the existing PMS handler to the frontend registry.
// Unmount stops this mount's broker sessions, encoders, trickplay, and
// downloads, and drops its watch subscription. The process-owned server
// stays up so an in-flight request can finish.
type Frontend struct {
	NewServer    func() (*Server, error)
	Server       *Server
	WatchRefresh frontend.WatchRefreshSubscriber
	// ExplicitWatchOnly preserves spike mode: only cross-surface scrobbles refresh.
	ExplicitWatchOnly bool
	StatusFunc        func(context.Context) frontend.Status
	ListenersFunc     func() []frontend.ListenerSpec
}

func (f Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "plex", Name: "Plex", DefaultPort: 32400, SettingsSchema: frontend.PortSchema()}
}
func (f Frontend) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Server == nil && f.NewServer != nil {
		var err error
		f.Server, err = f.NewServer()
		if err != nil {
			return nil, err
		}
	}
	if f.Server == nil && host.Plex != nil {
		retained, ok, err := host.Plex.Server(ctx)
		if err != nil {
			return nil, err
		}
		if ok {
			f.Server, _ = retained.(*Server)
		} else {
			deps, err := host.Plex.Deps(ctx)
			if err != nil {
				return nil, err
			}
			if deps.Logger == nil && host.Log != nil {
				deps.Logger = host.Log
			}
			srv := NewServer(deps)
			if deps.Streams != nil {
				srv.UsePlexStreams(deps.Streams)
			}
			if deps.Encoder != nil {
				srv.UsePlexEncoder(deps.Encoder)
			}
			host.Plex.BindServer(srv)
			f.Server = srv
		}
	}
	if f.Server == nil {
		return nil, errors.New("plex: frontend server is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Server != nil {
		f.Server.UsePlaybackBroker(host.Sessions)
		f.Server.bindHost(host)
	}
	status := f.StatusFunc
	if status == nil && host.Plex != nil {
		status = host.Plex.Status
	}
	listeners := f.ListenersFunc
	if listeners == nil && host.Plex != nil {
		listeners = host.Plex.Listeners
	}
	instance := &plexInstance{sessions: host.Sessions, server: f.Server, status: status, listeners: listeners}
	if f.WatchRefresh != nil {
		instance.unsubscribe = f.WatchRefresh(f.Server.WatchStateChanged, f.ExplicitWatchOnly)
	} else if host.Plex != nil {
		instance.unsubscribe = host.Plex.WatchRefresh(f.Server.WatchStateChanged)
	}
	return instance, nil
}

type plexInstance struct {
	server *Server
	// sessions is this mount's broker. The retained server is updated on each
	// mount so its playback handlers publish through the current broker.
	sessions    frontend.Sessions
	unsubscribe func()
	closeOnce   sync.Once
	status      func(context.Context) frontend.Status
	listeners   func() []frontend.ListenerSpec
}

func (i *plexInstance) Handler() http.Handler { return i.server }

// Plex shares the existing owner-console listener; the app owns that socket.
func (i *plexInstance) Listeners() []frontend.ListenerSpec {
	if i.listeners != nil {
		return i.listeners()
	}
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/"}}
}
func (i *plexInstance) Discovery() []frontend.Announcer { return nil }
func (i *plexInstance) Status(ctx context.Context) frontend.Status {
	if i.status != nil {
		return i.status(ctx)
	}
	return frontend.Status{}
}
func (i *plexInstance) Close(ctx context.Context) error {
	i.closeOnce.Do(func() {
		if i.unsubscribe != nil {
			i.unsubscribe()
		}
		if stop, ok := i.sessions.(plexProtocolStop); ok {
			_ = stop.StopProtocol(ctx, "plex")
		}
		if i.server != nil {
			i.server.releaseMountedPlayback()
		}
	})
	return nil
}

var _ frontend.Frontend = Frontend{}
