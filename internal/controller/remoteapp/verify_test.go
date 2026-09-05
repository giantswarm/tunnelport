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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The FQDN under test throughout. It is the shape a caller actually
// dials and therefore the shape the Teleport-side workload_identity
// dns_sans has to carry — the namespace segment is the one that drifted
// in giantswarm/giantswarm#37521.
const (
	testFQDN      = "smoke-app.smoke.svc.cluster.local"
	testWrongFQDN = "smoke-app.agentic-platform.svc.cluster.local"
)

// --- test PKI helpers -------------------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tunnelport-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{
		cert: cert,
		key:  key,
		pool: pool,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue mints a leaf server certificate with the given DNS SANs and
// validity window, signed by this CA.
func (ca *testCA) issue(t *testing.T, notBefore, notAfter time.Time, dnsNames ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "tunnelport-test-leaf"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// serveTLS starts a TLS listener on loopback serving cert and returns its
// address. Stands in for the ghostunnel sidecar: it answers a handshake
// and nothing else, which is exactly what the probe asks of it.
func serveTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Complete the handshake, then drop it. The probe writes no
			// application bytes, so there is nothing to read.
			go func() {
				if tc, ok := conn.(*tls.Conn); ok {
					_ = tc.HandshakeContext(context.Background())
				}
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// --- probeTunnel, handshake half ----------------------------------------

// TestProbeTLS_VerifiesMatchingCertificate is the happy path: a leaf
// whose SANs cover the FQDN, signed by the trusted CA.
func TestProbeTLS_VerifiesMatchingCertificate(t *testing.T) {
	ca := newTestCA(t)
	addr := serveTLS(t, ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), testFQDN))

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, ca.pool)
	if got.Result != ResultVerified {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultVerified)
	}
	if got.Detail != "" {
		t.Errorf("Detail = %q, want empty on success", got.Detail)
	}
	if got.ServerName != testFQDN {
		t.Errorf("ServerName = %q, want %q", got.ServerName, testFQDN)
	}
}

// TestProbeTLS_SANMismatchIsCertInvalid reproduces the incident in
// miniature: a valid, trusted, unexpired SVID whose dns_sans name the
// wrong namespace. Everything about the tunnel is up; only the name is
// wrong. This is the case a TCPSocket probe passes.
func TestProbeTLS_SANMismatchIsCertInvalid(t *testing.T) {
	ca := newTestCA(t)
	// The certificate is issued for the *old* namespace, exactly as the
	// 40 stale workload_identity resources were.
	addr := serveTLS(t, ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), testWrongFQDN))

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, ca.pool)
	if got.Result != ResultCertInvalid {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultCertInvalid)
	}
	// The detail has to name both sides of the mismatch: without the
	// presented SANs a reader cannot tell a namespace rename from a
	// wholly wrong identity.
	if !strings.Contains(got.Detail, "SAN mismatch") {
		t.Errorf("Detail = %q, want it to mention a SAN mismatch", got.Detail)
	}
	if !strings.Contains(got.Detail, testFQDN) {
		t.Errorf("Detail = %q, want it to name the expected FQDN %q", got.Detail, testFQDN)
	}
	if !strings.Contains(got.Detail, testWrongFQDN) {
		t.Errorf("Detail = %q, want it to name the presented SAN %q", got.Detail, testWrongFQDN)
	}
}

// TestProbeTLS_UntrustedChainIsCertInvalid covers a certificate that is
// perfectly valid for the name but not rooted in the SPIFFE bundle — the
// shape of a tunnel accidentally fronted by something else.
func TestProbeTLS_UntrustedChainIsCertInvalid(t *testing.T) {
	serving, verifying := newTestCA(t), newTestCA(t)
	addr := serveTLS(t, serving.issue(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), testFQDN))

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, verifying.pool)
	if got.Result != ResultCertInvalid {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultCertInvalid)
	}
	if !strings.Contains(got.Detail, "does not chain to the SPIFFE trust bundle") {
		t.Errorf("Detail = %q, want it to mention the chain", got.Detail)
	}
}

// TestProbeTLS_ExpiredCertificateIsCertInvalid covers a stalled renewal:
// tbot stopped refreshing the SVID and ghostunnel is still serving the
// old one.
func TestProbeTLS_ExpiredCertificateIsCertInvalid(t *testing.T) {
	ca := newTestCA(t)
	addr := serveTLS(t, ca.issue(t,
		time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour), testFQDN))

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, ca.pool)
	if got.Result != ResultCertInvalid {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultCertInvalid)
	}
	if !strings.Contains(got.Detail, "expired") {
		t.Errorf("Detail = %q, want it to mention expiry", got.Detail)
	}
}

