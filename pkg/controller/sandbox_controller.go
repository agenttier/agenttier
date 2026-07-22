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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/controller/warmpool"
	"github.com/agenttier/agenttier/pkg/notifications"
	agentotel "github.com/agenttier/agenttier/pkg/otel"
)

const (
	FinalizerName       = "agenttier.io/sandbox-cleanup"
	DefaultRequeueDelay = 30 * time.Second
	MaxRestartCount     = 5
	// RestartCountResetWindow defines how long a pod must run stably
	// (Ready + uptime ≥ window) before the restart counter resets to 0.
	// Sized to outlast a typical node-flap recovery without being so
	// long that long-uptime sandboxes lose the ability to self-heal
	// from a fresh budget.
	RestartCountResetWindow = 5 * time.Minute
)

// SandboxReconciler reconciles Sandbox objects.
type SandboxReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	Recorder           record.EventRecorder
	MaxConcurrency     int
	DefaultImage       string
	DefaultStorageSize string
	DefaultMountPath   string
	// AgentMemorySidecarImage, when non-empty, instructs the controller to
	// inject an opt-in mem0 sidecar into every mode: agent Pod and to set
	// MEM0_BASE_URL on the sandbox container so framework code can dial
	// the sidecar at localhost. Empty disables the feature entirely. Set
	// from the Helm flag optional.agentMemorySidecar.enabled+image.
	AgentMemorySidecarImage string
	// InstallNamespace is where AgentTier itself runs (and therefore where
	// the warm pool ConfigMap lives). Set from POD_NAMESPACE in the
	// controller deployment. Empty falls back to the warm pool's
	// DefaultNamespace constant.
	InstallNamespace string
	// PoolSandboxNamespace is where warm pool Pods + PVCs are provisioned —
	// the namespace the Router creates Sandboxes in. A pool Pod is only
	// claimable by a Sandbox in this same namespace (Pods can't move
	// namespaces). Set from SANDBOX_NAMESPACE in the controller deployment;
	// empty falls back to the warm pool's DefaultSandboxNamespace ("default").
	PoolSandboxNamespace string
	// Notifier, when non-nil, fans out lifecycle notifications (sandbox
	// Error / Stopped) to the channels named in NotifyChannels. Constructed
	// from Helm-driven channel config in cmd/controller; nil disables
	// notifications entirely (the default — no user is blocked without it).
	Notifier *notifications.Notifier
	// NotifyChannels is the set of channel names (webhook / slack / email) to
	// deliver lifecycle notifications to. Empty disables delivery even when
	// Notifier is set.
	NotifyChannels []string
}

