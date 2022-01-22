// Package pods pretends to run pods.  It is basically a minimal implementation of the scheduler (it
// nominates itself to run all pods) and the kubelet.
package pods

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/proxy/apis/config/scheme"
	"k8s.io/utils/pointer"
)

// UserCode is code that backs a container.
type UserCode func(pod *corev1.Pod, container *corev1.Container, stopCh <-chan struct{}) error

// Logger prints debugging messages.
type Logger interface {
	Printf(fmt string, args ...interface{})
}

// Manager manages pods, by pretending to the apiserver that it is a node running a kubelet.
type Manager struct {
	// K8s is a Kubernetes clientset.
	K8s kubernetes.Interface

	// L is a log sink.
	L Logger

	// UserCode is a map of image name to the code to run when that image is requested.
	UserCode map[string]UserCode

	// NodeName is the name of the node we're pretending to be.
	NodeName string

	// runningPods is a map of pod name to a channel that, when closed, stops the pod's
	// containers.
	runningPods map[string]chan<- struct{}
}

// registerNode registers a node with the apiserver, and posts kubelet status checks until the context is canceled.
func (m *Manager) registerNode(ctx context.Context) error {
	if _, err := m.K8s.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: m.NodeName,
		},
		Spec: corev1.NodeSpec{
			PodCIDR: "127.0.0.1/32",
		},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	m.L.Printf("node %v: registered ok", m.NodeName)

	start := metav1.Now()
	postHealthy := func() error {
		node, err := m.K8s.CoreV1().Nodes().Get(ctx, m.NodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("refresh node: %w", err)
		}
		now := metav1.Now()
		node.Status.Conditions = []corev1.NodeCondition{
			{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				LastHeartbeatTime:  now,
				LastTransitionTime: start,
				Reason:             "KubeletReady", // this isn't a constant in the k8s codebase!
				Message:            "pods.Manager is posting ready status",
			},
			{
				Type:               corev1.NodeDiskPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  now,
				LastTransitionTime: start,
				Reason:             "KubeletHasNoDiskPressure",
				Message:            "the kubelet does not care if you run out of disk",
			},
			{
				Type:               corev1.NodeMemoryPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  now,
				LastTransitionTime: start,
				Reason:             "KubeletHasSufficientMemory",
				Message:            "the kubelet does not care if you run out of memory",
			},
			{
				Type:               corev1.NodePIDPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  now,
				LastTransitionTime: start,
				Reason:             "KubeletHasSufficientPID",
				Message:            "the kubelet has an unbounded number of PIDs",
			},
		}
		node.Status.Phase = corev1.NodeRunning
		if _, err := m.K8s.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("set node status: %w", err)
		}
		m.L.Printf("node %v: posting healthy status", m.NodeName)
		return nil
	}

	if err := postHealthy(); err != nil {
		return fmt.Errorf("post initial healthy status: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			m.L.Printf("node %v: health updater exiting: %v", m.NodeName, ctx.Err())
			return ctx.Err()
		case <-time.After(5 * time.Second):
			if err := postHealthy(); err != nil {
				m.L.Printf("post healthy status: %v", err)
			}
		}
	}
}

func diffPods(a, b *corev1.Pod) ([]byte, error) {
	aData, bData := new(bytes.Buffer), new(bytes.Buffer)
	encoder := json.NewSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme, true)
	a, b = a.DeepCopy(), b.DeepCopy()
	a.ManagedFields, b.ManagedFields = nil, nil
	if err := encoder.Encode(a, aData); err != nil {
		return nil, fmt.Errorf("marshal old pod: %w", err)
	}
	if err := encoder.Encode(b, bData); err != nil {
		return nil, fmt.Errorf("marshal new pod: %w", err)
	}
	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(aData.Bytes(), bData.Bytes(), &v1.Pod{})
	if err != nil {
		return nil, fmt.Errorf("create two-way merge patch: %w", err)
	}
	return patchBytes, nil
}

