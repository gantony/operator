// Copyright (c) 2020-2025 Tigera, Inc. All rights reserved.

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

package utils

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	rbacv1 "k8s.io/api/rbac/v1"

	esv1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/elasticsearch/v1"
	kbv1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/kibana/v1"
	"github.com/go-logr/logr"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apps "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v3 "github.com/tigera/api/pkg/apis/projectcalico/v3"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/render"
	rmeta "github.com/tigera/operator/pkg/render/common/meta"
)

const TLS_CIPHERS_ENV_VAR_NAME = "TLS_CIPHER_SUITES"

// dCache is a global deduplication cache that is used to avoid unnecessary updates to objects. It is shared
// across all component handlers to ensure that objects are only updated when necessary.
//
// Note: we could instead create a cache per component handler, but that would require controllers to
// use the same handler across reconciles - which is a relatively big change to make.
var dCache *objectCache = newCache()

type ComponentHandler interface {
	CreateOrUpdateOrDelete(context.Context, render.Component, status.StatusManager) error

	// Set this component handler to "create only" operation - i.e. it only creates resources if
	// they do not already exist, and never tries to correct existing resources.
	//
	// When a component handler is "create only", and some of the objects that it is asked to
	// create already exist, but no other error occurs, the CreateOrUpdateOrDelete() method will
	// return an error that satisfies `errors.IsAlreadyExists`.  If a more serious error occurs,
	// the method will return that more serious error instead.  If none of the objects already
	// exist, and no other errors occur, the method will return nil.
	SetCreateOnly()
}

// cr is allowed to be nil in the case we don't want to put ownership on a resource,
// this is useful for CRD management so that they are not removed automatically.
func NewComponentHandler(log logr.Logger, cli client.Client, scheme *runtime.Scheme, cr metav1.Object) ComponentHandler {
	return &componentHandler{
		client: cli,
		scheme: scheme,
		cr:     cr,
		log:    log,
	}
}

type componentHandler struct {
	client     client.Client
	scheme     *runtime.Scheme
	cr         metav1.Object
	log        logr.Logger
	createOnly bool
}

func (c *componentHandler) SetCreateOnly() {
	c.createOnly = true
}

func (c *componentHandler) create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	// Make a deep copy of the object, so we can stash away the original object in the cache.
	cp := obj.DeepCopyObject().(client.Object)

	// Pass to the client.
	err := c.client.Create(ctx, obj, opts...)
	if err != nil {
		return err
	}

	// Update the caches so that we don't try to update the object on subsequent reconciliations.
	dCache.set(cp, obj.GetGeneration())
	return nil
}

func (c *componentHandler) update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	logCtx := ContextLoggerForResource(c.log, obj)
	if !c.needsUpdate(ctx, obj) {
		// The object does not need to be updated, so we can skip it.
		logCtx.V(2).Info("Object does not need to be updated, skipping")
		return nil
	}
	logCtx.V(2).Info("Object needs to be updated")

	// Make a deep copy of the object, so we can stash away the original object in the cache.
	cp := obj.DeepCopyObject().(client.Object)

	// Pass to the client.
	err := c.client.Update(ctx, obj, opts...)
	if err != nil {
		if errors.IsNotFound(err) {
			// Inlalidate our cached object if it was not found.
			dCache.delete(obj)
		}
		return err
	}

	// Update the caches so that we don't try to update the object on subsequent reconciliations.
	dCache.set(cp, obj.GetGeneration())
	return nil
}

func (c *componentHandler) delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	// Do a lookup of the object using the client cache to see if we actually need to send the delete.
	// This prevents us from sending lots of delete requests for objects that don't exist.
	logCtx := ContextLoggerForResource(c.log, obj)
	key := client.ObjectKeyFromObject(obj)
	if err := c.client.Get(ctx, key, obj); errors.IsNotFound(err) {
		logCtx.V(2).Info("Object does not exist, skipping delete call")
		dCache.delete(obj)
		return nil
	}

	// If the object is not in the cache, we can send the delete.
	err := c.client.Delete(ctx, obj, opts...)
	if err != nil {
		if errors.IsNotFound(err) {
			// Remove the object from the cache if it was not found.
			dCache.delete(obj)
		}
		return err
	}

	// Invalidate our cached object.
	dCache.delete(obj)
	return nil
}

