package pods

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/kubewall/kubewall/backend/handlers/workloads/replicaset"
	"github.com/r3labs/sse/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/yaml"

	"github.com/kubewall/kubewall/backend/handlers/base"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubewall/kubewall/backend/handlers/helpers"

	"net/http"

	"github.com/kubewall/kubewall/backend/container"
	"github.com/labstack/echo/v4"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/json"
)

// podCache stores pods with a TTL for reducing API server load
type podCache struct {
	pods      []v1.Pod
	timestamp time.Time
	mu        sync.RWMutex
}

var (
	// Global cache map: key = "config-cluster"
	podCaches = make(map[string]*podCache)
	cacheMu   sync.RWMutex
	cacheTTL  = 30 * time.Second // Cache pods for 30 seconds
)

type PodsHandler struct {
	BaseHandler       base.BaseHandler
	clientSet         *kubernetes.Clientset
	restConfig        *rest.Config
	replicasetHandler *replicaset.ReplicaSetHandler
}

func NewPodsRouteHandler(container container.Container, routeType base.RouteType) echo.HandlerFunc {
	return func(c echo.Context) error {
		handler := NewPodsHandler(c, container)

		switch routeType {
		case base.GetList:
			return handler.GetList(c) // Use custom implementation
		case base.GetDetails:
			return handler.GetDetails(c) // Use custom implementation
		case base.GetEvents:
			return handler.BaseHandler.GetEvents(c)
		case base.GetYaml:
			return handler.GetYaml(c) // Use custom implementation
		case base.Delete:
			return handler.BaseHandler.Delete(c)
		case base.GetLogs:
			return handler.GetLogs(c)
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "Unknown route type")
		}
	}
}

func NewPodsHandler(c echo.Context, container container.Container) *PodsHandler {
	config := c.QueryParam("config")
	cluster := c.QueryParam("cluster")

	clientSet := container.ClientSet(config, cluster)

	handler := &PodsHandler{
		BaseHandler: base.BaseHandler{
			Kind:         "Pod",
			Container:    container,
			RestClient:   clientSet.CoreV1().RESTClient(),
			QueryConfig:  config,
			QueryCluster: cluster,
			// No Informer needed anymore!
		},
		restConfig:        container.RestConfig(config, cluster),
		clientSet:         clientSet,
		replicasetHandler: replicaset.NewReplicaSetHandler(c, container),
	}

	return handler
}

func transformItems(items []any, b *base.BaseHandler) ([]byte, error) {
	var list []v1.Pod
	for _, obj := range items {
		if item, ok := obj.(*v1.Pod); ok {
			list = append(list, *item)
		}
	}
	podMetricsList := GetPodsMetricsList(b)
	t := TransformPodList(list, podMetricsList)

	return json.Marshal(t)
}

func GetPodsMetricsList(b *base.BaseHandler) *v1beta1.PodMetricsList {
	cacheKey := fmt.Sprintf(helpers.IsMetricServerAvailableCacheKeyFormat, b.QueryConfig, b.QueryCluster)
	value, exists := b.Container.Cache().GetIfPresent(cacheKey)
	if value == nil || value == false || !exists {
		return nil
	}
	podMetrics, err := b.Container.
		MetricClient(b.QueryConfig, b.QueryCluster).
		MetricsV1beta1().
		PodMetricses("").
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Info("failed to get pod metrics", "err", err)
	}
	return podMetrics
}

func (h *PodsHandler) GetLogs(c echo.Context) error {
	sseServer := sse.New()
	sseServer.AutoStream = true
	sseServer.EventTTL = 0
	config := c.QueryParam("config")
	cluster := c.QueryParam("cluster")
	name := c.Param("name")
	namespace := c.Param("namespace")
	container := c.QueryParam("container")

	var key string
	if container != "" {
		key = fmt.Sprintf("%s-%s-%s-%s-%s-logs", config, cluster, name, namespace, container)
	} else {
		key = fmt.Sprintf("%s-%s-%s-%s-logs", config, cluster, name, namespace)
	}
	go h.publishLogsToSSE(c, key, sseServer)

	sseServer.ServeHTTP(key, c.Response(), c.Request())

	return nil
}