// notify fans out a lifecycle notification for the sandbox to the configured
// channels, addressed to the sandbox owner. A no-op when no Notifier or
// channels are configured. Best-effort: delivery happens in background
// goroutines inside the Notifier and never blocks or fails the reconcile.
func (r *SandboxReconciler) notify(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, nType notifications.NotificationType, message string) {
	if r.Notifier == nil || len(r.NotifyChannels) == 0 {
		return
	}
	n := &notifications.Notification{
		Type:        nType,
		SandboxID:   string(sandbox.UID),
		SandboxName: sandbox.Name,
		Message:     message,
	}
	if sandbox.Spec.CreatedBy != nil {
		n.UserID = sandbox.Spec.CreatedBy.Sub
		n.UserEmail = sandbox.Spec.CreatedBy.Email
	}
	r.Notifier.Send(ctx, n, r.NotifyChannels)
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// One span per reconcile so a sandbox can be traced controller→router→pod.
	// Per-object identifiers are fine as span attributes (each span is a
	// discrete event, not an aggregated metric series — the cardinality rule
	// in the project guide is about Prometheus labels, not trace attributes).
	ctx, span := agentotel.Tracer("controller").Start(ctx, "controller.reconcile_sandbox")
	span.SetAttributes(
		attribute.String("sandbox.name", req.Name),
		attribute.String("sandbox.namespace", req.Namespace),
	)
	defer span.End()

	res, err := r.dispatch(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

// dispatch runs the reconcile state machine. It is wrapped by Reconcile, which
// owns the OTel span; dispatch enriches that span with the resolved phase.
func (r *SandboxReconciler) dispatch(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	sandbox := &agenttierv1alpha1.Sandbox{}
	if err := r.Get(ctx, req.NamespacedName, sandbox); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	agentotel.SpanFromContext(ctx).SetAttributes(
		attribute.String("sandbox.phase", string(sandbox.Status.Phase)))

	// Handle deletion
	if !sandbox.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sandbox)
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		controllerutil.AddFinalizer(sandbox, FinalizerName)
		if err := r.Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// State machine dispatch
	switch sandbox.Status.Phase {
	case "", agenttierv1alpha1.SandboxPhaseCreating:
		return r.reconcileCreating(ctx, sandbox)
	case agenttierv1alpha1.SandboxPhaseRunning:
		return r.reconcileRunning(ctx, sandbox)
	case agenttierv1alpha1.SandboxPhaseStopped:
		return r.reconcileStopped(ctx, sandbox)
	case agenttierv1alpha1.SandboxPhaseError:
		return r.reconcileError(ctx, sandbox)
	default:
		logger.Info("unknown sandbox phase", "phase", sandbox.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// reconcileCreating handles the full sandbox creation flow:
// 1. Resolve template
// 2. Merge specs
// 3. Create PVC
// 4. Create NetworkPolicy
// 5. Create Pod
// 6. Wait for Pod Ready → transition to Running
func (r *SandboxReconciler) reconcileCreating(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Set phase to Creating if not already
	if sandbox.Status.Phase == "" {
		sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseCreating
		if err := r.Status().Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(sandbox, corev1.EventTypeNormal, "Creating", "Sandbox creation started")
		logger.Info("sandbox creation started", "sandbox", sandbox.Name, "startedAt", time.Now().Format(time.RFC3339Nano))
	}

	// Step 1: Resolve template
	var templateSpec *agenttierv1alpha1.SandboxTemplateSpec
	var templateName string
	if sandbox.Spec.TemplateRef != nil {
		resolver := &TemplateResolver{Client: r.Client}
		resolved, err := resolver.Resolve(ctx, sandbox.Spec.TemplateRef, sandbox.Namespace)
		if err != nil {
			return r.transitionToError(ctx, sandbox, fmt.Sprintf("Template resolution failed: %v", err))
		}
		if resolved != nil {
			templateSpec = &resolved.Spec
			templateName = resolved.Name
			sandbox.Status.ResolvedTemplate = resolved.Name
			sandbox.Status.TemplateResourceVersion = resolved.ResourceVersion
		}
	}

	// Step 1.5: Try to claim a warm pool pod (instant startup path).
	//
	// Pool Pods live in the pool's sandbox namespace (PoolSandboxNamespace,
	// defaulting to "default" — where the Router creates Sandboxes). A pool
	// Pod is reused in place by the claiming Sandbox, and Kubernetes can't
	// move a Pod across namespaces, so we only claim when this Sandbox lives
	// in that same namespace. Sandboxes in other namespaces fall through to
	// a cold start.
	poolSandboxNS := r.PoolSandboxNamespace
	if poolSandboxNS == "" {
		poolSandboxNS = warmpool.DefaultSandboxNamespace
	}
	canClaim := sandbox.Status.PodName == "" &&
		templateName != "" &&
		sandbox.Namespace == poolSandboxNS
	if canClaim {
		claimedPod, claimedPVC, claimErr := warmpool.Claim(ctx, r.Client, poolSandboxNS, templateName)
		if claimErr == nil && claimedPod != "" {
			logger.Info("claimed warm pool pod", "sandbox", sandbox.Name, "pod", claimedPod, "pvc", claimedPVC)

			// Relabel the claimed pod to belong to this sandbox
			pod := &corev1.Pod{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: claimedPod}, pod); err == nil {
				// Claim strips the pool labels; an otherwise-empty labels map
				// round-trips back as nil, so guard before assigning.
				if pod.Labels == nil {
					pod.Labels = map[string]string{}
				}
				pod.Labels["agenttier.io/sandbox"] = sandbox.Name
				pod.Labels["agenttier.io/template"] = templateName
				// Adopt the claimed Pod: make this Sandbox its controller
				// owner so (a) the Owns(&Pod{}) watch drives prompt
				// self-healing on pod failure instead of waiting for the 30s
				// periodic requeue, and (b) the Pod is garbage-collected with
				// the Sandbox. Pool pods are created with no owner reference,
				// so there's no controller-owner conflict here.
				if err := controllerutil.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
					logger.Error(err, "failed to set owner reference on claimed pod, falling through to normal creation")
				} else if err := r.Update(ctx, pod); err != nil {
					logger.Error(err, "failed to relabel claimed pod, falling through to normal creation")
				} else {
					// Adopt the claimed PVC too, so it's GC'd with the Sandbox
					// and never mistaken for (or reaped as) a stray pool PVC.
					if claimedPVC != "" {
						pvc := &corev1.PersistentVolumeClaim{}
						if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: claimedPVC}, pvc); err == nil {
							// Strip the pool labels — warmpool.Claim removes
							// them from the Pod but not the PVC, leaving a
							// running sandbox's PVC looking like a pool
							// resource (a data-loss landmine for any reaper
							// that deletes pool PVCs by label).
							delete(pvc.Labels, warmpool.LabelPooled)
							delete(pvc.Labels, warmpool.LabelTemplate)
							if err := controllerutil.SetControllerReference(sandbox, pvc, r.Scheme); err != nil {
								logger.Error(err, "failed to set owner reference on claimed pvc", "pvc", claimedPVC)
							} else if err := r.Update(ctx, pvc); err != nil {
								logger.Error(err, "failed to adopt claimed pvc", "pvc", claimedPVC)
							}
						}
					}

					// Ensure NetworkPolicy BEFORE marking Running. A
					// warm-claimed sandbox must never report Running without
					// its network isolation in place — the cold-start path
					// treats an NP failure as fatal, and so must this one
					// (previously it only logged the error and proceeded,
					// leaving the sandbox Running with no default-deny / egress
					// rules and never retrying).
					networkSpec := sandbox.Spec.Network
					if networkSpec == nil && templateSpec != nil {
						networkSpec = templateSpec.Network
					}
					// Warm-pool path: HTTP-exec opt-in flows through the
					// resolved template's Harness.
					npOpts := NetworkPolicyOptions{RouterNamespace: r.InstallNamespace}
					if templateSpec != nil && templateSpec.Harness != nil &&
						templateSpec.Harness.UseHTTPExec != nil &&
						*templateSpec.Harness.UseHTTPExec {
						npOpts.AllowRouterIngressOn9000 = true
					}
					if err := r.ensureNetworkPolicy(ctx, sandbox, networkSpec, npOpts); err != nil {
						return r.transitionToError(ctx, sandbox, fmt.Sprintf("NetworkPolicy creation failed for warm-pool sandbox: %v", err))
					}

					// Isolation is in place — record the claim and mark Running.
					now := metav1.Now()
					sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseRunning
					sandbox.Status.PodName = claimedPod
					sandbox.Status.PVCName = claimedPVC
					sandbox.Status.StartedAt = &now
					sandbox.Status.LastActivityTimestamp = &now
					sandbox.Status.Message = ""
					if err := r.Status().Update(ctx, sandbox); err != nil {
						return ctrl.Result{}, err
					}

					startupDuration := now.Time.Sub(sandbox.CreationTimestamp.Time)
					logger.Info("sandbox is running (from warm pool)",
						"sandbox", sandbox.Name,
						"pod", claimedPod,
						"startupDurationMs", startupDuration.Milliseconds(),
					)
					r.Recorder.Eventf(sandbox, corev1.EventTypeNormal, "Running", "Sandbox is ready from warm pool (startup: %s)", startupDuration.Round(time.Millisecond))

					return ctrl.Result{RequeueAfter: DefaultRequeueDelay}, nil
				}
			}
		}
	}

	// Step 2: Merge sandbox spec with template and defaults
	defaults := &ControllerDefaults{
		Image:                   r.DefaultImage,
		MountPath:               r.DefaultMountPath,
		Storage:                 r.DefaultStorageSize,
		AgentMemorySidecarImage: r.AgentMemorySidecarImage,
	}
	mergedConfig := MergeSandboxWithTemplate(&sandbox.Spec, templateSpec, defaults)

	// Persist the merged AgentSpec onto sandbox status so /configure and
	// /invoke can read the resolved per-template caps without re-walking
	// the inheritance chain on every request. Without this a child
	// template that inherits MaxConcurrentInvokes from a parent would see
	// the cap silently ignored — the Router only inspects directly
	// referenced templates and never the chain.
	if templateSpec != nil && templateSpec.Harness != nil && templateSpec.Harness.Agent != nil {
		sandbox.Status.ResolvedAgentSpec = templateSpec.Harness.Agent.DeepCopy()
	}

	// HTTP-exec opt-in: when the resolved harness asks for it, materialize
	// the per-sandbox runtime-token Secret and plumb its name into the
	// merged config so the Pod spec mounts AGENTTIER_RUNTIME_TOKEN. Done
	// before PVC + NetworkPolicy so a token-Secret failure short-circuits
	// the create flow cleanly.
	if mergedConfig.UseHTTPExec {
		secretName, err := r.ensureRuntimeTokenSecret(ctx, sandbox)
		if err != nil {
			return r.transitionToError(ctx, sandbox, fmt.Sprintf("Runtime-token secret failed: %v", err))
		}
		mergedConfig.RuntimeTokenSecret = secretName

		// FR6 sandbox-scoped API key: auto-minted alongside the runtime
		// token, under the same HTTP-exec opt-in gate (design.md DD6) —
		// an agent that can reach the Router via the runtime token is
		// exactly the agent that needs its own scoped key to call back
		// into the Router. See scoped_key.go for the namespace-aware
		// revoke-on-delete rationale (DL7).
		scopedKeySecretName, err := r.ensureSandboxAPIKeySecret(ctx, sandbox, r.InstallNamespace)
		if err != nil {
			return r.transitionToError(ctx, sandbox, fmt.Sprintf("Scoped API key secret failed: %v", err))
		}
		mergedConfig.SandboxAPIKeySecret = scopedKeySecretName
	}

	// Step 3: Create PVC (if not already exists)
	storageSpec := sandbox.Spec.Storage
	if storageSpec == nil && templateSpec != nil {
		storageSpec = templateSpec.Storage
	}
	pvc, err := r.ensurePVC(ctx, sandbox, storageSpec)
	if err != nil {
		return r.transitionToError(ctx, sandbox, fmt.Sprintf("PVC creation failed: %v", err))
	}
	sandbox.Status.PVCName = pvc.Name
	mergedConfig.PVCName = pvc.Name

	// Step 4: Create NetworkPolicy
	networkSpec := sandbox.Spec.Network
	if networkSpec == nil && templateSpec != nil {
		networkSpec = templateSpec.Network
	}
	if err := r.ensureNetworkPolicy(ctx, sandbox, networkSpec, NetworkPolicyOptions{
		AllowRouterIngressOn9000: mergedConfig.UseHTTPExec,
		RouterNamespace:          r.InstallNamespace,
	}); err != nil {
		return r.transitionToError(ctx, sandbox, fmt.Sprintf("NetworkPolicy creation failed: %v", err))
	}

	// Step 5: Create Pod (if not already exists)
	podBuilder := &PodBuilder{DefaultImage: r.DefaultImage}
	desiredPod := podBuilder.Build(sandbox, mergedConfig)

	// Set owner reference on pod
	if err := controllerutil.SetControllerReference(sandbox, desiredPod, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner reference on Pod: %w", err)
	}

	existingPod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKeyFromObject(desiredPod), existingPod)
	if errors.IsNotFound(err) {
		logger.Info("creating sandbox pod", "pod", desiredPod.Name)
		if err := r.Create(ctx, desiredPod); err != nil {
			return r.transitionToError(ctx, sandbox, fmt.Sprintf("Pod creation failed: %v", err))
		}
		sandbox.Status.PodName = desiredPod.Name
		if err := r.Status().Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to check pod status. The controller also watches owned
		// Pods via SetupWithManager.Owns(&corev1.Pod{}), so an explicit
		// requeue is only needed to catch the brief window before the pod
		// exists in the cache. Keeping this at 1s trims ~2s off a cold start
		// without hammering the apiserver — almost all follow-up reconciles
		// come from the Pod watch, not this ticker.
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: Check if pod is Ready
	if isPodReady(existingPod) {
		now := metav1.Now()
		sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseRunning
		sandbox.Status.PodName = existingPod.Name
		sandbox.Status.StartedAt = &now
		sandbox.Status.LastActivityTimestamp = &now
		sandbox.Status.Message = ""
		if err := r.Status().Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}

		// Log startup duration
		startupDuration := now.Time.Sub(sandbox.CreationTimestamp.Time)
		logger.Info("sandbox is running",
			"sandbox", sandbox.Name,
			"pod", existingPod.Name,
			"startupDurationMs", startupDuration.Milliseconds(),
			"startupDuration", startupDuration.String(),
		)
		r.Recorder.Eventf(sandbox, corev1.EventTypeNormal, "Running", "Sandbox is ready (startup: %s)", startupDuration.Round(time.Millisecond))
		return ctrl.Result{RequeueAfter: DefaultRequeueDelay}, nil
	}

	// Pod exists but not ready yet — check for failures
	if existingPod.Status.Phase == corev1.PodFailed {
		reason := getPodFailureReason(existingPod)
		return r.transitionToError(ctx, sandbox, fmt.Sprintf("Pod failed: %s", reason))
	}

	// Still waiting for pod to be ready. Pod watch delivers the real
	// transition; this 1s poll just backstops any missed watch events.
	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}

