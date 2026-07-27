package apps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yundera/maison/internal/brand"
)

// Reachability: the single question the launch page asks on a loop — "can I send
// the user to this app yet, and if not, why not?". The answer combines two
// sources Maison already trusts:
//
//   - Docker container state (always available, never lies about what is running)
//   - an HTTP probe of the app's own URL (precise about 2xx vs 502 vs a TLS
//     handshake that hasn't a certificate yet, but only meaningful for an app
//     with a gateway URL the container can actually reach)
//
// The container state is the floor: an app whose containers aren't all up is
// "starting" no matter what HTTP says, and a container whose health check is
// still "starting" is "initializing" — the calm, patient state. Only once the
// containers are up and (if declared) healthy does the HTTP classification get to
// decide "ready" vs a specific failure. A healthy container whose URL the
// *container* can't reach is still reported ready: the health check already
// proved the app is listening, and it is the user's browser — not Maison — that
// will open it. See internal/server/launch.go for how phases render.

// Launch phases. These are what the launch page keys its copy off; they are a
// superset of the raw HTTP classifications below (a network error, for instance,
// resolves to "waiting"/"initializing"/"ready" depending on health + first boot).
const (
	PhaseStarting     = "starting"     // containers not all up yet
	PhaseInitializing = "initializing" // up but health=starting, or first boot still settling
	PhaseWaiting      = "waiting"      // up, but not answering yet (normal boot)
	PhaseReady        = "ready"        // a real response — go
	PhaseBadGateway   = "bad_gateway"  // 502/503/504: gateway up, app behind it isn't
	PhaseAppError     = "app_error"    // 500/etc: the app answered, with an error
	PhaseTLS          = "tls"          // certificate not issued yet (transient)
	PhaseNoURL        = "no_url"       // nothing web to open at all
)

// Raw HTTP probe classifications.
const (
	probeOK         = "ok"
	probeMaison     = brand.Slug // our own catch-all still standing in for the app host
	probeBadGateway = "bad_gateway"
	probeAppError   = "app_error"
	probeTLS        = "tls"
	probeNet        = "refused" // connection refused / reset / timeout / DNS — keep waiting
)

// AppRef is the minimal app identity the launch page needs to show a name + icon.
type AppRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// ContainerState is the aggregated Docker view of one app's containers.
type ContainerState struct {
	Running int    `json:"running"`
	Total   int    `json:"total"`
	Health  string `json:"health,omitempty"` // "", starting, healthy, unhealthy
}

// Reach is the launch page's poll response: the phase to render plus the raw
// facts behind it, and the URL to send the browser to once ready.
type Reach struct {
	App        AppRef         `json:"app"`
	URL        string         `json:"url,omitempty"`    // gateway URL the browser opens (empty for port-only apps)
	Scheme     string         `json:"scheme,omitempty"` // for a port-only app the client builds scheme://<host>:port
	Port       string         `json:"port,omitempty"`
	Index      string         `json:"index,omitempty"`
	Phase      string         `json:"phase"`
	Result     string         `json:"result,omitempty"` // raw HTTP classification, for the facts panel
	HTTPStatus int            `json:"http_status,omitempty"`
	Container  ContainerState `json:"container"`
	FirstBoot  bool           `json:"first_boot"`
}

