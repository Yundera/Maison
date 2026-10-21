package routes

// PortPicker chooses the host port a route is published on (ports renderer).
//
// The rules, in order:
//
//   - a port the app already has (its .env) is kept: a published port is part of
//     the app's address, and is never renumbered silently;
//   - otherwise the route's own container port, when it is 1024 or above and free —
//     Jellyfin on 8096 reads well, and is what its documentation says;
//   - otherwise the first free port in the range. Ports below 1024 always go there:
//     whichever app was installed first would otherwise own the short address.
//
// Free means not published by another app's container and not kept in another
// app's .env. Maison runs in a container and cannot see a host process bound
// outside Docker; `docker compose up` is the real check (see stackup).
type PortPicker struct {
	Taken     map[int]bool
	Low, High int
}

// Pick returns the port for a route whose container port is upstream and whose
// .env already holds kept (0 when it holds none). It marks the result taken, so
// two routes of one app never share a port.
func (p *PortPicker) Pick(kept, upstream int) int {
	if p.Taken == nil {
		p.Taken = map[int]bool{}
	}
	switch {
	case kept > 0:
		p.Taken[kept] = true
		return kept
	case upstream >= 1024 && !p.Taken[upstream]:
		p.Taken[upstream] = true
		return upstream
	}
	for port := p.Low; port <= p.High; port++ {
		if !p.Taken[port] {
			p.Taken[port] = true
			return port
		}
	}
	return 0
}
