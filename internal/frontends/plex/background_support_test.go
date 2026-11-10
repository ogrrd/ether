package plex

import ()

// waitBackground blocks until the enrichment queues are idle. A finished
// title fetch can queue colour work, so it loops until both are quiet.
func (s *Server) waitBackground() {
	if s.titleJobs == nil {
		if source, ok := s.d.LibraryGUIDs.(interface{ Wait() }); ok {
			source.Wait()
		}
		if source, ok := s.d.LibraryMetadata.(interface{ Wait() }); ok {
			source.Wait()
		}
		return
	}
	for i := 0; i < 4; i++ {
		s.titleJobs.wait()
		s.blurJobs.wait()
		if source, ok := s.d.LibraryGUIDs.(interface{ Wait() }); ok {
			source.Wait()
		}
		if source, ok := s.d.LibraryMetadata.(interface{ Wait() }); ok {
			source.Wait()
		}
	}
}

// wait blocks until nothing is queued or running (or the queue is shut
// down). Tests use it to make background enrichment deterministic.
func (q *bgQueue) wait() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) > 0 && q.ctx.Err() == nil {
		q.idle.Wait()
	}
}