// TestProbeTLS_NonTLSListenerIsCertInvalid pins the deliberate lumping
// decision: a plain-TCP listener connects but yields no verified
// session, which is reported as cert_invalid rather than as a separate
// result, because the caller-visible fact is the same. The detail is
// what distinguishes it.
func TestProbeTLS_NonTLSListenerIsCertInvalid(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Answer with something that is definitively not a TLS
			// ServerHello, so the handshake fails on the record layer
			// rather than on verification.
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	got := probeTunnel(context.Background(), probeTarget{addr: ln.Addr().String(), serverName: testFQDN}, newTestCA(t).pool)
	if got.Result != ResultCertInvalid {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultCertInvalid)
	}
	if !strings.Contains(got.Detail, "TLS handshake") {
		t.Errorf("Detail = %q, want the generic handshake-failure wording", got.Detail)
	}
}

// TestProbeTLS_ClosedPortIsUnreachable is the other half of the
// distinction the issue asks for: nothing accepted a connection, which
// must never be reported as a certificate fault.
func TestProbeTLS_ClosedPortIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	// Close it so the port is definitely refusing connections.
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := probeTunnel(context.Background(), probeTarget{addr: addr, serverName: testFQDN}, newTestCA(t).pool)
	if got.Result != ResultUnreachable {
		t.Fatalf("Result = %q (detail %q), want %q", got.Result, got.Detail, ResultUnreachable)
	}
	if !strings.Contains(got.Detail, "cannot connect") {
		t.Errorf("Detail = %q, want it to say it could not connect", got.Detail)
	}
}

// --- trust bundle -----------------------------------------------------

// TestLoadTrustBundle covers every way the bundle can be unusable. All
// of them must be errors so the caller flips
// tunnelport_tls_verification_available to 0 rather than reporting
// verdicts it cannot support.
func TestLoadTrustBundle(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)

	valid := filepath.Join(dir, "valid.pem")
	if err := os.WriteFile(valid, ca.pem, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "valid bundle", path: valid},
		{name: "unconfigured", path: "", wantErr: "no trust bundle configured"},
		{name: "missing file", path: filepath.Join(dir, "absent.pem"), wantErr: "no such file"},
		// The chart pre-creates the Secret with empty Data and tbot fills
		// it in after it joins Teleport, so this is the normal state for
		// the first minute of a fresh install.
		{name: "empty file", path: empty, wantErr: "trust bundle is empty"},
		{name: "unparseable", path: garbage, wantErr: "no parseable certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := loadTrustBundle(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("loadTrustBundle: %v", err)
				}
				if pool == nil {
					t.Fatal("pool is nil on success")
				}
				return
			}
			if err == nil {
				t.Fatalf("loadTrustBundle(%q) = nil error, want %q", tc.path, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// --- FQDN construction ------------------------------------------------

// TestServiceFQDN pins the name the probe verifies against. It has to be
// the name a caller dials, because that is what the SVID's dns_sans is
// compared to; a different shape here would verify something nobody uses.
func TestServiceFQDN(t *testing.T) {
	if got := serviceFQDN("smoke", "smoke-app", "cluster.local"); got != testFQDN {
		t.Errorf("serviceFQDN = %q, want %q", got, testFQDN)
	}
	if got := serviceFQDN("agent-platform", "mcp-kubernetes", "k8s.example"); got != "mcp-kubernetes.agent-platform.svc.k8s.example" {
		t.Errorf("serviceFQDN with custom domain = %q", got)
	}
}

// TestVerifyConfigDefaults pins that a zero-valued config still produces
// a working prober rather than a ticker with a zero interval (which
// panics) or a zero-timeout dial (which fails everything).
func TestVerifyConfigDefaults(t *testing.T) {
	got := VerifyConfig{Enabled: true}.withDefaults()
	if got.Interval != DefaultVerifyInterval {
		t.Errorf("Interval = %v, want %v", got.Interval, DefaultVerifyInterval)
	}
	if got.Timeout != DefaultVerifyTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultVerifyTimeout)
	}
	if got.Concurrency != DefaultVerifyConcurrency {
		t.Errorf("Concurrency = %d, want %d", got.Concurrency, DefaultVerifyConcurrency)
	}
	if got.ClusterDomain != DefaultClusterDomain {
		t.Errorf("ClusterDomain = %q, want %q", got.ClusterDomain, DefaultClusterDomain)
	}
	if got.TLSPort != tlsListenPortDefault {
		t.Errorf("TLSPort = %d, want %d", got.TLSPort, tlsListenPortDefault)
	}
	if got.Jitter != DefaultVerifyJitter {
		t.Errorf("Jitter = %v, want %v", got.Jitter, DefaultVerifyJitter)
	}
	if got.UpstreamTimeout != DefaultUpstreamProbeTimeout {
		t.Errorf("UpstreamTimeout = %v, want %v", got.UpstreamTimeout, DefaultUpstreamProbeTimeout)
	}
	// The spread must leave the other half of the interval for the probes
	// themselves, whatever interval an install picks: the smoke runs at
	// 15s and must not end up with a 30s jitter.
	if short := (VerifyConfig{Enabled: true, Interval: 16 * time.Second}).withDefaults(); short.Jitter != 8*time.Second {
		t.Errorf("Jitter at a 16s interval = %v, want the interval/2 clamp (8s)", short.Jitter)
	}
	if explicit := (VerifyConfig{Enabled: true, Jitter: 5 * time.Second}).withDefaults(); explicit.Jitter != 5*time.Second {
		t.Errorf("explicit Jitter = %v, want it kept", explicit.Jitter)
	}
	// A round must fit inside the cadence, or rounds pile up: the
	// defaults have to satisfy ceil(N/concurrency)*timeout < interval for
	// a realistic N. At 8-way concurrency and a 5s timeout, 2m covers
	// ~192 batches, i.e. well over a thousand RemoteApps.
	batchBudget := got.Interval / got.Timeout
	if batchBudget < 20 {
		t.Errorf("defaults allow only %d probe batches per round; too tight", batchBudget)
	}
}

