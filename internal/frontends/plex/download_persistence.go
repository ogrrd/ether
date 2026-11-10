package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

func (s *Server) downloadPersistence() frontend.DownloadQueuePersistence {
	var users func(context.Context) ([]frontend.Viewer, error)
	if s.host.Identity != nil {
		users = s.listUsers
	}
	return s.d.SessionStores.NewDownloadQueuePersistence(s.downloads, s.d.DB, s.d.Downloads, s.log, func() time.Duration { return s.cfg().Downloads.Expiry.D() }, s.downloadStorageID, users)
}
func (s *Server) retainDownload(path string, ready time.Time) {
	s.downloadPersistence().Retain(path, ready)
}
func (s *Server) stageDownloads() {
	if s.d.DB == nil {
		return
	}
	s.downloads.WakeDownloads()
}
func (s *Server) downloadStorageID() string {
	id := s.cfg().Server.MachineIdentifier
	if s.d.Cluster != nil {
		id += "/" + s.d.Cluster.NodeID()
	}
	return id
}
func (s *Server) saveDownloads() error { return s.downloadPersistence().Save() }
func (s *Server) loadDownloads()       { s.downloadPersistence().Load(s.ctx) }

// writeDownloads runs the queue writer with the dependencies bound when the
// server was built; reading Server fields from the goroutine itself would race
// with anything that configures the server after NewServer returns.
func (s *Server) writeDownloads(p frontend.DownloadQueuePersistence) {
	defer s.wg.Done()
	p.Run(s.ctx)
}
func (s *Server) stopDownloadsForRestart() { s.downloads.StopForRestart() }
