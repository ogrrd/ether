package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterplayback"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// runPublicRole is the explicit lifecycle for constructed server/delivery
// specialists. Role validation remains closed until shipping-path acceptance.
func (a *App) runPublicRole(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.bindRestart(ctx, cancel)
	if err := a.startCluster(ctx); err != nil {
		return err
	}
	defer a.cluster.Close()
	stopPrivate, err := a.startPrivate(ctx)
	if err != nil {
		return err
	}
	defer stopPrivate()
	var background backgroundGroup
	defer background.Wait()
	defer cancel()
	background.start(func() { a.runNodeControl(ctx) })
	if a.certs != nil {
		background.start(func() { a.certs.RunSharedSync(ctx, plextv.DefaultCertSyncInterval) })
	}
	if a.cfg.Get().Server.NodeRoles.Has(config.RoleServer) {
		a.startServerWorkers(ctx, &background)
	}
	cfg := a.cfg.Get()
	if cfg.Plex.SecureConnections == "required" && a.certs != nil {
		info := a.certs.Info()
		now := time.Now()
		if !info.Present || now.Before(info.NotBefore) || !now.Before(info.NotAfter) {
			return errors.New("public delivery certificate is not ready")
		}
	}
	front := a.opts.front
	if front == nil {
		front = newFrontDoor(a.tracker, a.log)
		defer front.close()
	}
	if !front.bound() {
		ln, err := a.listen(cfg)
		if err != nil {
			return err
		}
		front.serve(ln)
	}
	if a.certs != nil {
		front.setTLS(a.certs.TLSConfig)
	}
	front.setProxies(a.cfg.TrustedProxyCIDRs)
	front.setRemote(a.cfg.AllowRemoteConsole)
	if a.frontends != nil {
		a.frontends.start(ctx, front.ln.Addr())
		defer a.frontends.stop()
	}
	front.gate.Open(a.publicRoleHandler())
	a.markPublicReady()
	if a.cfg.Get().Server.NodeRoles.Has(config.RoleServer) {
		background.start(func() { a.runRegistrarAsLeader(ctx) })
		background.start(func() { a.registrar.RunPortMapping(ctx) })
		if a.registrar != nil {
			a.registrar.ListenerReady()
		}
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-a.privateErrors():
	case result = <-a.clusterFatal:
	case result = <-front.errc:
	}
	// Preserve existing public representations, private transport and membership
	// until exact obligations retire. Deadline reports unresolved, never deletes it.
	a.drainPrivate()
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	front.gate.Close(shutdown)
	a.deployment.publicReady.Store(false)
	a.deployment.ready.Store(false)
	a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessPublic, errorClass: privateReadinessError(result)})
	if a.registrar != nil {
		a.registrar.Stop()
	}
	if errors.Is(result, http.ErrServerClosed) {
		result = nil
	}
	if result != nil {
		return result
	}
	return a.runResult()
}

func (a *App) publicRoleHandler() http.Handler {
	if a.cfg.Get().Server.NodeRoles.Has(config.RoleDelivery) {
		return a.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if clusterplayback.IsPublicRepresentation(r) {
			http.NotFound(w, r)
			return
		}
		a.handler.ServeHTTP(w, r)
	})
}
func (a *App) startServerWorkers(ctx context.Context, g *backgroundGroup) {
	a.catalog.WarmSearch(ctx)
	// Every node that serves search keeps its own long-tail index.
	g.start(func() { a.catalog.RunTail(ctx) })
	g.start(func() { a.cluster.RunAsLeader(ctx, "catalogue sync", a.catalog.Run) })
	g.start(func() { a.cluster.RunAsLeader(ctx, "trending charts", a.catalog.RunTrends) })
	g.start(func() { a.cluster.RunAsLeader(ctx, "collection refresh", a.collections.Run) })
	g.start(func() { a.trakt.Start(ctx) })
	g.start(func() { a.cluster.RunAsLeader(ctx, "database maintenance", a.maintainDB) })
	if a.plexWatch != nil {
		g.start(func() { a.plexWatch.Run(ctx) })
		g.start(func() { a.cluster.RunAsLeader(ctx, "plex watch imports", a.plexWatch.RunImports) })
	}
}

func (a *App) connectServerHooks() {
	a.cfg.SetNotifier(a.cluster)
	a.certs.SetEventBus(a.cluster)
	a.certs.SetRenewalAllowed(a.cluster.IsLeader)
	bus := a.clusterNode()
	a.users.SetBus(bus)
	a.safety.SetBus(bus)
	a.personalize.SetBus(bus)
	a.collections.SetBus(bus)
	a.episodeorder.SetBus(bus)
	a.closers = append(a.closers, func() {
		a.users.Close()
		a.safety.Close()
		a.personalize.SetBus(nil)
		a.collections.SetBus(nil)
		a.episodeorder.SetBus(nil)
	})
	a.cfg.Subscribe(a.applyServerConfig)
	a.closers = append(a.closers, a.cfg.Close)
}
func (a *App) applyServerConfig(c config.Config) {
	a.tmdb.UpdateConfig(c.Catalog)
	a.catalog.UpdateConfig(c.Catalog)
	a.llm.UpdateConfig(c.LLM)
	a.trakt.UpdateConfig(c.Trakt)
	a.subtitles.UpdateConfig(c.OpenSubtitles)
	a.subtitles.UpdateProviders(c.Subtitles.Providers)
	a.publishLibrarySections()
	a.registrar.Trigger()
	if a.frontends != nil {
		a.frontends.reconcile(c)
	}
}
