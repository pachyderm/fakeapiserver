package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/server/v3/embed"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/client-go/kubernetes"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"

	cmtesting "k8s.io/kubernetes/cmd/kube-controller-manager/app/testing"

	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/pointer"
)

type l struct {
}

func (l) Errorf(format string, args ...any) {
	log.Printf(format, args...)
}
func (l) Fatalf(format string, args ...any) {
	log.Fatalf(format, args...)
}
func (l) Logf(format string, args ...any) {
	log.Printf(format, args...)
}

func main() {
	cfg := embed.NewConfig()
	cfg.Dir = "default.etcd"
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()
	select {
	case <-e.Server.ReadyNotify():
		log.Printf("Server is ready!")
	case <-time.After(60 * time.Second):
		e.Server.Stop() // trigger a shutdown
		log.Printf("Server took too long to start!")
	}
	var etcdURL string
	for _, u := range cfg.ACUrls {
		etcdURL = u.String()
		if etcdURL != "" {
			break
		}
	}

	storageConfig := storagebackend.NewDefaultConfig(path.Join(uuid.New().String(), "registry"), nil)
	storageConfig.Transport.ServerList = []string{etcdURL}
	storageConfig.Transport.CertFile = cfg.ClientTLSInfo.CertFile
	storageConfig.Transport.KeyFile = cfg.ClientTLSInfo.KeyFile
	opts := apiservertesting.NewDefaultTestServerOptions()
	api, err := apiservertesting.StartTestServer(l{}, opts, nil, storageConfig)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("** kube-apiserver listening on %v\n", api.ClientConfig.Host)
	defer api.TearDownFn()

	kubeconfigFD, err := os.CreateTemp("", "test-apiserver-kubeconfig-*")
	if err != nil {
		log.Fatal(err)
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
	fmt.Printf("%#v\n", api.ClientConfig)
	fmt.Printf("%#v\n", kubeconfig)
	if err := clientcmd.WriteToFile(kubeconfig, kubeconfigFD.Name()); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("kubeconfig at %v\n", kubeconfigFD.Name())

	ctrlmgr, err := cmtesting.StartTestServer(l{}, []string{
		fmt.Sprintf("--kubeconfig=%s", kubeconfigFD.Name()),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ctrlmgr.TearDownFn()

	k8s, err := kubernetes.NewForConfig(api.ClientConfig)
	if err != nil {
		log.Fatal(err)
	}
	applyRCAndWatch(k8s)
}

func applyRCAndWatch(k8s *kubernetes.Clientset) {
	{
		podWatch, err := k8s.CoreV1().Pods("default").Watch(context.TODO(), v1.ListOptions{
			Watch: true,
		})
		if err != nil {
			log.Fatal(err)
		}
		defer podWatch.Stop()
		go func() {
			for result := range podWatch.ResultChan() {
				fmt.Printf("pod: %#v\n", result)
			}
		}()
	}
	{
		rcWatch, err := k8s.CoreV1().ReplicationControllers("default").Watch(context.TODO(), v1.ListOptions{Watch: true})
		if err != nil {
			log.Fatal(err)
		}
		defer rcWatch.Stop()
		go func() {
			for result := range rcWatch.ResultChan() {
				fmt.Printf("rc: %#v\n", result)
			}
		}()
	}
	k8s.CoreV1().ReplicationControllers("default").Create(context.TODO(), &corev1.ReplicationController{
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
						},
					},
				},
			},
		},
	}, v1.CreateOptions{})
	select {}
}
