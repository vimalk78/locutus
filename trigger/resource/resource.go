package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/brancz/locutus/trigger"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/brancz/locutus/client"
	"github.com/brancz/locutus/feedback"
	"github.com/brancz/locutus/rollout"
)

const (
	resyncPeriod = 0 * time.Minute
)

type ResourcesTriggerConfig struct {
	MainResource string                  `json:"mainResource"`
	Resources    []ResourceTriggerConfig `json:"resources"`
}

type ResourceTriggerConfig struct {
	Name                     string                    `json:"name"`
	Kind                     string                    `json:"kind"`
	APIVersion               string                    `json:"apiVersion"`
	Namespace                string                    `json:"namespace,omitempty"`
	KeyTransformationConfigs []KeyTransformationConfig `json:"keyTransformations"`
}

type KeyTransformationConfig struct {
	Action      string `json:"action"`
	Regex       string `json:"regex"`
	Replacement string `json:"replacement"`
}

type Trigger struct {
	trigger.ExecutionRegister

	logger log.Logger
	client *client.Client

	infs  map[string]cache.SharedIndexInformer
	inf   cache.SharedIndexInformer
	queue workqueue.RateLimitingInterface
}

func NewTrigger(ctx context.Context, logger log.Logger, client *client.Client, configFile string) (*Trigger, error) {
	t := &Trigger{
		logger: logger,
		client: client,
		queue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "resource"),
		infs:   map[string]cache.SharedIndexInformer{},
	}

	f, err := os.Open(configFile)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open config file")
	}
	var config ResourcesTriggerConfig
	err = yaml.NewYAMLOrJSONDecoder(f, 100).Decode(&config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse config file")
	}

	for _, r := range config.Resources {
		key := r.Name
		if len(r.Namespace) > 0 {
			key = r.Namespace + "/" + r.Name
		}

		c, err := client.ClientFor(r.APIVersion, r.Kind, r.Namespace)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create client for %s in %s", r.Kind, r.APIVersion)
		}
		inf := cache.NewSharedIndexInformer(
			&cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					return c.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return c.Watch(ctx, options)
				},
			},
			&unstructured.Unstructured{}, resyncPeriod, cache.Indexers{},
		)
		h, err := NewResourceHandlers(log.With(t.logger, "resource-handler", r.Name), inf, t.enqueue, t.keyFunc, r.KeyTransformationConfigs)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create resource handlers for %s in %s", r.Kind, r.APIVersion)
		}
		inf.AddEventHandler(h)
		t.infs[key] = inf
	}

	t.inf = t.infs[config.MainResource]

	return t, nil
}

func (p *Trigger) Run(ctx context.Context) error {
	defer p.queue.ShutDown()

	p.logger.Log("msg", "resources trigger started")

	go p.worker(ctx)
	for _, inf := range p.infs {
		go func(informer cache.SharedIndexInformer) {
			informer.Run(ctx.Done())
		}(inf)
	}

	<-ctx.Done()
	return nil
}

func (p *Trigger) keyFunc(obj interface{}) (string, bool) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return k, false
	}
	return k, true
}

func (p *Trigger) enqueue(obj interface{}) {
	if obj == nil {
		return
	}

	key, ok := obj.(string)
	if !ok {
		key, ok = p.keyFunc(obj)
		if !ok {
			return
		}
	}

	p.queue.Add(key)
}

func (p *Trigger) worker(ctx context.Context) {
	for p.processNextWorkItem(ctx) {
	}
}

func (p *Trigger) processNextWorkItem(ctx context.Context) bool {
	key, quit := p.queue.Get()
	if quit {
		return false
	}
	defer p.queue.Done(key)

	err := p.sync(ctx, key.(string))
	if err == nil {
		p.queue.Forget(key)
		return true
	}

	level.Warn(p.logger).Log("msg", "sync failed", "key", key, "err", err)

	utilruntime.HandleError(errors.Wrap(err, fmt.Sprintf("Sync %q failed", key)))
	p.queue.AddRateLimited(key)

	return true
}

func (p *Trigger) sync(ctx context.Context, key string) error {
	level.Info(p.logger).Log("msg", "sync triggered", "key", key)

	obj, exists, err := p.inf.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	cfg, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	return p.Execute(ctx, &rollout.Config{
		RawConfig: cfg,
		Feedback:  feedback.NewFeedback(p.logger, p.client, obj.(*unstructured.Unstructured)),
	})
}