// reconcileRunning checks timeouts and pod health.
func (r *SandboxReconciler) reconcileRunning(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) (ctrl.Result, error) {
	// Check if pod still exists
	if sandbox.Status.PodName == "" {
		return r.transitionToError(ctx, sandbox, "Pod name not set in Running state")
	}

	// User-requested stop: the Router annotates the sandbox with
	// `agenttier.io/action: stop` to signal a graceful stop. We honor it
	// here in the Running-phase reconcile loop. Resume is handled at the
	// REST layer by transitioning the phase directly (no annotation
	// needed). Clearing the annotation prevents replay loops.
	if sandbox.Annotations["agenttier.io/action"] == "stop" {
		delete(sandbox.Annotations, "agenttier.io/action")
		if err := r.Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return r.stopSandbox(ctx, sandbox, "User requested stop")
	}

	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Status.PodName}, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Pod disappeared — infrastructure failure (no pod object,
			// so isInfrastructureFailure(nil) → true).
			return r.handleInfrastructureFailure(ctx, sandbox, "Pod disappeared unexpectedly", nil)
		}
		return ctrl.Result{}, err
	}

	// Check pod failures
	if pod.Status.Phase == corev1.PodFailed {
		reason := getPodFailureReason(pod)
		return r.handleInfrastructureFailure(ctx, sandbox, reason, pod)
	}

	// Check pod completion. Pods run with RestartPolicy:Never, so a main
	// process that exits 0 leaves the pod in phase Succeeded — which no
	// other branch handles, leaving the Sandbox reporting Running forever
	// while its pod is dead (misleading every API/UI consumer, especially
	// for agent-mode or one-shot CMD sandboxes). Treat completion as a stop:
	// transition to Stopped and clean up the pod (resumable, terminal).
	if pod.Status.Phase == corev1.PodSucceeded {
		return r.stopSandbox(ctx, sandbox, "Sandbox process completed (pod exited 0)")
	}

	// Reset RestartCount once the pod has been stably Running for the
	// stability window. Without this, a sandbox that experiences five
	// transient node failures over its lifetime would exhaust the
	// restart budget and go terminal even though each individual failure
	// fully recovered. The window is conservative (5 minutes) so we
	// don't reset on a pod that's only briefly Ready before the next
	// flap.
	if sandbox.Status.RestartCount > 0 && sandbox.Status.StartedAt != nil {
		uptime := time.Since(sandbox.Status.StartedAt.Time)
		if uptime >= RestartCountResetWindow && isPodReady(pod) {
			r.Recorder.Eventf(sandbox, corev1.EventTypeNormal, "RestartCountReset", "Pod stable for %s; resetting restart count from %d to 0", uptime.Round(time.Second), sandbox.Status.RestartCount)
			sandbox.Status.RestartCount = 0
			if err := r.Status().Update(ctx, sandbox); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Check idle timeout
	if sandbox.Spec.IdleTimeout != nil && sandbox.Spec.IdleTimeout.Duration > 0 {
		if sandbox.Status.LastActivityTimestamp != nil {
			idle := time.Since(sandbox.Status.LastActivityTimestamp.Time)
			if idle >= sandbox.Spec.IdleTimeout.Duration {
				r.Recorder.Event(sandbox, corev1.EventTypeNormal, "IdleTimeout", fmt.Sprintf("Idle for %s", idle))
				return r.stopSandbox(ctx, sandbox, "Idle timeout exceeded")
			}
		}
	}

	// Check max runtime
	if sandbox.Spec.Timeout != nil && sandbox.Spec.Timeout.Duration > 0 {
		if sandbox.Status.StartedAt != nil {
			runtime := time.Since(sandbox.Status.StartedAt.Time)
			if runtime >= sandbox.Spec.Timeout.Duration {
				r.Recorder.Event(sandbox, corev1.EventTypeNormal, "MaxRuntimeReached", fmt.Sprintf("Running for %s", runtime))
				return r.stopSandbox(ctx, sandbox, "Max runtime exceeded")
			}
		}
	}

	// Calculate next requeue
	requeueAfter := DefaultRequeueDelay
	if sandbox.Spec.IdleTimeout != nil && sandbox.Spec.IdleTimeout.Duration > 0 && sandbox.Status.LastActivityTimestamp != nil {
		remaining := sandbox.Spec.IdleTimeout.Duration - time.Since(sandbox.Status.LastActivityTimestamp.Time)
		if remaining > 0 && remaining < requeueAfter {
			requeueAfter = remaining + time.Second
		}
	}
	if sandbox.Spec.Timeout != nil && sandbox.Spec.Timeout.Duration > 0 && sandbox.Status.StartedAt != nil {
		remaining := sandbox.Spec.Timeout.Duration - time.Since(sandbox.Status.StartedAt.Time)
		if remaining > 0 && remaining < requeueAfter {
			requeueAfter = remaining + time.Second
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileStopped handles stopped sandboxes (autoResume check).
func (r *SandboxReconciler) reconcileStopped(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) (ctrl.Result, error) {
	// Nothing to do — wait for user action (resume or delete)
	return ctrl.Result{}, nil
}

// reconcileError handles error state with restart backoff.
//
// Two paths:
//
//   - Permanent error (RestartCount >= MaxRestartCount): nothing to do,
//     stay in Error indefinitely. The user has to delete + recreate.
//   - Transient error (handleInfrastructureFailure put us here with
//     RestartCount < Max): wait for the exponential-backoff window to
//     elapse, then transition back to Creating so the next reconcile
//     rebuilds the pod. The window is gated on Status.Message
//     containing "before restart attempt" so we don't infinite-loop on
//     errors that aren't restart-eligible (e.g. terminal validation
//     failures).
func (r *SandboxReconciler) reconcileError(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.RestartCount >= MaxRestartCount {
		return ctrl.Result{}, nil // Stay in error permanently
	}

	// Only auto-rebuild when handleInfrastructureFailure put us here.
	// Other paths (transitionToError from validation, template
	// resolution failures, etc.) should not be auto-rebuilt — they need
	// user intervention or a controller-side fix.
	if !strings.Contains(sandbox.Status.Message, "before restart attempt") {
		return ctrl.Result{RequeueAfter: DefaultRequeueDelay}, nil
	}

	backoff := calculateBackoffDelay(sandbox.Status.RestartCount)

	// startedAt of the last successful pod run is our "when did we last
	// fail" reference. Without a more direct timestamp we approximate by
	// using StartedAt; reconciler updates land in tight succession after
	// handleInfrastructureFailure, so the gap is at most one requeue.
	if sandbox.Status.StartedAt != nil {
		elapsed := time.Since(sandbox.Status.StartedAt.Time)
		if elapsed < backoff {
			return ctrl.Result{RequeueAfter: backoff - elapsed}, nil
		}
	}

	// Backoff elapsed — transition back to Creating to rebuild the pod.
	r.Recorder.Eventf(sandbox, corev1.EventTypeNormal, "Restarting", "Backoff window elapsed (%s); restarting pod (attempt %d/%d)", backoff, sandbox.Status.RestartCount, MaxRestartCount)
	sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseCreating
	sandbox.Status.Message = ""
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDelete cleans up all child resources.
func (r *SandboxReconciler) reconcileDelete(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		return ctrl.Result{}, nil
	}

	logger.Info("cleaning up sandbox resources", "sandbox", sandbox.Name)

	// Delete Pod
	if sandbox.Status.PodName != "" {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Status.PodName, Namespace: sandbox.Namespace}}
		if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Delete PVC
	if sandbox.Status.PVCName != "" {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Status.PVCName, Namespace: sandbox.Namespace}}
		if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Delete NetworkPolicy
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name + "-netpol", Namespace: sandbox.Namespace}}
	if err := r.Delete(ctx, np); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// FR6.5: revoke the sandbox's scoped API key. This CANNOT rely on
	// owner-reference GC — the hashed record lives in r.InstallNamespace
	// while the Sandbox lives in sandbox.Namespace, and owner references
	// don't cross namespaces (DL7, decisions.md). Explicit delete here,
	// from the finalizer path, is the only mechanism that actually fires.
	// The plaintext injection Secret (same namespace as the Pod) is
	// unaffected — it's still owner-ref GC'd like runtime_token.go's.
	if err := r.revokeSandboxAPIKey(ctx, sandbox, r.InstallNamespace); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(sandbox, FinalizerName)
	if err := r.Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(sandbox, corev1.EventTypeNormal, "Deleted", "Sandbox deleted")
	logger.Info("sandbox cleanup complete", "sandbox", sandbox.Name)
	return ctrl.Result{}, nil
}