// getCacheKey returns the cache key for this cluster
func (h *PodsHandler) getCacheKey() string {
	return fmt.Sprintf("%s-%s", h.BaseHandler.QueryConfig, h.BaseHandler.QueryCluster)
}

// fetchPodsWithCache fetches pods from API with pagination and caching
func (h *PodsHandler) fetchPodsWithCache(ctx context.Context, namespace string) ([]v1.Pod, error) {
	cacheKey := h.getCacheKey()

	// Check cache first
	cacheMu.RLock()
	cache, exists := podCaches[cacheKey]
	cacheMu.RUnlock()

	if exists {
		cache.mu.RLock()
		if time.Since(cache.timestamp) < cacheTTL {
			pods := cache.pods
			cache.mu.RUnlock()

			// Filter by namespace if specified
			if namespace != "" {
				filtered := make([]v1.Pod, 0)
				for _, pod := range pods {
					if pod.Namespace == namespace {
						filtered = append(filtered, pod)
					}
				}
				return filtered, nil
			}
			return pods, nil
		}
		cache.mu.RUnlock()
	}

	// Cache miss or expired - fetch from API
	allPods, err := h.fetchPodsFromAPI(ctx, namespace)
	if err != nil {
		return nil, err
	}

	// Update cache (only if fetching all namespaces)
	if namespace == "" {
		cacheMu.Lock()
		if podCaches[cacheKey] == nil {
			podCaches[cacheKey] = &podCache{}
		}
		cache := podCaches[cacheKey]
		cacheMu.Unlock()

		cache.mu.Lock()
		cache.pods = allPods
		cache.timestamp = time.Now()
		cache.mu.Unlock()
	}

	return allPods, nil
}

// fetchPodsFromAPI fetches pods directly from Kubernetes API with pagination
func (h *PodsHandler) fetchPodsFromAPI(ctx context.Context, namespace string) ([]v1.Pod, error) {
	var allPods []v1.Pod

	opts := metav1.ListOptions{
		Limit: 500, // Fetch 500 pods at a time
	}

	for {
		var podList *v1.PodList
		var err error

		if namespace != "" {
			podList, err = h.clientSet.CoreV1().Pods(namespace).List(ctx, opts)
		} else {
			podList, err = h.clientSet.CoreV1().Pods("").List(ctx, opts)
		}

		if err != nil {
			return nil, err
		}

		// Strip unused fields to reduce memory
		for i := range podList.Items {
			helpers.StripUnusedFields(&podList.Items[i])
		}

		allPods = append(allPods, podList.Items...)

		// Check if there are more pages
		if podList.Continue == "" {
			break
		}
		opts.Continue = podList.Continue
	}

	return allPods, nil
}

// GetList handles pod list requests using direct API calls instead of informers
func (h *PodsHandler) GetList(c echo.Context) error {
	streamID := fmt.Sprintf("%s-%s-%s", h.BaseHandler.QueryConfig, h.BaseHandler.QueryCluster, h.BaseHandler.Kind)

	// Fetch pods in background
	go func() {
		pods, err := h.fetchPodsWithCache(c.Request().Context(), c.QueryParam("namespace"))
		if err != nil {
			log.Error("failed to fetch pods", "error", err)
			return
		}

		// Get metrics
		podMetricsList := GetPodsMetricsList(&h.BaseHandler)

		// Transform and publish
		transformed := TransformPodList(pods, podMetricsList)
		data, err := json.Marshal(transformed)
		if err != nil {
			log.Error("failed to marshal pods", "error", err)
			return
		}

		h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{
			Data: data,
		})
	}()

	// Start watch for real-time updates
	go h.watchPods(c.Request().Context(), streamID)

	h.BaseHandler.Container.SSE().ServeHTTP(streamID, c.Response(), c.Request())
	return nil
}