func (c *componentHandler) needsUpdate(ctx context.Context, obj client.Object) bool {
	logCtx := ContextLoggerForResource(c.log, obj)

	// Only update the object if one of the following is true:
	// - the object is not in the generation cache. This means we have not created this object before.
	// - the generation on the cluster is newer than the generation we have cached. This means that the object has been updated on the cluster.
	// - the object differs from the last cached version. This means the operator wants to update the object.
	// This helps prevent us from updating objects unnecessarily.
	cachedObj, cachedGen, ok := dCache.get(obj)
	if !ok {
		// The object is not in the cache, so we need to update it.
		logCtx.V(3).Info("Object is not in the cache, we should create it")
		return true
	}

	// The object is in the cache, check if the generation is out of date.
	if cachedGen < obj.GetGeneration() {
		// The cached generation is older than the current generation in the cluster.
		logCtx.V(2).Info("Object on cluster has been modified since last reconcile")
		return true
	}

	// The cached generation is the same or newer than the current generation in the cluster.
	// Check if the caller has updated the object since we last cached it.
	if reflect.DeepEqual(cachedObj, obj) {
		// No change to the object since we last cached it, so we don't need to update it.
		return false
	}

	logCtx.V(2).Info("Controller has updated the object since last reconcile")
	return true
}

