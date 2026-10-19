package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"horopter.dev/horopter/internal/flux"
)

// resource is one kind the client watches; eventsAbout narrows core Events to one Flux kind.
type resource struct {
	gvr         schema.GroupVersionResource
	listKind    string
	eventsAbout string
}

var (
	gitRepositories = resource{
		gvr: schema.GroupVersionResource{
			Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories",
		},
		listKind: "GitRepositoryList",
	}
	kustomizations = resource{
		gvr: schema.GroupVersionResource{
			Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations",
		},
		listKind: "KustomizationList",
	}
	helmReleases = resource{
		gvr: schema.GroupVersionResource{
			Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases",
		},
		listKind: "HelmReleaseList",
	}
	events = schema.GroupVersionResource{Version: "v1", Resource: "events"}

	watched = []resource{
		gitRepositories, kustomizations, helmReleases,
		{gvr: events, listKind: "EventList", eventsAbout: "GitRepository"},
		{gvr: events, listKind: "EventList", eventsAbout: "Kustomization"},
		{gvr: events, listKind: "EventList", eventsAbout: "HelmRelease"},
	}
)

// titleWait bounds the wait for a NewArtifact Event's commit subject; variables for tests.
var (
	titleWait  = time.Minute
	titleRetry = 5 * time.Second
)

// watcher needs no lock on releases: only the HelmRelease informer's goroutine touches it.
type watcher struct {
	cluster    string
	poster     *poster
	now        func() time.Time
	releases   map[string]flux.ReleaseMemory
	gitEvents  cache.Store
	repos      cache.Store
	gitQueue   workqueue.TypedDelayingInterface[string]
	gitStarted map[string]flux.GitRepository
}

func watch(ctx context.Context, dyn dynamic.Interface, cluster string, p *poster) error {
	w := &watcher{
		cluster: cluster, poster: p, now: time.Now,
		releases:   map[string]flux.ReleaseMemory{},
		gitQueue:   workqueue.NewTypedDelayingQueue[string](),
		gitStarted: map[string]flux.GitRepository{},
	}
	var synced []cache.InformerSynced
	for _, r := range watched {
		informer := dynamicinformer.NewFilteredDynamicInformer(dyn, r.gvr, metav1.NamespaceAll,
			0, cache.Indexers{}, eventSelector(r.eventsAbout)).Informer()
		if _, err := informer.AddEventHandler(w.handler(r)); err != nil {
			return fmt.Errorf("watch %s: %w", r.gvr.Resource, err)
		}
		switch {
		case r == gitRepositories:
			w.repos = informer.GetStore()
		case r.eventsAbout == "GitRepository":
			w.gitEvents = informer.GetStore()
		}
		go informer.Run(ctx.Done())
		synced = append(synced, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return errors.New("stopped before the first list of Flux objects completed")
	}
	slog.Info("listed every Flux object; watching")
	go w.runGitQueue(ctx)
	p.run(ctx)
	return nil
}

func eventSelector(kind string) dynamicinformer.TweakListOptionsFunc {
	if kind == "" {
		return nil
	}
	return func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("involvedObject.kind", kind).String()
	}
}

func (w *watcher) handler(r resource) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { w.observe(r, nil, obj) },
		UpdateFunc: func(old, cur any) {
			if old.(*unstructured.Unstructured).GetResourceVersion() !=
				cur.(*unstructured.Unstructured).GetResourceVersion() {
				w.observe(r, old, cur)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok && r == helmReleases {
				delete(w.releases, string(u.GetUID()))
			}
		},
	}
}

