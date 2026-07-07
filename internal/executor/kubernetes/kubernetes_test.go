package kubernetes

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testExecutor(client *fake.Clientset) *Executor {
	e := New(config.Executor{
		Name:      "k8s",
		Type:      config.ExecutorKubernetes,
		Namespace: "renovate",
		Image:     "renovate/renovate:41",
		CachePVC:  "renovate-cache",
		JobTTL:    time.Hour,
		Env:       map[string]string{"RENOVATE_REDIS_URL": "redis://cache:6379"},
	}, client, slog.New(slog.DiscardHandler))
	e.pollInterval = 5 * time.Millisecond // speed up tests
	return e
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonCron,
	}
}

// completeJob polls until a job exists, then marks it succeeded/failed.
func completeJob(t *testing.T, client *fake.Clientset, succeed bool) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := client.BatchV1().Jobs("renovate").List(context.Background(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				job := jobs.Items[0]
				if succeed {
					job.Status.Succeeded = 1
				} else {
					job.Status.Failed = 1
				}
				_, _ = client.BatchV1().Jobs("renovate").UpdateStatus(context.Background(), &job, metav1.UpdateOptions{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestRunCreatesJobAndSucceeds(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	completeJob(t, client, true)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	jobs, _ := client.BatchV1().Jobs("renovate").List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Labels["app.kubernetes.io/managed-by"] != "renovate-server" {
		t.Errorf("managed-by label missing: %v", job.Labels)
	}
	if job.Annotations["renovate-server.io/repo"] != "top-group/app" {
		t.Errorf("repo annotation = %q", job.Annotations["renovate-server.io/repo"])
	}
	if job.Annotations["renovate-server.io/platform"] != "gl" {
		t.Errorf("platform annotation = %q", job.Annotations["renovate-server.io/platform"])
	}
	pod := job.Spec.Template.Spec
	if pod.Containers[0].Image != "renovate/renovate:41" {
		t.Errorf("image = %q", pod.Containers[0].Image)
	}
	envMap := map[string]string{}
	for _, ev := range pod.Containers[0].Env {
		envMap[ev.Name] = ev.Value
	}
	if envMap["RENOVATE_REPOSITORIES"] != "top-group/app" {
		t.Errorf("RENOVATE_REPOSITORIES = %q", envMap["RENOVATE_REPOSITORIES"])
	}
	if envMap["RENOVATE_REDIS_URL"] != "redis://cache:6379" {
		t.Errorf("custom env missing: %v", envMap)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "renovate-cache" {
		t.Errorf("cache volume wrong: %+v", pod.Volumes)
	}
	if pod.Containers[0].VolumeMounts[0].MountPath != "/tmp/renovate/cache" {
		t.Errorf("mount = %+v", pod.Containers[0].VolumeMounts)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d", *job.Spec.BackoffLimit)
	}
	if *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttl = %d", *job.Spec.TTLSecondsAfterFinished)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot not set")
	}
}

func TestRunJobFails(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	completeJob(t, client, false)
	if err := e.Run(t.Context(), spec()); err == nil {
		t.Fatal("want error for failed job")
	}
}

func TestRunContextCancelled(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestAdoptRunning(t *testing.T) {
	client := fake.NewClientset()
	// one active adoptable job, one finished job, one foreign job
	active := jobFixture("renovate-abc-1", "top-group/app", "gl")
	finished := jobFixture("renovate-def-2", "top-group/done", "gl")
	finished.Status.Succeeded = 1
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "renovate"}}
	for _, j := range []*batchv1.Job{active, finished, foreign} {
		if _, err := client.BatchV1().Jobs("renovate").Create(t.Context(), j, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	e := testExecutor(client)
	adopted, err := e.AdoptRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted = %d, want 1", len(adopted))
	}
	if adopted[0].Repo != (platform.Repo{Platform: "gl", FullName: "top-group/app"}) {
		t.Fatalf("adopted repo = %+v", adopted[0].Repo)
	}

	// Wait resolves when the job completes.
	done := make(chan error, 1)
	go func() { done <- adopted[0].Wait(t.Context()) }()
	time.Sleep(10 * time.Millisecond)
	got, err := client.BatchV1().Jobs("renovate").Get(t.Context(), "renovate-abc-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got.Status.Succeeded = 1
	if _, err := client.BatchV1().Jobs("renovate").UpdateStatus(t.Context(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func jobFixture(name, repo, platformName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "renovate",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "renovate-server"},
			Annotations: map[string]string{
				"renovate-server.io/repo":     repo,
				"renovate-server.io/platform": platformName,
				"renovate-server.io/reason":   "push",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "renovate", Image: "renovate/renovate"}},
				},
			},
		},
	}
}
