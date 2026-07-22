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

package webhookdelivery

// eventReasonToWebhookType maps a Kubernetes Event's Reason to the fixed
// FR5.2 webhook event-type vocabulary. Reasons not present here simply
// produce no webhook delivery — not every internal Event reason is a
// subscribable FR5.2 type (e.g. "RestartCountReset" or "Restarting" are
// operational detail, not one of the fixed vocabulary types).
//
// All 12 FR5.2 event types now have a live Kubernetes Event source
// (resolves DL9, decisions.md — see the RESOLVED note there):
//   - sandbox.* (5): emitted by r.Recorder.Event/Eventf against a Sandbox
//     object in pkg/controller/sandbox_controller.go and lifecycle.go.
//   - backup.*, share.*, agent.invoke.* (7): emitted by
//     Server.emitSandboxEvent (pkg/router/events.go) — a plain
//     corev1.Event Create via the Router's existing k8sClient, no
//     EventRecorder/broadcaster needed — from pkg/router/backup_handlers.go,
//     pkg/router/handlers.go's share handlers, and pkg/router/agent/invoke.go
//     respectively.
var eventReasonToWebhookType = map[string]string{
	"Creating": "sandbox.creating",
	"Running":  "sandbox.running",
	"Stopped":  "sandbox.stopped",
	"Error":    "sandbox.error",
	"Deleted":  "sandbox.deleting",

	"BackupCreated": "backup.created",
	"BackupPruned":  "backup.pruned",

	"ShareGranted": "share.granted",
	"ShareRevoked": "share.revoked",

	"AgentInvokeStarted":   "agent.invoke.started",
	"AgentInvokeCompleted": "agent.invoke.completed",
	"AgentInvokeFailed":    "agent.invoke.failed",
}

// webhookTypeForReason returns the FR5.2 event type for a K8s Event reason,
// and false if the reason has no webhook mapping.
func webhookTypeForReason(reason string) (string, bool) {
	t, ok := eventReasonToWebhookType[reason]
	return t, ok
}
