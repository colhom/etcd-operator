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

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/etcd-operator/pkg/analytics"
	"github.com/coreos/etcd-operator/pkg/backup/s3/s3config"
	"github.com/coreos/etcd-operator/pkg/cluster"
	"github.com/coreos/etcd-operator/pkg/spec"
	"github.com/coreos/etcd-operator/pkg/util/constants"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"

	"github.com/Sirupsen/logrus"
	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api/unversioned"
	"k8s.io/client-go/1.5/pkg/api/v1"
	v1beta1extensions "k8s.io/client-go/1.5/pkg/apis/extensions/v1beta1"
)

var (
	supportedPVProvisioners = map[string]struct{}{
		"kubernetes.io/gce-pd":  {},
		"kubernetes.io/aws-ebs": {},
	}

	ErrVersionOutdated = errors.New("requested version is outdated in apiserver")

	initRetryWaitTime = 30 * time.Second
)

type rawEvent struct {
	Type   string
	Object json.RawMessage
}

type Event struct {
	Type   string
	Object *spec.EtcdCluster
}

type Controller struct {
	logger *logrus.Entry

	Config
	clusters    map[string]*cluster.Cluster
	stopChMap   map[string]chan struct{}
	waitCluster sync.WaitGroup
}

type Config struct {
	MasterHost    string
	Namespace     string
	PVProvisioner string
	s3config.S3Context
	KubeCli                                   kubernetes.Interface
	EtcdPeerCACertPath, EtcdPeerCAKeyPath     string
	EtcdClientCACertPath, EtcdClientCAKeyPath string
}

func (c *Config) Validate() error {
	//TODO(chom): validate CA TLS assets (expiry, is CA, etc)
	if _, ok := supportedPVProvisioners[c.PVProvisioner]; !ok {
		return fmt.Errorf(
			"persistent volume provisioner %s is not supported: options = %v",
			c.PVProvisioner, supportedPVProvisioners,
		)
	}
	allEmpty := len(c.S3Context.AWSConfig) == 0 && len(c.S3Context.AWSSecret) == 0 && len(c.S3Context.S3Bucket) == 0
	allSet := len(c.S3Context.AWSConfig) != 0 && len(c.S3Context.AWSSecret) != 0 && len(c.S3Context.S3Bucket) != 0
	if !(allEmpty || allSet) {
		return errors.New("AWS/S3 related configs should be all set or all empty")
	}
	return nil
}

func New(cfg Config) *Controller {
	return &Controller{
		logger: logrus.WithField("pkg", "controller"),

		Config:    cfg,
		clusters:  make(map[string]*cluster.Cluster),
		stopChMap: map[string]chan struct{}{},
	}
}

func (c *Controller) Run() error {
	var (
		watchVersion string
		err          error
	)

	if len(c.Config.AWSConfig) != 0 {
		// AWS config/creds should be initialized only once here.
		// It will be shared and used by potential cluster's S3 backup manager to manage storage on operator side.
		err := setupS3Env(c.Config.KubeCli, c.Config.S3Context, c.Config.Namespace)
		if err != nil {
			return err
		}
	}

	for {
		watchVersion, err = c.initResource()
		if err == nil {
			break
		}
		c.logger.Errorf("initialization failed: %v", err)
		c.logger.Infof("retry in %v...", initRetryWaitTime)
		time.Sleep(initRetryWaitTime)
		// todo: add max retry?
	}

	c.logger.Infof("starts running from watch version: %s", watchVersion)

	defer func() {
		for _, stopC := range c.stopChMap {
			close(stopC)
		}
		c.waitCluster.Wait()
	}()

	eventCh, errCh := c.monitor(watchVersion)

	go func() {
		for event := range eventCh {
			clus := event.Object
			clus.Spec.Cleanup()

			switch event.Type {
			case "ADDED":
				stopC := make(chan struct{})
				nc := cluster.New(c.makeClusterConfig(), clus, stopC, &c.waitCluster)

				c.stopChMap[clus.Name] = stopC
				c.clusters[clus.Name] = nc

				analytics.ClusterCreated()
				clustersCreated.Inc()
				clustersTotal.Inc()

			case "MODIFIED":
				c.clusters[clus.Name].Update(clus)
				clustersModified.Inc()

			case "DELETED":
				c.clusters[clus.Name].Delete()
				delete(c.clusters, clus.Name)
				analytics.ClusterDeleted()
				clustersDeleted.Inc()
				clustersTotal.Dec()
			}
		}
	}()
	return <-errCh
}

