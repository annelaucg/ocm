package statuscontroller

import (
	"context"
	"fmt"
	"time"

	"sync"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"

	workv1client "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	workinformer "open-cluster-management.io/api/client/work/informers/externalversions/work/v1"
	worklister "open-cluster-management.io/api/client/work/listers/work/v1"
	workapiv1 "open-cluster-management.io/api/work/v1"
	"open-cluster-management.io/sdk-go/pkg/patcher"

	commonhelper "open-cluster-management.io/ocm/pkg/common/helpers"
	"open-cluster-management.io/ocm/pkg/common/queue"
	"open-cluster-management.io/ocm/pkg/work/helper"
	"open-cluster-management.io/ocm/pkg/work/spoke/conditions"
	"open-cluster-management.io/ocm/pkg/work/spoke/statusfeedback"
)

const statusFeedbackConditionType = "StatusFeedbackSynced"

// AvailableStatusController is to update the available status conditions of both manifests and manifestworks.
// It is also used to get the status value based on status feedback configuration and get condition values
// based on condition rules in manifestwork. The functions are logically disinct, however, they are put in the same
// control loop to reduce live get call to spoke apiserver and status update call to hub apiserver.
type AvailableStatusController struct {
	patcher            patcher.Patcher[*workapiv1.ManifestWork, workapiv1.ManifestWorkSpec, workapiv1.ManifestWorkStatus]
	manifestWorkLister worklister.ManifestWorkNamespaceLister
	spokeDynamicClient dynamic.Interface
	statusReader       *statusfeedback.StatusReader
	conditionReader    *conditions.ConditionReader
	syncInterval       time.Duration

	// watch-based scraping
	watchScraper   FeedbackScraper
	watchStartOnce sync.Once
}

// NewAvailableStatusController returns a AvailableStatusController
func NewAvailableStatusController(
	recorder events.Recorder,
	spokeDynamicClient dynamic.Interface,
	manifestWorkClient workv1client.ManifestWorkInterface,
	manifestWorkInformer workinformer.ManifestWorkInformer,
	manifestWorkLister worklister.ManifestWorkNamespaceLister,
	maxJSONRawLength int32,
	syncInterval time.Duration,
) (factory.Controller, error) {
	conditionReader, err := conditions.NewConditionReader()
	if err != nil {
		return nil, err
	}

	controller := &AvailableStatusController{
		patcher: patcher.NewPatcher[
			*workapiv1.ManifestWork, workapiv1.ManifestWorkSpec, workapiv1.ManifestWorkStatus](
			manifestWorkClient),
		manifestWorkLister: manifestWorkLister,
		spokeDynamicClient: spokeDynamicClient,
		syncInterval:       syncInterval,
		statusReader:       statusfeedback.NewStatusReader().WithMaxJsonRawLength(maxJSONRawLength),
		conditionReader:    conditionReader,
	}

	// initialize watch scraper with callback to patch one resource status
	watchCb := func(mwName string, resMeta workapiv1.ManifestResourceMeta, obj *unstructured.Unstructured, deleted bool) {
		_ = controller.patchOneResourceStatus(context.TODO(), mwName, resMeta, obj, deleted)
	}
	controller.watchScraper = NewWatchScraper(spokeDynamicClient, watchCb)

	return factory.New().
		WithInformersQueueKeysFunc(queue.QueueKeyByMetaName, manifestWorkInformer.Informer()).
		WithSync(controller.sync).ToController("AvailableStatusController", recorder), nil
}

func (c *AvailableStatusController) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	manifestWorkName := controllerContext.QueueKey()
	// sync a particular manifestwork
	manifestWork, err := c.manifestWorkLister.Get(manifestWorkName)
	if errors.IsNotFound(err) {
		// work not found, could have been deleted, do nothing.
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to fetch manifestwork %q: %w", manifestWorkName, err)
	}

	// lazily start watch scraper once we have a context
	c.watchStartOnce.Do(func() { c.watchScraper.Start(ctx) })

	err = c.syncManifestWork(ctx, manifestWork)
	if err != nil {
		return fmt.Errorf("unable to sync manifestwork %q: %w", manifestWork.Name, err)
	}

	// requeue with a certain jitter
	controllerContext.Queue().AddAfter(manifestWorkName, wait.Jitter(c.syncInterval, 0.9))
	return nil
}

