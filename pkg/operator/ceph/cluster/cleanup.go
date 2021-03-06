/*
Copyright 2020 The Rook Authors. All rights reserved.

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

package cluster

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookv1 "github.com/rook/rook/pkg/apis/rook.io/v1"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mgr"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	"github.com/rook/rook/pkg/operator/ceph/cluster/rbd"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/ceph/file/mds"
	"github.com/rook/rook/pkg/operator/ceph/object"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util"
	batch "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clusterCleanUpPolicyRetryInterval = 5 //seconds
	// CleanupAppName is the cluster clean up job name
	CleanupAppName = "rook-ceph-cleanup"
)

var (
	volumeName      = "cleanup-volume"
	dataDirHostPath = "ROOK_DATA_DIR_HOST_PATH"
	namespaceDir    = "ROOK_NAMESPACE_DIR"
	monitorSecret   = "ROOK_MON_SECRET"
	clusterFSID     = "ROOK_CLUSTER_FSID"
)

func (c *ClusterController) startClusterCleanUp(stopCleanupCh chan struct{}, cluster *cephv1.CephCluster, cephHosts []string, monSecret, clusterFSID string) {
	logger.Infof("starting clean up for cluster %q", cluster.Name)
	err := c.waitForCephDaemonCleanUp(stopCleanupCh, cluster, time.Duration(clusterCleanUpPolicyRetryInterval)*time.Second)
	if err != nil {
		logger.Errorf("failed to wait till ceph daemons are destroyed. %v", err)
		return
	}

	c.startCleanUpJobs(cluster, cephHosts, monSecret, clusterFSID)
}

func (c *ClusterController) startCleanUpJobs(cluster *cephv1.CephCluster, cephHosts []string, monSecret, clusterFSID string) {
	for _, hostName := range cephHosts {
		logger.Infof("starting clean up job on node %q", hostName)
		jobName := k8sutil.TruncateNodeName("cluster-cleanup-job-%s", hostName)
		podSpec := c.cleanUpJobTemplateSpec(cluster, monSecret, clusterFSID)
		podSpec.Spec.NodeSelector = map[string]string{v1.LabelHostname: hostName}
		labels := controller.AppLabels(CleanupAppName, cluster.Namespace)
		labels[CleanupAppName] = "true"
		job := &batch.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: cluster.Namespace,
				Labels:    labels,
			},
			Spec: batch.JobSpec{
				Template: podSpec,
			},
		}

		// Apply annotations
		cephv1.GetCleanupAnnotations(cluster.Spec.Annotations).ApplyToObjectMeta(&job.ObjectMeta)

		if err := k8sutil.RunReplaceableJob(c.context.Clientset, job, true); err != nil {
			logger.Errorf("failed to run cluster clean up job on node %q. %v", hostName, err)
		}
	}
}

func (c *ClusterController) cleanUpJobContainer(cluster *cephv1.CephCluster, monSecret, cephFSID string) v1.Container {
	volumeMounts := []v1.VolumeMount{}
	envVars := []v1.EnvVar{}
	if cluster.Spec.DataDirHostPath != "" {
		hostPathVolumeMount := v1.VolumeMount{Name: volumeName, MountPath: cluster.Spec.DataDirHostPath}
		devMount := v1.VolumeMount{Name: "devices", MountPath: "/dev"}
		volumeMounts = append(volumeMounts, hostPathVolumeMount)
		volumeMounts = append(volumeMounts, devMount)
		envVars = append(envVars, []v1.EnvVar{
			{Name: dataDirHostPath, Value: cluster.Spec.DataDirHostPath},
			{Name: namespaceDir, Value: cluster.Namespace},
			{Name: monitorSecret, Value: monSecret},
			{Name: clusterFSID, Value: cephFSID},
			{Name: "ROOK_LOG_LEVEL", Value: "DEBUG"},
			mon.PodNamespaceEnvVar(cluster.Namespace),
		}...)
	}

	return v1.Container{
		Name:            "host-cleanup",
		Image:           c.rookImage,
		SecurityContext: osd.PrivilegedContext(),
		VolumeMounts:    volumeMounts,
		Env:             envVars,
		Args:            []string{"ceph", "clean"},
		Resources:       cephv1.GetCleanupResources(cluster.Spec.Resources),
	}
}

func (c *ClusterController) cleanUpJobTemplateSpec(cluster *cephv1.CephCluster, monSecret, clusterFSID string) v1.PodTemplateSpec {
	volumes := []v1.Volume{}
	hostPathVolume := v1.Volume{Name: volumeName, VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: cluster.Spec.DataDirHostPath}}}
	devVolume := v1.Volume{Name: "devices", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev"}}}
	volumes = append(volumes, hostPathVolume)
	volumes = append(volumes, devVolume)

	podSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name: CleanupAppName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				c.cleanUpJobContainer(cluster, monSecret, clusterFSID),
			},
			Volumes:           volumes,
			RestartPolicy:     v1.RestartPolicyOnFailure,
			PriorityClassName: cephv1.GetCleanupPriorityClassName(cluster.Spec.PriorityClassNames),
		},
	}

	// Apply placement
	rookPlacement := rookv1.Placement(cephv1.GetCleanupPlacement(cluster.Spec.Placement))
	rookPlacement.ApplyToPodSpec(&podSpec.Spec)

	return podSpec
}

func (c *ClusterController) waitForCephDaemonCleanUp(stopCleanupCh chan struct{}, cluster *cephv1.CephCluster, retryInterval time.Duration) error {
	logger.Infof("waiting for all the ceph daemons to be cleaned up in the cluster %q", cluster.Namespace)
	for {
		select {
		case <-time.After(retryInterval):
			cephHosts, err := c.getCephHosts(cluster.Namespace)
			if err != nil {
				return errors.Wrap(err, "failed to list ceph daemon nodes")
			}

			if len(cephHosts) == 0 {
				logger.Info("all ceph daemons are cleaned up")
				return nil
			}

			logger.Debugf("waiting for ceph daemons in cluster %q to be cleaned up. Retrying in %q",
				cluster.Namespace, retryInterval.String())
			break
		case <-stopCleanupCh:
			return errors.New("cancelling the host cleanup job")
		}
	}
}

func (c *ClusterController) getCephHosts(namespace string) ([]string, error) {
	cephPodCount := map[string]int{}
	cephAppNames := []string{mon.AppName, mgr.AppName, osd.AppName, object.AppName, mds.AppName, rbd.AppName}
	nodeNameList := util.NewSet()
	hostNameList := []string{}

	// get all the node names where ceph daemons are running
	for _, app := range cephAppNames {
		appLabelSelector := fmt.Sprintf("app=%s", app)
		podList, err := c.context.Clientset.CoreV1().Pods(namespace).List(metav1.ListOptions{LabelSelector: appLabelSelector})
		if err != nil {
			return hostNameList, errors.Wrapf(err, "could not list the %q pods", app)
		}
		for _, cephPod := range podList.Items {
			podNodeName := cephPod.Spec.NodeName
			if !nodeNameList.Contains(podNodeName) {
				nodeNameList.Add(podNodeName)
			}
		}
		cephPodCount[app] = len(podList.Items)
	}
	logger.Infof("existing ceph daemons in the namespace %q: rook-ceph-mon: %d, rook-ceph-osd: %d, rook-ceph-mds: %d, rook-ceph-rgw: %d, rook-ceph-mgr: %d, rook-ceph-rbd-mirror: %d",
		namespace, cephPodCount["rook-ceph-mon"], cephPodCount["rook-ceph-osd"], cephPodCount["rook-ceph-mds"], cephPodCount["rook-ceph-rgw"], cephPodCount["rook-ceph-mgr"], cephPodCount["rook-ceph-rbd-mirror"])

	for nodeName := range nodeNameList.Iter() {
		podHostName, err := k8sutil.GetNodeHostName(c.context.Clientset, nodeName)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get hostname from node %q", nodeName)
		}
		hostNameList = append(hostNameList, podHostName)
	}

	return hostNameList, nil
}

func (c *ClusterController) getCleanUpDetails(namespace string) (string, string, error) {
	clusterInfo, _, _, err := mon.LoadClusterInfo(c.context, namespace)
	if err != nil {
		return "", "", errors.Wrap(err, "failed to get cluster info")
	}

	return clusterInfo.MonitorSecret, clusterInfo.FSID, nil
}
