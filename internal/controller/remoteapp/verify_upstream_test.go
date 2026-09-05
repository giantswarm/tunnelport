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
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The HTTP half of the probe (giantswarm/tunnelport#110). The listener
// below stands in for the whole path behind ghostunnel — tbot, the
// Teleport proxy, the app service, the app — and answers whatever the
// test tells it to. What the probe must get right is the classification:
// a 504 from the Teleport app service is the incident, a 401 from an
// OAuth resource server is a working tunnel, and a request that never
// gets an answer is the incident with tbot's dial hanging instead of
// failing.

// serveHTTPS starts a TLS listener serving handler and returns its
// address. Unlike serveTLS it speaks HTTP after the handshake.
func serveHTTPS(t *testing.T, cert tls.Certificate, handler http.Handler) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// statusHandler answers every request with code and records what it saw.
type statusHandler struct {
	code int
	mu   sync.Mutex
	last *http.Request
}

func (h *statusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.last = r.Clone(context.Background())
	h.mu.Unlock()
	w.WriteHeader(h.code)
	_, _ = w.Write([]byte("body the probe must not care about"))
}

func (h *statusHandler) request() *http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// verifiedTarget returns a CA, a leaf valid for testFQDN and a probeTarget
// pointing at addr with the upstream probe on.
func verifiedTarget(t *testing.T, path string, timeout time.Duration) (*testCA, tls.Certificate, func(addr string) probeTarget) {
	t.Helper()
	ca := newTestCA(t)
	leaf := ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), testFQDN)
	return ca, leaf, func(addr string) probeTarget {
		return probeTarget{addr: addr, serverName: testFQDN, path: path, upstreamTimeout: timeout}
	}
}

// TestProbeTunnel_ClassifiesUpstreamStatus pins the split the issue asks
// for. The gateway statuses are what the Teleport proxy and app service
// return when *they* cannot reach the next hop; every other status — an
// app's own 500 included — proves the path through Teleport answers.
func TestProbeTunnel_ClassifiesUpstreamStatus(t *testing.T) {
	ca, leaf, target := verifiedTarget(t, "/", time.Second)

	for _, tc := range []struct {
		code int
		want UpstreamResult
	}{
		{code: http.StatusOK, want: UpstreamReachable},
		// The mcp-oauth resource servers behind gazelle's tunnels answer
		// an unauthenticated GET with 401. That is a working tunnel.
		{code: http.StatusUnauthorized, want: UpstreamReachable},
		{code: http.StatusNotFound, want: UpstreamReachable},
		{code: http.StatusInternalServerError, want: UpstreamReachable},
		{code: http.StatusBadGateway, want: UpstreamUnreachable},
		{code: http.StatusServiceUnavailable, want: UpstreamUnreachable},
		// The incident.
		{code: http.StatusGatewayTimeout, want: UpstreamUnreachable},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			h := &statusHandler{code: tc.code}
			addr := serveHTTPS(t, leaf, h)

			got := probeTunnel(context.Background(), target(addr), ca.pool)
			if got.Result != ResultVerified {
				t.Fatalf("TLS Result = %q (detail %q), want verified", got.Result, got.Detail)
			}
			if got.Upstream.Result != tc.want {
				t.Errorf("Upstream.Result = %q, want %q", got.Upstream.Result, tc.want)
			}
			if got.Upstream.StatusCode != tc.code {
				t.Errorf("Upstream.StatusCode = %d, want %d", got.Upstream.StatusCode, tc.code)
			}
			if want := "https://" + addr + "/"; got.Upstream.URL != want {
				t.Errorf("Upstream.URL = %q, want %q", got.Upstream.URL, want)
			}
			if got.Upstream.Detail != "" {
				t.Errorf("Upstream.Detail = %q, want empty when a status was read", got.Upstream.Detail)
			}
		})
	}
}

