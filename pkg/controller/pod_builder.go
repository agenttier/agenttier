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
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

const (
	sandboxContainerName = "sandbox"
	workspaceVolumeName  = "workspace"
	tmpVolumeName        = "tmp"
	defaultShell         = "/bin/bash"

	// tmpVolumeSizeLimit caps the size of the in-memory /tmp tmpfs. Anything
	// the user wants to keep should land in /workspace (PVC-backed) anyway;
	// 256 MiB is enough headroom for tmux sockets, pip caches, npm caches,
	// and the usual scratch space without giving runaway processes a way
	// to OOM the node by writing to /tmp forever.
	tmpVolumeSizeLimit = "256Mi"
)

var (
	defaultUser  int64 = 1000
	defaultGroup int64 = 1000
)

// PodBuilder constructs Pod specs for sandboxes with proper security context,
// volume mounts, environment variables, and init containers.
type PodBuilder struct {
	DefaultImage string
}

// MergedPodConfig holds the fully resolved configuration for building a pod.
type MergedPodConfig struct {
	Image           string
	ImagePullPolicy corev1.PullPolicy
	ImagePullSecret string
	Command         []string
	Args            []string
	WorkingDir      string
	Shell           string
	Env             []corev1.EnvVar
	Resources       *corev1.ResourceRequirements
	RuntimeClass    *string
	Privileged      bool
	MountPath       string
	PVCName         string
	// UseHTTPExec mirrors HarnessSpec.UseHTTPExec after template inheritance
	// has been resolved. When true, the controller injects the in-pod
	// runtime token env var on the sandbox container and adds an ingress
	// rule to the NetworkPolicy permitting the Router→sandbox:9000 hop.
	// Default false (today's SPDY behavior).
	UseHTTPExec bool
	// RuntimeTokenSecret is the name of the per-sandbox Secret holding the
	// AGENTTIER_RUNTIME_TOKEN value. Empty when UseHTTPExec is false. The
	// controller creates this Secret with a 32-byte random token; the
	// Sandbox owns it via owner reference so it's GC'd on delete.
	RuntimeTokenSecret string
	// SandboxAPIKeySecret is the name of the per-sandbox Secret holding the
	// AGENTTIER_SANDBOX_API_KEY value (FR6) — a scoped API key bound to
	// exactly this sandbox that lets an in-pod agent call back into the
	// Router without holding its owner's full-access key. Empty when
	// UseHTTPExec is false (auto-minted under the same opt-in gate as
	// RuntimeTokenSecret; see scoped_key.go). The Sandbox owns this Secret
	// via owner reference so it's GC'd on delete — unlike the hashed
	// record in the Router's api-key store, which needs explicit
	// finalizer-path cleanup (DL7).
	SandboxAPIKeySecret string
	Sidecars            []corev1.Container
	InitContainers      []corev1.Container
	InitScripts         []string
	Files               []agenttierv1alpha1.FileSpec
	Credentials         []agenttierv1alpha1.CredentialRef
	ServiceAccount      string // Kubernetes ServiceAccount name (for IRSA)
}

// Build creates a Pod for the given sandbox with the merged configuration.
func (b *PodBuilder) Build(sandbox *agenttierv1alpha1.Sandbox, config *MergedPodConfig) *corev1.Pod {
	podName := fmt.Sprintf("%s-pod", sandbox.Name)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				"agenttier.io/sandbox":  sandbox.Name,
				"agenttier.io/managed":  "true",
				"agenttier.io/template": sandbox.Status.ResolvedTemplate,
			},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Pod-level security context: fsGroup ensures volume group ownership
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &defaultUser,
				RunAsGroup: &defaultGroup,
				FSGroup:    &defaultGroup,
			},
			Containers: []corev1.Container{
				b.buildMainContainer(config),
			},
			Volumes:        b.buildVolumes(config),
			InitContainers: b.buildInitContainers(config),
		},
	}

	// Set creator annotation
	if sandbox.Spec.CreatedBy != nil {
		pod.Annotations["agenttier.io/created-by"] = sandbox.Spec.CreatedBy.Email
	}

	// Set RuntimeClass
	if config.RuntimeClass != nil {
		pod.Spec.RuntimeClassName = config.RuntimeClass
	}

	// Set ServiceAccount (for IRSA credential injection)
	if config.ServiceAccount != "" {
		pod.Spec.ServiceAccountName = config.ServiceAccount
	}

	// Set image pull secret
	if config.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: config.ImagePullSecret},
		}
	}

	// Add sidecars
	pod.Spec.Containers = append(pod.Spec.Containers, config.Sidecars...)

	// Add credential volumes
	b.addCredentialVolumes(pod, config.Credentials)

	// SECURITY ENFORCEMENT: Strip any dangerous fields regardless of user input
	b.enforceSecurityInvariants(pod)

	return pod
}

