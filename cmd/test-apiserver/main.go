package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/pachyderm/fakeapiserver/pkg/pods"
	"go.etcd.io/etcd/server/v3/embed"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	cmtesting "k8s.io/kubernetes/cmd/kube-controller-manager/app/testing"
	"k8s.io/utils/pointer"
)

type l struct {
}

func (l) Errorf(format string, args ...interface{}) {
	log.Printf(format, args...)
}
func (l) Fatalf(format string, args ...interface{}) {
	log.Fatalf(format, args...)
}
func (l) Logf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Create an etcd server.
	cfg := embed.NewConfig()
	cfg.Dir = "default.etcd" // TODO: make unique per invocation
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}
	defer e.Close()
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(60 * time.Second):
		e.Server.Stop() // trigger a shutdown
		return errors.New("etcd server took too long to start")
	}
	var etcdURL string
	for _, u := range cfg.ACUrls {
		etcdURL = u.String()
		if etcdURL != "" {
			break
		}
	}
	if etcdURL == "" {
		return errors.New("no etcd URL?")
	}

	// Shut up k8s warnings.
	rest.SetDefaultWarningHandler(
		rest.NewWarningWriter(os.Stderr, rest.WarningWriterOptions{
			// only print a given warning the first time we receive it
			Deduplicate: true,
			// highlight the output with color when the output supports it
			Color: false,
		},
		),
	)

	// Create an apiserver pointed at the etcd we just created.
	storageConfig := storagebackend.NewDefaultConfig(path.Join(uuid.New().String(), "registry"), nil)
	storageConfig.Transport.ServerList = []string{etcdURL}
	storageConfig.Transport.CertFile = cfg.ClientTLSInfo.CertFile
	storageConfig.Transport.KeyFile = cfg.ClientTLSInfo.KeyFile
	opts := apiservertesting.NewDefaultTestServerOptions()
	api, err := apiservertesting.StartTestServer(l{}, opts, nil, storageConfig)
	if err != nil {
		return fmt.Errorf("start kube-apiserer: %w", err)
	}
	defer api.TearDownFn()
	log.Printf("** kube-apiserver listening on %v", api.ClientConfig.Host)

	// Write a kubeconfig file to /tmp.
	kubeconfigFD, err := os.CreateTemp("", "test-apiserver-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create file for kubeconfig: %w", err)
	}
	kubeconfigFD.Close()
	defer os.Remove(kubeconfigFD.Name())

	kubeconfig := clientapi.Config{
		Preferences: *clientapi.NewPreferences(),
		Extensions:  make(map[string]runtime.Object),
		Clusters: map[string]*clientapi.Cluster{
			"test-apiserver": {
				InsecureSkipTLSVerify: true,
				Server:                api.ClientConfig.Host,
				// TODO: make this work ;)
				//CertificateAuthorityData: api.ClientConfig.CAData,
			},
		},
		Contexts: map[string]*clientapi.Context{
			"test-apiserver": {
				Cluster:  "test-apiserver",
				AuthInfo: "test-apiserver",
			},
		},
		CurrentContext: "test-apiserver",
		AuthInfos: map[string]*clientapi.AuthInfo{
			"test-apiserver": {
				Token: api.ClientConfig.BearerToken,
			},
		},
	}
	if err := clientcmd.WriteToFile(kubeconfig, kubeconfigFD.Name()); err != nil {
		return fmt.Errorf("write kubeconfig file: %w", err)
	}

	// Link this kubeconfig to /tmp/kubeconfig for easy CLI manipulation when only one is running.
	os.Remove("/tmp/kubeconfig")                       // nolint: errcheck
	os.Symlink(kubeconfigFD.Name(), "/tmp/kubeconfig") // nolint: errcheck
	log.Printf("** kubeconfig at %v\n", kubeconfigFD.Name())

	// Create a kube-controller-manager.
	ctrlmgr, err := cmtesting.StartTestServer(l{}, []string{
		fmt.Sprintf("--kubeconfig=%s", kubeconfigFD.Name()),
	})
	if err != nil {
		return fmt.Errorf("start kube-controller-manager: %w", err)
	}
	defer ctrlmgr.TearDownFn()

	// Connect to the apiserver.
	k8s, err := kubernetes.NewForConfig(api.ClientConfig)
	if err != nil {
		return fmt.Errorf("create clientset connected to the apiserver: %w", err)
	}

	// Run our mini-kubelet.
	pmDone := make(chan struct{})
	pm := pods.Manager{
		K8s: k8s,
		UserCode: map[string]pods.UserCode{
			"busybox": func(pod *corev1.Pod, container *corev1.Container, stopCh <-chan struct{}) error {
				for i := 0; ; i++ {
					select {
					case <-time.After(time.Second):
						log.Printf("**** this is busybox on %v, iteration %d", pod.Name, i)
					case <-stopCh:
						log.Printf("**** busybox on %v killed", pod.Name)
						return errors.New("killed")
					}
				}
			},
		},
	}
	go func() {
		defer close(pmDone)
		if err := pm.Run(context.Background(), "default"); err != nil {
			log.Fatalf("pod manager died: %v", err)
		}
	}()

	// Add an RC.
	applyRCAndWatch(k8s)

	intCh := make(chan os.Signal, 1)
	signal.Notify(intCh, os.Interrupt)

	// Block.
	select {
	case <-pmDone:
		log.Printf("pod manager exited; shutting down")
		return nil
	case <-intCh:
		log.Printf("interrupt; shutting down")
		signal.Stop(intCh)
		return nil
	}
}

func applyRCAndWatch(k8s *kubernetes.Clientset) {
	if _, err := k8s.CoreV1().Services("default").Create(context.TODO(), &corev1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-rc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{
					Name:       "foo",
					Protocol:   corev1.ProtocolTCP,
					Port:       1234,
					TargetPort: intstr.FromInt(1234),
				},
			},
			Selector: map[string]string{"app": "test"},
		},
	}, v1.CreateOptions{}); err != nil {
		panic(err)
	}
	if _, err := k8s.CoreV1().ReplicationControllers("default").Create(context.TODO(), &corev1.ReplicationController{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-rc",
			Namespace: "default",
		},
		Spec: corev1.ReplicationControllerSpec{
			Replicas: pointer.Int32Ptr(2),
			Selector: map[string]string{"app": "test"},
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "busybox",
							Image:   "busybox",
							Command: []string{"sleep", "infinity"},
							Ports: []corev1.ContainerPort{
								{
									Name:          "foo",
									ContainerPort: 1234,
								},
							},
						},
					},
				},
			},
		},
	}, v1.CreateOptions{}); err != nil {
		panic(err)
	}
}