// Run reads from the provided k8s apiserver and runs pending pods until the context expires.
func (m *Manager) Run(ctx context.Context, namespace string) error {
	if m.UserCode == nil {
		m.UserCode = make(map[string]UserCode)
	}
	if m.L == nil {
		m.L = log.Default()
	}
	if m.NodeName == "" {
		m.NodeName = "localhost"
	}
	m.runningPods = make(map[string]chan<- struct{})

	// Pretend to be a node.
	nctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		m.L.Printf("node %v: start registration", m.NodeName)
		if err := m.registerNode(nctx); err != nil && !errors.Is(err, context.Canceled) {
			m.L.Printf("register node: %v", err)
		}
	}()

	// Read pods through the shared informer.
	inf := informers.NewSharedInformerFactoryWithOptions(m.K8s, time.Hour, informers.WithNamespace(namespace)).Core().V1().Pods().Informer()
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			if pod.Spec.NodeName != "" && pod.Spec.NodeName != m.NodeName {
				return
			}
			if _, ok := m.runningPods[pod.Name]; ok {
				m.L.Printf("got an 'Added' event for already-running pod %v", pod.Name)
				return
			}
			// The most correct implemenation would check for a PodScheduled condition, ensure
			// that we are the node that is supposed to run it, and then start it.  Right now,
			// we combine scheduling and starting.
			m.L.Printf("pod %v: starting", pod.Name)
			ch, err := m.runPod(ctx, pod)
			if err != nil {
				m.L.Printf("failed to start pod %v: %v", pod.Name, err)
			}
			m.runningPods[pod.Name] = ch
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod := newObj.(*v1.Pod)
			patchBytes, err := diffPods(oldObj.(*corev1.Pod), newObj.(*corev1.Pod))
			if err != nil {
				patchBytes = []byte(fmt.Sprintf("problem diffing pods: %v", err))
			}
			m.L.Printf("pod %s update: %s", pod.Name, patchBytes)

			if pod.Spec.NodeName != m.NodeName {
				return
			}
			c, ok := m.runningPods[pod.Name]
			if !ok {
				m.L.Printf("receivied modification event for unknown pod %v", pod.Name)
				return
			}
			// We should probably check if now() > DeletionTimestamp, but then we need to react
			// to the passage of time rather than just k8s events, so deferred for now.
			if pod.DeletionTimestamp != nil {
				m.L.Printf("deleting pod %v", pod.Name)
				close(c)
				delete(m.runningPods, pod.Name)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			if pod.Spec.NodeName != m.NodeName {
				return
			}
			if _, ok := m.runningPods[pod.Name]; ok {
				// Deletion through the API happens by setting the DeletionTimestamp.  We
				// remove it from runningPods at deletion request time, rather than here, so
				// that we don't close the cancellation channel more than once.
				m.L.Printf("unexpected request to delete pod %v", pod.Name)
				delete(m.runningPods, pod.Name)
			}
		},
	})
	inf.Run(ctx.Done())
	return nil
}

type finishedContainer struct {
	name string
	err  error
}

