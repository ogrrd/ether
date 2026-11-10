package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

func (s *Server) downloadPersistence() frontend.DownloadQueuePersistence {
	var users func(context.Context) ([]frontend.Viewer, error)
	if s.d.Auth != nil {
		users = s.d.Auth.Users
	}
	return s.runtime.NewDownloadQueuePersistence(s.downloads, s.runtimeDB(), s.runtime.Downloads(), s.log, func() time.Duration { return s.cfg().Downloads.Expiry.D() }, s.downloadStorageID, users)
}
func (s *Server) retainDownload(path string, ready time.Time) {
	s.downloadPersistence().Retain(path, ready)
}
func (s *Server) stageDownloads() {
	if s.runtimeDB() == nil {
		return
	}
	s.downloads.WakeDownloads()
}
func (s *Server) downloadStorageID() string {
	id := s.cfg().Server.MachineIdentifier
	if c := s.clusterNode(); c != nil {
		id += "/" + c.NodeID()
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