// --- RunOnce ----------------------------------------------------------

// remoteApp builds a RemoteApp fixture for the round tests. `ready` is
// recorded on status for the fixture's own bookkeeping; newRoundVerifier
// turns it into a Ready (or not) tunnel pod, because that — not
// status.ready — is what the round's gate reads.
func remoteApp(namespace, name string, ready bool) *accessv1alpha1.RemoteApp {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:   name,
			Port:      8080,
			TokenName: name + "-token",
		},
	}
	cr.Status.Ready = ready
	return cr
}

// newRoundVerifier wires a verifier over a fake client holding crs and one
// tunnel pod per CR (Ready iff the fixture says so), with a stub prober
// returning byName[serverName] (defaulting to verified). The upstream
// probe is off here: these rounds are about the handshake half and the
// machinery around it; verify_upstream_test.go turns it on.
func newRoundVerifier(t *testing.T, bundlePath string, byName map[string]Verification, crs ...*accessv1alpha1.RemoteApp) (*TLSVerifier, *VerificationStore) {
	t.Helper()
	store := NewVerificationStore(true, false)
	objs := make([]client.Object, 0, 2*len(crs))
	for _, cr := range crs {
		objs = append(objs, cr, tunnelPodFor(cr.Namespace, cr.Name, cr.Name+"-pod", cr.Status.Ready))
	}
	v := &TLSVerifier{
		Client: fake.NewClientBuilder().
			WithScheme(verifyTestScheme(t)).
			WithObjects(objs...).
			Build(),
		Config: VerifyConfig{
			Enabled:         true,
			TrustBundleFile: bundlePath,
			Interval:        time.Minute,
			Timeout:         time.Second,
			Concurrency:     4,
		},
		Store:  store,
		Events: make(chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp], 16),
		probe: func(_ context.Context, t probeTarget, _ *x509.CertPool) Verification {
			if v, ok := byName[t.serverName]; ok {
				return v
			}
			return Verification{Result: ResultVerified, ServerName: t.serverName, Upstream: UpstreamProbe{Result: UpstreamNotProbed}}
		},
		// Rounds must be instant in tests; the spread is covered by
		// TestVerifyConfigDefaults and TestRunOnce_JitterSpreadsReadyTargets.
		jitter: func(time.Duration) time.Duration { return 0 },
	}
	return v, store
}

func verifyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := accessv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// tunnelPodFor builds the pod fixture the Ready gate reads: labelled the way
// renderDeployment labels a tunnel pod, with PodReady set as asked.
func tunnelPodFor(namespace, remoteApp, name string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				LabelRole:              LabelRoleValue,
				LabelRemoteAppInstance: remoteApp,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	} else {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	}
	return pod
}