// runPod accepts a pending pod, starts the containers, and reports the pod status to Kubernetes as
// the containers run.  A stop channel is returned that, when closed, kills the pod.
//
// The operating philosophy is that the apiserver tells us a pod wants to run, and then we ignore
// the apiserver as we run the pod, overwriting anything that changes there with our status.
// (Deletion is special-cased above.)
func (m *Manager) runPod(ctx context.Context, pod *corev1.Pod) (chan<- struct{}, error) {
	closeCh := make(chan struct{})
	var runningContainers int64
	pp := m.K8s.CoreV1().Pods(pod.Namespace)

	// Schedule the pod onto this node.
	if err := pp.Bind(ctx, &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		Target:     corev1.ObjectReference{Kind: "Node", Name: m.NodeName},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("schedule pod onto this node: %w", err)
	}

	// Refresh the pod.
	pod, err := pp.Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("refresh pod after scheduling: %w", err)
	}

	// As containers exit, the exit status is passed over doneCh.
	doneCh := make(chan finishedContainer)

	// Start each container.
	for _, c := range pod.Spec.Containers {
		m.L.Printf("pod %v: container %v: attempting to start", pod.Name, c.Name)
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: c.Name})
		status := &pod.Status.ContainerStatuses[len(pod.Status.ContainerStatuses)-1]

		// See if there is code we can run to implement this container.
		code, ok := m.UserCode[c.Image]
		if !ok {
			// No code to run, pretend that there's a problem pulling the
			// container.
			m.L.Printf("pod %v: container %v: no UserCode to run", pod.Name, c.Name)
			status.State.Terminated = &corev1.ContainerStateTerminated{
				Reason:  fmt.Sprintf("No entry in the UserCode map for image %q", c.Image),
				Message: fmt.Sprintf("No entry in the UserCode map for image %q", c.Image),
			}
			continue
		}

		// Container pulled, update the status and run it.
		status.State.Running = &corev1.ContainerStateRunning{
			StartedAt: metav1.Now(),
		}
		status.Started = pointer.Bool(true)
		status.Ready = true
		runningContainers++
		go func(p *corev1.Pod, c corev1.Container) {
			m.L.Printf("pod %v: container %v: starting", p.Name, c.Name)
			err := code(p, &c, closeCh)
			doneCh <- finishedContainer{name: c.Name, err: err}
			m.L.Printf("pod %v: container %v: user code done (error: %v)", p.Name, c.Name, err)
		}(pod, c)
	}

	// Update the pod status to indicate that containers are running or have failed.
	start := metav1.Now()
	var notReadyContainers []string
	for _, s := range pod.Status.ContainerStatuses {
		if s.State.Running == nil {
			notReadyContainers = append(notReadyContainers, s.Name)
		}
	}
	containersReady := corev1.PodCondition{
		Type:               corev1.ContainersReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: start,
	}
	if len(notReadyContainers) == 0 {
		pod.Status.Phase = corev1.PodRunning
	} else {
		containersReady.Status = corev1.ConditionFalse
		containersReady.Reason = status.ContainersNotReady
		containersReady.Message = fmt.Sprintf("containers with unready status: %v", notReadyContainers)
	}
	ready := *(containersReady.DeepCopy())
	ready.Type = corev1.PodReady

	pod.Status.Conditions = append(pod.Status.Conditions,
		corev1.PodCondition{
			Type:               corev1.PodInitialized,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: start,
		},
		containersReady,
		ready,
	)
	pod.Status.StartTime = &start
	pod.Status.HostIP = "127.0.0.1"
	pod.Status.PodIP = "127.0.0.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "127.0.0.1"}}
	pod, err = pp.UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("update pod after starting containers: %v", err)
	}
	go func() {
		// Watch container exits and keep the pod status up to date.
		for runningContainers > 0 {
			select {
			case c := <-doneCh:
				runningContainers--
				m.L.Printf("pod %v: container %v exited: %v", pod.Name, c.name, c.err)
				pod, err := pp.Get(ctx, pod.Name, metav1.GetOptions{})
				if err != nil {
					m.L.Printf("pod %v: container %v: set exited container status: retrieve pod: %v", pod.Name, c.name, err)
					break
				}

				// Update the status for this container.
				var exitCode int32
				reason := "Completed"
				if c.err != nil {
					exitCode = 1
					reason = "Error"
				}
				end := metav1.Now()
				for i := range pod.Status.ContainerStatuses {
					// Find this container status and edit in place.
					st := &(pod.Status.ContainerStatuses[i])
					if st.Name == c.name {
						st.Ready = false
						st.Started = pointer.Bool(false)
						st.State.Running = nil
						st.State.Terminated = &v1.ContainerStateTerminated{
							ExitCode:   exitCode,
							Reason:     reason,
							FinishedAt: end,
							StartedAt:  start,
						}
						if c.err != nil {
							st.State.Terminated.Message = c.err.Error()
						}
						break
					}
				}

				// Send update to the apiserver.
				pod, err = pp.UpdateStatus(ctx, pod, metav1.UpdateOptions{})
				if err != nil {
					m.L.Printf("pod %v: container %v: update pod status: %v", pod.Name, c.name, err)
				}

			case <-ctx.Done():
				m.L.Printf("pod %v: context finished before all containers exited: %v (current status: %#v)", pod.Name, err, pod.Status)
				return
			}
		}

		// There are now 0 runnning containers, so update the pod status itself and return.
		m.L.Printf("pod %v: all containers have exited", pod.Name)
		pod, err := pp.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			m.L.Printf("pod %v: set final pod status: refresh: %v", pod.Name, err)
			return
		}

		// Determine the outcome of this container (success/failure).
		var containerErrors bool
		for _, st := range pod.Status.ContainerStatuses {
			if t := st.State.Terminated; t != nil {
				if t.ExitCode != 0 {
					containerErrors = true
					break
				}
			}
		}
		if containerErrors {
			pod.Status.Phase = corev1.PodFailed
		} else {
			pod.Status.Phase = corev1.PodSucceeded
		}

		// Update the readiness conditions.
		for i := range pod.Status.Conditions {
			c := &(pod.Status.Conditions[i])
			switch c.Type {
			case corev1.ContainersReady, corev1.PodReady:
				c.Reason = status.PodCompleted
				c.LastTransitionTime = metav1.Now()
				c.Message = ""
			}
		}

		// Send update to the apiserver.
		pod, err = pp.UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		if err != nil {
			m.L.Printf("pod %v: set final pod status: update: %v", pod.Name, err)
		}

		m.L.Printf("pod %v: ending loop", pod.Name)
		return
	}()
	return closeCh, nil
}