// observe maps one object's update. old is nil when the object is first listed or added.
func (w *watcher) observe(r resource, old, cur any) {
	switch {
	case r == gitRepositories:
		key, err := cache.MetaNamespaceKeyFunc(cur)
		if err != nil {
			slog.Error("could not key a GitRepository; skipping it", "error", err)
			return
		}
		w.gitQueue.Add(key)
	case r == kustomizations:
		observeAs(old, cur, func(prev *flux.Kustomization, ks flux.Kustomization) {
			w.poster.enqueue(flux.MapKustomization(w.cluster, prev, ks))
		})
	case r == helmReleases:
		observeAs(old, cur, func(prev *flux.HelmRelease, hr flux.HelmRelease) {
			evs, mem := flux.MapHelmRelease(w.cluster, w.releases[hr.Metadata.UID], prev, hr)
			w.releases[hr.Metadata.UID] = mem
			w.poster.enqueue(evs)
		})
	case r.eventsAbout != "":
		observeAs(nil, cur, func(_ *flux.Event, ev flux.Event) {
			if ev.InvolvedObject.Kind != r.eventsAbout {
				return
			}
			w.poster.enqueue(flux.MapEvent(w.cluster, ev))
			if ev.Reason == "NewArtifact" {
				w.gitQueue.Add(ev.InvolvedObject.Namespace + "/" + ev.InvolvedObject.Name)
			}
		})
	}
}

// observeAs decodes old and cur and passes them to f, logging an object that does not decode.
func observeAs[T any](old, cur any, f func(prev *T, obj T)) {
	obj, err := decode[T](cur)
	if err != nil {
		slog.Error("could not read a watched object; skipping it", "error", err)
		return
	}
	var prev *T
	if old != nil {
		p, err := decode[T](old)
		if err != nil {
			// A nil prev re-posts everything derivable, which the server's dedup absorbs.
			slog.Warn("could not read a watched object's previous state; re-deriving it",
				"error", err)
		} else {
			prev = &p
		}
	}
	f(prev, obj)
}

func decode[T any](obj any) (T, error) {
	var v T
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return v, fmt.Errorf("unexpected %T from an informer", obj)
	}
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return v, fmt.Errorf("encode %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
	}
	return v, nil
}

func (w *watcher) runGitQueue(ctx context.Context) {
	go func() {
		<-ctx.Done()
		w.gitQueue.ShutDown()
	}()
	for {
		key, shutdown := w.gitQueue.Get()
		if shutdown {
			return
		}
		w.startCommit(key)
		w.gitQueue.Done(key)
	}
}

// startCommit posts a commit's start, titled by its NewArtifact Event or its short revision.
func (w *watcher) startCommit(key string) {
	repo, ok := w.cachedRepo(key)
	if !ok || repo.Status.Artifact == nil {
		return
	}
	var prev *flux.GitRepository
	if started, ok := w.gitStarted[key]; ok {
		prev = &started
	}
	title, ok := flux.ArtifactTitle(repo, w.newArtifactEvents())
	if !ok {
		if w.now().Sub(repo.Status.Artifact.LastUpdateTime) < titleWait {
			w.gitQueue.AddAfter(key, titleRetry)
			return
		}
		title = flux.ShortRevision(repo.Status.Artifact.Revision)
	}
	evs := flux.MapGitRepository(w.cluster, prev, repo, title)
	w.gitStarted[key] = repo
	w.poster.enqueue(evs)
}

// cachedRepo is the GitRepository key names, and false if it is gone or unreadable.
func (w *watcher) cachedRepo(key string) (flux.GitRepository, bool) {
	obj, exists, err := w.repos.GetByKey(key)
	if err != nil {
		slog.Error("could not read a GitRepository from the cache", "key", key, "error", err)
		return flux.GitRepository{}, false
	}
	if !exists {
		return flux.GitRepository{}, false
	}
	repo, err := decode[flux.GitRepository](obj)
	if err != nil {
		slog.Error("could not read a GitRepository; its commits are not recorded", "error", err)
		return flux.GitRepository{}, false
	}
	return repo, true
}

func (w *watcher) newArtifactEvents() []flux.Event {
	var out []flux.Event
	for _, o := range w.gitEvents.List() {
		if ev, err := decode[flux.Event](o); err == nil && ev.Reason == "NewArtifact" {
			out = append(out, ev)
		}
	}
	return out
}