// TestProbeTunnel_RequestShape pins what the far end sees: a GET on the
// configured path, an identifiable User-Agent so the probe can be told
// apart from callers in the app's access log, and Connection: close so
// nothing lingers.
func TestProbeTunnel_RequestShape(t *testing.T) {
	ca, leaf, target := verifiedTarget(t, "/healthz", time.Second)
	h := &statusHandler{code: http.StatusOK}
	addr := serveHTTPS(t, leaf, h)

	got := probeTunnel(context.Background(), target(addr), ca.pool)
	if got.Upstream.Result != UpstreamReachable {
		t.Fatalf("Upstream = %+v, want reachable", got.Upstream)
	}
	req := h.request()
	if req == nil {
		t.Fatal("upstream saw no request")
	}
	if req.Method != http.MethodGet || req.URL.Path != "/healthz" {
		t.Errorf("request = %s %s, want GET /healthz", req.Method, req.URL.Path)
	}
	if ua := req.Header.Get("User-Agent"); ua != probeUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, probeUserAgent)
	}
	if !req.Close {
		t.Error("request did not ask for Connection: close")
	}
	if want := "https://" + addr + "/healthz"; got.Upstream.URL != want {
		t.Errorf("Upstream.URL = %q, want %q", got.Upstream.URL, want)
	}
}

// TestProbeTunnel_NoResponseWithinBudgetIsUnreachable is the incident's
// other face: tbot accepts the connection and holds it while its own dial
// to the proxy hangs. Nothing comes back, and that has to be classified —
// with a message that says "nothing came back", not a stack of net errors.
func TestProbeTunnel_NoResponseWithinBudgetIsUnreachable(t *testing.T) {
	ca, leaf, target := verifiedTarget(t, "/", 300*time.Millisecond)
	addr := serveHTTPS(t, leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))

	got := probeTunnel(context.Background(), target(addr), ca.pool)
	if got.Result != ResultVerified {
		t.Fatalf("TLS Result = %q, want verified", got.Result)
	}
	if got.Upstream.Result != UpstreamUnreachable {
		t.Fatalf("Upstream.Result = %q, want unreachable", got.Upstream.Result)
	}
	if got.Upstream.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 with no response", got.Upstream.StatusCode)
	}
	if !strings.Contains(got.Upstream.Detail, "within 300ms") || !strings.Contains(got.Upstream.Detail, addr) {
		t.Errorf("Detail = %q, want the budget and the URL named", got.Upstream.Detail)
	}
}

// TestProbeTunnel_ConnectionClosedIsUnreachable covers tbot closing the
// client connection when its dial to the proxy fails outright: the probe
// sees EOF instead of a status line.
func TestProbeTunnel_ConnectionClosedIsUnreachable(t *testing.T) {
	ca, leaf, target := verifiedTarget(t, "/", time.Second)
	// serveTLS completes the handshake and drops the connection — exactly
	// the shape of a tunnel whose far end refused.
	addr := serveTLS(t, leaf)

	got := probeTunnel(context.Background(), target(addr), ca.pool)
	if got.Result != ResultVerified {
		t.Fatalf("TLS Result = %q, want verified", got.Result)
	}
	if got.Upstream.Result != UpstreamUnreachable {
		t.Fatalf("Upstream.Result = %q, want unreachable", got.Upstream.Result)
	}
	if !strings.Contains(got.Upstream.Detail, "no HTTP response from") {
		t.Errorf("Detail = %q, want the no-response wording", got.Upstream.Detail)
	}
}

// TestDescribeUpstreamError pins that the no-response wording is stable
// across rounds. A *net.OpError carries the probe's ephemeral source port;
// left in the message it would change the outcome every round, re-enqueue
// the RemoteApp every round, and rewrite its status every round.
func TestDescribeUpstreamError(t *testing.T) {
	const url = "https://app.ns.svc.cluster.local:8443/"
	reset := &net.OpError{
		Op: "read", Net: "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 43210},
		Addr:   &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 8443},
		Err:    errors.New("connection reset by peer"),
	}
	got := describeUpstreamError(url, 10*time.Second, reset)
	if strings.Contains(got, "43210") || strings.Contains(got, "10.0.0.1") {
		t.Errorf("message leaks the ephemeral source address: %q", got)
	}
	if want := "no HTTP response from " + url + ": connection reset by peer"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}

	timeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if got := describeUpstreamError(url, 10*time.Second, timeout); got != "no HTTP response from "+url+" within 10s" {
		t.Errorf("timeout message = %q", got)
	}
	if got := describeUpstreamError(url, 10*time.Second, io.EOF); got != "no HTTP response from "+url+": EOF" {
		t.Errorf("EOF message = %q", got)
	}
}

