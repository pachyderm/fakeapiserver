package pods

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type testLogger struct {
	sync.Mutex
	testing.TB
}

func (l *testLogger) Printf(msg string, args ...interface{}) {
	l.Lock()
	defer l.Unlock()
	l.TB.Helper()
	l.TB.Logf(msg, args...)
}

func TestLifecycle(t *testing.T) {
	testData := []struct {
		name      string
		pod       *v1.Pod
		kill      int
		waitStart bool
	}{
		{
			name: "container that runs to completion",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "ok",
							Image: "ok",
						},
					},
				},
			},
			waitStart: true,
		},
		{
			name: "container that errors",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "err",
							Image: "err",
						},
					},
				},
			},
			waitStart: true,
		},
		{
			name: "container that runs forever and is killed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "slow",
							Image: "slow",
						},
					},
				},
			},
			waitStart: true,
			kill:      1,
		},
		{
			name: "container with an image that can't be pulled",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "invalid",
							Image: "invalid",
						},
					},
				},
			},
			waitStart: false,
		},
		{
			name: "container multiple long-running images",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "a",
							Image: "slow",
						},
						{
							Name:  "b",
							Image: "slow",
						},
					},
				},
			},
			waitStart: true,
			kill:      2,
		},
	}
	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			ctx, c := context.WithCancel(context.Background())
			k8s := fake.NewSimpleClientset()
			k8s.PrependReactor("create", "pods/binding", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				// SIGH!!! The fake k8s explodes into a million pieces on the next Get if a binding is
				// created.  https://github.com/kubernetes/client-go/issues/911.
				return true, nil, nil
			})
			test.pod.Spec.NodeName = "localhost"

			startCh := make(chan struct{})
			killedCh := make(chan struct{})
			m := &Manager{
				K8s: k8s,
				L:   &testLogger{TB: t},
				UserCode: map[string]UserCode{
					"ok": func(pod *corev1.Pod, container *corev1.Container, stopCh <-chan struct{}) error {
						startCh <- struct{}{}
						return nil
					},
					"err": func(pod *corev1.Pod, container *corev1.Container, stopCh <-chan struct{}) error {
						startCh <- struct{}{}
						return errors.New("uh oh")
					},
					"slow": func(pod *corev1.Pod, container *corev1.Container, stopCh <-chan struct{}) error {
						startCh <- struct{}{}
						<-stopCh
						killedCh <- struct{}{}
						return errors.New("killed")
					},
				},
			}
			managerExitCh := make(chan struct{})
			go func() {
				defer close(managerExitCh)
				if err := m.Run(ctx, "default"); err != nil {
					t.Logf("manager.Run: %v", err)
				}
			}()

			if _, err := k8s.CoreV1().Pods("default").Create(ctx, test.pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("create pod: %v", err)
			}

			if test.waitStart {
				startTimeout := time.After(5 * time.Second)
				for i := 0; i < len(test.pod.Spec.Containers); i++ {
					select {
					case <-startTimeout:
						t.Error("timeout waiting for pod to run")
					case <-startCh:
					}
				}
			}
			close(startCh)

			if test.kill > 0 {
				pod, err := k8s.CoreV1().Pods("default").Get(ctx, test.pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("fetch pod: %v", err)
				}
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				if _, err := k8s.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("kill pod: %v", err)
				}
				for i := 0; i < test.kill; i++ {
					select {
					case <-time.After(5 * time.Second):
						t.Error("timeout waiting for pod to be killed")
					case <-killedCh:
					}
				}
				close(killedCh)
			}

			var podStopped bool
			for i := time.Duration(0); i < 5*time.Second; i += 10 * time.Millisecond {
				time.Sleep(10 * time.Millisecond)
				pod, err := k8s.CoreV1().Pods("default").Get(ctx, test.pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Logf("wait for termination: get pod: %v", err)
					continue
				}
				if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
					podStopped = true
					break
				}
			}
			if !podStopped {
				t.Errorf("pod never stopped")
			}

			c()
			select {
			case <-time.After(5 * time.Second):
				t.Error("timeout waiting for manager to exit")
			case <-managerExitCh:
			}
		})
	}
}