func (c *AvailableStatusController) syncManifestWork(ctx context.Context, originalManifestWork *workapiv1.ManifestWork) error {
	klog.Infof("Reconciling ManifestWork %q", originalManifestWork.Name)
	manifestWork := originalManifestWork.DeepCopy()

	// do nothing when finalizer is not added.
	if !commonhelper.HasFinalizer(manifestWork.Finalizers, workapiv1.ManifestWorkFinalizer) {
		return nil
	}

	// wait until work has the applied condition.
	if cond := meta.FindStatusCondition(manifestWork.Status.Conditions, workapiv1.WorkApplied); cond == nil {
		return nil
	}

	// handle status condition of manifests
	// TODO revist this controller since this might bring races when user change the manifests in spec.
	for index, manifest := range manifestWork.Status.ResourceStatus.Manifests {
		obj, availableStatusCondition, err := buildAvailableStatusCondition(manifest.ResourceMeta, c.spokeDynamicClient)
		manifestConditions := &manifestWork.Status.ResourceStatus.Manifests[index].Conditions
		meta.SetStatusCondition(manifestConditions, availableStatusCondition)
		if err != nil {
			// skip getting status values if resource is not available.
			continue
		}

		option := helper.FindManifestConfiguration(manifest.ResourceMeta, manifestWork.Spec.ManifestConfigs)
		klog.Infof("Feedback scrape type: %s", option.FeedbackScrapeType)

		// If resource is configured for WATCH, register target in the watch scraper infra; else ensure removal
		if option != nil && option.FeedbackScrapeType == workapiv1.FeedbackScrapeTypeWATCH {
			klog.Infof("WATCH feedback enabled: registering target for ManifestWork %q, GVR=%s/%s/%s, ns/name=%s/%s",
				manifestWork.Name,
				manifest.ResourceMeta.Group, manifest.ResourceMeta.Version, manifest.ResourceMeta.Resource,
				manifest.ResourceMeta.Namespace, manifest.ResourceMeta.Name)
			gvr := schema.GroupVersionResource{Group: manifest.ResourceMeta.Group, Version: manifest.ResourceMeta.Version, Resource: manifest.ResourceMeta.Resource}
			target := ResourceTarget{GVR: gvr, Namespace: manifest.ResourceMeta.Namespace, Name: manifest.ResourceMeta.Name}
			c.watchScraper.UpsertTarget(manifestWork.Name, manifest.ResourceMeta, target)
		} else {
			c.watchScraper.RemoveTarget(manifestWork.Name, manifest.ResourceMeta)
		}

		// Read status of the resource according to feedback rules.
		values, statusFeedbackCondition := c.getFeedbackValues(obj, option)
		meta.SetStatusCondition(manifestConditions, statusFeedbackCondition)
		manifestWork.Status.ResourceStatus.Manifests[index].StatusFeedbacks.Values = values

		// Update manifest conditions according to condition rules
		c.evaluateConditionRules(ctx, manifestConditions, obj, option, manifestWork.Generation)
	}

	// aggregate condtions generated by rules and update work conditions
	aggregatedConditions := conditions.AggregateManifestConditions(manifestWork.Generation, manifestWork.Status.ResourceStatus.Manifests)
	for _, condition := range aggregatedConditions {
		meta.SetStatusCondition(&manifestWork.Status.Conditions, condition)
	}
	conditions.PruneConditionsGeneratedByConditionRules(&manifestWork.Status.Conditions, manifestWork.Generation)

	// no work if the status of manifestwork does not change
	if equality.Semantic.DeepEqual(originalManifestWork.Status.ResourceStatus, manifestWork.Status.ResourceStatus) &&
		equality.Semantic.DeepEqual(originalManifestWork.Status.Conditions, manifestWork.Status.Conditions) {
		return nil
	}

	// update status of manifestwork. if this conflicts, try again later
	_, err := c.patcher.PatchStatus(ctx, manifestWork, manifestWork.Status, originalManifestWork.Status)
	return err
}

