/*
Copyright 2026 Giant Swarm.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remoteapp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// verify.go closes two detection gaps, one layer apart.
//
// The first is giantswarm/giantswarm#37521 "gap 2": every signal the
// operator exposed said a tunnel was healthy while ghostunnel served an
// SVID whose `dns_sans` did not match the Service DNS name callers
// dialled. tbot joined, the SVID was issued, ghostunnel bound :8443, and
// the sidecar's readiness probe is a bare TCPSocket connect — which a
// wrong-SAN certificate passes. 40 tunnels were broken for ~2 days and
// the only observable was 21 MCPServers Failed one hop downstream.
//
// The second is giantswarm/tunnelport#110: with the certificate check in
// place, a Teleport app service whose gRPC connection to its auth server
// had gone stale answered every new app session with 504 Gateway Timeout
// for thirteen minutes. ghostunnel forwarded each request faithfully
// (its log shows the bytes going out and the 179-byte 504 coming back),
// so the handshake verified, the pods were Ready, and the RemoteApps
// stayed Ready/Verified/Identity/Serving while every caller failed. The
// break was behind the tunnel, where nothing on the consumer side looks.
//
// The fix for both is to check the one thing no passive observation can
// see: what the tunnel actually does when used. The operator dials its
// own rendered Service over TLS with ServerName set to the FQDN callers
// use, verifies the presented chain against the SPIFFE trust bundle, and
// then — on that same verified session — sends one HTTP request through
// the tunnel and looks at what comes back. The outcome is published as
// per-RemoteApp metrics plus the `TunnelVerified` and `UpstreamReachable`
// conditions.
//
// # Position on ADR 0003
//
// ADR 0003 restricts RemoteApp.status to k8s-visible state. Its two
// stated reasons are (a) avoid the `pods/log` RBAC grant and (b) never
// couple status classification to tbot's log format, which is not a
// stable API. An outbound TLS handshake plus one HTTP request against the
// operator's own rendered Service trips neither: it needs no additional
// RBAC at all (the verifier reads the RemoteApp and Pod lists it already
// watches, and reads the trust bundle from a file the kubelet mounts —
// never from the API), and X.509, TLS and HTTP status codes are about as
// stable a set of contracts as exists.
//
// What it does change is that two conditions now derive from an *active
// probe* rather than passive observation, so this file owes two things
// the passive conditions do not:
//
//   - Honest unknowns. "I could not verify" is never reported as "the
//     certificate is bad", and "I did not send a request" is never
//     reported as "the upstream is down". No trust bundle, no probe yet,
//     a tunnel that is not Ready, or a handshake that failed each produce
//     Unknown or a distinct result — see resultNotReady,
//     UpstreamNotProbed and VerificationStore.BundleAvailable.
//   - Off switches. The whole mechanism is gated on VerifyConfig.Enabled
//     and the HTTP half additionally on VerifyConfig.UpstreamProbe; with
//     either off the corresponding condition is removed rather than left
//     at Unknown.
//
// # Ready
//
// TunnelVerified deliberately does NOT feed `status.ready`: Ready=True
// with Verified=False is the SAN-drift incident, a legitimate and highly
// actionable state, and the alert on cert_invalid covers it.
// UpstreamReachable does feed it. A tunnel whose far end answers nothing
// but 504 is not usable, and the consumers of Ready — muster's MCPServer,
// `kubectl get remoteapp` — are where people looked during #110 and saw
// green. Folding the verdict in puts the degradation where they already
// look; the reason `UpstreamUnreachable` on the Ready condition says
// which layer failed.
//
// The consequence for this file is that the "is the tunnel claiming to
// serve?" gate can no longer read `status.ready`, which now contains the
// probe's own verdict and would make a failing upstream un-probe itself.
// The gate reads pod readiness directly, via the same summarizeStatus the
// reconciler uses — the join-level fact `status.ready` used to be.

// VerificationResult is the outcome of one TLS probe against one
// RemoteApp's tunnel. The values are the `result` label on
// tunnelport_remoteapp_tls_verification, so they are a public contract:
// the PrometheusRule expressions match on them literally.
//
// Keeping "cannot connect" and "connects but the certificate does not
// verify" apart is the whole point — collapsing them would put the
// incident's failure mode back in the same bucket as an ordinary outage,
// which the crashloop alert already covers.
type VerificationResult string

const (
	// ResultVerified means the handshake completed and the served chain
	// verified against the trust bundle for the Service FQDN.
	ResultVerified VerificationResult = "verified"

	// ResultCertInvalid means TCP connected and TLS did not reach a
	// verified session: a SAN that does not cover the FQDN, a chain that
	// does not root in the SPIFFE bundle, an expired leaf, or any other
	// handshake failure. This is the state the SAN-drift incident would
	// have produced.
	//
	// Non-certificate handshake failures land here rather than in a
	// bucket of their own on purpose. From a caller's side they are the
	// same actionable fact — "something answers on :8443 and I cannot
	// establish a trusted session with it" — and a second result value
	// would only add an alert expression without adding a decision. The
	// distinction survives in Verification.Detail, which reaches the
	// condition message and the logs.
	ResultCertInvalid VerificationResult = "cert_invalid"

	// ResultUnreachable means no TCP connection could be established:
	// DNS failure, connection refused, or timeout. A RemoteApp only
	// reaches this state while its pods report Ready, which makes it
	// genuinely alarming rather than a restatement of "pods are down".
	ResultUnreachable VerificationResult = "unreachable"

	// ResultNotReady means the tunnel does not claim to be serving yet,
	// so it was not probed. Reported rather than omitted so every
	// RemoteApp contributes exactly one series and
	// `count by (result) (...)` is a complete inventory; never alerted
	// on, because pod-level readiness is already covered by
	// TunnelPortTunnelCrashLooping and the Ready condition.
	ResultNotReady VerificationResult = "not_ready"
)

// UpstreamResult is the outcome of the HTTP request sent through the
// tunnel once the TLS session verified. Like VerificationResult it is a
// metric label value and therefore a public contract.
type UpstreamResult string

const (
	// UpstreamReachable means the far end answered with an HTTP status
	// that is not a gateway failure. 200, 401 and 404 all count: the
	// question is whether the path through Teleport to the app answers at
	// all, not whether the probe is authorised to use it.
	UpstreamReachable UpstreamResult = "reachable"

	// UpstreamUnreachable means the request went out and came back with
	// 502, 503 or 504 — what the Teleport proxy or app service return when
	// they cannot reach the next hop — or no HTTP response arrived within
	// the budget. The incident's 504 lands here.
	UpstreamUnreachable UpstreamResult = "unreachable"

	// UpstreamNotProbed means no request was sent: the tunnel was not
	// Ready, the handshake did not verify (there is no trusted session to
	// send on), or the upstream probe is disabled. Reported as Unknown on
	// the condition, never as a failure.
	UpstreamNotProbed UpstreamResult = "not_probed"
)

// UpstreamProbe is the HTTP half of one probe. StatusCode is 0 when no
// response was read; URL is what the request was sent to, which is what a
// reader needs to reproduce it with curl.
type UpstreamProbe struct {
	// Result is the coarse classification.
	Result UpstreamResult

	// StatusCode is the HTTP status the far end answered with, 0 when
	// none arrived.
	StatusCode int

	// URL is the request URL as seen from the operator:
	// https://<name>.<namespace>.svc.<domain>:<port><path>.
	URL string

	// Detail is a one-line diagnosis for the no-response case (timeout,
	// EOF, malformed response). Empty when a status was read — the
	// condition builder formats those from StatusCode and URL.
	Detail string
}

// Verification is one probe outcome. Detail carries the diagnosis for a
// human (it becomes the TunnelVerified condition message); Result is what
// the metric and the alerts key on. Upstream is the HTTP half, filled in
// only when Result is ResultVerified.
//
// The struct is comparable on purpose: RunOnce detects change with `!=`,
// and a field that differs on every healthy round would re-enqueue the
// fleet every tick. That is why the time of the last good upstream probe
// lives in the store rather than here.
type Verification struct {
	// Result is the coarse, alertable classification of the handshake.
	Result VerificationResult

	// Detail is a one-line diagnosis, empty when Result is
	// ResultVerified. For ResultCertInvalid it names the specific X.509
	// fault, which is what tells a reader "wrong SAN" apart from
	// "unknown CA" without reading logs.
	Detail string

	// ServerName is the FQDN the probe verified against — the name a
	// caller dials and therefore the name the SVID's dns_sans must
	// cover.
	ServerName string

	// Upstream is what the request through the verified session got
	// back. Result is UpstreamNotProbed unless the handshake verified and
	// the upstream probe is on.
	Upstream UpstreamProbe
}

// VerificationReader is the read side of the verification store, as the
// reconciler consumes it. An interface (rather than the concrete store)
// keeps status tests able to inject fixed outcomes without running a
// prober.
type VerificationReader interface {
	// Enabled reports whether TLS verification is configured at all.
	// False means computeStatus must not emit a TunnelVerified or
	// UpstreamReachable condition — and must remove stale ones.
	Enabled() bool

	// UpstreamProbeEnabled reports whether the HTTP request through the
	// tunnel is configured. False removes the UpstreamReachable condition
	// and leaves Ready join-level, exactly as before the probe existed.
	UpstreamProbeEnabled() bool

	// Result returns the latest outcome for one RemoteApp. The bool is
	// false when no round has covered it yet, which is Unknown, not a
	// failure.
	Result(key types.NamespacedName) (Verification, bool)

	// LastUpstreamSuccess returns when this RemoteApp's upstream last
	// answered with a non-gateway status, as observed by this replica.
	// The bool is false when it never has. It reaches the condition
	// message only while the upstream is unreachable, so a healthy tunnel
	// does not rewrite its status every round.
	LastUpstreamSuccess(key types.NamespacedName) (time.Time, bool)

	// UpstreamDownSince returns when the current outage of this
	// RemoteApp's upstream began; the bool is false when no outage is
	// open. An outage stays open through rounds that did not probe (pods
	// restarting), so the Event logic can tell "the pods came back and
	// the upstream is still down" from a new failure.
	UpstreamDownSince(key types.NamespacedName) (time.Time, bool)

	// UpstreamRecoveredAt returns when the last outage of this
	// RemoteApp's upstream ended — the first reachable round after an
	// unreachable one, however many not-probed rounds lay between. The
	// bool is false when no outage has ended yet. It is what lets the
	// reconciler emit exactly one recovery Event per outage.
	UpstreamRecoveredAt(key types.NamespacedName) (time.Time, bool)
}

// VerifyConfig carries the operator-level knobs for the verifier. Like
// PodDefaults these are Helm values plumbed through main.go, not CR
// fields: what the operator can verify is a property of the install, not
// of one RemoteApp. The one per-RemoteApp knob, the probe path, lives on
// the CR (spec.probe.path) because it is a fact about the app.
type VerifyConfig struct {
	// Enabled turns the whole mechanism on. Off means no probes, no
	// metrics, no conditions.
	Enabled bool

	// Interval is the wait between probe rounds. Every round re-reads
	// the trust bundle and re-probes every Ready RemoteApp, so this is
	// also the worst-case detection latency.
	Interval time.Duration

	// Timeout bounds dial + handshake for a single RemoteApp.
	Timeout time.Duration

	// Jitter is the window over which one round's probes are spread:
	// every Ready RemoteApp is scheduled at a random offset in [0,
	// Jitter) from the round start, so ~40 tunnels do not open ~40
	// Teleport app sessions at the same instant every Interval. Clamped
	// to Interval/2 so a misconfiguration cannot make rounds overlap.
	Jitter time.Duration

	// Concurrency bounds how many RemoteApps are probed at once. A round
	// therefore costs at most Jitter + ceil(N/Concurrency) *
	// (Timeout+UpstreamTimeout) in the pathological case where every
	// probe times out, and about Jitter when tunnels answer; the former
	// has to stay under Interval for the cadence to hold.
	Concurrency int

	// UpstreamProbe turns on the HTTP request through the verified
	// session. Off restores the pre-#110 surface exactly: TunnelVerified
	// only, Ready join-level, no UpstreamReachable condition.
	UpstreamProbe bool

	// UpstreamTimeout bounds the HTTP request and response through the
	// tunnel, separately from Timeout because the request crosses the
	// Teleport proxy and the app service and legitimately takes longer
	// than an in-cluster handshake. Exceeding it is UpstreamUnreachable.
	UpstreamTimeout time.Duration

	// TrustBundleFile is the path to the PEM bundle the probe verifies
	// against — the `svid_bundle.pem` key of the chart's singleton
	// trust-bundle Secret (ADR 0008), mounted read-only into the manager
	// pod. Read from the filesystem, never through the API server, so
	// the operator keeps its "no verbs on secrets" posture (ADR 0008).
	TrustBundleFile string

	// ClusterDomain is the cluster's DNS domain. It completes the FQDN
	// `<name>.<namespace>.svc.<ClusterDomain>` the probe both dials and
	// pins as ServerName.
	ClusterDomain string

	// TLSPort is the tunnel's TLS port — the same value that reaches the
	// renderer as PodDefaults.GhostunnelListenPort, so the probe can
	// never target a port the Service does not expose.
	TLSPort int32
}

// defaults for VerifyConfig fields left at zero. Exposed as constants so
// main.go's flag defaults and the tests agree.
const (
	// DefaultVerifyInterval is the probe cadence. Short relative to the
	// 20m `for:` window on the alerts, so a sustained failure is
	// sampled ~10 times before it pages, and short relative to tbot's
	// 20m SVID renewal so a bad renewal is caught within one cycle.
	DefaultVerifyInterval = 2 * time.Minute

	// DefaultVerifyTimeout bounds one dial + handshake. Generous for an
	// in-cluster hop; the point is to fail a wedged tunnel rather than
	// to measure latency.
	DefaultVerifyTimeout = 5 * time.Second

	// DefaultVerifyJitter spreads a round's probes over its first
	// quarter. Long enough that a fleet of ~40 tunnels averages one
	// Teleport app session per second rather than 40 at once; short
	// enough that a round still ends well before the next begins.
	DefaultVerifyJitter = 30 * time.Second

	// DefaultVerifyConcurrency keeps a round over a few hundred
	// RemoteApps well inside DefaultVerifyInterval without opening an
	// unbounded number of sockets at once.
	DefaultVerifyConcurrency = 8

	// DefaultUpstreamProbeTimeout bounds the request through the tunnel.
	// The incident's requests hung for 30s before the 504 arrived; a
	// healthy app behind Teleport answers in well under a second. 10s
	// leaves room for a slow app without letting a wedged path stall the
	// round.
	DefaultUpstreamProbeTimeout = 10 * time.Second

	// DefaultProbePath is the request path when spec.probe.path is unset.
	DefaultProbePath = "/"

	// DefaultClusterDomain is the stock Kubernetes DNS domain.
	DefaultClusterDomain = "cluster.local"

	// probeUserAgent identifies the operator's requests in the app's and
	// Teleport's access logs, so a reader can tell the probe apart from
	// real callers.
	probeUserAgent = "tunnelport-upstream-probe"
)

// withDefaults returns cfg with zero-valued fields filled in.
func (c VerifyConfig) withDefaults() VerifyConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultVerifyInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultVerifyTimeout
	}
	if c.Jitter <= 0 {
		c.Jitter = DefaultVerifyJitter
	}
	if c.Jitter > c.Interval/2 {
		// Half the interval is the most the spread can take and still
		// leave the other half for the probes themselves.
		c.Jitter = c.Interval / 2
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultVerifyConcurrency
	}
	if c.UpstreamTimeout <= 0 {
		c.UpstreamTimeout = DefaultUpstreamProbeTimeout
	}
	if c.ClusterDomain == "" {
		c.ClusterDomain = DefaultClusterDomain
	}
	if c.TLSPort == 0 {
		c.TLSPort = tlsListenPortDefault
	}
	return c
}

// serviceFQDN returns the DNS name a caller dials for one RemoteApp. The
// operator renders the Service with name = CR name in the CR's
// namespace, so this is derived rather than looked up — and it is
// exactly the name the Teleport-side `workload_identity` dns_sans has to
// carry. The SAN-drift incident was precisely a mismatch between this
// string and the certificate.
func serviceFQDN(namespace, name, clusterDomain string) string {
	return fmt.Sprintf("%s.%s.svc.%s", name, namespace, clusterDomain)
}

// probePath returns the request path for one RemoteApp's upstream probe.
func probePath(cr *accessv1alpha1.RemoteApp) string {
	if cr.Spec.Probe != nil && cr.Spec.Probe.Path != "" {
		return cr.Spec.Probe.Path
	}
	return DefaultProbePath
}

// probeTarget is everything one probe needs to know about its tunnel.
type probeTarget struct {
	// addr is the host:port dialled.
	addr string

	// serverName is the TLS ServerName and the HTTP Host.
	serverName string

	// path is the request path of the upstream probe; empty means the
	// probe stops after the handshake.
	path string

	// upstreamTimeout bounds the HTTP exchange. The handshake is bounded
	// by the context instead.
	upstreamTimeout time.Duration
}

// TLSVerifier is the manager Runnable that performs the probe rounds. It
// owns no status writes: it records outcomes in a VerificationStore and
// nudges the reconciler through a channel source, so RemoteApp.status
// keeps exactly one writer (reconcileStatus).
type TLSVerifier struct {
	// Client lists RemoteApps and tunnel pods from the manager's cache.
	// Read-only; the verifier never writes to the API server.
	Client client.Client

	// Config is the resolved operator-level configuration.
	Config VerifyConfig

	// Store receives every outcome and publishes the metrics.
	Store *VerificationStore

	// Events carries one event per RemoteApp whose outcome changed. The
	// reconciler consumes it via source.Channel, which turns a probe
	// result into a normal reconcile pass that refreshes the conditions.
	// Buffered and best-effort: a full channel means a reconcile is
	// already pending, so dropping is correct rather than lossy.
	Events chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp]

	// probe performs one dial. Nil means probeTunnel, which is what
	// production always uses.
	//
	// The seam exists so a round can be exercised end to end — listing,
	// the Ready gate, change detection, the store swap, the enqueue —
	// without the test needing the cluster DNS that resolves a Service
	// FQDN. probeTunnel itself is covered directly against real TLS and
	// HTTP listeners; what this lets the round tests avoid is re-testing
	// the network stack to reach the logic around it.
	probe func(ctx context.Context, t probeTarget, roots *x509.CertPool) Verification

	// jitter draws one target's start offset within [0, max). Nil means
	// uniformly random; tests pin it to zero so a round is instant.
	jitter func(max time.Duration) time.Duration
}

// prober returns the dial function this verifier should use.
func (v *TLSVerifier) prober() func(context.Context, probeTarget, *x509.CertPool) Verification {
	if v.probe != nil {
		return v.probe
	}
	return probeTunnel
}

// jitterDelay returns one target's start offset.
func (v *TLSVerifier) jitterDelay(max time.Duration) time.Duration {
	if v.jitter != nil {
		return v.jitter(max)
	}
	if max <= 0 {
		return 0
	}
	return rand.N(max) // #nosec G404 -- scheduling spread, not a security decision
}

// NeedLeaderElection makes the verifier run on the elected leader only.
// Two consequences, both wanted: the probe load does not multiply by
// replicaCount, and each RemoteApp yields exactly one metric series
// across the whole install, so `== 1` alert expressions cannot fire once
// per replica.
func (v *TLSVerifier) NeedLeaderElection() bool { return true }

// Start runs probe rounds until ctx is cancelled. The first round runs
// immediately rather than after Interval: on a leader handover the
// store starts empty, and an empty store means no series, which means a
// firing alert would resolve and have to re-arm its whole `for:` window.
func (v *TLSVerifier) Start(ctx context.Context) error {
	cfg := v.Config.withDefaults()
	logger := log.FromContext(ctx).WithName("tls-verifier")

	if !cfg.Enabled {
		logger.Info("TLS verification disabled; not probing tunnels")
		return nil
	}

	logger.Info("starting tunnel verification",
		"interval", cfg.Interval,
		"timeout", cfg.Timeout,
		"jitter", cfg.Jitter,
		"upstreamProbe", cfg.UpstreamProbe,
		"upstreamTimeout", cfg.UpstreamTimeout,
		"trustBundleFile", cfg.TrustBundleFile,
		"clusterDomain", cfg.ClusterDomain,
		"tlsPort", cfg.TLSPort,
	)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		if err := v.RunOnce(ctx, cfg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A failed round is not fatal: the next tick retries, and
			// the store keeps the previous outcomes rather than
			// pretending everything broke at once.
			logger.Error(err, "tunnel verification round failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce performs a single probe round: load the trust bundle, list
// every RemoteApp and every tunnel pod, probe the RemoteApps whose pods
// claim the tunnel is up, and publish the outcomes.
//
// Exported so the smoke harness's negative case and the unit tests can
// drive one deterministic round instead of waiting on a ticker.
func (v *TLSVerifier) RunOnce(ctx context.Context, cfg VerifyConfig) error {
	cfg = cfg.withDefaults()
	logger := log.FromContext(ctx).WithName("tls-verifier")

	roots, err := loadTrustBundle(cfg.TrustBundleFile)
	if err != nil {
		// Blind, not broken. Drop every per-RemoteApp series and flip
		// the availability gauge so
		// TunnelPortTLSVerificationUnavailable can say "the check
		// itself is down" — the one thing worse than a silent tunnel
		// failure is a silent failure of the check for it.
		v.Store.SetBundleUnavailable()
		return fmt.Errorf("load trust bundle %q: %w", cfg.TrustBundleFile, err)
	}

	list := &accessv1alpha1.RemoteAppList{}
	if err := v.Client.List(ctx, list); err != nil {
		return fmt.Errorf("list RemoteApps: %w", err)
	}

	// One cached list of every tunnel pod, grouped per RemoteApp. This is
	// the join-level readiness `status.ready` used to carry before it
	// started folding in the probe's own verdict; reading the pods keeps
	// the gate's predicate identical to the reconciler's and immune to
	// what this round is about to publish.
	pods := &corev1.PodList{}
	if err := v.Client.List(ctx, pods, client.MatchingLabels{LabelRole: LabelRoleValue}); err != nil {
		return fmt.Errorf("list tunnel pods: %w", err)
	}
	podsByApp := groupTunnelPods(pods.Items)

	type target struct {
		key      types.NamespacedName
		ready    bool
		probe    probeTarget
		at       time.Time
		previous Verification
		hadPrev  bool
	}

	roundStart := time.Now()
	targets := make([]target, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		if cr.DeletionTimestamp != nil {
			// Mid-deletion: the Service is on its way out, so a probe
			// would report unreachable for a resource nobody can fix.
			continue
		}
		key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
		fqdn := serviceFQDN(cr.Namespace, cr.Name, cfg.ClusterDomain)
		ready, _ := summarizeStatus(podsByApp[key])
		prev, had := v.Store.Result(key)
		t := target{
			key:   key,
			ready: ready,
			probe: probeTarget{
				addr:            net.JoinHostPort(fqdn, itoa(cfg.TLSPort)),
				serverName:      fqdn,
				upstreamTimeout: cfg.UpstreamTimeout,
			},
			at:       roundStart,
			previous: prev,
			hadPrev:  had,
		}
		if cfg.UpstreamProbe {
			t.probe.path = probePath(cr)
		}
		if ready {
			t.at = roundStart.Add(v.jitterDelay(cfg.Jitter))
		}
		targets = append(targets, t)
	}
	// Earliest start first, so a worker never sleeps past a target that
	// is already due. Stable, so equal offsets keep list order.
	sort.SliceStable(targets, func(a, b int) bool { return targets[a].at.Before(targets[b].at) })

	results := make([]Verification, len(targets))
	probe := v.prober()
	work := make(chan int)
	var wg sync.WaitGroup
	workers := min(cfg.Concurrency, len(targets))
	for range workers {
		wg.Go(func() {
			for i := range work {
				t := targets[i]
				if !t.ready {
					// Not claiming to serve, so not probed. The whole
					// premise of this check is "the tunnel says it is
					// healthy — is it?", and probing a tunnel that
					// makes no such claim would report failures for
					// pods that are merely still starting.
					results[i] = Verification{
						Result:     ResultNotReady,
						ServerName: t.probe.serverName,
						Upstream:   UpstreamProbe{Result: UpstreamNotProbed},
					}
					continue
				}
				sleepUntil(ctx, t.at)
				probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				results[i] = probe(probeCtx, t.probe, roots)
				cancel()
			}
		})
	}
	for i := range targets {
		work <- i
	}
	close(work)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		// Shutting down mid-round. Whatever the interrupted probes
		// returned describes the cancellation, not the tunnels; keep the
		// previous round's outcomes rather than publish it.
		return err
	}

	observed := make(map[types.NamespacedName]Verification, len(targets))
	changed := make([]types.NamespacedName, 0, 4)
	for i, t := range targets {
		observed[t.key] = results[i]
		if !t.hadPrev || t.previous != results[i] {
			changed = append(changed, t.key)
		}
		if results[i].Result != ResultVerified && results[i].Result != ResultNotReady {
			logger.Info("tunnel failed TLS verification",
				"remoteapp", t.key,
				"serverName", t.probe.serverName,
				"result", string(results[i].Result),
				"detail", results[i].Detail,
			)
		}
		if results[i].Upstream.Result == UpstreamUnreachable {
			logger.Info("tunnel upstream unreachable",
				"remoteapp", t.key,
				"url", results[i].Upstream.URL,
				"status", results[i].Upstream.StatusCode,
				"detail", results[i].Upstream.Detail,
			)
		}
	}

	// Replace wholesale rather than merge: a RemoteApp that has been
	// deleted since the last round is absent from `observed` and must
	// stop reporting, or its alert would outlive the resource.
	v.Store.Replace(observed)

	// Deterministic order so a test can assert the enqueue sequence and
	// so log/event ordering does not depend on map iteration.
	sort.Slice(changed, func(a, b int) bool {
		if changed[a].Namespace != changed[b].Namespace {
			return changed[a].Namespace < changed[b].Namespace
		}
		return changed[a].Name < changed[b].Name
	})
	for _, key := range changed {
		v.enqueue(key)
	}
	return nil
}

// groupTunnelPods buckets tunnel pods by the RemoteApp they belong to,
// using the same label the reconciler's pod watch routes on.
func groupTunnelPods(pods []corev1.Pod) map[types.NamespacedName][]corev1.Pod {
	out := make(map[types.NamespacedName][]corev1.Pod)
	for i := range pods {
		name := pods[i].Labels[LabelRemoteAppInstance]
		if name == "" {
			continue
		}
		key := types.NamespacedName{Namespace: pods[i].Namespace, Name: name}
		out[key] = append(out[key], pods[i])
	}
	return out
}

// sleepUntil blocks until at or until ctx is done, whichever is first.
func sleepUntil(ctx context.Context, at time.Time) {
	d := time.Until(at)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// enqueue nudges the reconciler to refresh one RemoteApp's conditions.
// Best-effort by design: a full buffer means a reconcile for that
// RemoteApp is already queued, and the reconciler re-reads the store, so
// the dropped event carries no information the pending one lacks.
func (v *TLSVerifier) enqueue(key types.NamespacedName) {
	if v.Events == nil {
		return
	}
	cr := &accessv1alpha1.RemoteApp{}
	cr.Namespace = key.Namespace
	cr.Name = key.Name
	select {
	case v.Events <- event.TypedGenericEvent[*accessv1alpha1.RemoteApp]{Object: cr}:
	default:
	}
}

// loadTrustBundle reads and parses the PEM CA bundle. An empty path, a
// missing file, or a file with no parseable certificate are all the same
// condition — "nothing to verify against" — and all return an error so
// the caller flips the availability gauge instead of reporting bogus
// failures.
//
// The bundle is re-read every round on purpose. tbot rewrites the
// Secret on SVID renewal and the kubelet propagates that into the mount
// asynchronously; caching the pool would pin the operator to whatever
// the bundle was at startup, which is the same class of staleness bug
// this whole file exists to catch.
func loadTrustBundle(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, errors.New("no trust bundle configured")
	}
	pem, err := os.ReadFile(path) // #nosec G304 -- operator-level flag, not user input
	if err != nil {
		return nil, err
	}
	if len(pem) == 0 {
		// The chart pre-creates the Secret with empty Data and tbot
		// fills it in later (ADR 0008), so an empty file is the normal
		// state for the first minute of a fresh install, not a fault.
		return nil, errors.New("trust bundle is empty (tbot has not written it yet?)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("trust bundle contains no parseable certificate")
	}
	return pool, nil
}

// probeTunnel dials t.addr, completes a TLS handshake with ServerName
// pinned to t.serverName, verifies the served chain against roots, and —
// when t.path is set and the handshake verified — sends one HTTP request
// through that session and classifies what comes back.
//
// It is the caller's ServerName that matters for the handshake. ghostunnel
// will serve its SVID to anyone; the question is whether that SVID is
// valid *for the name the Service is reached by*, which is the exact
// check curl does in hack/smoke/consumer/tls-probe.yaml and the exact
// check that the wrong-SAN incident failed.
//
// The request rides the verified session rather than a second
// connection: one dial, one handshake, one Teleport app session per
// RemoteApp per round, and the request provably travels the path a real
// caller's would. Without a path no application bytes are written at
// all, which is what the TLS-only mode was and still is.
func probeTunnel(ctx context.Context, t probeTarget, roots *x509.CertPool) Verification {
	v := Verification{ServerName: t.serverName, Upstream: UpstreamProbe{Result: UpstreamNotProbed}}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		v.Result = ResultUnreachable
		v.Detail = fmt.Sprintf("cannot connect to %s: %s", t.addr, err)
		return v
	}
	defer func() { _ = conn.Close() }()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: t.serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		v.Result = ResultCertInvalid
		v.Detail = describeHandshakeError(t.serverName, err)
		return v
	}
	v.Result = ResultVerified

	if t.path != "" {
		v.Upstream = probeUpstream(tlsConn, t)
	}

	// Close the TLS layer rather than only the socket, so the peer gets a
	// close_notify instead of a reset. The deferred conn.Close above is
	// still the safety net; this just keeps ghostunnel from logging a
	// broken connection for every probe of every tunnel, forever.
	_ = tlsConn.Close()
	return v
}

// probeUpstream sends GET t.path over an already-verified session and
// reads the status line. The body is never read: the status is the
// verdict, and draining a response from an arbitrary app is not the
// operator's business. `Connection: close` tells the far end the same.
//
// 502, 503 and 504 are what the Teleport proxy and app service return
// when the next hop does not answer them, so they — and no response at
// all — are the unreachable class. Every other status, including 401 from
// an OAuth resource server and 5xx from the app's own code, proves the
// path through Teleport answers and is therefore reachable.
func probeUpstream(conn net.Conn, t probeTarget) UpstreamProbe {
	p := UpstreamProbe{URL: "https://" + t.addr + t.path}

	req, err := http.NewRequest(http.MethodGet, p.URL, nil)
	if err != nil {
		p.Result = UpstreamUnreachable
		p.Detail = fmt.Sprintf("cannot build probe request for %s: %s", p.URL, err)
		return p
	}
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Close = true

	if err := conn.SetDeadline(time.Now().Add(t.upstreamTimeout)); err != nil {
		p.Result = UpstreamUnreachable
		p.Detail = fmt.Sprintf("cannot arm probe deadline for %s: %s", p.URL, err)
		return p
	}
	if err := req.Write(conn); err != nil {
		p.Result = UpstreamUnreachable
		p.Detail = describeUpstreamError(p.URL, t.upstreamTimeout, err)
		return p
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		p.Result = UpstreamUnreachable
		p.Detail = describeUpstreamError(p.URL, t.upstreamTimeout, err)
		return p
	}
	_ = resp.Body.Close()

	p.StatusCode = resp.StatusCode
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		p.Result = UpstreamUnreachable
	default:
		p.Result = UpstreamReachable
	}
	return p
}

// describeUpstreamError distinguishes "nothing came back in time" from
// "the connection broke", the two ways a tunnel with a dead far end
// presents: tbot holds the client connection open while its own dial to
// the proxy hangs (timeout), or closes it when that dial fails (EOF,
// connection reset).
//
// The text has to be stable across rounds: it becomes the condition
// message, and RunOnce detects change with `!=` on the whole outcome. A
// *net.OpError renders its source address — an ephemeral port that
// differs on every probe — so only its inner error is kept.
func describeUpstreamError(url string, timeout time.Duration, err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Sprintf("no HTTP response from %s within %s", url, timeout)
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return fmt.Sprintf("no HTTP response from %s within %s", url, timeout)
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Err != nil {
		err = opErr.Err
	}
	return fmt.Sprintf("no HTTP response from %s: %s", url, err)
}

// describeHandshakeError turns a handshake failure into a one-line
// diagnosis that names the fault class first. The prefixes are stable
// strings so the runbook can tell a reader what to look for, and the
// SAN case leads with the mismatch because that is the failure this
// check was built for.
//
// Go wraps verification faults in *tls.CertificateVerificationError, so
// errors.As is required — a type switch on the top-level error sees only
// the wrapper.
func describeHandshakeError(serverName string, err error) string {
	if hostErr, ok := errors.AsType[x509.HostnameError](err); ok {
		return fmt.Sprintf(
			"served certificate is not valid for %s (SAN mismatch): presented %s",
			serverName, strings.Join(certificateNames(hostErr.Certificate), ", "))
	}

	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return fmt.Sprintf(
			"served certificate does not chain to the SPIFFE trust bundle: %s", err)
	}

	if invalidErr, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		if invalidErr.Reason == x509.Expired {
			return fmt.Sprintf("served certificate is expired or not yet valid: %s", err)
		}
		return fmt.Sprintf("served certificate is invalid: %s", err)
	}

	return fmt.Sprintf("TLS handshake to %s failed: %s", serverName, err)
}

// certificateNames lists the DNS SANs (falling back to the CN) a
// certificate actually carries, so the condition message can say what
// was presented instead of only what was expected. In the SAN-drift
// incident this is the line that would have named the stale namespace.
func certificateNames(cert *x509.Certificate) []string {
	if cert == nil {
		return []string{"<no certificate>"}
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames
	}
	if cert.Subject.CommonName != "" {
		return []string{"CN=" + cert.Subject.CommonName}
	}
	return []string{"<no DNS SANs>"}
}

// SetupVerifier registers the verifier as a manager Runnable and returns
// the event channel the reconciler must watch. Kept here rather than in
// main.go so the wiring (leader election, channel buffer, store) travels
// with the code that depends on it.
func SetupVerifier(mgr ctrl.Manager, cfg VerifyConfig, store *VerificationStore) (chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp], error) {
	// Buffer sized for a burst where every RemoteApp on a large consumer
	// MC changes outcome in one round (a chart-wide rollout, say). Past
	// that, enqueue drops — see its comment for why that is safe.
	events := make(chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp], 256)
	verifier := &TLSVerifier{
		Client: mgr.GetClient(),
		Config: cfg,
		Store:  store,
		Events: events,
	}
	if err := mgr.Add(verifier); err != nil {
		return nil, fmt.Errorf("add TLS verifier runnable: %w", err)
	}
	return events, nil
}
