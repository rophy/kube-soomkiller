package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// PodInformer provides a node-scoped cache of pods with UID-based lookup.
type PodInformer struct {
	informer cache.SharedIndexInformer
	indexer  cache.Indexer
}

const (
	// uidIndex is the name of the custom indexer for pod UIDs
	uidIndex = "uid"
)

// NewPodInformer creates an informer that watches only pods on the specified node.
func NewPodInformer(client kubernetes.Interface, nodeName string, resyncPeriod time.Duration) *PodInformer {
	listWatcher := cache.NewListWatchFromClient(
		client.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
	)

	informer := cache.NewSharedIndexInformer(
		listWatcher,
		&corev1.Pod{},
		resyncPeriod,
		cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			uidIndex:             uidIndexFunc,
		},
	)

	return &PodInformer{
		informer: informer,
		indexer:  informer.GetIndexer(),
	}
}

// uidIndexFunc indexes pods by their UID
func uidIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	return []string{string(pod.UID)}, nil
}

// Run starts the informer. Call this in a goroutine.
func (p *PodInformer) Run(stopCh <-chan struct{}) {
	klog.Info("Starting pod informer")
	p.informer.Run(stopCh)
}

// WaitForCacheSync blocks until the informer cache is synced.
func (p *PodInformer) WaitForCacheSync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, p.informer.HasSynced)
}

// GetPodByUID returns the pod with the given UID, or nil if not found.
func (p *PodInformer) GetPodByUID(uid string) *corev1.Pod {
	objs, err := p.indexer.ByIndex(uidIndex, uid)
	if err != nil {
		klog.InfoS("Failed to look up pod by UID", "uid", uid, "err", err)
		return nil
	}
	if len(objs) == 0 {
		return nil
	}

	pod, ok := objs[0].(*corev1.Pod)
	if !ok {
		return nil
	}

	return pod
}

// ListPods returns all pods currently in the cache.
func (p *PodInformer) ListPods() []*corev1.Pod {
	objs := p.indexer.List()
	pods := make([]*corev1.Pod, 0, len(objs))
	for _, obj := range objs {
		if pod, ok := obj.(*corev1.Pod); ok {
			pods = append(pods, pod)
		}
	}
	return pods
}
