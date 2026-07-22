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

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// canAccessSandbox reports whether sub's owner (its UserID) is allowed to
// see events for the sandbox named sandboxName in sandboxNamespace — the
// controller-side defense-in-depth half of task #50's fix for the
// cross-tenant webhook disclosure hole. The Router's handleCreateWebhook
// (pkg/router/webhook_handlers.go) already enforces this at
// subscription-creation time, but this loop must not blindly trust a
// persisted record forever: a subscription created before the Router-side
// fix shipped, or reached via any future bug in that check, must still not
// leak another tenant's sandbox events. This mirrors
// pkg/router/handlers.go's userCanAccessSandbox logic (owner, admin, or an
// explicit share grant) without importing the router package — the two
// have no shared home for this today, and importing router from here would
// invert the existing router -> webhookdelivery dependency into a cycle.
//
// A Sandbox that no longer exists (deleted mid-flight, or the event
// predates a since-pruned object) is treated as "no access" — there's
// nothing to check ownership against, and silently delivering an orphaned
// event is not the safer default.
func canAccessSandbox(ctx context.Context, c client.Client, sub Subscription, sandboxNamespace, sandboxName string) bool {
	if sub.IsAdmin {
		return true
	}
	sandbox := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: sandboxNamespace, Name: sandboxName}, sandbox); err != nil {
		return false
	}
	return userCanAccessSandbox(sandbox, sub.UserID)
}

// userCanAccessSandbox mirrors pkg/router/handlers.go's function of the
// same name, keyed on a bare user-sub string instead of a *router.Claims
// (this package has no Claims type and importing the router package's
// would invert the dependency graph) — owner match or an explicit
// per-user/per-group share grant. Admin is handled by the caller
// (canAccessSandbox), since that's a property of the subscription record,
// not the sandbox.
func userCanAccessSandbox(sandbox *agenttierv1alpha1.Sandbox, userSub string) bool {
	if userSub == "" {
		return false
	}
	if sandbox.Spec.CreatedBy != nil && sandbox.Spec.CreatedBy.Sub == userSub {
		return true
	}
	if sandbox.Spec.Sharing == nil {
		return false
	}
	for _, u := range sandbox.Spec.Sharing.Users {
		if u.Identity != "" && u.Identity == userSub {
			return true
		}
	}
	return false
}
