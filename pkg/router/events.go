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

package router

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// emitSandboxEvent writes a Kubernetes Event against sandbox, mirroring
// pkg/router/agent/handler.go's recordAuditEvent pattern exactly — a plain
// corev1.Event Create via the existing k8sClient, no controller-runtime
// EventRecorder/broadcaster plumbing needed. This is the FR5 wiring point:
// the controller's webhook_delivery loop (pkg/controller/webhook_delivery)
// watches Events with InvolvedObject.Kind=Sandbox as its event source, so an
// Event written here becomes visible to webhook subscriptions once task #43
// maps Reason -> the fixed FR5.2 event-type vocabulary (e.g.
// Reason="BackupCreated" -> "backup.created").
//
// Best-effort: a Create failure is logged and never propagated to the
// caller — event delivery is a secondary concern, not something that should
// fail a backup/share/invoke request that otherwise succeeded.
func (s *Server) emitSandboxEvent(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, eventType, reason, message string) {
	if sandbox == nil {
		return
	}
	now := metav1.Now()
	evt := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: sandbox.Name + ".",
			Namespace:    sandbox.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Sandbox",
			APIVersion: agenttierv1alpha1.GroupVersion.String(),
			Namespace:  sandbox.Namespace,
			Name:       sandbox.Name,
			UID:        sandbox.UID,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType, // corev1.EventTypeNormal | corev1.EventTypeWarning
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source: corev1.EventSource{
			Component: "agenttier-router",
		},
	}
	if err := s.k8sClient.Create(ctx, evt); err != nil {
		s.logger.Warn("failed to write sandbox event",
			"sandbox", sandbox.Name, "reason", reason, "error", err)
	}
}
