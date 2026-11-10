package experience

import "github.com/braedonsaunders/ether/internal/frontend"

// HomeCandidate, AssembleHome and the generic HomeAssembly live in
// internal/frontend (host_sessions_experience.go).
type HomeCandidate = frontend.HomeCandidate
type HomeAssembly[P any] = frontend.HomeAssembly[P]

var AssembleHome = frontend.AssembleHome
