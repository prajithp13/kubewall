package pods

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/labstack/echo/v4"
	"github.com/r3labs/sse/v2"
	v1 "k8s.io/api/core/v1"
)

func (h *PodsHandler) NodePods(c echo.Context) {
	// Fetch pods using direct API call instead of informer
	pods, err := h.fetchPodsWithCache(c.Request().Context(), "")
	if err != nil || len(pods) == 0 {
		return
	}

	// group pods by node
	podsByNode := make(map[string][]v1.Pod, 8)
	for _, pod := range pods {
		node := pod.Spec.NodeName
		if node == "" {
			node = "unscheduled"
		}
		podsByNode[node] = append(podsByNode[node], pod)
	}

	if len(podsByNode) == 0 {
		return
	}

	// fetch metrics once
	podsMetricsList := GetPodsMetricsList(&h.BaseHandler)

	nodes := make([]string, 0, len(podsByNode))
	for n := range podsByNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	for _, node := range nodes {
		perNodePods := podsByNode[node]
		transformed := TransformPodList(perNodePods, podsMetricsList)

		data, err := json.Marshal(transformed)
		if err != nil {
			data = []byte("{}")
		}

		streamID := fmt.Sprintf("%s-%s-%s-node-pods", h.BaseHandler.QueryConfig, h.BaseHandler.QueryCluster, node)
		h.BaseHandler.Container.SSE().Publish(streamID, &sse.Event{Data: data})
	}
}
