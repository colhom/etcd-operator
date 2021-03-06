// Copyright 2016 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sutil

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/etcd-operator/pkg/spec"
	"github.com/coreos/etcd-operator/pkg/util"
	"github.com/coreos/etcd-operator/pkg/util/constants"
	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/coreos/etcd-operator/pkg/util/retryutil"

	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api"
	apierrors "k8s.io/client-go/1.5/pkg/api/errors"
	"k8s.io/client-go/1.5/pkg/api/meta"
	"k8s.io/client-go/1.5/pkg/api/meta/metatypes"
	"k8s.io/client-go/1.5/pkg/api/v1"
	"k8s.io/client-go/1.5/pkg/labels"
	"k8s.io/client-go/1.5/pkg/util/intstr"
	"k8s.io/client-go/1.5/rest"
)

const (
	// TODO: This is constant for current purpose. We might make it configurable later.
	etcdDir                    = "/var/etcd"
	dataDir                    = etcdDir + "/data"
	backupFile                 = "/var/etcd/latest.backup"
	etcdVersionAnnotationKey   = "etcd.version"
	annotationPrometheusScrape = "prometheus.io/scrape"
	annotationPrometheusPort   = "prometheus.io/port"
)

func GetEtcdVersion(pod *v1.Pod) string {
	return pod.Annotations[etcdVersionAnnotationKey]
}

func SetEtcdVersion(pod *v1.Pod, version string) {
	pod.Annotations[etcdVersionAnnotationKey] = version
}

func GetPodNames(pods []*v1.Pod) []string {
	res := []string{}
	for _, p := range pods {
		res = append(res, p.Name)
	}
	return res
}

func makeRestoreInitContainerSpec(backupAddr, name, token, version string) string {
	spec := []v1.Container{
		{
			Name:  "fetch-backup",
			Image: "tutum/curl",
			Command: []string{
				"/bin/sh", "-c",
				fmt.Sprintf("curl -o %s %s", backupFile, util.MakeBackupURL(backupAddr, version)),
			},
			VolumeMounts: []v1.VolumeMount{
				{Name: "etcd-data", MountPath: etcdDir},
			},
		},
		{
			Name:  "restore-datadir",
			Image: MakeEtcdImage(version),
			Command: []string{
				"/bin/sh", "-c",
				fmt.Sprintf("ETCDCTL_API=3 etcdctl snapshot restore %[1]s"+
					" --name %[2]s"+
					" --initial-cluster %[2]s=http://%[2]s:2380"+
					" --initial-cluster-token %[3]s"+
					" --initial-advertise-peer-urls http://%[2]s:2380"+
					" --data-dir %[4]s", backupFile, name, token, dataDir),
			},
			VolumeMounts: []v1.VolumeMount{
				{Name: "etcd-data", MountPath: etcdDir},
			},
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func MakeEtcdImage(version string) string {
	return fmt.Sprintf("quay.io/coreos/etcd:v%v", version)
}

func GetNodePortString(srv *v1.Service) string {
	return fmt.Sprint(srv.Spec.Ports[0].NodePort)
}

func MakeBackupHostPort(clusterName string) string {
	return fmt.Sprintf("%s:%d", MakeBackupName(clusterName), constants.DefaultBackupPodHTTPPort)
}

func PodWithNodeSelector(p *v1.Pod, ns map[string]string) *v1.Pod {
	p.Spec.NodeSelector = ns
	return p
}

func MakeBackupName(clusterName string) string {
	return fmt.Sprintf("%s-backup-sidecar", clusterName)
}

func CreateMemberService(kubecli kubernetes.Interface, ns string, svc *v1.Service) (*v1.Service, error) {
	retSvc, err := kubecli.Core().Services(ns).Create(svc)
	if err != nil {
		return nil, err
	}
	return retSvc, nil
}

func CreateEtcdService(kubecli kubernetes.Interface, clusterName, ns string, owner metatypes.OwnerReference) (*v1.Service, error) {
	svc := makeEtcdService(clusterName)
	addOwnerRefToObject(svc.GetObjectMeta(), owner)
	retSvc, err := kubecli.Core().Services(ns).Create(svc)
	if err != nil {
		return nil, err
	}
	return retSvc, nil
}

// CreateAndWaitPod is a workaround for self hosted and util for testing.
// We should eventually get rid of this in critical code path and move it to test util.
func CreateAndWaitPod(kubecli kubernetes.Interface, ns string, pod *v1.Pod, timeout time.Duration) (*v1.Pod, error) {
	_, err := kubecli.Core().Pods(ns).Create(pod)
	if err != nil {
		return nil, err
	}

	interval := 3 * time.Second
	var retPod *v1.Pod
	retryutil.Retry(interval, int(timeout/(interval)), func() (bool, error) {
		retPod, err = kubecli.Core().Pods(ns).Get(pod.Name)
		if err != nil {
			return false, err
		}
		switch retPod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodPending:
			return false, nil
		default:
			return false, fmt.Errorf("unexpected pod status.phase: %v", retPod.Status.Phase)
		}
	})

	return retPod, nil
}

func makeEtcdService(clusterName string) *v1.Service {
	labels := map[string]string{
		"app":          "etcd",
		"etcd_cluster": clusterName,
	}
	svc := &v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:   clusterName,
			Labels: labels,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "client",
					Port:       2379,
					TargetPort: intstr.FromInt(2379),
					Protocol:   v1.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}
	return svc
}

// TODO: converge the port logic with member ClientAddr() and PeerAddr()
func NewMemberServiceManifest(etcdName, clusterName string, owner metatypes.OwnerReference) *v1.Service {
	svc := &v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name: etcdName,
			Labels: map[string]string{
				"etcd_cluster": clusterName,
			},
			Annotations: map[string]string{
				annotationPrometheusScrape: "true",
				annotationPrometheusPort:   "2379",
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "server",
					Port:       2380,
					TargetPort: intstr.FromInt(2380),
					Protocol:   v1.ProtocolTCP,
				},
				{
					Name:       "client",
					Port:       2379,
					TargetPort: intstr.FromInt(2379),
					Protocol:   v1.ProtocolTCP,
				},
			},
			Selector: map[string]string{
				"app":          "etcd",
				"etcd_node":    etcdName,
				"etcd_cluster": clusterName,
			},
		},
	}
	addOwnerRefToObject(svc.GetObjectMeta(), owner)
	return svc
}