func (c *componentHandler) createOrUpdateObject(ctx context.Context, obj client.Object, osType rmeta.OSType) error {
	om, ok := obj.(metav1.ObjectMetaAccessor)
	if !ok {
		return fmt.Errorf("object is not ObjectMetaAccessor")
	}

	multipleOwners := checkIfMultipleOwnersLabel(om.GetObjectMeta())
	// Add owner ref for controller owned resources,
	switch obj.(type) {
	case *v3.UISettings:
		// Never add controller ref for UISettings since these are always GCd through the UISettingsGroup.
	default:
		if c.cr != nil && !skipAddingOwnerReference(c.cr, om.GetObjectMeta()) {
			if multipleOwners {
				if err := controllerutil.SetOwnerReference(c.cr, om.GetObjectMeta(), c.scheme); err != nil {
					return err
				}
			} else {
				if err := controllerutil.SetControllerReference(c.cr, om.GetObjectMeta(), c.scheme); err != nil {
					return err
				}
			}
		}
	}

	logCtx := ContextLoggerForResource(c.log, obj)
	key := client.ObjectKeyFromObject(obj)

	// Ensure that if the object is something the creates a pod that it is scheduled on nodes running the operating
	// system as specified by the osType.
	ensureOSSchedulingRestrictions(obj, osType)

	// Make sure any objects with images also have an image pull policy.
	modifyPodSpec(obj, setImagePullPolicy)
	// Order volumes and volume mounts
	modifyPodSpec(obj, orderVolumes)
	modifyPodSpec(obj, orderVolumeMounts)

	// Modify Liveness and Readiness probe default values if they are not set for this object.
	setProbeTimeouts(obj)

	// Make sure we have our standard selector and pod labels
	setStandardSelectorAndLabels(obj)

	if err := ensureTLSCiphers(ctx, obj, c.client); err != nil {
		return fmt.Errorf("failed to set TLS Ciphers: %w", err)
	}

	cur, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		logCtx.V(2).Info("Failed converting object", "obj", obj)
		return fmt.Errorf("failed converting object %+v", obj)
	}

	// Check to see if the object exists or not - this determines whether we should create or update.
	err := c.client.Get(ctx, key, cur)
	if err != nil {
		// Invalidate our cached object.
		dCache.delete(obj)

		if !errors.IsNotFound(err) {
			// Anything other than "Not found" we should retry.
			return err
		}

		// Check to see if the object's Namespace exists, and whether the Namespace
		// is currently terminating. We cannot create objects in a terminating Namespace.
		namespaceTerminating := false
		if ns := cur.GetNamespace(); ns != "" {
			nsKey := client.ObjectKey{Name: ns}
			namespace, err := GetIfExists[v1.Namespace](ctx, nsKey, c.client)
			if err != nil {
				logCtx.WithValues("key", nsKey).Error(err, "Failed to get Namespace.")
				return err
			}
			if namespace != nil {
				namespaceTerminating = namespace.GetDeletionTimestamp() != nil
			}
		}
		if namespaceTerminating {
			logCtx.Info("Object's Namespace is terminating, skipping creation.")
			return nil
		}

		// Otherwise, if it was not found, we should create it and move on.
		logCtx.V(2).Info("Object does not exist, creating it", "error", err)
		if multipleOwners {
			labels := om.GetObjectMeta().GetLabels()
			delete(labels, common.MultipleOwnersLabel)
			om.GetObjectMeta().SetLabels(labels)
		}
		err = c.create(ctx, obj)
		if err != nil {
			logCtx.WithValues("key", key).Error(err, "Failed to create object.")
			return err
		}
		return nil
	}

	if c.createOnly {
		// This component handler only creates resources if they do not already exist.
		logCtx.Info("Create-only operation, ignoring existing object")
		return errors.NewAlreadyExists(
			schema.GroupResource{
				Group:    obj.GetObjectKind().GroupVersionKind().Group,
				Resource: obj.GetObjectKind().GroupVersionKind().Kind,
			},
			obj.GetName(),
		)
	}

	// The object exists. Update it, unless the user has marked it as "ignored".
	if IgnoreObject(cur) {
		logCtx.Info("Ignoring annotated object")
		return nil
	}
	logCtx.V(2).Info("Resource already exists, update it")

	// if mergeState returns nil we don't want to update the object
	if mobj := mergeState(obj, cur); mobj != nil {
		switch obj.(type) {
		case *batchv1.Job:
			// Jobs can't be updated, they can only be deleted then created
			if err := c.delete(ctx, obj); err != nil {
				logCtx.WithValues("key", key).Info("Failed to delete job for recreation.")
				return err
			}

			// Do the Create() with the merged object so that we preserve external labels/annotations.
			resetMetadataForCreate(mobj)
			if err := c.create(ctx, mobj); err != nil {
				logCtx.WithValues("key", key).Error(err, "Failed to create Job.")
				return err
			}
			return nil
		case *v1.Secret:
			objSecret := obj.(*v1.Secret)
			curSecret := cur.(*v1.Secret)
			// Secret types are immutable, we need to delete the old version if the type has changed. If the
			// object type is unset, it will result in SecretTypeOpaque, so this difference can be excluded.
			if objSecret.Type != curSecret.Type &&
				!(len(objSecret.Type) == 0 && curSecret.Type == v1.SecretTypeOpaque) {
				if err := c.delete(ctx, obj); err != nil {
					logCtx.WithValues("key", key).Info("Failed to delete secret for recreation.")
					return err
				}

				// Do the Create() with the merged object so that we preserve external labels/annotations.
				resetMetadataForCreate(mobj)
				if err := c.create(ctx, mobj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to create Secret.")
					return err
				}
				return nil
			}
		case *v1.Service:
			objService := obj.(*v1.Service)
			curService := cur.(*v1.Service)
			if objService.Spec.ClusterIP == "None" && curService.Spec.ClusterIP != "None" {
				// We don't want this service to have a cluster IP, but it has got one already.  Need to recreate
				// the service to remove it.
				logCtx.WithValues("key", key).Info("Service already exists and has unwanted ClusterIP, recreating service.")
				if err := c.delete(ctx, obj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to delete Service for recreation.")
					return err
				}

				// Do the Create() with the merged object so that we preserve external labels/annotations.
				resetMetadataForCreate(mobj)
				if err := c.create(ctx, mobj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to recreate service.", "obj", obj)
					return err
				}
				return nil
			}
		case *rbacv1.RoleBinding:
			curRoleBinding := cur.(*rbacv1.RoleBinding)
			objRoleBinding := obj.(*rbacv1.RoleBinding)
			if objRoleBinding.RoleRef.Name != curRoleBinding.RoleRef.Name {
				// RoleRef field of RoleBinding can't be modified, so delete and recreate the entire RoleBinding
				if err = c.delete(ctx, obj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to delete RoleBinding for recreation.")
					return err
				}

				// Do the Create() with the merged object so that we preserve external labels/annotations.
				resetMetadataForCreate(mobj)
				if err = c.create(ctx, mobj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to recreate RoleBinding")
					return err
				}
				return nil
			}
		case *rbacv1.ClusterRoleBinding:
			curClusterRoleBinding := cur.(*rbacv1.ClusterRoleBinding)
			objClusterRoleBinding := obj.(*rbacv1.ClusterRoleBinding)
			if objClusterRoleBinding.RoleRef.Name != curClusterRoleBinding.RoleRef.Name {
				// RoleRef field of ClusterRoleBinding can't be modified, so delete and recreate the entire ClusterRoleBinding
				if err = c.delete(ctx, obj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to delete ClusterRoleBinding for recreation.")
					return err
				}

				// Do the Create() with the merged object so that we preserve external labels/annotations.
				resetMetadataForCreate(mobj)
				if err = c.create(ctx, mobj); err != nil {
					logCtx.WithValues("key", key).Error(err, "Failed to recreate ClusterRoleBinding")
					return err
				}
				return nil
			}
		}
		if err := c.update(ctx, mobj); err != nil {
			logCtx.WithValues("key", key).Info("Failed to update object.")
			return err
		}
	}
	return nil
}