// GetDetails handles single pod detail requests
func (h *PodsHandler) GetDetails(c echo.Context) error {
	namespace := c.QueryParam("namespace")
	name := c.Param("name")
	streamID := fmt.Sprintf("%s-%s-%s-%s-%s", h.BaseHandler.QueryConfig, h.BaseHandler.QueryCluster, h.BaseHandler.Kind, namespace, name)

	go func() {
		pod, err := h.clientSet.CoreV1().Pods(namespace).Get(c.Request().Context(), name, metav1.GetOptions{})
		if err != nil {
			log.Error("failed to get pod", "error", err, "namespace", namespace, "name", name)
			h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{
				Data: []byte(fmt.Sprintf(`{"error": "%s"}`, err.Error())),
			})
			return
		}

		helpers.StripUnusedFields(pod)
		data, err := json.Marshal(pod)
		if err != nil {
			log.Error("failed to marshal pod", "error", err)
			return
		}

		h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{
			Data: data,
		})
	}()

	h.BaseHandler.Container.SSE().ServeHTTP(streamID, c.Response(), c.Request())
	return nil
}

// GetYaml handles pod YAML requests
func (h *PodsHandler) GetYaml(c echo.Context) error {
	namespace := c.QueryParam("namespace")
	name := c.Param("name")
	streamID := fmt.Sprintf("%s-%s-%s-%s-%s-yaml", h.BaseHandler.QueryConfig, h.BaseHandler.QueryCluster, h.BaseHandler.Kind, namespace, name)

	go func() {
		pod, err := h.clientSet.CoreV1().Pods(namespace).Get(c.Request().Context(), name, metav1.GetOptions{})
		if err != nil {
			log.Error("failed to get pod", "error", err, "namespace", namespace, "name", name)
			return
		}

		// Marshal to YAML (same pattern as base handler)
		y, err := yaml.Marshal(pod)
		if err != nil {
			log.Error("failed to marshal pod to YAML", "error", err)
			return
		}

		data, err := json.Marshal(echo.Map{
			"data": y,
		})
		if err != nil {
			log.Error("failed to marshal YAML wrapper", "error", err)
			return
		}

		h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{
			Data: data,
		})
	}()

	h.BaseHandler.Container.SSE().ServeHTTP(streamID, c.Response(), c.Request())
	return nil
}

// watchPods watches for pod changes and publishes updates via SSE
func (h *PodsHandler) watchPods(ctx context.Context, streamID string) {
	timeoutSeconds := int64(300) // 5 minute timeout
	watcher, err := h.clientSet.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		Watch:           true,
		TimeoutSeconds:  &timeoutSeconds,
		ResourceVersion: "0", // Start from current state
	})
	if err != nil {
		log.Error("failed to create pod watcher", "error", err)
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Watcher closed, exit
				return
			}

			// Invalidate cache on any change
			cacheKey := h.getCacheKey()
			cacheMu.Lock()
			delete(podCaches, cacheKey)
			cacheMu.Unlock()

			// Fetch fresh data and publish
			pods, err := h.fetchPodsWithCache(context.Background(), "")
			if err != nil {
				log.Error("failed to fetch pods after watch event", "error", err)
				continue
			}

			podMetricsList := GetPodsMetricsList(&h.BaseHandler)
			transformed := TransformPodList(pods, podMetricsList)
			data, err := json.Marshal(transformed)
			if err != nil {
				log.Error("failed to marshal pods", "error", err)
				continue
			}

			h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{
				Data: data,
			})

			// Log the event type
			if event.Type == watch.Added || event.Type == watch.Modified || event.Type == watch.Deleted {
				log.Debug("pod watch event", "type", event.Type)
			}
		}
	}
}