// TestProbeTunnel_NoPathSendsNothing pins the off switch at the probe
// level: without a path the probe is exactly the pre-#110 handshake and
// writes no application bytes at all.
func TestProbeTunnel_NoPathSendsNothing(t *testing.T) {
	ca, leaf, _ := verifiedTarget(t, "", time.Second)
	h := &statusHandler{code: http.StatusOK}
	addr := serveHTTPS(t, leaf, h)

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, ca.pool)
	if got.Result != ResultVerified {
		t.Fatalf("TLS Result = %q, want verified", got.Result)
	}
	if got.Upstream.Result != UpstreamNotProbed {
		t.Errorf("Upstream.Result = %q, want not_probed", got.Upstream.Result)
	}
	// Give a stray request a moment to land, then insist none did.
	time.Sleep(50 * time.Millisecond)
	if h.request() != nil {
		t.Error("a request reached the upstream although no path was configured")
	}
}

// TestProbeTunnel_CertInvalidSkipsUpstream pins the honest-unknown rule
// for the HTTP half: with no verified session there is nothing to send
// the request over, and the upstream verdict must be "not probed", never
// "unreachable" — a wrong-SAN tunnel is a certificate problem, and saying
// its upstream is down would send the responder to the wrong layer.
func TestProbeTunnel_CertInvalidSkipsUpstream(t *testing.T) {
	ca := newTestCA(t)
	wrong := ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), testWrongFQDN)
	h := &statusHandler{code: http.StatusOK}
	addr := serveHTTPS(t, wrong, h)

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN, path: "/", upstreamTimeout: time.Second}, ca.pool)
	if got.Result != ResultCertInvalid {
		t.Fatalf("TLS Result = %q, want cert_invalid", got.Result)
	}
	if got.Upstream.Result != UpstreamNotProbed {
		t.Errorf("Upstream.Result = %q, want not_probed", got.Upstream.Result)
	}
}

// --- RunOnce with the upstream probe on -----------------------------------

// newUpstreamRoundVerifier is newRoundVerifier with the HTTP half on and a
// prober that records the targets it was handed and answers from byName.
func newUpstreamRoundVerifier(t *testing.T, byName map[string]Verification, objs ...client.Object) (*TLSVerifier, *VerificationStore, *[]probeTarget) {
	t.Helper()
	store := NewVerificationStore(true, true)
	var (
		mu   sync.Mutex
		seen []probeTarget
	)
	v := &TLSVerifier{
		Client: fake.NewClientBuilder().WithScheme(verifyTestScheme(t)).WithObjects(objs...).Build(),
		Config: VerifyConfig{
			Enabled:         true,
			UpstreamProbe:   true,
			UpstreamTimeout: 2 * time.Second,
			TrustBundleFile: writeBundle(t),
			Interval:        time.Minute,
			Timeout:         time.Second,
			Concurrency:     4,
		},
		Store:  store,
		Events: make(chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp], 16),
		probe: func(_ context.Context, t probeTarget, _ *x509.CertPool) Verification {
			mu.Lock()
			seen = append(seen, t)
			mu.Unlock()
			if v, ok := byName[t.serverName]; ok {
				return v
			}
			return Verification{
				Result:     ResultVerified,
				ServerName: t.serverName,
				Upstream:   UpstreamProbe{Result: UpstreamReachable, StatusCode: 200, URL: "https://" + t.addr + t.path},
			}
		},
		jitter: func(time.Duration) time.Duration { return 0 },
	}
	return v, store, &seen
}

