package statuscontroller

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	workapiv1 "open-cluster-management.io/api/work/v1"
)

type watchScraper struct {
	dynClient dynamic.Interface
	cb        FeedbackUpdate

	factory   dynamicinformer.DynamicSharedInformerFactory
	informers map[schema.GroupVersionResource]cache.SharedIndexInformer

	// key: gvr|ns|name -> set(mwName)
	watchers map[string]sets.Set[string]
	mu       sync.RWMutex
}

func NewWatchScraper(dyn dynamic.Interface, cb FeedbackUpdate) *watchScraper {
	return &watchScraper{
		dynClient: dyn,
		cb:        cb,
		factory:   dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0),
		informers: map[schema.GroupVersionResource]cache.SharedIndexInformer{},
		watchers:  map[string]sets.Set[string]{},
	}
}

func (w *watchScraper) Start(ctx context.Context) {
	go w.factory.Start(ctx.Done())
}

func (w *watchScraper) Stop() {}

func (w *watchScraper) UpsertTarget(mwName string, meta workapiv1.ManifestResourceMeta, target ResourceTarget) {
	gvr := target.GVR
	key := w.keyFor(target)

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.watchers[key]; !ok {
		w.watchers[key] = sets.New[string]()
	}
	w.watchers[key].Insert(mwName)

	if _, ok := w.informers[gvr]; !ok {
		inf := w.factory.ForResource(gvr).Informer()
		inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    w.onEvent(gvr, false),
			UpdateFunc: func(oldObj, newObj interface{}) { w.onEvent(gvr, false)(newObj) },
			DeleteFunc: w.onEvent(gvr, true),
		})
		w.informers[gvr] = inf
	}
}

func (w *watchScraper) RemoveTarget(mwName string, meta workapiv1.ManifestResourceMeta) {
	gvr := schema.GroupVersionResource{Group: meta.Group, Version: meta.Version, Resource: meta.Resource}
	key := fmt.Sprintf("%s|%s|%s|%s", gvr.String(), meta.Namespace, meta.Name, "")
	w.mu.Lock()
	if set, ok := w.watchers[key]; ok {
		set.Delete(mwName)
		if set.Len() == 0 {
			delete(w.watchers, key)
		}
	}
	w.mu.Unlock()
}

func (w *watchScraper) onEvent(gvr schema.GroupVersionResource, deleted bool) func(obj interface{}) {
	return func(obj interface{}) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				if uu, ok := tomb.Obj.(*unstructured.Unstructured); ok {
					u = uu
				} else {
					return
				}
			} else {
				return
			}
		}
		key := fmt.Sprintf("%s|%s|%s|%s", gvr.String(), u.GetNamespace(), u.GetName(), "")
		w.mu.RLock()
		targets, ok := w.watchers[key]
		if !ok {
			w.mu.RUnlock()
			return
		}
		names := targets.UnsortedList()
		w.mu.RUnlock()

		meta := workapiv1.ManifestResourceMeta{
			Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
			Namespace: u.GetNamespace(), Name: u.GetName(),
		}
		for _, mw := range names {
			w.cb(mw, meta, u, deleted)
		}
	}
}

func (w *watchScraper) keyFor(t ResourceTarget) string {
	return fmt.Sprintf("%s|%s|%s|%s", t.GVR.String(), t.Namespace, t.Name, "")
}
