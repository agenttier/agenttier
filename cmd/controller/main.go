/*
Copyright 2024 AgentTier Authors.

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
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/controller"
	"github.com/agenttier/agenttier/pkg/controller/backup"
	"github.com/agenttier/agenttier/pkg/controller/warmpool"
	agenttierwebhook "github.com/agenttier/agenttier/pkg/controller/webhook"
	webhookdelivery "github.com/agenttier/agenttier/pkg/controller/webhook_delivery"
	"github.com/agenttier/agenttier/pkg/crds"
	"github.com/agenttier/agenttier/pkg/governance"
	"github.com/agenttier/agenttier/pkg/notifications"
	agentotel "github.com/agenttier/agenttier/pkg/otel"
	"github.com/agenttier/agenttier/pkg/version"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agenttierv1alpha1.AddToScheme(scheme))
	// VolumeSnapshot CRDs from external-snapshotter so the controller
	// can read VolumeSnapshot objects (created by the Router on
	// /sandboxes/{id}/clone) and provision PVCs from them.
	utilruntime.Must(snapshotv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr             string
		healthProbeAddr         string
		leaderElect             bool
		maxConcurrency          int
		defaultImage            string
		defaultStorageSize      string
		defaultMountPath        string
		agentMemorySidecarImage string
		namespace               string
		sandboxNamespace        string
		enableWebhook           bool
		webhookFailurePolicy    string
		manageCRDs              bool
		backupSnapshots         bool
		backupInterval          time.Duration
		backupRetention         time.Duration
		backupSnapshotClass     string
		webhookDelivery         bool
		webhookDeliveryInterval time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metrics endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8082", "The address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.IntVar(&maxConcurrency, "max-concurrency", 10, "Maximum number of concurrent reconciles.")
	flag.StringVar(&defaultImage, "default-image", "ghcr.io/agenttier/sandbox-general:latest", "Default sandbox container image.")
	flag.StringVar(&defaultStorageSize, "default-storage-size", "10Gi", "Default PVC storage size.")
	flag.StringVar(&defaultMountPath, "default-mount-path", "/workspace", "Default PVC mount path.")
	flag.StringVar(&agentMemorySidecarImage, "agent-memory-sidecar-image", "",
		"When set, every mode: agent sandbox Pod gains a mem0 sidecar at "+
			"this image and MEM0_BASE_URL=http://localhost:11434 is "+
			"injected into the sandbox container. Empty = feature off.")
	// Install namespace — drives where the warm pool ConfigMap, pool Pods,
	// and pool PVCs live. Defaults to the POD_NAMESPACE env var (set via
	// the downward API in the Helm chart). Falls back to "agenttier" if
	// neither flag nor env are set, matching the chart's default install
	// namespace.
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace where AgentTier is installed. Defaults to POD_NAMESPACE env var.")
	// Sandbox namespace — where the Router creates Sandboxes and therefore
	// where warm pool Pods + PVCs must be provisioned so a claimed pod can
	// be reused in place. Defaults to SANDBOX_NAMESPACE env var, falling
	// back to the warm pool's DefaultSandboxNamespace ("default") when unset.
	flag.StringVar(&sandboxNamespace, "sandbox-namespace", os.Getenv("SANDBOX_NAMESPACE"),
		"Namespace where Sandboxes (and warm pool Pods) live. Defaults to SANDBOX_NAMESPACE env var, then \"default\".")
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"Serve the Sandbox validating/mutating admission webhook. Requires a serving certificate mounted at the webhook server cert dir (provided by cert-manager via the Helm chart).")
	flag.StringVar(&webhookFailurePolicy, "webhook-failure-policy", "Fail",
		"failurePolicy of the MutatingWebhookConfiguration (Fail or Ignore). Must be Fail in production. "+
			"An Ignore policy allows kubectl to bypass admission governance when the webhook is unavailable (M3 audit finding).")
	flag.BoolVar(&manageCRDs, "manage-crds", true,
		"Apply the controller's bundled CRDs on startup (create-or-update). Keeps CRDs in lockstep with the running controller version, since Helm only installs CRDs on first install and never upgrades them. Set false if you manage CRDs out-of-band (e.g. GitOps/ArgoCD).")
	flag.BoolVar(&backupSnapshots, "backup-snapshots", false,
		"Enable scheduled VolumeSnapshot backups of managed sandbox PVCs (disaster-recovery Layer 1). Requires a VolumeSnapshotClass and the CSI snapshotter.")
	flag.DurationVar(&backupInterval, "backup-interval", 6*time.Hour, "Interval between backup snapshot passes.")
	flag.DurationVar(&backupRetention, "backup-retention", 14*24*time.Hour, "How long to keep backup snapshots before pruning.")
	flag.StringVar(&backupSnapshotClass, "backup-snapshot-class", "", "VolumeSnapshotClass for backup snapshots. Empty uses the cluster default.")
	flag.BoolVar(&webhookDelivery, "webhook-delivery", false,
		"Enable the FR5 webhook-subscription delivery loop (leader-only): matches Sandbox lifecycle Events against subscriptions the Router stores and delivers signed HTTP POSTs with retry/backoff and auto-disable.")
	flag.DurationVar(&webhookDeliveryInterval, "webhook-delivery-interval", 30*time.Second, "Interval between webhook delivery passes.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Default the sandbox namespace to where the Router creates Sandboxes
	// ("default") when neither the flag nor SANDBOX_NAMESPACE env is set.
	if sandboxNamespace == "" {
		sandboxNamespace = warmpool.DefaultSandboxNamespace
	}

	setupLog.Info("starting AgentTier controller",
		"version", version.Version,
		"commit", version.GitCommit,
	)

	// Reconcile spans are emitted per-reconcile (controller.reconcile_sandbox);
	// the warm-pool reconciler logger picks up the trace-context handler so any
	// spans threaded into reconcile contexts auto-correlate in logs.
	bootLogger := slog.New(agentotel.NewSlogContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
	))
	otelShutdownCtx, otelShutdownCancel := context.WithCancel(context.Background())
	defer otelShutdownCancel()
	otelShutdown, err := agentotel.Setup(otelShutdownCtx,
		agentotel.LoadConfigFromEnv("agenttier-controller", version.Version),
		bootLogger)
	if err != nil {
		setupLog.Error(err, "OpenTelemetry setup failed")
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(ctx); err != nil {
			bootLogger.Warn("OTel shutdown returned an error", "err", err)
		}
	}()

	restCfg := ctrl.GetConfigOrDie()

	// Apply (create-or-update) the controller's bundled CRDs before the
	// manager starts. Helm installs CRDs only on first install and never on
	// upgrade, so without this a release that adds a CRD field left the field
	// unusable until an operator ran `kubectl apply -f config/crd/` by hand.
	// Running it here also covers fresh installs — the Sandbox informer needs
	// the CRD to exist before mgr.Start. Disable with --manage-crds=false when
	// CRDs are managed out-of-band (GitOps).
	if manageCRDs {
		applyCtx, applyCancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := crds.Apply(applyCtx, restCfg, bootLogger); err != nil {
			applyCancel()
			setupLog.Error(err, "failed to apply bundled CRDs")
			os.Exit(1)
		}
		applyCancel()
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "agenttier-controller-leader",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Build the notifications fan-out from Helm-driven env config. Each
	// channel is opt-in (configured only when its env vars are set); secrets
	// arrive via Secret-backed env refs, never literals in the chart. When no
	// channel is configured the Notifier is left nil and notifications are a
	// no-op — no user is blocked without it.
	notifier := notifications.NewNotifier(bootLogger)
	var notifyChannels []string
	if url := os.Getenv("NOTIFY_WEBHOOK_URL"); url != "" {
		notifier.RegisterChannel(notifications.NewWebhookChannel(url))
		notifyChannels = append(notifyChannels, "webhook")
	}
	if url := os.Getenv("NOTIFY_SLACK_WEBHOOK_URL"); url != "" {
		notifier.RegisterChannel(notifications.NewSlackChannel(url))
		notifyChannels = append(notifyChannels, "slack")
	}
	if host := os.Getenv("NOTIFY_SMTP_HOST"); host != "" {
		port := 587
		if p, perr := strconv.Atoi(os.Getenv("NOTIFY_SMTP_PORT")); perr == nil && p > 0 {
			port = p
		}
		from := os.Getenv("NOTIFY_SMTP_FROM")
		if from == "" {
			from = "agenttier@localhost"
		}
		notifier.RegisterChannel(notifications.NewEmailChannel(host, port,
			os.Getenv("NOTIFY_SMTP_USERNAME"), os.Getenv("NOTIFY_SMTP_PASSWORD"), from))
		notifyChannels = append(notifyChannels, "email")
	}
	if len(notifyChannels) == 0 {
		notifier = nil // nothing configured → disable
	} else {
		setupLog.Info("notifications enabled", "channels", notifyChannels)
	}

	// Register the Sandbox reconciler
	if err := (&controller.SandboxReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("agenttier-controller"),
		MaxConcurrency:          maxConcurrency,
		DefaultImage:            defaultImage,
		DefaultStorageSize:      defaultStorageSize,
		DefaultMountPath:        defaultMountPath,
		AgentMemorySidecarImage: agentMemorySidecarImage,
		InstallNamespace:        namespace,
		PoolSandboxNamespace:    sandboxNamespace,
		Notifier:                notifier,
		NotifyChannels:          notifyChannels,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}

	// Register the validating/mutating admission webhook for Sandbox
	// resources when enabled. It stamps spec.createdBy from the
	// authenticated user and runs governance Check at admission time,
	// closing the kubectl-bypass. Gated behind --enable-webhook because it
	// requires a serving certificate (mounted by cert-manager via the Helm
	// chart); installs without cert infra leave it off and rely on the
	// Router-side enforcement.
	//
	// Governance-bypass risk check (M3): when the webhook's failurePolicy is
	// not "Fail", a controller or node restart can leave the cluster in a
	// window where direct kubectl writes bypass admission governance entirely.
	// Operators on flaky control planes who set Ignore accept this trade-off
	// explicitly — log an ERROR so it's always visible in structured logs and
	// cannot go unnoticed.
	if enableWebhook && webhookFailurePolicy != "Fail" {
		bootLogger.Error("admission webhook failurePolicy is not Fail",
			"failurePolicy", webhookFailurePolicy,
			"risk", "governance bypass: when the webhook is unreachable, "+
				"Sandbox writes are admitted without governance validation. "+
				"Set --webhook-failure-policy=Fail (the default) for production installs.")
	}
	if enableWebhook {
		decoder := admission.NewDecoder(mgr.GetScheme())
		store := governance.NewConfigMapStore(mgr.GetClient())
		mgr.GetWebhookServer().Register("/validate-sandbox",
			&webhook.Admission{
				Handler: agenttierwebhook.NewSandboxValidator(mgr.GetClient(), store, decoder),
			})
		setupLog.Info("Sandbox admission webhook registered", "path", "/validate-sandbox")
	}

	// Health and readiness probes
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start warm pool reconciler (runs only on the leader, respects leader election)
	wpReconciler := warmpool.NewReconciler(mgr.GetClient(), bootLogger, namespace, sandboxNamespace)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		wpReconciler.RunLoop(ctx)
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add warm pool reconciler")
		os.Exit(1)
	}

	// Start the scheduled-backup loop (leader-only) when enabled. Snapshots
	// managed sandbox PVCs on an interval and prunes by retention; restore is
	// the existing spec.cloneFromSnapshot path.
	if backupSnapshots {
		backupScheduler := backup.NewScheduler(mgr.GetClient(), bootLogger, sandboxNamespace, backupInterval, backupRetention, backupSnapshotClass)
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			backupScheduler.RunLoop(ctx)
			return nil
		})); err != nil {
			setupLog.Error(err, "unable to add backup scheduler")
			os.Exit(1)
		}
	}

	// Start the FR5 webhook-subscription delivery loop (leader-only) when
	// enabled. Subscriptions themselves are Router-owned Secrets
	// (pkg/router/webhook_store.go); this loop only reads them and the
	// Sandbox-kind Events the Sandbox reconciler already emits — it owns
	// delivery, retry/backoff, and auto-disable, per design.md's Component
	// Map ("Router side only" for CRUD, controller for delivery). Runs in
	// the install namespace, where subscription Secrets and the delivery
	// cursor ConfigMap both live.
	if webhookDelivery {
		deliverer := webhookdelivery.NewDeliverer(mgr.GetClient(), bootLogger, namespace, webhookDeliveryInterval)
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			deliverer.RunLoop(ctx)
			return nil
		})); err != nil {
			setupLog.Error(err, "unable to add webhook delivery loop")
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

	fmt.Println("controller stopped")
}
