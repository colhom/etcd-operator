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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coreos/etcd-operator/pkg/analytics"
	"github.com/coreos/etcd-operator/pkg/backup/s3/s3config"
	"github.com/coreos/etcd-operator/pkg/chaos"
	"github.com/coreos/etcd-operator/pkg/controller"
	"github.com/coreos/etcd-operator/pkg/garbagecollection"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil/election"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil/election/resourcelock"
	"github.com/coreos/etcd-operator/version"

	"github.com/Sirupsen/logrus"
	"golang.org/x/time/rate"
	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api"
	"k8s.io/client-go/1.5/pkg/labels"
	"k8s.io/client-go/1.5/rest"
	"k8s.io/client-go/1.5/tools/record"
)

var (
	analyticsEnabled     bool
	pvProvisioner        string
	masterHost           string
	tlsInsecure          bool
	certFile             string
	keyFile              string
	caFile               string
	etcdClientCAKeyFile  string
	etcdClientCACertFile string
	etcdPeerCAKeyFile    string
	etcdPeerCACertFile   string
	namespace            string
	awsSecret            string
	awsConfig            string
	s3Bucket             string
	gcInterval           time.Duration

	chaosLevel int

	printVersion bool
)

var (
	leaseDuration = 15 * time.Second
	renewDuration = 5 * time.Second
	retryPeriod   = 3 * time.Second
)

func init() {
	flag.BoolVar(&analyticsEnabled, "analytics", true, "Send analytical event (Cluster Created/Deleted etc.) to Google Analytics")

	flag.StringVar(&pvProvisioner, "pv-provisioner", "kubernetes.io/gce-pd", "persistent volume provisioner type")
	flag.StringVar(&masterHost, "master", "", "API Server addr, e.g. ' - NOT RECOMMENDED FOR PRODUCTION - http://127.0.0.1:8080'. Omit parameter to run in on-cluster mode and utilize the service account token.")
	flag.StringVar(&certFile, "cert-file", "", " - NOT RECOMMENDED FOR PRODUCTION - Path to public K8S TLS certificate file.")
	flag.StringVar(&keyFile, "key-file", "", "- NOT RECOMMENDED FOR PRODUCTION - Path to private K8S TLS certificate file.")
	flag.StringVar(&caFile, "ca-file", "", "- NOT RECOMMENDED FOR PRODUCTION - Path to K8S TLS CA file.")
	flag.BoolVar(&tlsInsecure, "tls-insecure", false, "- NOT RECOMMENDED FOR PRODUCTION - Don't verify API server's CA certificate.")
	flag.StringVar(&etcdClientCAKeyFile, "etcd-client-ca-key-file", "/etc/etcd-operator-ca/client-ca-key.pem", "- NOT RECOMMENDED FOR PRODUCTION - Path to etcd cluster client CA Key file.")
	flag.StringVar(&etcdClientCACertFile, "etcd-client-ca-cert-file", "/etc/etcd-operator-ca/client-ca-cert.pem", "- NOT RECOMMENDED FOR PRODUCTION - Path to etcd cluster client CA cert file.")
	flag.StringVar(&etcdPeerCAKeyFile, "etcd-peer-ca-key-file", "/etc/etcd-operator-ca/peer-ca-key.pem", "- NOT RECOMMENDED FOR PRODUCTION - Path to etcd cluster peer CA Key file.")
	flag.StringVar(&etcdPeerCACertFile, "etcd-peer-ca-cert-file", "/etc/etcd-operator-ca/peer-ca-cert.pem", "- NOT RECOMMENDED FOR PRODUCTION - Path to etcd cluster peer CA Key file.")

	flag.StringVar(&awsSecret, "backup-aws-secret", "", "The name of the kube secret object that stores the aws credential file.")
	flag.StringVar(&awsConfig, "backup-aws-config", "", "The name of the kube configmap object that presents the aws config file.")
	flag.StringVar(&s3Bucket, "backup-s3-bucket", "", "The name of the aws S3 bucket to store backups.")
	// chaos level will be removed once we have a formal tool to inject failures.
	flag.IntVar(&chaosLevel, "chaos-level", -1, "DO NOT USE IN PRODUCTION - level of chaos injected into the etcd clusters created by the operator.")
	flag.BoolVar(&printVersion, "version", false, "Show version and quit")
	flag.DurationVar(&gcInterval, "gc-interval", 10*time.Minute, "GC interval")
	flag.Parse()

	namespace = os.Getenv("MY_POD_NAMESPACE")
	if len(namespace) == 0 {
		namespace = "default"
	}
}