func resetMetadataForCreate(obj client.Object) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
}

func (c *componentHandler) CreateOrUpdateOrDelete(ctx context.Context, component render.Component, status status.StatusManager) error {
	// Before creating the component, make sure that it is ready. This provides a hook to do
	// dependency checking for the component.
	cmpLog := c.log.WithValues("component", reflect.TypeOf(component))
	cmpLog.V(2).Info("Checking if component is ready")
	if !component.Ready() {
		cmpLog.Info("Component is not ready, skipping")
		return nil
	}
	cmpLog.V(2).Info("Reconciling")

	// Iterate through each object that comprises the component and attempt to create it,
	// or update it if needed.
	var daemonSets []types.NamespacedName
	var deployments []types.NamespacedName
	var statefulsets []types.NamespacedName
	var cronJobs []types.NamespacedName

	objsToCreate, objsToDelete := component.Objects()
	osType := component.SupportedOSType()

	var alreadyExistsErr error = nil

	for _, obj := range objsToCreate {
		key := client.ObjectKeyFromObject(obj)

		// Pass in a DeepCopy so any modifications made by createOrUpdateObject won't be included
		// if we need to retry the function
		alreadyRetriedConflict := false
	conflictRetry:
		err := c.createOrUpdateObject(ctx, obj.DeepCopyObject().(client.Object), osType)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				// Remember that we've had an "already exists" error, but otherwise
				// carry on.
				alreadyExistsErr = err
			} else if errors.IsConflict(err) && !alreadyRetriedConflict {
				// If the error is a resource Conflict, try the update again.
				cmpLog.WithValues("key", key, "conflict_message", err).Info("Failed to update object, retrying.")
				alreadyRetriedConflict = true
				goto conflictRetry
			} else {
				cmpLog.Error(err, "Failed to create or update object", "key", key)
				return err
			}
		}

		// Keep track of some objects so we can report on their status.
		switch obj.(type) {
		case *apps.Deployment:
			deployments = append(deployments, key)
		case *apps.DaemonSet:
			daemonSets = append(daemonSets, key)
		case *apps.StatefulSet:
			statefulsets = append(statefulsets, key)
		case *batchv1.CronJob:
			cronJobs = append(cronJobs, key)
		}

		continue
	}

	if status != nil {
		// Add the objects to the status manager so we can report on their status.
		if len(daemonSets) > 0 {
			status.AddDaemonsets(daemonSets)
		}
		if len(deployments) > 0 {
			status.AddDeployments(deployments)
		}
		if len(statefulsets) > 0 {
			status.AddStatefulSets(statefulsets)
		}
		if len(cronJobs) > 0 {
			status.AddCronJobs(cronJobs)
		}
	}

	for _, obj := range objsToDelete {
		err := c.delete(ctx, obj)
		if err != nil && !errors.IsNotFound(err) {
			logCtx := ContextLoggerForResource(c.log, obj)
			logCtx.Error(err, fmt.Sprintf("Error deleting object %v", obj))
			return err
		}

		key := client.ObjectKeyFromObject(obj)
		if status != nil {
			switch obj.(type) {
			case *apps.Deployment:
				status.RemoveDeployments(key)
			case *apps.DaemonSet:
				status.RemoveDaemonsets(key)
			case *apps.StatefulSet:
				status.RemoveStatefulSets(key)
			case *batchv1.CronJob:
				status.RemoveCronJobs(key)
			}
		}
	}

	cmpLog.V(1).Info("Done reconciling component")
	// TODO Get each controller to explicitly call ReadyToMonitor on the status manager instead of doing it here.
	if status != nil {
		status.ReadyToMonitor()
	}

	// alreadyExistsErr is only non-nil if this component handler is in "create only" mode and
	// one (or more) of objsToCreate already existed.
	return alreadyExistsErr
}

// skipAddingOwnerReference returns true if owner is a namespaced resource and
// controlled object is a cluster scoped resource.
func skipAddingOwnerReference(owner, controlled metav1.Object) bool {
	ownerNs := owner.GetNamespace()
	controlledNs := controlled.GetNamespace()
	if ownerNs != "" && controlledNs == "" {
		return true
	}
	return false
}

func checkIfMultipleOwnersLabel(controlled metav1.Object) bool {
	labels := controlled.GetLabels()
	_, ok := labels[common.MultipleOwnersLabel]
	return ok
}