// stopSandbox deletes the pod but preserves the PVC.
func (r *SandboxReconciler) stopSandbox(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, reason string) (ctrl.Result, error) {
	// SnapshotOnStop: if the (effective) storage spec opts in, capture a
	// VolumeSnapshot of the workspace PVC before the pod is torn down so the
	// stopped state can be restored or cloned. A snapshot failure is logged
	// and evented but does NOT block the stop — we don't strand the sandbox
	// in Running over a snapshot hiccup.
	if sandbox.Status.PVCName != "" && r.snapshotOnStopEnabled(ctx, sandbox) {
		if err := r.snapshotSandboxPVC(ctx, sandbox); err != nil {
			log.FromContext(ctx).Error(err, "SnapshotOnStop failed; stopping without a snapshot", "sandbox", sandbox.Name)
			r.Recorder.Eventf(sandbox, corev1.EventTypeWarning, "SnapshotFailed", "SnapshotOnStop failed: %v", err)
		}
	}

	if sandbox.Status.PodName != "" {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Status.PodName, Namespace: sandbox.Namespace}}
		if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseStopped
	sandbox.Status.PodName = ""
	sandbox.Status.StartedAt = nil
	sandbox.Status.Message = reason
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(sandbox, corev1.EventTypeNormal, "Stopped", reason)
	r.notify(ctx, sandbox, notifications.NotifySandboxStopped, reason)
	return ctrl.Result{}, nil
}