func main() {
	if printVersion {
		fmt.Println("etcd-operator", version.Version)
		os.Exit(0)
	}

	if analyticsEnabled {
		analytics.Enable()
	}

	analytics.OperatorStarted()

	id, err := os.Hostname()
	if err != nil {
		logrus.Fatalf("failed to get hostname: %v", err)
	}

	// TODO: replace this to client-go once leader election pacakge is imported
	//       https://github.com/kubernetes/client-go/issues/28
	rl := &resourcelock.EndpointsLock{
		EndpointsMeta: api.ObjectMeta{
			Namespace: namespace,
			Name:      "etcd-operator",
		},
		Client: k8sutil.MustCreateClient(masterHost, tlsInsecure, &rest.TLSClientConfig{
			CertFile: certFile,
			KeyFile:  keyFile,
			CAFile:   caFile,
		}),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: &record.FakeRecorder{},
		},
	}

	election.RunOrDie(election.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDuration,
		RetryPeriod:   retryPeriod,
		Callbacks: election.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				logrus.Fatalf("leader election lost")
			},
		},
	})
	panic("unreachable")
}

func run(stop <-chan struct{}) {
	cfg := newControllerConfig()
	if err := cfg.Validate(); err != nil {
		logrus.Fatalf("invalid operator config: %v", err)
	}

	go periodicFullGC(cfg.KubeCli, cfg.Namespace, gcInterval)

	startChaos(context.Background(), cfg.KubeCli, cfg.Namespace, chaosLevel)

	for {
		c := controller.New(cfg)
		err := c.Run()
		switch err {
		case controller.ErrVersionOutdated:
		default:
			logrus.Fatalf("controller Run() ended with failure: %v", err)
		}
	}
}

func newControllerConfig() controller.Config {
	tlsConfig := rest.TLSClientConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	}
	kubecli := k8sutil.MustCreateClient(masterHost, tlsInsecure, &tlsConfig)
	cfg := controller.Config{
		MasterHost:    masterHost,
		Namespace:     namespace,
		PVProvisioner: pvProvisioner,
		S3Context: s3config.S3Context{
			AWSSecret: awsSecret,
			AWSConfig: awsConfig,
			S3Bucket:  s3Bucket,
		},
		KubeCli:              kubecli,
		EtcdClientCACertPath: etcdClientCACertFile,
		EtcdClientCAKeyPath:  etcdClientCAKeyFile,
		EtcdPeerCACertPath:   etcdPeerCACertFile,
		EtcdPeerCAKeyPath:    etcdPeerCAKeyFile,
	}
	if len(cfg.MasterHost) == 0 {
		logrus.Info("use in cluster client from k8s library")
		cfg.MasterHost = k8sutil.MustGetInClusterMasterHost()
	}

	return cfg
}

func periodicFullGC(k8s kubernetes.Interface, ns string, d time.Duration) {
	gc := garbagecollection.New(k8s, ns)
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		<-timer.C
		err := gc.FullyCollect()
		if err != nil {
			logrus.Warningf("failed to cleanup resources: %v", err)
		}
	}
}

func startChaos(ctx context.Context, k8s kubernetes.Interface, ns string, chaosLevel int) {
	m := chaos.NewMonkeys(k8s)
	ls := labels.SelectorFromSet(map[string]string{"app": "etcd"})

	switch chaosLevel {
	case 1:
		logrus.Info("chaos level = 1: randomly kill one etcd pod every 30 seconds at 50%")
		c := &chaos.CrashConfig{
			Namespace: ns,
			Selector:  ls,

			KillRate:        rate.Every(30 * time.Second),
			KillProbability: 0.5,
			KillMax:         1,
		}

		go m.CrushPods(ctx, c)

	case 2:
		logrus.Info("chaos level = 2: randomly kill at most five etcd pods every 30 seconds at 50%")
		c := &chaos.CrashConfig{
			Namespace: ns,
			Selector:  ls,

			KillRate:        rate.Every(30 * time.Second),
			KillProbability: 0.5,
			KillMax:         5,
		}

		go m.CrushPods(ctx, c)

	default:
	}
}
