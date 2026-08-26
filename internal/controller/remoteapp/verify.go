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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// verify.go closes the detection gap giantswarm/giantswarm#37521 calls
// "gap 2": every signal the operator exposed said a tunnel was healthy
// while ghostunnel served an SVID whose `dns_sans` did not match the
// Service DNS name callers dialled. tbot joined, the SVID was issued,
// ghostunnel bound :8443, and the sidecar's readiness probe is a bare
// TCPSocket connect — which a wrong-SAN certificate passes. 40 tunnels
// were broken for ~2 days and the only observable was 21 MCPServers
// Failed one hop downstream.
//
// The fix is to check the one thing no passive observation can see: what
// the tunnel actually serves. The operator dials its own rendered
// Service over TLS with ServerName set to the FQDN callers use, verifies
// the presented chain against the SPIFFE trust bundle, and publishes the
// outcome as a per-RemoteApp metric plus a `TunnelVerified` condition.
//
// # Position on ADR 0003
//
// ADR 0003 restricts RemoteApp.status to k8s-visible state. Its two
// stated reasons are (a) avoid the `pods/log` RBAC grant and (b) never
// couple status classification to tbot's log format, which is not a
// stable API. An outbound TLS handshake against the operator's own
// rendered Service trips neither: it needs no additional RBAC at all
// (the verifier reads the RemoteApp list it already watches, and reads
// the trust bundle from a file the kubelet mounts — never from the API),
// and X.509 plus TLS is about as stable a contract as exists.
//
// What it does change is that one condition now derives from an *active
// probe* rather than passive observation, so this file owes two things
// the passive conditions do not:
//
//   - Honest unknowns. "I could not verify" is never reported as "the
//     certificate is bad". No trust bundle, no probe yet, or a tunnel
//     that is not Ready each produce Unknown or a distinct result — see
//     resultNotReady and VerificationStore.BundleAvailable.
//   - An off switch. The whole mechanism is gated on
//     VerifyConfig.Enabled; with it off the operator behaves exactly as
//     it did before, condition included (computeStatus removes a stale
//     one).
//
// The probe deliberately does NOT feed `status.ready`. Ready is
// documented as join-level and other components consume it; widening it
// would be a silent behavioural change. TunnelVerified is additive.

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
	// reaches this state while reporting Ready=True, which makes it
	// genuinely alarming rather than a restatement of "pods are down".
	ResultUnreachable VerificationResult = "unreachable"

	// ResultNotReady means the tunnel does not claim to be serving yet,
	// so it was not probed. Reported rather than omitted so every
	// RemoteApp contributes exactly one series and
	// `count by (result) (...)` is a complete inventory; never alerted
	// on, because pod-level readiness is already covered by
	// TunnelPortTunnelCrashLooping and status.ready.
	ResultNotReady VerificationResult = "not_ready"
)

// Verification is one probe outcome. Detail carries the diagnosis for a
// human (it becomes the TunnelVerified condition message); Result is what
// the metric and the alerts key on.
type Verification struct {
	// Result is the coarse, alertable classification.
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
}

// VerificationReader is the read side of the verification store, as the
// reconciler consumes it. An interface (rather than the concrete store)
// keeps status tests able to inject fixed outcomes without running a
// prober.
type VerificationReader interface {
	// Enabled reports whether TLS verification is configured at all.
	// False means computeStatus must not emit a TunnelVerified
	// condition — and must remove a stale one.
	Enabled() bool

	// Result returns the latest outcome for one RemoteApp. The bool is
	// false when no round has covered it yet, which is Unknown, not a
	// failure.
	Result(key types.NamespacedName) (Verification, bool)
}

// VerifyConfig carries the operator-level knobs for the TLS verifier.
// Like PodDefaults these are Helm values plumbed through main.go, not CR
// fields: what the operator can verify is a property of the install, not
// of one RemoteApp.
type VerifyConfig struct {
	// Enabled turns the whole mechanism on. Off means no probes, no
	// metrics, no condition.
	Enabled bool

	// Interval is the wait between probe rounds. Every round re-reads
	// the trust bundle and re-probes every Ready RemoteApp, so this is
	// also the worst-case detection latency.
	Interval time.Duration

	// Timeout bounds dial + handshake for a single RemoteApp.
	Timeout time.Duration

	// Concurrency bounds how many RemoteApps are probed at once. A round
	// over N RemoteApps therefore costs at most
	// ceil(N/Concurrency)*Timeout, which has to stay under Interval for
	// the cadence to hold.
	Concurrency int

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

	// DefaultVerifyConcurrency keeps a round over a few hundred
	// RemoteApps well inside DefaultVerifyInterval without opening an
	// unbounded number of sockets at once.
	DefaultVerifyConcurrency = 8

	// DefaultClusterDomain is the stock Kubernetes DNS domain.
	DefaultClusterDomain = "cluster.local"
)