// snapshotOnStopEnabled reports whether SnapshotOnStop is set on the sandbox's
// effective storage spec (sandbox spec wins; otherwise inherited from the
// resolved template's storage).
func (r *SandboxReconciler) snapshotOnStopEnabled(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) bool {
	if sandbox.Spec.Storage != nil {
		return sandbox.Spec.Storage.SnapshotOnStop
	}
	if sandbox.Spec.TemplateRef != nil {
		resolver := &TemplateResolver{Client: r.Client}
		if resolved, err := resolver.Resolve(ctx, sandbox.Spec.TemplateRef, sandbox.Namespace); err == nil &&
			resolved != nil && resolved.Spec.Storage != nil {
			return resolved.Spec.Storage.SnapshotOnStop
		}
	}
	return false
}

// snapshotSandboxPVC creates a VolumeSnapshot of the sandbox's workspace PVC.
// The snapshot is intentionally NOT owner-referenced to the Sandbox: the point
// of SnapshotOnStop is to preserve state that can outlive the sandbox (restore
// / clone later), so it must survive sandbox deletion. The operator manages
// snapshot lifecycle.
func (r *SandboxReconciler) snapshotSandboxPVC(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox) error {
	pvcName := sandbox.Status.PVCName
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-stopsnap-%d", sandbox.Name, time.Now().Unix()),
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				"agenttier.io/sandbox":       sandbox.Name,
				"agenttier.io/snapshot-kind": "on-stop",
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvcName,
			},
		},
	}
	if err := r.Create(ctx, snap); err != nil {
		return fmt.Errorf("create VolumeSnapshot: %w", err)
	}
	r.Recorder.Eventf(sandbox, corev1.EventTypeNormal, "Snapshotted", "Created VolumeSnapshot %s before stopping", snap.Name)
	return nil
}