func (c *Controller) findAllClusters() (string, error) {
	c.logger.Info("finding existing clusters...")
	clusterList, err := k8sutil.GetClusterList(c.Config.KubeCli.Core().GetRESTClient(), c.Config.Namespace)
	if err != nil {
		return "", err
	}

	for i := range clusterList.Items {
		clus := clusterList.Items[i]

		if clus.Status.IsFailed() {
			c.logger.Infof("ignore failed cluster %s", clus.Name)
			continue
		}

		clus.Spec.Cleanup()

		stopC := make(chan struct{})
		nc := cluster.New(c.makeClusterConfig(), &clus, stopC, &c.waitCluster)
		c.stopChMap[clus.Name] = stopC
		c.clusters[clus.Name] = nc
	}

	return clusterList.ResourceVersion, nil
}

func (c *Controller) makeClusterConfig() cluster.Config {
	return cluster.Config{
		PVProvisioner: c.PVProvisioner,
		S3Context:     c.S3Context,

		MasterHost: c.MasterHost,
		KubeCli:    c.KubeCli,
	}
}

func (c *Controller) initResource() (string, error) {
	watchVersion := "0"
	err := c.createTPR()
	if err != nil {
		if k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			// TPR has been initialized before. We need to recover existing cluster.
			watchVersion, err = c.findAllClusters()
			if err != nil {
				return "", err
			}
		} else {
			return "", fmt.Errorf("fail to create TPR: %v", err)
		}
	}
	err = k8sutil.CreateStorageClass(c.KubeCli, c.PVProvisioner)
	if err != nil {
		if !k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			return "", fmt.Errorf("fail to create storage class: %v", err)
		}
	}
	return watchVersion, nil
}

func (c *Controller) createTPR() error {
	tpr := &v1beta1extensions.ThirdPartyResource{
		ObjectMeta: v1.ObjectMeta{
			Name: constants.TPRName,
		},
		Versions: []v1beta1extensions.APIVersion{
			{Name: "v1"},
		},
		Description: "Managed etcd clusters",
	}
	_, err := c.KubeCli.Extensions().ThirdPartyResources().Create(tpr)
	if err != nil {
		return err
	}

	return k8sutil.WaitEtcdTPRReady(c.KubeCli.Core().GetRESTClient(), 3*time.Second, 30*time.Second, c.Namespace)
}

func (c *Controller) monitor(watchVersion string) (<-chan *Event, <-chan error) {
	eventCh := make(chan *Event)
	// On unexpected error case, controller should exit
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)

		host := c.MasterHost
		ns := c.Namespace
		httpcli := c.KubeCli.Core().GetRESTClient().Client

		for {
			resp, err := k8sutil.WatchClusters(host, ns, httpcli, watchVersion)
			if err != nil {
				errCh <- err
				return
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				errCh <- errors.New("Invalid status code: " + resp.Status)
				return
			}

			c.logger.Infof("start watching at %v", watchVersion)

			decoder := json.NewDecoder(resp.Body)
			for {
				ev, st, err := pollEvent(decoder)
				if err != nil {
					if err == io.EOF { // apiserver will close stream periodically
						c.logger.Debug("apiserver closed stream")
						break
					}

					c.logger.Errorf("received invalid event from API server: %v", err)
					errCh <- err
					return
				}

				if st != nil {
					if st.Code == http.StatusGone { // event history is outdated
						errCh <- ErrVersionOutdated // go to recovery path
						return
					}
					c.logger.Fatalf("unexpected status response from API server: %v", st.Message)
				}

				c.logger.Debugf("etcd cluster event: %v %v", ev.Type, ev.Object.Spec)

				watchVersion = ev.Object.ObjectMeta.ResourceVersion
				eventCh <- ev
			}

			resp.Body.Close()
		}
	}()

	return eventCh, errCh
}

func pollEvent(decoder *json.Decoder) (*Event, *unversioned.Status, error) {
	re := &rawEvent{}
	err := decoder.Decode(re)
	if err != nil {
		if err == io.EOF {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("fail to decode raw event from apiserver (%v)", err)
	}

	if re.Type == "ERROR" {
		status := &unversioned.Status{}
		err = json.Unmarshal(re.Object, status)
		if err != nil {
			return nil, nil, fmt.Errorf("fail to decode (%s) into unversioned.Status (%v)", re.Object, err)
		}
		return nil, status, nil
	}

	ev := &Event{
		Type:   re.Type,
		Object: &spec.EtcdCluster{},
	}
	err = json.Unmarshal(re.Object, ev.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("fail to unmarshal EtcdCluster object from data (%s): %v", re.Object, err)
	}
	return ev, nil, nil
}