// buildMainContainer creates the primary sandbox container.
func (b *PodBuilder) buildMainContainer(config *MergedPodConfig) corev1.Container {
	image := config.Image
	if image == "" {
		image = b.DefaultImage
	}

	container := corev1.Container{
		Name:  sandboxContainerName,
		Image: image,
		Stdin: true,
		TTY:   true,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      workspaceVolumeName,
				MountPath: config.MountPath,
			},
			{
				// Writable in-memory /tmp. The container's root filesystem
				// is read-only by default (see buildSecurityContext), and
				// many common tools — tmux, pip, npm, gcc — refuse to run
				// without a writable /tmp. A tmpfs emptyDir is the
				// conventional fix: per-pod, sized, ephemeral, and
				// invisible to other pods on the node. Anything the user
				// wants to keep belongs in /workspace anyway.
				Name:      tmpVolumeName,
				MountPath: "/tmp",
			},
		},
		Env:             config.Env,
		SecurityContext: b.buildSecurityContext(config.Privileged),
	}

	// Set command if specified (otherwise use image default)
	if len(config.Command) > 0 {
		container.Command = config.Command
	}
	if len(config.Args) > 0 {
		container.Args = config.Args
	}

	// Set working directory
	if config.WorkingDir != "" {
		container.WorkingDir = config.WorkingDir
	} else {
		container.WorkingDir = config.MountPath
	}

	// Set resources
	if config.Resources != nil {
		container.Resources = *config.Resources
	}

	// Set image pull policy
	if config.ImagePullPolicy != "" {
		container.ImagePullPolicy = config.ImagePullPolicy
	}

	// Add credential env vars
	for _, cred := range config.Credentials {
		if cred.MountAs == "env" || cred.MountAs == "" {
			container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{
				Prefix: cred.EnvPrefix,
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cred.SecretName,
					},
				},
			})
		}
	}

	// Add credential file mounts
	for _, cred := range config.Credentials {
		if cred.MountAs == "file" && cred.MountPath != "" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      fmt.Sprintf("cred-%s", cred.SecretName),
				MountPath: cred.MountPath,
				ReadOnly:  true,
			})
		}
	}

	// In-pod runtime token (HTTP-exec opt-in path). Sourced from a
	// per-sandbox Secret the controller created alongside the Pod —
	// keeps the value stable across pod restarts and lets RBAC restrict
	// who can read it. The runtime binary inside the container reads
	// AGENTTIER_RUNTIME_TOKEN from env at startup; the Router sends the
	// same token as a Bearer header on every dial.
	if config.UseHTTPExec && config.RuntimeTokenSecret != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "AGENTTIER_RUNTIME_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: config.RuntimeTokenSecret,
					},
					Key: "token",
				},
			},
		})
	}

	// Sandbox-scoped API key (FR6). Lets an in-pod agent call back into
	// the Router (e.g. to stop/resume itself, invoke files/ports APIs)
	// using a credential bound to exactly this sandbox rather than its
	// owner's full-access key. Same opt-in gate and injection pattern as
	// AGENTTIER_RUNTIME_TOKEN above — see scoped_key.go.
	if config.UseHTTPExec && config.SandboxAPIKeySecret != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "AGENTTIER_SANDBOX_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: config.SandboxAPIKeySecret,
					},
					Key: "key",
				},
			},
		})
	}

	return container
}

// buildSecurityContext creates the restrictive security context.
// Default: non-root, read-only root fs, drop ALL caps, seccomp RuntimeDefault.
func (b *PodBuilder) buildSecurityContext(privileged bool) *corev1.SecurityContext {
	if privileged {
		// Relaxed security context for privileged sandboxes
		return &corev1.SecurityContext{
			RunAsUser:  &defaultUser,
			RunAsGroup: &defaultGroup,
		}
	}

	falseVal := false
	trueVal := true

	return &corev1.SecurityContext{
		RunAsNonRoot:             &trueVal,
		RunAsUser:                &defaultUser,
		RunAsGroup:               &defaultGroup,
		ReadOnlyRootFilesystem:   &trueVal,
		AllowPrivilegeEscalation: &falseVal,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildVolumes creates the volume list for the pod.
func (b *PodBuilder) buildVolumes(config *MergedPodConfig) []corev1.Volume {
	tmpSize := resource.MustParse(tmpVolumeSizeLimit)
	volumes := []corev1.Volume{
		{
			Name: workspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: config.PVCName,
				},
			},
		},
		{
			// Memory-backed /tmp. Sized to keep a runaway process from
			// OOMing the node. emptyDir{Medium: Memory} counts against the
			// Pod's memory limits, so this also surfaces as a clean OOM
			// kill rather than node-wide pressure if the cap is exceeded.
			Name: tmpVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &tmpSize,
				},
			},
		},
	}

	// Add credential volumes (file-mounted secrets)
	for _, cred := range config.Credentials {
		if cred.MountAs == "file" {
			volumes = append(volumes, corev1.Volume{
				Name: fmt.Sprintf("cred-%s", cred.SecretName),
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: cred.SecretName,
					},
				},
			})
		}
	}

	return volumes
}

