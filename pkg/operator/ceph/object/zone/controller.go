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

// Package objectzone to manage a rook object zone.
package zone

import (
	"context"
	"fmt"
	"reflect"
	"syscall"
	"time"

	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/object"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util/exec"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controllerName = "ceph-object-zone-controller"
)

var waitForRequeueIfObjectZoneNotReady = reconcile.Result{Requeue: true, RequeueAfter: 10 * time.Second}

var logger = capnslog.NewPackageLogger("github.com/rook/rook", controllerName)

var cephObjectZoneKind = reflect.TypeOf(cephv1.CephObjectZone{}).Name()

// Sets the type meta for the controller main object
var controllerTypeMeta = metav1.TypeMeta{
	Kind:       cephObjectZoneKind,
	APIVersion: fmt.Sprintf("%s/%s", cephv1.CustomResourceGroup, cephv1.Version),
}

// ReconcileObjectZone reconciles a ObjectZone object
type ReconcileObjectZone struct {
	client      client.Client
	scheme      *runtime.Scheme
	context     *clusterd.Context
	clusterInfo *cephclient.ClusterInfo
}

// Add creates a new CephObjectZone Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, context *clusterd.Context) error {
	return add(mgr, newReconciler(mgr, context))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, context *clusterd.Context) reconcile.Reconciler {
	// Add the cephv1 scheme to the manager scheme so that the controller knows about it
	mgrScheme := mgr.GetScheme()
	cephv1.AddToScheme(mgr.GetScheme())

	return &ReconcileObjectZone{
		client:  mgr.GetClient(),
		scheme:  mgrScheme,
		context: context,
	}
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	logger.Info("successfully started")

	// Watch for changes on the CephObjectZone CRD object
	err = c.Watch(&source.Kind{Type: &cephv1.CephObjectZone{TypeMeta: controllerTypeMeta}}, &handler.EnqueueRequestForObject{}, opcontroller.WatchControllerPredicate())
	if err != nil {
		return err
	}

	return nil
}

// Reconcile reads that state of the cluster for a CephObjectZone object and makes changes based on the state read
// and what is in the CephObjectZone.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileObjectZone) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// workaround because the rook logging mechanism is not compatible with the controller-runtime loggin interface
	reconcileResponse, err := r.reconcile(request)
	if err != nil {
		logger.Errorf("failed to reconcile: %v", err)
	}

	return reconcileResponse, err
}

func (r *ReconcileObjectZone) reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the CephObjectZone instance
	cephObjectZone := &cephv1.CephObjectZone{}
	err := r.client.Get(context.TODO(), request.NamespacedName, cephObjectZone)
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("CephObjectZone resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, "failed to get CephObjectZone")
	}

	// The CR was just created, initializing status fields
	if cephObjectZone.Status == nil {
		updateStatus(r.client, request.NamespacedName, k8sutil.Created)
	}

	// Make sure a CephCluster is present otherwise do nothing
	_, isReadyToReconcile, cephClusterExists, reconcileResponse := opcontroller.IsReadyToReconcile(r.client, r.context, request.NamespacedName, controllerName)
	if !isReadyToReconcile {
		// This handles the case where the Ceph Cluster is gone and we want to delete that CR
		//
		if !cephObjectZone.GetDeletionTimestamp().IsZero() && !cephClusterExists {
			// Return and do not requeue. Successful deletion.
			return reconcile.Result{}, nil
		}
		return reconcileResponse, nil
	}

	// DELETE: the CR was deleted
	if !cephObjectZone.GetDeletionTimestamp().IsZero() {
		logger.Debugf("deleting zone CR %q", cephObjectZone.Name)

		// Return and do not requeue. Successful deletion.
		return reconcile.Result{}, nil
	}

	// Populate clusterInfo during each reconcile
	r.clusterInfo, _, _, err = mon.LoadClusterInfo(r.context, request.NamespacedName.Namespace)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to populate cluster info")
	}

	// validate the zone settings
	err = validateZoneCR(cephObjectZone)
	if err != nil {
		updateStatus(r.client, request.NamespacedName, k8sutil.ReconcileFailedStatus)
		return reconcile.Result{}, errors.Wrapf(err, "invalid CephObjectZone CR %q", cephObjectZone.Name)
	}

	// Start object reconciliation, updating status for this
	updateStatus(r.client, request.NamespacedName, k8sutil.ReconcilingStatus)

	// Make sure an ObjectZoneGroup is present
	realmName, reconcileResponse, err := r.reconcileObjectZoneGroup(cephObjectZone)
	if err != nil {
		return reconcileResponse, err
	}

	// Make sure zone group has been created in Ceph Cluster
	reconcileResponse, err = r.reconcileCephZoneGroup(cephObjectZone, realmName)
	if err != nil {
		return reconcileResponse, err
	}

	// Create Ceph Zone
	reconcileResponse, err = r.createCephZone(cephObjectZone, realmName)
	if err != nil {
		return r.setFailedStatus(request.NamespacedName, "failed to create ceph zone", err)
	}

	// Set Ready status, we are done reconciling
	updateStatus(r.client, request.NamespacedName, k8sutil.ReadyStatus)

	// Return and do not requeue
	logger.Debug("zone done reconciling")
	return reconcile.Result{}, nil
}