// TestRunOnce_GatesOnPodReadinessNotStatusReady pins the gate change that
// folding the verdict into Ready forces. If the round still read
// status.ready, a failing upstream would flip Ready to False, the next
// round would skip the tunnel as "not ready", the condition would go
// Unknown, Ready would flip back, and the tunnel would oscillate forever
// without ever being re-probed for recovery.
func TestRunOnce_GatesOnPodReadinessNotStatusReady(t *testing.T) {
	// status.ready says false (the probe took it down), but the pod is
	// Ready: must be probed.
	probeMe := remoteApp("smoke", "upstream-down", false)
	// status.ready says true (stale), but no pod is Ready: must not be.
	skipMe := remoteApp("smoke", "pods-gone", true)

	v, store, seen := newUpstreamRoundVerifier(t, nil,
		probeMe, tunnelPodFor("smoke", "upstream-down", "upstream-down-pod", true),
		skipMe, tunnelPodFor("smoke", "pods-gone", "pods-gone-pod", false),
	)
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(*seen) != 1 || (*seen)[0].serverName != serviceFQDN("smoke", "upstream-down", DefaultClusterDomain) {
		t.Errorf("probed %+v, want only the RemoteApp whose pod is Ready", *seen)
	}
	got, ok := store.Result(types.NamespacedName{Namespace: "smoke", Name: "pods-gone"})
	if !ok || got.Result != ResultNotReady || got.Upstream.Result != UpstreamNotProbed {
		t.Errorf("pods-gone result = %+v (present=%v), want not_ready / not_probed", got, ok)
	}
}

// TestRunOnce_UpstreamPathFollowsSpec pins the per-RemoteApp knob and its
// default, and that turning the HTTP half off hands the prober no path at
// all — which is what makes probeTunnel skip the request.
func TestRunOnce_UpstreamPathFollowsSpec(t *testing.T) {
	plain := remoteApp("smoke", "plain", true)
	custom := remoteApp("smoke", "custom", true)
	custom.Spec.Probe = &accessv1alpha1.ProbeSpec{Path: "/healthz"}

	v, _, seen := newUpstreamRoundVerifier(t, nil,
		plain, tunnelPodFor("smoke", "plain", "plain-pod", true),
		custom, tunnelPodFor("smoke", "custom", "custom-pod", true),
	)
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	paths := map[string]string{}
	for _, tgt := range *seen {
		paths[tgt.serverName] = tgt.path
		if tgt.upstreamTimeout != 2*time.Second {
			t.Errorf("%s: upstreamTimeout = %v, want the configured 2s", tgt.serverName, tgt.upstreamTimeout)
		}
	}
	if got := paths[serviceFQDN("smoke", "plain", DefaultClusterDomain)]; got != DefaultProbePath {
		t.Errorf("default path = %q, want %q", got, DefaultProbePath)
	}
	if got := paths[serviceFQDN("smoke", "custom", DefaultClusterDomain)]; got != "/healthz" {
		t.Errorf("spec.probe.path = %q, want /healthz", got)
	}

	// Off switch.
	*seen = nil
	v.Config.UpstreamProbe = false
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce with upstream off: %v", err)
	}
	for _, tgt := range *seen {
		if tgt.path != "" {
			t.Errorf("%s: path = %q with the upstream probe off, want none", tgt.serverName, tgt.path)
		}
	}
}

// TestRunOnce_UpstreamChangeIsEnqueued pins that an upstream verdict
// change alone — same certificate, same handshake — nudges the
// reconciler, and that a healthy round does not. Folding into Ready is
// worthless if the condition only refreshes when something unrelated
// triggers a reconcile.
func TestRunOnce_UpstreamChangeIsEnqueued(t *testing.T) {
	cr := remoteApp("agent-platform", "mcp-capi", true)
	fqdn := serviceFQDN("agent-platform", "mcp-capi", DefaultClusterDomain)
	answers := map[string]Verification{}
	v, store, _ := newUpstreamRoundVerifier(t, answers, cr, tunnelPodFor("agent-platform", "mcp-capi", "mcp-capi-pod", true))
	key := types.NamespacedName{Namespace: "agent-platform", Name: "mcp-capi"}

	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if got := drain(v.Events); len(got) != 1 {
		t.Fatalf("first round enqueued %v, want the new RemoteApp", got)
	}
	if _, ok := store.LastUpstreamSuccess(key); !ok {
		t.Fatal("a reachable upstream did not record a last success")
	}

	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got := drain(v.Events); len(got) != 0 {
		t.Fatalf("healthy second round enqueued %v, want nothing", got)
	}

	// The incident: handshake fine, 504 behind it.
	answers[fqdn] = Verification{
		Result:     ResultVerified,
		ServerName: fqdn,
		Upstream:   UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: "https://" + fqdn + ":8443/"},
	}
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if got := drain(v.Events); len(got) != 1 || got[0] != "agent-platform/mcp-capi" {
		t.Fatalf("third round enqueued %v, want only the RemoteApp whose upstream broke", got)
	}
	got, _ := store.Result(key)
	if got.Upstream.Result != UpstreamUnreachable || got.Upstream.StatusCode != 504 {
		t.Errorf("stored upstream = %+v, want the 504", got.Upstream)
	}
	if _, ok := store.LastUpstreamSuccess(key); !ok {
		t.Error("the last success was forgotten as soon as the upstream failed; the message needs it")
	}
}