// withDefaults returns cfg with zero-valued fields filled in.
func (c VerifyConfig) withDefaults() VerifyConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultVerifyInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultVerifyTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultVerifyConcurrency
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

// TLSVerifier is the manager Runnable that performs the probe rounds. It
// owns no status writes: it records outcomes in a VerificationStore and
// nudges the reconciler through a channel source, so RemoteApp.status
// keeps exactly one writer (reconcileStatus).
type TLSVerifier struct {
	// Client lists RemoteApps from the manager's cache. Read-only; the
	// verifier never writes to the API server.
	Client client.Client

	// Config is the resolved operator-level configuration.
	Config VerifyConfig

	// Store receives every outcome and publishes the metrics.
	Store *VerificationStore

	// Events carries one event per RemoteApp whose outcome changed. The
	// reconciler consumes it via source.Channel, which turns a probe
	// result into a normal reconcile pass that refreshes the condition.
	// Buffered and best-effort: a full channel means a reconcile is
	// already pending, so dropping is correct rather than lossy.
	Events chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp]

	// probe performs one dial. Nil means probeTLS, which is what
	// production always uses.
	//
	// The seam exists so a round can be exercised end to end — listing,
	// the Ready gate, change detection, the store swap, the enqueue —
	// without the test needing the cluster DNS that resolves a Service
	// FQDN. probeTLS itself is covered directly against real TLS
	// listeners; what this lets the round tests avoid is re-testing the
	// network stack to reach the logic around it.
	probe func(ctx context.Context, addr, serverName string, roots *x509.CertPool) Verification
}

// prober returns the dial function this verifier should use.
func (v *TLSVerifier) prober() func(context.Context, string, string, *x509.CertPool) Verification {
	if v.probe != nil {
		return v.probe
	}
	return probeTLS
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

	logger.Info("starting TLS verification",
		"interval", cfg.Interval,
		"timeout", cfg.Timeout,
		"trustBundleFile", cfg.TrustBundleFile,
		"clusterDomain", cfg.ClusterDomain,
		"tlsPort", cfg.TLSPort,
	)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		if err := v.RunOnce(ctx, cfg); err != nil {
			// A failed round is not fatal: the next tick retries, and
			// the store keeps the previous outcomes rather than
			// pretending everything broke at once.
			logger.Error(err, "TLS verification round failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce performs a single probe round: load the trust bundle, list
// every RemoteApp, probe the Ready ones, and publish the outcomes.
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

	type target struct {
		key      types.NamespacedName
		ready    bool
		fqdn     string
		addr     string
		previous Verification
		hadPrev  bool
	}

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
		prev, had := v.Store.Result(key)
		targets = append(targets, target{
			key:      key,
			ready:    cr.Status.Ready,
			fqdn:     fqdn,
			addr:     net.JoinHostPort(fqdn, itoa(cfg.TLSPort)),
			previous: prev,
			hadPrev:  had,
		})
	}

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
					results[i] = Verification{Result: ResultNotReady, ServerName: t.fqdn}
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				results[i] = probe(probeCtx, t.addr, t.fqdn, roots)
				cancel()
			}
		})
	}
	for i := range targets {
		work <- i
	}
	close(work)
	wg.Wait()

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
				"serverName", t.fqdn,
				"result", string(results[i].Result),
				"detail", results[i].Detail,
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

// enqueue nudges the reconciler to refresh one RemoteApp's condition.
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

// probeTLS dials addr, completes a TLS handshake with ServerName pinned
// to serverName, and verifies the served chain against roots.
//
// It is the caller's ServerName that matters here. ghostunnel will serve
// its SVID to anyone; the question is whether that SVID is valid *for
// the name the Service is reached by*, which is the exact check curl
// does in hack/smoke/consumer/tls-probe.yaml and the exact check that
// the wrong-SAN incident failed.
//
// No application bytes are written: a handshake is all the certificate
// claim needs, and staying silent avoids opening a tunnel connection
// through to the Teleport-side app on every round.
func probeTLS(ctx context.Context, addr, serverName string, roots *x509.CertPool) Verification {
	v := Verification{ServerName: serverName}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		v.Result = ResultUnreachable
		v.Detail = fmt.Sprintf("cannot connect to %s: %s", addr, err)
		return v
	}
	defer func() { _ = conn.Close() }()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		v.Result = ResultCertInvalid
		v.Detail = describeHandshakeError(serverName, err)
		return v
	}
	v.Result = ResultVerified
	return v
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