// handleInfrastructureFailure attempts auto-restart with backoff.
//
// Auto-restart is only triggered for failures that look like
// infrastructure problems (OOMKilled, Evicted, NodeLost, pod
// disappearance, CrashLoopBackOff). User-initiated failures (Completed
// with non-zero exit, application errors in the entrypoint) go straight
// to terminal Error so we don't pointlessly burn 5 restart attempts on
// a CMD that's never going to succeed.
//
// Path on infra failure:
//  1. Increment RestartCount.
//  2. If RestartCount >= MaxRestartCount, transition to terminal Error.
//  3. Otherwise transition to Error phase (not Creating directly) so
//     reconcileError applies exponential backoff before recreating the
//     pod. The backoff prevents thundering-herd on a flapping node.
func (r *SandboxReconciler) handleInfrastructureFailure(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, reason string, pod *corev1.Pod) (ctrl.Result, error) {
	// User-error path: a CMD that exited non-zero (Completed reason) is
	// almost always a config bug or an application crash that retrying
	// won't fix. Restart-loop it once for the rare case where the user
	// genuinely wants the container respawned, then go terminal.
	if !isInfrastructureFailure(pod) {
		r.Recorder.Eventf(sandbox, corev1.EventTypeWarning, "ApplicationError", "Pod failed with application error (no auto-restart): %s", reason)
		return r.transitionToError(ctx, sandbox, fmt.Sprintf("Pod failed: %s", reason))
	}

	sandbox.Status.RestartCount++

	// Use the same comparison as reconcileError so both paths agree on
	// MaxRestartCount as the inclusive upper bound. Previously this used
	// `>` (allowing 6 restarts when the limit was 5) while reconcileError
	// used `>=`; the off-by-one let infrastructure failures squeeze in
	// one extra restart attempt past what the Error-phase enforcer
	// thought was the limit.
	if sandbox.Status.RestartCount >= MaxRestartCount {
		return r.transitionToError(ctx, sandbox, fmt.Sprintf("Restart limit exceeded (%d attempts): %s", MaxRestartCount, reason))
	}

	r.Recorder.Eventf(sandbox, corev1.EventTypeWarning, "AutoRestarted", "Pod failed (%s), restarting (attempt %d/%d) with %s backoff", reason, sandbox.Status.RestartCount, MaxRestartCount, calculateBackoffDelay(sandbox.Status.RestartCount))

	// Transition to Error first; reconcileError will apply the backoff
	// requeue and then move us back to Creating once the delay elapses.
	// This is a deliberate change from the previous "go back to Creating
	// immediately" path: instant recreation under a flapping node would
	// thrash the apiserver + scheduler. Exponential backoff gives the
	// underlying infra time to stabilize (10s → 20s → 40s → 80s → 160s).
	sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseError
	sandbox.Status.PodName = ""
	sandbox.Status.Message = fmt.Sprintf("Pod failed (%s), waiting %s before restart attempt %d/%d", reason, calculateBackoffDelay(sandbox.Status.RestartCount), sandbox.Status.RestartCount, MaxRestartCount)
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// transitionToError moves sandbox to Error phase.
func (r *SandboxReconciler) transitionToError(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, message string) (ctrl.Result, error) {
	sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseError
	sandbox.Status.Message = message
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(sandbox, corev1.EventTypeWarning, "Error", message)
	r.notify(ctx, sandbox, notifications.NotifyError, message)
	return ctrl.Result{}, nil
}

// isPodReady checks if a pod has the Ready condition set to True.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// getPodFailureReason extracts a human-readable failure reason from a pod.
func getPodFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return fmt.Sprintf("%s: %s", cs.State.Terminated.Reason, cs.State.Terminated.Message)
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return "CrashLoopBackOff"
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "Unknown failure"
}

// SetupWithManager registers the controller with the manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenttierv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.MaxConcurrency,
		}).
		Complete(r)
}