// mergeState returns the object to pass to Update given the current and desired object states.
func mergeState(desired client.Object, current runtime.Object) client.Object {
	// Take a copy of the desired object, so we can merge values into it without
	// adjusting the caller's copy.
	desired = desired.DeepCopyObject().(client.Object)

	currentMeta := current.(metav1.ObjectMetaAccessor).GetObjectMeta()
	desiredMeta := desired.(metav1.ObjectMetaAccessor).GetObjectMeta()

	// Merge common metadata fields if not present on the desired state.
	if desiredMeta.GetResourceVersion() == "" {
		desiredMeta.SetResourceVersion(currentMeta.GetResourceVersion())
	}
	if desiredMeta.GetUID() == "" {
		desiredMeta.SetUID(currentMeta.GetUID())
	}
	if reflect.DeepEqual(desiredMeta.GetCreationTimestamp(), metav1.Time{}) {
		desiredMeta.SetCreationTimestamp(currentMeta.GetCreationTimestamp())
	}

	// Update the generation on the desired object to match the current object.
	desiredMeta.SetGeneration(currentMeta.GetGeneration())

	// Merge annotations by reconciling the ones that components expect, but leaving everything else
	// as-is.
	currentAnnotations := common.MapExistsOrInitialize(currentMeta.GetAnnotations())
	desiredAnnotations := common.MapExistsOrInitialize(desiredMeta.GetAnnotations())
	mergedAnnotations := common.MergeMaps(currentAnnotations, desiredAnnotations)
	desiredMeta.SetAnnotations(mergedAnnotations)

	// Merge labels by reconciling the ones that components expect, but leaving everything else
	// as-is.
	currentLabels := common.MapExistsOrInitialize(currentMeta.GetLabels())
	desiredLabels := common.MapExistsOrInitialize(desiredMeta.GetLabels())
	mergedLabels := common.MergeMaps(currentLabels, desiredLabels)
	desiredMeta.SetLabels(mergedLabels)

	if checkIfMultipleOwnersLabel(desiredMeta) {
		currentOwnerReferences := currentMeta.GetOwnerReferences()
		desiredOwnerReferences := desiredMeta.GetOwnerReferences()
		mergedOwnerReferences := common.MergeOwnerReferences(desiredOwnerReferences, currentOwnerReferences)
		desiredMeta.SetOwnerReferences(mergedOwnerReferences)
		labels := desiredMeta.GetLabels()
		delete(labels, common.MultipleOwnersLabel)
		desiredMeta.SetLabels(labels)
	}

	switch desired.(type) {
	case *v1.Service:
		// Services are a special case since some fields (namely ClusterIP) are defaulted
		// and we need to maintain them on updates.
		cs := current.(*v1.Service)
		ds := desired.(*v1.Service)
		if ds.Spec.ClusterIP != "None" {
			// We want this service to keep its cluster IP.
			ds.Spec.ClusterIP = cs.Spec.ClusterIP
		}
		return ds
	case *batchv1.Job:
		cj := current.(*batchv1.Job)
		dj := desired.(*batchv1.Job)

		if len(cj.Spec.Template.Spec.Containers) != len(dj.Spec.Template.Spec.Containers) {
			return dj
		}

		for i := range cj.Spec.Template.Spec.Containers {
			if cj.Spec.Template.Spec.Containers[i].Image != dj.Spec.Template.Spec.Containers[i].Image {
				return dj
			}
		}

		// We're only comparing jobs based off of annotations and containers images for now so we can send a signal to recreate a job.
		// Later we might want to have some better comparison of jobs so that a changed in the container spec would trigger
		// a recreation of the job
		if reflect.DeepEqual(cj.Spec.Template.Annotations, dj.Spec.Template.Annotations) {
			return nil
		}

		return dj
	case *apps.Deployment:
		cd := current.(*apps.Deployment)
		dd := desired.(*apps.Deployment)
		// Only take the replica count if our desired count is nil so that
		// any Deployments where we specify a replica count we will retain
		// control over the count.
		if dd.Spec.Replicas == nil {
			dd.Spec.Replicas = cd.Spec.Replicas
		}

		// Merge the template's labels.
		currentLabels := common.MapExistsOrInitialize(cd.Spec.Template.GetObjectMeta().GetLabels())
		desiredLabels := common.MapExistsOrInitialize(dd.Spec.Template.GetObjectMeta().GetLabels())
		mergedLabels := common.MergeMaps(currentLabels, desiredLabels)
		dd.Spec.Template.SetLabels(mergedLabels)

		// Merge the template's annotations.
		currentAnnotations := common.MapExistsOrInitialize(cd.Spec.Template.GetObjectMeta().GetAnnotations())
		desiredAnnotations := common.MapExistsOrInitialize(dd.Spec.Template.GetObjectMeta().GetAnnotations())
		mergedAnnotations := common.MergeMaps(currentAnnotations, desiredAnnotations)
		dd.Spec.Template.SetAnnotations(mergedAnnotations)

		return dd
	case *apps.DaemonSet:
		cd := current.(*apps.DaemonSet)
		dd := desired.(*apps.DaemonSet)

		// Merge the template's labels.
		currentLabels := common.MapExistsOrInitialize(cd.Spec.Template.GetObjectMeta().GetLabels())
		desiredLabels := common.MapExistsOrInitialize(dd.Spec.Template.GetObjectMeta().GetLabels())
		mergedLabels := common.MergeMaps(currentLabels, desiredLabels)
		dd.Spec.Template.SetLabels(mergedLabels)

		// Merge the template's annotations.
		currentAnnotations := common.MapExistsOrInitialize(cd.Spec.Template.GetObjectMeta().GetAnnotations())
		desiredAnnotations := common.MapExistsOrInitialize(dd.Spec.Template.GetObjectMeta().GetAnnotations())
		mergedAnnotations := common.MergeMaps(currentAnnotations, desiredAnnotations)
		dd.Spec.Template.SetAnnotations(mergedAnnotations)

		return dd
	case *v1.ServiceAccount:
		// ServiceAccounts generate a new token if we don't include the existing one.
		csa := current.(*v1.ServiceAccount)
		dsa := desired.(*v1.ServiceAccount)
		if len(csa.Secrets) != 0 && len(dsa.Secrets) == 0 {
			// Only copy the secrets if they exist, and we haven't specified them explicitly
			// on the new object.
			dsa.Secrets = csa.Secrets
		}
		if len(csa.ImagePullSecrets) != 0 && len(dsa.ImagePullSecrets) == 0 {
			// For example on OCP, the service account gets ImagePullSecrets added. If we don't merge this into the
			// object, we will create a version update, immediately followed by another update by their controller,
			// and then our controllers watching the service account will reconcile again, causing a loop.
			dsa.ImagePullSecrets = csa.ImagePullSecrets
		}
		return dsa
	case *esv1.Elasticsearch:
		// Only update if the spec has changed
		csa := current.(*esv1.Elasticsearch)
		dsa := desired.(*esv1.Elasticsearch)

		if reflect.DeepEqual(csa.Spec, dsa.Spec) {
			return csa
		}

		// ECK sets these values so we need to copy them over to avoid and update battle
		// Note: This should be revisited when the ECK version moves to GA, as it would be impossible to remove annotations
		// or finalizers from Elasticsearch.
		dsa.Annotations = csa.Annotations
		dsa.Finalizers = csa.Finalizers
		dsa.Status = csa.Status
		return dsa
	case *kbv1.Kibana:
		// Only update if the spec has changed
		csa := current.(*kbv1.Kibana)
		dsa := desired.(*kbv1.Kibana)
		if reflect.DeepEqual(csa.Spec, dsa.Spec) {
			return csa
		}

		// ECK sets these values so we need to copy them over to avoid and update battle
		// Note: This should be revisited when the ECK version moves to GA, as it would be impossible to remove annotations
		// or finalizers from Kibana.
		dsa.Annotations = csa.Annotations
		dsa.Finalizers = csa.Finalizers
		dsa.Spec.ElasticsearchRef = csa.Spec.ElasticsearchRef
		dsa.Status = csa.Status
		return dsa
	case *v3.UISettings:
		// Only update if the spec has changed
		cui := current.(*v3.UISettings)
		dui := desired.(*v3.UISettings)
		if reflect.DeepEqual(cui.Spec, dui.Spec) {
			return cui
		}

		// UISettings are always owned by the group, so never modify the OwnerReferences that are returned by the
		// APIServer.
		dui.SetOwnerReferences(cui.GetOwnerReferences())
		return dui
	case *v3.NetworkPolicy:
		cnp := current.(*v3.NetworkPolicy)
		dnp := desired.(*v3.NetworkPolicy)
		if reflect.DeepEqual(cnp.Spec, dnp.Spec) {
			return nil
		}
		return dnp
	case *v3.Tier:
		ct := current.(*v3.Tier)
		dt := desired.(*v3.Tier)
		if reflect.DeepEqual(ct.Spec, dt.Spec) {
			return nil
		}
		return dt
	default:
		// Default to just using the desired state, with an updated RV.
		return desired
	}
}