func AddRecoveryToPod(pod *v1.Pod, clusterName, name, token string, cs *spec.ClusterSpec) {
	pod.Annotations[v1.PodInitContainersBetaAnnotationKey] =
		makeRestoreInitContainerSpec(MakeBackupHostPort(clusterName), name, token, cs.Version)
}

func addOwnerRefToObject(o meta.Object, r metatypes.OwnerReference) {
	o.SetOwnerReferences(append(o.GetOwnerReferences(), r))
}

func MakeEtcdPod(m *etcdutil.Member, initialCluster []string, clusterName, state, token string, cs *spec.ClusterSpec, owner metatypes.OwnerReference) *v1.Pod {
	commands := fmt.Sprintf("/usr/local/bin/etcd --data-dir=%s --name=%s --initial-advertise-peer-urls=%s "+
		"--listen-peer-urls=http://0.0.0.0:2380 --listen-client-urls=http://0.0.0.0:2379 --advertise-client-urls=%s "+
		"--initial-cluster=%s --initial-cluster-state=%s",
		dataDir, m.Name, m.PeerAddr(), m.ClientAddr(), strings.Join(initialCluster, ","), state)
	if state == "new" {
		commands = fmt.Sprintf("%s --initial-cluster-token=%s", commands, token)
	}
	container := containerWithLivenessProbe(etcdContainer(commands, cs.Version), etcdLivenessProbe())
	pod := &v1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: m.Name,
			Labels: map[string]string{
				"app":          "etcd",
				"etcd_node":    m.Name,
				"etcd_cluster": clusterName,
			},
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers:    []v1.Container{container},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{Name: "etcd-data", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
			},
		},
	}

	SetEtcdVersion(pod, cs.Version)

	if cs.Pod != nil {
		if cs.Pod.AntiAffinity {
			pod = PodWithAntiAffinity(pod, clusterName)
		}

		if len(cs.Pod.NodeSelector) != 0 {
			pod = PodWithNodeSelector(pod, cs.Pod.NodeSelector)
		}
	}
	addOwnerRefToObject(pod.GetObjectMeta(), owner)
	return pod
}

func MustGetInClusterMasterHost() string {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	return cfg.Host
}

// tlsConfig isn't modified inside this function.
// The reason it's a pointer is that it's not necessary to have tlsconfig to create a client.
func MustCreateClient(host string, tlsInsecure bool, tlsConfig *rest.TLSClientConfig) kubernetes.Interface {
	var cfg *rest.Config
	if len(host) == 0 {
		var err error
		cfg, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	} else {
		cfg = &rest.Config{
			Host:  host,
			QPS:   100,
			Burst: 100,
		}
		hostUrl, err := url.Parse(host)
		if err != nil {
			panic(fmt.Sprintf("failed to parse host url %s : %v", host, err))
		}
		if hostUrl.Scheme == "https" {
			cfg.TLSClientConfig = *tlsConfig
			cfg.Insecure = tlsInsecure
		}
	}

	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

func IsKubernetesResourceAlreadyExistError(err error) bool {
	return apierrors.IsAlreadyExists(err)
}

func IsKubernetesResourceNotFoundError(err error) bool {
	return apierrors.IsNotFound(err)
}

// We are using internal api types for cluster related.
func ClusterListOpt(clusterName string) api.ListOptions {
	return api.ListOptions{
		LabelSelector: labels.SelectorFromSet(newLablesForCluster(clusterName)),
	}
}

func newLablesForCluster(clusterName string) map[string]string {
	return map[string]string{
		"etcd_cluster": clusterName,
		"app":          "etcd",
	}
}