func (r *ReconcileObjectZone) createCephZone(zone *cephv1.CephObjectZone, realmName string) (reconcile.Result, error) {
	logger.Infof("creating object zone %q in zonegroup %q in realm %q", zone.Name, zone.Spec.ZoneGroup, realmName)

	realmArg := fmt.Sprintf("--rgw-realm=%s", realmName)
	zoneGroupArg := fmt.Sprintf("--rgw-zonegroup=%s", zone.Spec.ZoneGroup)
	zoneArg := fmt.Sprintf("--rgw-zone=%s", zone.Name)
	objContext := object.NewContext(r.context, r.clusterInfo, zone.Name)

	// get zone group to see if master zone exists yet
	output, err := object.RunAdminCommandNoRealm(objContext, "zonegroup", "get", realmArg, zoneGroupArg)
	if err != nil {
		if code, ok := exec.ExitStatus(err); ok && code == int(syscall.ENOENT) {
			return reconcile.Result{}, errors.Wrapf(err, "ceph zone group %q not found", zone.Spec.ZoneGroup)
		} else {
			return reconcile.Result{}, errors.Wrapf(err, "radosgw-admin zonegroup get failed with code %d", code)
		}
	}

	// check if master zone group does not exist yet for period
	masterZone, err := decodeMasterZone(output)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to parse `radosgw-admin zonegroup get` output")
	}

	masterArg := ""
	// master zone does not exist yet for zone group
	if masterZone == "" {
		masterArg = "--master"
	}

	// create zone
	_, err = object.RunAdminCommandNoRealm(objContext, "zone", "get", realmArg, zoneGroupArg, zoneArg)
	if err != nil {
		if code, ok := exec.ExitStatus(err); ok && code == int(syscall.ENOENT) {
			logger.Debugf("ceph zone %q not found, running `radosgw-admin zone create`", zone.Name)
			_, err := object.RunAdminCommandNoRealm(objContext, "zone", "create", realmArg, zoneGroupArg, zoneArg, masterArg)
			if err != nil {
				return reconcile.Result{}, errors.Wrapf(err, "failed to create ceph zone %q", zone.Name)
			}
		} else {
			return reconcile.Result{}, errors.Wrapf(err, "radosgw-admin zone get failed with code %d", code)
		}
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileObjectZone) reconcileObjectZoneGroup(zone *cephv1.CephObjectZone) (string, reconcile.Result, error) {
	// Verify the object zone API object actually exists
	zoneGroup, err := r.context.RookClientset.CephV1().CephObjectZoneGroups(zone.Namespace).Get(zone.Spec.ZoneGroup, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return "", waitForRequeueIfObjectZoneNotReady, err
		}
		return "", waitForRequeueIfObjectZoneNotReady, errors.Wrapf(err, "error getting cephObjectZoneGroup %v", zone.Spec.ZoneGroup)
	}

	logger.Infof("CephObjectZoneGroup %v found", zoneGroup.Name)
	return zoneGroup.Spec.Realm, reconcile.Result{}, nil
}

func (r *ReconcileObjectZone) reconcileCephZoneGroup(zone *cephv1.CephObjectZone, realmName string) (reconcile.Result, error) {
	realmArg := fmt.Sprintf("--rgw-realm=%s", realmName)
	zoneGroupArg := fmt.Sprintf("--rgw-zonegroup=%s", zone.Spec.ZoneGroup)
	objContext := object.NewContext(r.context, r.clusterInfo, zone.Name)

	_, err := object.RunAdminCommandNoRealm(objContext, "zonegroup", "get", realmArg, zoneGroupArg)
	if err != nil {
		if code, ok := exec.ExitStatus(err); ok && code == int(syscall.ENOENT) {
			return waitForRequeueIfObjectZoneNotReady, errors.Wrapf(err, "ceph zone group %q not found", zone.Spec.ZoneGroup)
		} else {
			return waitForRequeueIfObjectZoneNotReady, errors.Wrapf(err, "radosgw-admin zonegroup get failed with code %d", code)
		}
	}

	logger.Infof("Zone group %q found in Ceph cluster to create ceph zone %q", zone.Spec.ZoneGroup, zone.Name)
	return reconcile.Result{}, nil
}

func (r *ReconcileObjectZone) setFailedStatus(name types.NamespacedName, errMessage string, err error) (reconcile.Result, error) {
	updateStatus(r.client, name, k8sutil.ReconcileFailedStatus)
	return reconcile.Result{}, errors.Wrapf(err, "%s", errMessage)
}

// updateStatus updates an zone with a given status
func updateStatus(client client.Client, name types.NamespacedName, status string) {
	objectZone := &cephv1.CephObjectZone{}
	if err := client.Get(context.TODO(), name, objectZone); err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("CephObjectZone resource not found. Ignoring since object must be deleted.")
			return
		}
		logger.Warningf("failed to retrieve object zone %q to update status to %q. %v", name, status, err)
		return
	}
	if objectZone.Status == nil {
		objectZone.Status = &cephv1.Status{}
	}

	objectZone.Status.Phase = status
	if err := opcontroller.UpdateStatus(client, objectZone); err != nil {
		logger.Errorf("failed to set object zone %q status to %q. %v", name, status, err)
		return
	}
	logger.Debugf("object zone %q status updated to %q", name, status)
}