// Get returns one app by id from the reconciled list, or false if absent.
func (r *Registry) Get(ctx context.Context, id string) (App, bool) {
	list, err := r.List(ctx)
	if err != nil {
		return App{}, false
	}
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// ProjectState aggregates the live Docker state of one project's containers. found
// is false when Docker is unreachable or the project has no containers.
func (r *Registry) ProjectState(ctx context.Context, id string) (running, total int, health string, found bool) {
	conts, err := r.dx.ListProjectContainers(ctx)
	if err != nil {
		return 0, 0, "", false
	}
	ps := &projectState{}
	for _, c := range conts {
		if c.Project != id {
			continue
		}
		found = true
		ps.total++
		if c.State == "running" {
			ps.running++
		}
		switch c.Health {
		case HealthHealthy:
			ps.healthy++
		case HealthUnhealthy:
			ps.unhealthy++
		case HealthStarting:
			ps.starting++
		}
	}
	return ps.running, ps.total, ps.health(), found
}

// Reach evaluates whether app can be opened yet and why not. It is the body
// behind both /api/apps/{id}/reachable and the gate's /__maison/reachable.
func (r *Registry) Reach(ctx context.Context, app App) Reach {
	scheme := app.Scheme
	if scheme == "" {
		scheme = "http"
	}
	res := Reach{
		App:       AppRef{ID: app.ID, Name: app.Name, Icon: app.Icon},
		URL:       gatewayURL(app),
		Scheme:    scheme,
		Port:      app.Port,
		Index:     app.Index,
		FirstBoot: !r.HasReached(app.ID),
	}
	running, total, health, _ := r.ProjectState(ctx, app.ID)
	res.Container = ContainerState{Running: running, Total: total, Health: health}
	res.Phase = r.derivePhase(ctx, &res, app.ID, health, running, total)
	return res
}

// derivePhase applies the readiness ladder described at the top of the file and,
// as a side effect, stamps the first-boot marker the moment an app first reads as
// ready.
func (r *Registry) derivePhase(ctx context.Context, res *Reach, id, health string, running, total int) string {
	if res.URL == "" && res.Port == "" {
		return PhaseNoURL
	}
	// Container floor: nothing is ready until every container is up.
	if total == 0 || running < total {
		return PhaseStarting
	}
	// Up, but its own health check says it is still coming up.
	if health == HealthStarting {
		return PhaseInitializing
	}

	// A gateway URL is one the backend can probe directly for a precise verdict.
	if res.URL != "" {
		result, status := httpProbe(ctx, res.URL)
		res.Result, res.HTTPStatus = result, status
		switch result {
		case probeOK:
			r.MarkReached(id)
			return PhaseReady
		case probeMaison:
			return PhaseStarting // the app host still resolves to our catch-all
		case probeBadGateway:
			return PhaseBadGateway
		case probeAppError:
			return PhaseAppError
		case probeTLS:
			return PhaseTLS
		default: // network error: trust the health check if we have one
			return r.settleUnreachable(id, res, health)
		}
	}

	// Port-only app: the backend has no reachable URL to probe (the port is
	// published on the host, and the browser — not Maison — reaches it). Lean on
	// the health check; the launch page pings it client-side to catch the
	// no-health-check case.
	return r.settleUnreachable(id, res, health)
}

// settleUnreachable decides the phase when the backend couldn't get an HTTP answer
// (no probeable URL, or the probe failed at the network layer). A healthy
// container is authoritative — its health check already proved the app listens —
// so we call it ready; otherwise we keep waiting, patiently on first boot.
func (r *Registry) settleUnreachable(id string, res *Reach, health string) string {
	if health == HealthHealthy {
		r.MarkReached(id)
		return PhaseReady
	}
	if res.FirstBoot {
		return PhaseInitializing
	}
	return PhaseWaiting
}

// gatewayURL is the public URL the backend can probe and the browser can open:
// the resolved x-compose-app web URL, else one built from an x-casaos hostname.
// Empty for a port-only app (no gateway host), which the client handles instead.
func gatewayURL(app App) string {
	if app.URL != "" {
		return app.URL
	}
	if app.Hostname != "" {
		scheme := app.Scheme
		if scheme == "" {
			scheme = "http"
		}
		u := scheme + "://" + app.Hostname
		if app.Index != "" && app.Index != "/" {
			if !strings.HasPrefix(app.Index, "/") {
				u += "/"
			}
			u += app.Index
		}
		return u
	}
	return ""
}

// probeClient is dedicated to reachability checks: a short timeout so a slow app
// doesn't stall a poll, and redirects left unfollowed so a 3xx (an SSO bounce, a
// login redirect) reads as "the app is up" rather than being chased.
var probeClient = &http.Client{
	Timeout:       4 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// httpProbe issues one GET and classifies the outcome. status is 0 when no HTTP
// response was received (network/TLS error).
func httpProbe(ctx context.Context, rawurl string) (result string, status int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return probeNet, 0
	}
	req.Header.Set("User-Agent", brand.Name+"-LaunchProbe")
	resp, err := probeClient.Do(req)
	if err != nil {
		return classifyErr(err), 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	// Our own catch-all marks its responses; that means the app host is still
	// standing in on Maison, not the real app answering.
	if resp.Header.Get(brand.Header) != "" {
		return probeMaison, resp.StatusCode
	}
	return classifyStatus(resp.StatusCode), resp.StatusCode
}

// classifyStatus maps a status code to a probe result. Anything below 500 counts
// as reachable — deliberately including 3xx, 401, 403 and 404: a redirect to an
// identity provider or an auth challenge both mean the app itself is up and
// answering, which is what "ready" is about.
func classifyStatus(code int) string {
	if code >= 500 {
		switch code {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return probeBadGateway
		default:
			return probeAppError
		}
	}
	return probeOK
}

// classifyErr distinguishes a certificate-not-ready-yet handshake failure (a
// transient, self-clearing state on a freshly-provisioned app) from any other
// network error, which we lump together as "keep waiting".
func classifyErr(err error) string {
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &recErr) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &hostnameErr) ||
		errors.As(err, &certInvalid) {
		return probeTLS
	}
	if s := err.Error(); strings.Contains(s, "certificate") ||
		strings.Contains(s, "tls:") || strings.Contains(s, "x509") {
		return probeTLS
	}
	return probeNet
}
