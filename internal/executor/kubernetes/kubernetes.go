// Package kubernetes runs renovate as Kubernetes Jobs and can re-adopt
// running Jobs after a server restart via labels.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

const (
	labelManagedBy     = "app.kubernetes.io/managed-by"
	labelManagedByVal  = "renovate-server"
	labelRepoHash      = "renovate-server.io/repo-hash"
	annotationRepo     = "renovate-server.io/repo"
	annotationPlatform = "renovate-server.io/platform"
	annotationReason   = "renovate-server.io/reason"
	cacheMountPath     = "/tmp/renovate/cache"
)

type Executor struct {
	name         string
	client       k8s.Interface
	namespace    string
	image        string
	cachePVC     string
	jobTTL       time.Duration
	env          map[string]string
	pollInterval time.Duration
	log          *slog.Logger
}

func New(cfg config.Executor, client k8s.Interface, log *slog.Logger) *Executor {
	return &Executor{
		name:         cfg.Name,
		client:       client,
		namespace:    cfg.Namespace,
		image:        cfg.Image,
		cachePVC:     cfg.CachePVC,
		jobTTL:       cfg.JobTTL,
		env:          cfg.Env,
		pollInterval: time.Second,
		log:          log.With("executor", cfg.Name),
	}
}

// NewClientFromEnv builds a clientset from in-cluster config, falling back
// to $KUBECONFIG / ~/.kube/config for local development.
func NewClientFromEnv() (k8s.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, fmt.Errorf("kube config: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("kube config: %w", err)
		}
	}
	return k8s.NewForConfig(cfg)
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	job := e.buildJob(spec)
	created, err := e.client.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	e.log.Info("job created", "job", created.Name, "repo", spec.Repo.Key())
	return e.waitForJob(ctx, created.Name)
}

// AdoptRunning lists managed Jobs still active and returns wait handles so
// the dispatcher can re-lock their repos after a restart.
func (e *Executor) AdoptRunning(ctx context.Context) ([]executor.AdoptedRun, error) {
	list, err := e.client.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + labelManagedByVal,
	})
	if err != nil {
		return nil, fmt.Errorf("list managed jobs: %w", err)
	}
	var adopted []executor.AdoptedRun
	for i := range list.Items {
		job := list.Items[i]
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}
		repo := platform.Repo{
			Platform: job.Annotations[annotationPlatform],
			FullName: job.Annotations[annotationRepo],
		}
		if repo.Platform == "" || repo.FullName == "" {
			continue
		}
		name := job.Name
		adopted = append(adopted, executor.AdoptedRun{
			Repo: repo,
			Wait: func(ctx context.Context) error { return e.waitForJob(ctx, name) },
		})
	}
	sort.Slice(adopted, func(i, j int) bool { return adopted[i].Repo.Key() < adopted[j].Repo.Key() })
	return adopted, nil
}

func (e *Executor) waitForJob(ctx context.Context, name string) error {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		job, err := e.client.BatchV1().Jobs(e.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("get job %q: %w", name, err)
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 {
			return fmt.Errorf("job %q failed", name)
		}
	}
}

func (e *Executor) buildJob(spec executor.RunSpec) *batchv1.Job {
	hash := repoHash(spec.Repo.Key())
	name := fmt.Sprintf("renovate-%s-%s", hash, strconv.FormatInt(time.Now().UnixNano(), 36))

	env := []corev1.EnvVar{{Name: "RENOVATE_REPOSITORIES", Value: spec.Repo.FullName}}
	keys := make([]string, 0, len(e.env))
	for k := range e.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: e.env[k]})
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if e.cachePVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: e.cachePVC},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "cache", MountPath: cacheMountPath})
	}

	backoffLimit := int32(0)
	ttl := int32(e.jobTTL.Seconds())
	runAsNonRoot := true
	noPrivEsc := false

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.namespace,
			Labels: map[string]string{
				labelManagedBy: labelManagedByVal,
				labelRepoHash:  hash,
			},
			Annotations: map[string]string{
				annotationRepo:     spec.Repo.FullName,
				annotationPlatform: spec.Repo.Platform,
				annotationReason:   string(spec.Reason),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{labelManagedBy: labelManagedByVal},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:         "renovate",
						Image:        e.image,
						Env:          env,
						VolumeMounts: mounts,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPrivEsc,
						},
					}},
				},
			},
		},
	}
}

func repoHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}