// TestRunOnce_JitterSpreadsReadyTargets pins that the spread is applied
// to Ready targets only and honoured before the probe runs: the prober
// must not see a target before its scheduled offset.
func TestRunOnce_JitterSpreadsReadyTargets(t *testing.T) {
	ready := remoteApp("smoke", "spread", true)
	notReady := remoteApp("smoke", "starting", false)
	v, _, _ := newUpstreamRoundVerifier(t, nil,
		ready, tunnelPodFor("smoke", "spread", "spread-pod", true),
		notReady, tunnelPodFor("smoke", "starting", "starting-pod", false),
	)
	const offset = 150 * time.Millisecond
	var asked []time.Duration
	v.jitter = func(max time.Duration) time.Duration {
		asked = append(asked, max)
		return offset
	}
	v.Config.Jitter = 20 * time.Second

	var probedAt time.Time
	inner := v.probe
	v.probe = func(ctx context.Context, tgt probeTarget, roots *x509.CertPool) Verification {
		probedAt = time.Now()
		return inner(ctx, tgt, roots)
	}

	start := time.Now()
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(asked) != 1 || asked[0] != 20*time.Second {
		t.Errorf("jitter drawn %v times with max %v; want once, for the Ready target only, with the configured window", len(asked), asked)
	}
	if probedAt.Sub(start) < offset {
		t.Errorf("probe ran %v after the round started, before its %v offset", probedAt.Sub(start), offset)
	}
}

// TestSleepUntil_ReturnsOnCancel keeps a shutdown from waiting out the
// jitter window.
func TestSleepUntil_ReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepUntil(ctx, time.Now().Add(5*time.Second))
	if time.Since(start) > time.Second {
		t.Errorf("sleepUntil ignored the cancelled context for %v", time.Since(start))
	}
}

// TestGroupTunnelPods pins the grouping key: the pod's namespace plus the
// RemoteApp label, so two RemoteApps of the same name in different
// namespaces never share a readiness verdict.
func TestGroupTunnelPods(t *testing.T) {
	pods := []client.Object{
		tunnelPodFor("a", "app", "a-1", true),
		tunnelPodFor("b", "app", "b-1", false),
		tunnelPodFor("a", "other", "a-2", true),
	}
	list := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		list = append(list, *p.(*corev1.Pod))
	}
	got := groupTunnelPods(list)
	if len(got[types.NamespacedName{Namespace: "a", Name: "app"}]) != 1 ||
		len(got[types.NamespacedName{Namespace: "b", Name: "app"}]) != 1 ||
		len(got[types.NamespacedName{Namespace: "a", Name: "other"}]) != 1 {
		t.Errorf("groupTunnelPods = %v, want one pod under each of a/app, b/app, a/other", got)
	}
	unlabelled := tunnelPodFor("a", "app", "a-3", true)
	delete(unlabelled.Labels, LabelRemoteAppInstance)
	if got := groupTunnelPods([]corev1.Pod{*unlabelled}); len(got) != 0 {
		t.Errorf("a pod without the RemoteApp label was grouped: %v", got)
	}
}

// listener sanity: make sure the fixtures above really speak TLS on a
// loopback address the probe can dial, so a failure elsewhere is not
// misread as a network problem.
func TestServeHTTPS_Fixture(t *testing.T) {
	ca, leaf, _ := verifiedTarget(t, "/", time.Second)
	addr := serveHTTPS(t, leaf, &statusHandler{code: http.StatusTeapot})
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: testFQDN, RootCAs: ca.pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("fixture does not accept a verified TLS connection: %v", err)
	}
	_ = conn.Close()
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("fixture address %q is not host:port: %v", addr, err)
	}
}