// writeBundle drops a valid CA bundle on disk and returns its path.
func writeBundle(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "svid_bundle.pem")
	if err := os.WriteFile(path, newTestCA(t).pem, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

// TestRunOnce_SkipsNotReadyAndDeleting pins the Ready gate. The premise
// of the whole check is "the tunnel claims to be healthy — is it?", so a
// tunnel that makes no such claim must not be probed (it would report
// unreachable for pods that are merely starting), and one mid-deletion
// must not be probed at all.
func TestRunOnce_SkipsNotReadyAndDeleting(t *testing.T) {
	ready := remoteApp("smoke", "ready-app", true)
	notReady := remoteApp("smoke", "starting-app", false)
	deleting := remoteApp("smoke", "going-away", true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test/keep-in-fake-client"}

	var probed []string
	v, store := newRoundVerifier(t, writeBundle(t), nil, ready, notReady, deleting)
	inner := v.probe
	v.probe = func(ctx context.Context, t probeTarget, roots *x509.CertPool) Verification {
		probed = append(probed, t.serverName)
		return inner(ctx, t, roots)
	}

	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(probed) != 1 || probed[0] != serviceFQDN("smoke", "ready-app", DefaultClusterDomain) {
		t.Errorf("probed = %v, want only the Ready RemoteApp", probed)
	}

	got, ok := store.Result(types.NamespacedName{Namespace: "smoke", Name: "starting-app"})
	if !ok || got.Result != ResultNotReady {
		t.Errorf("not-ready RemoteApp result = %+v (present=%v), want %q", got, ok, ResultNotReady)
	}
	if _, ok := store.Result(types.NamespacedName{Namespace: "smoke", Name: "going-away"}); ok {
		t.Error("deleting RemoteApp still has a result; it must stop reporting")
	}
}

// TestRunOnce_EnqueuesOnlyChanges pins the reconcile-nudge contract:
// every RemoteApp is new on the first round, and a second round with
// unchanged outcomes must be silent. Without that, the channel would
// re-enqueue the whole fleet on every tick and the reconciler would
// re-patch status forever.
func TestRunOnce_EnqueuesOnlyChanges(t *testing.T) {
	bundle := writeBundle(t)
	results := map[string]Verification{}
	// Two namespaces on purpose: the verifier lists cluster-wide, and on a
	// real consumer MC every RemoteApp sits in its own namespace.
	v, _ := newRoundVerifier(t, bundle, results,
		remoteApp("smoke", "app-a", true),
		remoteApp("agent-platform", "app-b", true),
	)

	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if got := drain(v.Events); len(got) != 2 {
		t.Fatalf("first round enqueued %v, want both RemoteApps", got)
	}

	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got := drain(v.Events); len(got) != 0 {
		t.Fatalf("second round enqueued %v, want nothing on unchanged outcomes", got)
	}

	// Now break one tunnel: exactly that one must be enqueued.
	broken := serviceFQDN("agent-platform", "app-b", DefaultClusterDomain)
	results[broken] = Verification{
		Result:     ResultCertInvalid,
		Detail:     "served certificate is not valid for " + broken + " (SAN mismatch)",
		ServerName: broken,
	}
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	got := drain(v.Events)
	if len(got) != 1 || got[0] != "agent-platform/app-b" {
		t.Fatalf("third round enqueued %v, want only agent-platform/app-b", got)
	}
}

// TestRunOnce_MissingBundleClearsResults pins the honest-unknown rule at
// the round level: with no bundle the operator drops every verdict rather
// than keeping stale ones, and reports the loss of the ability to judge.
func TestRunOnce_MissingBundleClearsResults(t *testing.T) {
	bundleDir := t.TempDir()
	bundle := filepath.Join(bundleDir, "svid_bundle.pem")
	if err := os.WriteFile(bundle, newTestCA(t).pem, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	v, store := newRoundVerifier(t, bundle, nil, remoteApp("smoke", "app-a", true))
	if err := v.RunOnce(context.Background(), v.Config); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := store.Result(types.NamespacedName{Namespace: "smoke", Name: "app-a"}); !ok {
		t.Fatal("no result after a successful round")
	}

	if err := os.Remove(bundle); err != nil {
		t.Fatalf("remove bundle: %v", err)
	}
	if err := v.RunOnce(context.Background(), v.Config); err == nil {
		t.Fatal("RunOnce with a missing bundle returned nil error")
	}
	if _, ok := store.Result(types.NamespacedName{Namespace: "smoke", Name: "app-a"}); ok {
		t.Error("result survived the loss of the trust bundle; the operator would " +
			"keep asserting a verdict it can no longer make")
	}
}

// drain empties the event channel and returns "namespace/name" strings.
func drain(ch chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp]) []string {
	var out []string
	for {
		select {
		case ev := <-ch:
			out = append(out, ev.Object.Namespace+"/"+ev.Object.Name)
		default:
			return out
		}
	}
}