// patchOneResourceStatus updates a single manifest's status in the given manifestwork
func (c *AvailableStatusController) patchOneResourceStatus(ctx context.Context, mwName string, resMeta workapiv1.ManifestResourceMeta, obj *unstructured.Unstructured, deleted bool) error {
	klog.Infof("WATCH event: updating status for ManifestWork %q, GVR=%s/%s/%s, ns/name=%s/%s, deleted=%t",
		mwName, resMeta.Group, resMeta.Version, resMeta.Resource, resMeta.Namespace, resMeta.Name, deleted)
	// fetch from lister
	work, err := c.manifestWorkLister.Get(mwName)
	if err != nil {
		return err
	}
	copy := work.DeepCopy()

	// locate manifest index
	idx := -1
	for i, m := range copy.Status.ResourceStatus.Manifests {
		if m.ResourceMeta.Group == resMeta.Group && m.ResourceMeta.Version == resMeta.Version && m.ResourceMeta.Resource == resMeta.Resource && m.ResourceMeta.Namespace == resMeta.Namespace && m.ResourceMeta.Name == resMeta.Name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	manifestConditions := &copy.Status.ResourceStatus.Manifests[idx].Conditions
	cond := metav1.Condition{Type: workapiv1.ManifestAvailable}
	if deleted {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ResourceNotAvailable"
		cond.Message = "Resource is not available"
		meta.SetStatusCondition(manifestConditions, cond)
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "ResourceAvailable"
		cond.Message = "Resource is available"
		meta.SetStatusCondition(manifestConditions, cond)

		// recompute feedback values via existing logic
		option := helper.FindManifestConfiguration(copy.Status.ResourceStatus.Manifests[idx].ResourceMeta, copy.Spec.ManifestConfigs)
		values, statusFeedbackCondition := c.getFeedbackValues(obj, option)
		meta.SetStatusCondition(manifestConditions, statusFeedbackCondition)
		copy.Status.ResourceStatus.Manifests[idx].StatusFeedbacks.Values = values

		// evaluate condition rules
		c.evaluateConditionRules(ctx, manifestConditions, obj, option, copy.Generation)
	}

	// aggregate and patch
	aggregated := conditions.AggregateManifestConditions(copy.Generation, copy.Status.ResourceStatus.Manifests)
	for _, ac := range aggregated {
		meta.SetStatusCondition(&copy.Status.Conditions, ac)
	}
	conditions.PruneConditionsGeneratedByConditionRules(&copy.Status.Conditions, copy.Generation)

	if equality.Semantic.DeepEqual(work.Status, copy.Status) {
		return nil
	}
	_, err = c.patcher.PatchStatus(ctx, copy, copy.Status, work.Status)
	return err
}

func (c *AvailableStatusController) getFeedbackValues(
	obj *unstructured.Unstructured,
	option *workapiv1.ManifestConfigOption) ([]workapiv1.FeedbackValue, metav1.Condition) {
	var errs []error
	var values []workapiv1.FeedbackValue

	if option == nil || len(option.FeedbackRules) == 0 {
		return values, metav1.Condition{
			Type:   statusFeedbackConditionType,
			Reason: "NoStatusFeedbackSynced",
			Status: metav1.ConditionTrue,
		}
	}

	for _, rule := range option.FeedbackRules {
		valuesByRule, err := c.statusReader.GetValuesByRule(obj, rule)
		if err != nil {
			errs = append(errs, err)
		}
		if len(valuesByRule) > 0 {
			values = append(values, valuesByRule...)
		}
	}

	err := utilerrors.NewAggregate(errs)

	if err != nil {
		return values, metav1.Condition{
			Type:    statusFeedbackConditionType,
			Reason:  "StatusFeedbackSyncFailed",
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Sync status feedback failed with error %v", err),
		}
	}

	return values, metav1.Condition{
		Type:   statusFeedbackConditionType,
		Reason: "StatusFeedbackSynced",
		Status: metav1.ConditionTrue,
	}
}

// evaluateConditionRules updates manifestConditions based on configured condition rules for the manifest
func (c *AvailableStatusController) evaluateConditionRules(ctx context.Context,
	manifestConditions *[]metav1.Condition, obj *unstructured.Unstructured, option *workapiv1.ManifestConfigOption, generation int64,
) {
	// Evaluate rules
	var newConditions []metav1.Condition
	if option != nil && len(option.ConditionRules) > 0 {
		newConditions = c.conditionReader.EvaluateConditions(ctx, obj, option.ConditionRules)
	}

	// Update manifest conditions with latest condition rule results and observed generation
	for _, condition := range newConditions {
		condition.ObservedGeneration = generation
		meta.SetStatusCondition(manifestConditions, condition)
	}

	// Remove conditions set by old rules that no longer exist
	// Conditions are merged into the existing slice because they are managed in multiple controllers
	// (e.g. manifestwork_reconciler adds the "Applied" condition), so we must explicitly
	// delete conditions that were created by rules which no longer exist.
	conditions.PruneConditionsGeneratedByConditionRules(manifestConditions, generation)
}

// buildAvailableStatusCondition returns a StatusCondition with type Available for a given manifest resource
func buildAvailableStatusCondition(resourceMeta workapiv1.ManifestResourceMeta,
	dynamicClient dynamic.Interface) (*unstructured.Unstructured, metav1.Condition, error) {
	conditionType := workapiv1.ManifestAvailable

	if len(resourceMeta.Resource) == 0 || len(resourceMeta.Version) == 0 || len(resourceMeta.Name) == 0 {
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  "IncompletedResourceMeta",
			Message: "Resource meta is incompleted",
		}, fmt.Errorf("incomplete resource meta")
	}

	gvr := schema.GroupVersionResource{
		Group:    resourceMeta.Group,
		Version:  resourceMeta.Version,
		Resource: resourceMeta.Resource,
	}

	obj, err := dynamicClient.Resource(gvr).Namespace(resourceMeta.Namespace).Get(context.TODO(), resourceMeta.Name, metav1.GetOptions{})

	switch {
	case errors.IsNotFound(err):
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionFalse,
			Reason:  "ResourceNotAvailable",
			Message: "Resource is not available",
		}, err
	case err != nil:
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  "FetchingResourceFailed",
			Message: fmt.Sprintf("Failed to fetch resource: %v", err),
		}, err
	}

	return obj, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  "ResourceAvailable",
		Message: "Resource is available",
	}, nil
}