// modifyPodSpec is a helper for pulling out pod specifications from an arbitrary object.
func modifyPodSpec(obj client.Object, f func(*v1.PodSpec)) {
	switch x := obj.(type) {
	case *v1.PodTemplate:
		f(&x.Template.Spec)
	case *apps.Deployment:
		f(&x.Spec.Template.Spec)
	case *apps.DaemonSet:
		f(&x.Spec.Template.Spec)
	case *apps.StatefulSet:
		f(&x.Spec.Template.Spec)
	case *batchv1.CronJob:
		f(&x.Spec.JobTemplate.Spec.Template.Spec)
	case *batchv1.Job:
		f(&x.Spec.Template.Spec)
	case *kbv1.Kibana:
		f(&x.Spec.PodTemplate.Spec)
	case *esv1.Elasticsearch:
		// elasticsearch resource describes multiple nodeSets which each have a pod spec.
		nodeSets := x.Spec.NodeSets
		for i := range nodeSets {
			f(&nodeSets[i].PodTemplate.Spec)
		}
	}
}

// setImagePullPolicy ensures that an image pull policy is set if not set already.
func setImagePullPolicy(podSpec *v1.PodSpec) {
	for i := range podSpec.Containers {
		if len(podSpec.Containers[i].ImagePullPolicy) == 0 {
			podSpec.Containers[i].ImagePullPolicy = v1.PullIfNotPresent
		}
	}
}

