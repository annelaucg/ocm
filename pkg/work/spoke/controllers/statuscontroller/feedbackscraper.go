package statuscontroller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	workapiv1 "open-cluster-management.io/api/work/v1"
)

type ResourceTarget struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

// FeedbackUpdate is a callback invoked when a watched resource changes
type FeedbackUpdate func(mwName string, meta workapiv1.ManifestResourceMeta, obj *unstructured.Unstructured, deleted bool)

// FeedbackScraper abstracts how status feedback is collected (poll or watch)
type FeedbackScraper interface {
	UpsertTarget(mwName string, meta workapiv1.ManifestResourceMeta, target ResourceTarget)
	RemoveTarget(mwName string, meta workapiv1.ManifestResourceMeta)
	Start(ctx context.Context)
	Stop()
}
