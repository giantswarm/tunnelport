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

package main

import (
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
	remoteappctrl "github.com/giantswarm/tunnelport/internal/controller/remoteapp"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(accessv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// flags carries every command-line flag this binary parses. Grouping them
// in a struct lets parseFlags() stay a pure function and keeps main()
// from having to thread a dozen scalars through helper calls.
type flags struct {
	metricsAddr          string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
	webhookCertPath      string
	webhookCertName      string
	webhookCertKey       string
	probeAddr            string
	enableLeaderElection bool
	secureMetrics        bool
	enableHTTP2          bool

	tbotImage      string
	tbotCPURequest string
	tbotMemRequest string
	tbotCPULimit   string
	tbotMemLimit   string
	tbotInsecure   bool

	zapOpts zap.Options
}

// parseFlags binds every CLI flag onto a flags value and calls flag.Parse.
// Splitting this out of main() keeps the manager-wiring block readable and
// makes the flag surface introspectable from a future test if ever needed.
func parseFlags() flags {
	var f flags

	flag.StringVar(&f.tbotImage, "tbot-image", "public.ecr.aws/gravitational/teleport-distroless:16",
		"Container image for the tbot sidecar. The same image is used for every rendered tbot Deployment.")
	flag.StringVar(&f.tbotCPURequest, "tbot-cpu-request", "50m",
		"CPU request applied to the tbot container.")
	flag.StringVar(&f.tbotMemRequest, "tbot-memory-request", "64Mi",
		"Memory request applied to the tbot container.")
	flag.StringVar(&f.tbotCPULimit, "tbot-cpu-limit", "200m",
		"CPU limit applied to the tbot container.")
	flag.StringVar(&f.tbotMemLimit, "tbot-memory-limit", "256Mi",
		"Memory limit applied to the tbot container.")
	flag.BoolVar(&f.tbotInsecure, "tbot-insecure", false,
		"Render tbot configs with `insecure: true` so pods skip Teleport proxy "+
			"TLS verification. Development-only — never set in production. Useful for "+
			"kind-based smoke tests where the proxy is reached by IP and the cert SAN "+
			"does not match.")
	flag.StringVar(&f.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&f.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&f.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&f.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&f.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&f.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&f.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&f.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&f.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&f.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	f.zapOpts = zap.Options{Development: true}
	f.zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	return f
}

// leaderElectionNamespaceOrExit returns the POD_NAMESPACE env var or
// exits cleanly with a directional error. controller-runtime accepts
// an empty namespace and falls back to in-cluster-config detection,
// but that's fragile (the Lease silently lands in the wrong place
// if the in-cluster path resolves a stale namespace), so the chart
// is the single source of truth via the downward API.
func leaderElectionNamespaceOrExit() string {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		setupLog.Error(nil, "POD_NAMESPACE is empty",
			"hint", "the helm chart sets this via the downward API; "+
				"set POD_NAMESPACE explicitly when running the binary "+
				"outside the chart")
		os.Exit(1)
	}
	return ns
}

// parseQuantityOrExit parses a Kubernetes resource.Quantity from a flag
// value and exits cleanly on error. The kubebuilder default uses
// resource.MustParse, which panics with a stack trace on a typo — useless
// noise in an operator's startup logs. This variant logs once at error
// level and exits 1.
func parseQuantityOrExit(name, value string) resource.Quantity {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		setupLog.Error(err, "invalid quantity flag", "flag", name, "value", value)
		os.Exit(1)
	}
	return q
}

// buildReconcilerConfig translates flag values into the reconciler's
// PodDefaults struct. Quantity parsing is the failure-prone bit, so it
// lives here behind parseQuantityOrExit rather than inline in main().
func buildReconcilerConfig(f flags) remoteappctrl.PodDefaults {
	return remoteappctrl.PodDefaults{
		TbotImage: f.tbotImage,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    parseQuantityOrExit("tbot-cpu-request", f.tbotCPURequest),
				corev1.ResourceMemory: parseQuantityOrExit("tbot-memory-request", f.tbotMemRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    parseQuantityOrExit("tbot-cpu-limit", f.tbotCPULimit),
				corev1.ResourceMemory: parseQuantityOrExit("tbot-memory-limit", f.tbotMemLimit),
			},
		},
		Insecure: f.tbotInsecure,
	}
}

func main() {
	f := parseFlags()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&f.zapOpts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	var tlsOpts []func(*tls.Config)
	if !f.enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(f.webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", f.webhookCertPath, "webhook-cert-name", f.webhookCertName, "webhook-cert-key", f.webhookCertKey)

		webhookServerOptions.CertDir = f.webhookCertPath
		webhookServerOptions.CertName = f.webhookCertName
		webhookServerOptions.KeyName = f.webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   f.metricsAddr,
		SecureServing: f.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if f.secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(f.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", f.metricsCertPath, "metrics-cert-name", f.metricsCertName, "metrics-cert-key", f.metricsCertKey)

		metricsServerOptions.CertDir = f.metricsCertPath
		metricsServerOptions.CertName = f.metricsCertName
		metricsServerOptions.KeyName = f.metricsCertKey
	}

	// Cache scoping. The default controller-runtime cache subscribes to
	// every object of every watched GVK cluster-wide; for Pods that pulls
	// every Pod on the consumer MC into the operator's informer,
	// inflating RSS for objects status synthesis never consults. We pin a
	// label selector on the Pod informer so only tbot pods reach the
	// cache:
	//
	//   - Pods carrying `tunnelport.giantswarm.io/role=tbot` — the
	//     reconciler-stamped label on every tbot pod template
	//     (`renderDeployment`). These are the only pods status
	//     synthesis ever consults.
	//
	// ConfigMap / Service / Deployment / ServiceAccount caches stay
	// unscoped because `Owns(...)` already routes them via OwnerReferences.
	tbotPodLabel := labels.SelectorFromSet(map[string]string{
		"tunnelport.giantswarm.io/role": "tbot",
	})
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Label: tbotPodLabel},
		},
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: f.probeAddr,
		LeaderElection:         f.enableLeaderElection,
		LeaderElectionID:       "21e2b113.giantswarm.io",
		// Scope the leader-election Lease to the operator's own pod
		// namespace. POD_NAMESPACE is injected via the downward API
		// in helm/tunnelport/templates/deployment.yaml; out-of-chart
		// deployments must set the env var explicitly. Empty would
		// fall through to controller-runtime's in-cluster-config
		// detection — fragile, so we fail fast instead.
		LeaderElectionNamespace: leaderElectionNamespaceOrExit(),
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// The reconciler doesn't yet call Recorder.Eventf(...); wiring stays
	// here so a later bundle can emit Events without re-touching main.go.
	recorder := mgr.GetEventRecorder("remoteapp-controller")

	if err := (&remoteappctrl.Reconciler{
		Client:      mgr.GetClient(),
		PodDefaults: buildReconcilerConfig(f),
		Recorder:    recorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up RemoteApp controller")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