// ensureTLSCiphers sets the TLSCipherSuites configuration as a Env Var to the Deployments and DaemonSets.
func ensureTLSCiphers(ctx context.Context, obj client.Object, c client.Client) error {
	var containers []v1.Container
	switch obj := obj.(type) {
	case *apps.Deployment:
		containers = obj.Spec.Template.Spec.Containers
	case *apps.DaemonSet:
		containers = obj.Spec.Template.Spec.Containers
	default:
		return nil
	}

	_, installationSpec, err := GetInstallation(ctx, c)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		} else {
			return err
		}
	}

	for i := range containers {
		exists := false
		for _, envVar := range containers[i].Env {
			if envVar.Name == TLS_CIPHERS_ENV_VAR_NAME {
				exists = true
				break
			}
		}
		envVarValue := installationSpec.TLSCipherSuites.ToString()
		if !exists && envVarValue != "" {
			containers[i].Env = append(containers[i].Env, v1.EnvVar{
				Name:  TLS_CIPHERS_ENV_VAR_NAME,
				Value: envVarValue,
			})
		}
	}

	return nil
}

func orderVolumes(podSpec *v1.PodSpec) {
	slices.SortFunc(podSpec.Volumes, func(a, b v1.Volume) int {
		return strings.Compare(a.Name, b.Name)
	})
}

func orderVolumeMounts(podSpec *v1.PodSpec) {
	for _, container := range podSpec.Containers {
		slices.SortFunc(container.VolumeMounts, func(a, b v1.VolumeMount) int {
			return strings.Compare(a.Name, b.Name)
		})
	}
}

// ensureOSSchedulingRestrictions ensures that if obj is a type that creates pods and if osType is not OSTypeAny that a
// node selector is set on the pod template for the "kubernetes.io/os" label to ensure that the pod is scheduled
// on a node running an operating system as specified by osType.
func ensureOSSchedulingRestrictions(obj client.Object, osType rmeta.OSType) {
	if osType == rmeta.OSTypeAny {
		return
	}

	// Some object types don't have a v1.PodSpec an instead use a custom spec. Handle those here.
	switch x := obj.(type) {
	case *monitoringv1.Alertmanager:
		// Prometheus operator types don't have a template spec which is of v1.PodSpec type.
		// We can't add it to the podSpecs list and assign osType in the for loop below.
		podSpec := &x.Spec
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = make(map[string]string)
		}
		podSpec.NodeSelector["kubernetes.io/os"] = string(osType)
		return
	case *monitoringv1.Prometheus:
		// Prometheus operator types don't have a template spec which is of v1.PodSpec type.
		// We can't add it to the podSpecs list and assign osType in the for loop below.
		podSpec := &x.Spec
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = make(map[string]string)
		}
		podSpec.NodeSelector["kubernetes.io/os"] = string(osType)
		return
	}

	// Handle objects that do use a v1.PodSpec.
	f := func(podSpec *v1.PodSpec) {
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = make(map[string]string)
		}
		podSpec.NodeSelector["kubernetes.io/os"] = string(osType)
	}
	modifyPodSpec(obj, f)
}