// buildInitContainers creates init containers for file deployment and init scripts.
func (b *PodBuilder) buildInitContainers(config *MergedPodConfig) []corev1.Container {
	var initContainers []corev1.Container

	// Permission fix: ensure workspace is writable by sandbox user
	// This runs as root (UID 0) to chown the PVC mount point
	// Uses the same sandbox image to avoid pulling from Docker Hub
	rootUser := int64(0)
	initContainers = append(initContainers, corev1.Container{
		Name:    "fix-permissions",
		Image:   config.Image,
		Command: []string{"sh", "-c", fmt.Sprintf("chown -R %d:%d %s && chmod 775 %s", defaultUser, defaultGroup, config.MountPath, config.MountPath)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolumeName, MountPath: config.MountPath},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &rootUser,
		},
	})

	// Init container for deploying template files to workspace
	if len(config.Files) > 0 {
		fileDeployer := b.buildFileDeployerInit(config)
		initContainers = append(initContainers, fileDeployer)
	}

	// Init container for running init scripts
	if len(config.InitScripts) > 0 {
		scriptRunner := b.buildScriptRunnerInit(config)
		initContainers = append(initContainers, scriptRunner)
	}

	// User-defined init containers
	initContainers = append(initContainers, config.InitContainers...)

	return initContainers
}

// buildFileDeployerInit creates an init container that writes template files to the workspace.
//
// Files are delivered via base64-encoded `printf '%s' '<base64>' | base64 -d`,
// the same pattern used by /configure's writeFiles in pkg/router/agent/configure.go.
// We intentionally avoid heredocs: a heredoc terminator is a fixed string and
// any user content containing that string on its own line silently truncates
// the file. base64 has no marker collisions and is binary-safe.
func (b *PodBuilder) buildFileDeployerInit(config *MergedPodConfig) corev1.Container {
	script := "#!/bin/sh\nset -e\n"
	for _, f := range config.Files {
		if f.Content == "" {
			continue
		}
		// %q-quote the encoded payload so shell metachars are neutralized.
		// printf '%s' is portable across busybox/dash/bash; the doubled %%s
		// keeps the literal %s through the Go format string.
		encoded := base64.StdEncoding.EncodeToString([]byte(f.Content))
		script += fmt.Sprintf(
			"mkdir -p $(dirname '%s') && printf '%%s' %q | base64 -d > '%s'\n",
			f.Path, encoded, f.Path,
		)
		if f.Mode != nil {
			script += fmt.Sprintf("chmod %o '%s'\n", *f.Mode, f.Path)
		}
	}

	return corev1.Container{
		Name:    "file-deployer",
		Image:   config.Image,
		Command: []string{"/bin/sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolumeName, MountPath: config.MountPath},
			{Name: tmpVolumeName, MountPath: "/tmp"},
		},
		SecurityContext: b.buildSecurityContext(false),
	}
}

// buildScriptRunnerInit creates an init container that executes init scripts.
func (b *PodBuilder) buildScriptRunnerInit(config *MergedPodConfig) corev1.Container {
	// Concatenate all init scripts
	script := "#!/bin/sh\nset -e\n"
	for _, s := range config.InitScripts {
		script += s + "\n"
	}

	return corev1.Container{
		Name:    "init-scripts",
		Image:   config.Image,
		Command: []string{"/bin/sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolumeName, MountPath: config.MountPath},
			{Name: tmpVolumeName, MountPath: "/tmp"},
		},
		Env: config.Env,
		// Init scripts may need to install packages (npm, pip) which require
		// writing to the home directory and /tmp. We allow a writable rootfs
		// for init containers only — the main sandbox container stays read-only.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  &defaultUser,
			RunAsGroup: &defaultGroup,
		},
	}
}

// addCredentialVolumes adds Secret-backed volumes for file-mounted credentials.
func (b *PodBuilder) addCredentialVolumes(pod *corev1.Pod, credentials []agenttierv1alpha1.CredentialRef) {
	for _, cred := range credentials {
		if cred.MountAs == "file" {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: fmt.Sprintf("cred-%s", cred.SecretName),
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: cred.SecretName,
					},
				},
			})
		}
	}
}

// enforceSecurityInvariants strips any dangerous fields from the pod spec.
// This is a defense-in-depth measure — regardless of what the user or template specifies,
// these fields are NEVER allowed on sandbox pods.
func (b *PodBuilder) enforceSecurityInvariants(pod *corev1.Pod) {
	// Never allow host networking
	pod.Spec.HostNetwork = false
	pod.Spec.HostPID = false
	pod.Spec.HostIPC = false

	// Never allow hostPath volumes
	var safeVolumes []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.HostPath == nil {
			safeVolumes = append(safeVolumes, v)
		}
	}
	pod.Spec.Volumes = safeVolumes
}
