/*
Copyright 2026.

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
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	maintenancev1 "linode.com/node-maintenance-controller/api/v1"
	"linode.com/node-maintenance-controller/internal/controller"
	"linode.com/node-maintenance-controller/internal/poller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(maintenancev1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	// --- Standard controller-runtime flags ---
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// --- Linode token Secret flags ---
	var linodeTokenSecretName string
	var linodeTokenSecretNamespace string
	var linodeTokenSecretKey string
	var linodeAPIEndpoint string
	flag.StringVar(&linodeTokenSecretName, "linode-token-secret-name", "linode-token",
		"Name of the Kubernetes Secret containing the Linode API token.")
	flag.StringVar(&linodeTokenSecretNamespace, "linode-token-secret-namespace", "",
		"Namespace of the Linode token Secret. Defaults to the value of the POD_NAMESPACE env var.")
	flag.StringVar(&linodeTokenSecretKey, "linode-token-secret-key", "token",
		"Key within the Linode token Secret that holds the token value.")
	flag.StringVar(&linodeAPIEndpoint, "linode-api-endpoint", "",
		"Linode API base URL. Defaults to the linodego default (https://api.linode.com).")

	// --- Poller configuration flags ---
	var pollInterval time.Duration
	var maintenanceWindow time.Duration
	var uncordonDelay time.Duration
	flag.DurationVar(&pollInterval, "poll-interval", 10*time.Minute,
		"How often the Linode maintenance API is polled (e.g. 10m, 5m).")
	flag.DurationVar(&maintenanceWindow, "maintenance-window", 30*time.Minute,
		"Look-ahead duration: nodes with maintenance scheduled within this window receive signals (e.g. 30m, 24h).")
	flag.DurationVar(&uncordonDelay, "post-maintenance-uncordon-delay", 5*time.Minute,
		"Duration to wait after maintenance completes before uncordoning the node (e.g. 5m). 0 means immediate.")

	// --- Cordon / drain flags ---
	var cordonNodes bool
	var drainNodes bool
	var drainTimeout time.Duration
	var drainMaxRetries int
	var drainIgnoreDaemonSets bool
	var drainDeleteEmptyDirData bool
	var drainRetryInterval time.Duration
	flag.BoolVar(&cordonNodes, "cordon-nodes", false,
		"If true, cordon nodes (spec.unschedulable=true) when maintenance signals are applied. "+
			"Does not drain. Independent of --drain-nodes.")
	flag.BoolVar(&drainNodes, "drain-nodes", false,
		"If true, drain nodes before maintenance. Implies --cordon-nodes.")
	flag.DurationVar(&drainTimeout, "drain-timeout", 5*time.Minute,
		"Per-attempt timeout for the kubectl drain operation (e.g. 5m, 10m).")
	flag.IntVar(&drainMaxRetries, "drain-max-retries", 5,
		"Maximum number of drain attempts before giving up. 0 means unlimited retries.")
	flag.BoolVar(&drainIgnoreDaemonSets, "drain-ignore-daemonsets", true,
		"If true, DaemonSet-owned pods are ignored during drain.")
	flag.BoolVar(&drainDeleteEmptyDirData, "drain-delete-emptydir-data", false,
		"If true, pods with emptyDir volumes are evicted during drain. "+
			"Defaults to false to avoid data loss.")
	flag.DurationVar(&drainRetryInterval, "drain-retry-interval", 1*time.Minute,
		"Duration to wait between failed drain attempts (e.g. 30s, 1m).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Resolve the Secret namespace: explicit flag > POD_NAMESPACE env var.
	if linodeTokenSecretNamespace == "" {
		linodeTokenSecretNamespace = os.Getenv("POD_NAMESPACE")
	}
	if linodeTokenSecretNamespace == "" {
		setupLog.Error(nil, "Linode token Secret namespace is not set; "+
			"use --linode-token-secret-namespace or ensure the POD_NAMESPACE env var is injected "+
			"(add a Downward API fieldRef for metadata.namespace to the manager Deployment)")
		os.Exit(1)
	}

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

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
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
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "433a1cc7.linode.com",
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

	kubeClientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create Kubernetes clientset")
		os.Exit(1)
	}

	if err := (&controller.NodeMaintenanceReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorder("node-maintenance-controller"),
		KubeClientset:           kubeClientset,
		UncordonDelay:           uncordonDelay,
		CordonNodes:             cordonNodes,
		DrainNodes:              drainNodes,
		DrainTimeout:            drainTimeout,
		DrainMaxRetries:         drainMaxRetries,
		DrainIgnoreDaemonSets:   drainIgnoreDaemonSets,
		DrainDeleteEmptyDirData: drainDeleteEmptyDirData,
		DrainRetryInterval:      drainRetryInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "nodemaintenance")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Register the Linode maintenance poller as a manager.Runnable.
	// It will start alongside the manager and be stopped when the manager stops.
	maintenancePoller := poller.New(mgr.GetClient(), mgr.GetAPIReader(), poller.Config{
		SecretName:        linodeTokenSecretName,
		SecretNamespace:   linodeTokenSecretNamespace,
		SecretKey:         linodeTokenSecretKey,
		PollInterval:      pollInterval,
		MaintenanceWindow: maintenanceWindow,
		APIEndpoint:       linodeAPIEndpoint,
	})
	if err := mgr.Add(maintenancePoller); err != nil {
		setupLog.Error(err, "Failed to register Linode maintenance poller")
		os.Exit(1)
	}

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