// setProbeTimeouts modifies liveness and readiness probe default values if they are not set in the object.
// Default values from k8s are sometimes too small, e.g., 1s for timeout, and Calico components might
// be restarted prematurely. This function updates some threshold and seconds to a larger value when
// they are not set in probes.
//
// For liveness probe: timeout defaults to 5s and period to 60s so that one component will be restarted
// after around 3 minutes (3 default failure threshold).
// For readiness probe: timeout defaults to 5s and period to 30s so that one component will be removed
// from service after 1.5 minute (3 default failure threshold).
func setProbeTimeouts(obj client.Object) {
	const (
		failureThreshold            = 3
		livenessProbePeriodSeconds  = 60
		readinessProbePeriodSeconds = 30
		successThreshold            = 1
		timeoutSeconds              = 5
	)

	var containers []v1.Container
	switch obj := obj.(type) {
	case *apps.Deployment:
		containers = obj.Spec.Template.Spec.Containers
	case *apps.DaemonSet:
		containers = obj.Spec.Template.Spec.Containers
	case *esv1.Elasticsearch:
		for _, nodeset := range obj.Spec.NodeSets {
			containers = append(containers, nodeset.PodTemplate.Spec.Containers...)
		}
	case *kbv1.Kibana:
		containers = obj.Spec.PodTemplate.Spec.Containers
	case *monitoringv1.Prometheus:
		containers = obj.Spec.Containers
	default:
		return
	}

	for _, container := range containers {
		if container.LivenessProbe != nil {
			lp := container.LivenessProbe
			if lp.FailureThreshold == 0 {
				lp.FailureThreshold = failureThreshold
			}
			if lp.PeriodSeconds == 0 {
				lp.PeriodSeconds = livenessProbePeriodSeconds
			}
			if lp.SuccessThreshold == 0 {
				lp.SuccessThreshold = successThreshold
			}
			if lp.TimeoutSeconds == 0 {
				lp.TimeoutSeconds = timeoutSeconds
			}
		}

		if container.ReadinessProbe != nil {
			rp := container.ReadinessProbe
			if rp.FailureThreshold == 0 {
				rp.FailureThreshold = failureThreshold
			}
			if rp.PeriodSeconds == 0 {
				rp.PeriodSeconds = readinessProbePeriodSeconds
			}
			if rp.SuccessThreshold == 0 {
				rp.SuccessThreshold = successThreshold
			}
			if rp.TimeoutSeconds == 0 {
				rp.TimeoutSeconds = timeoutSeconds
			}
		}
	}
}

// setStandardSelectorAndLabels will set the k8s-app and app.kubernetes.io/name Labels on the podTemplates
// for Deployments and Daemonsets. If there is no Selector specified a selector will also be added
// that selects the k8s-app label.
func setStandardSelectorAndLabels(obj client.Object) {
	var podTemplate *v1.PodTemplateSpec
	var name string
	switch obj := obj.(type) {
	case *apps.Deployment:
		d := obj
		name = d.ObjectMeta.Name
		if d.ObjectMeta.Labels == nil {
			d.ObjectMeta.Labels = make(map[string]string)
		}
		d.ObjectMeta.Labels["k8s-app"] = name
		d.ObjectMeta.Labels["app.kubernetes.io/name"] = name
		if d.Spec.Selector == nil {
			d.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"k8s-app": name,
				},
			}
		}
		podTemplate = &d.Spec.Template
	case *apps.DaemonSet:
		d := obj
		name = d.ObjectMeta.Name
		if d.Spec.Selector == nil {
			d.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"k8s-app": name,
				},
			}
		}
		podTemplate = &d.Spec.Template
	default:
		return
	}

	if podTemplate.ObjectMeta.Labels == nil {
		podTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	if podTemplate.ObjectMeta.Labels["k8s-app"] == "" {
		podTemplate.ObjectMeta.Labels["k8s-app"] = name
	}
	if podTemplate.ObjectMeta.Labels["app.kubernetes.io/name"] == "" {
		podTemplate.ObjectMeta.Labels["app.kubernetes.io/name"] = name
	}
}

// ReadyFlag is used to synchronize access to a boolean flag
// flag that can be shared between go routines. The flag can be
// marked as ready once,as part of a initialization procedure and
// read multiple times afterwards
type ReadyFlag struct {
	mu      sync.RWMutex
	isReady bool
}

// IsReady returns true if was marked as ready
func (r *ReadyFlag) IsReady() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isReady
}

// MarkAsReady sets the flag as true
func (r *ReadyFlag) MarkAsReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isReady = true
}
