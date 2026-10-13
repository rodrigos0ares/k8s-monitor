package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Client wraps a Kubernetes clientset bound to a specific context.
type Client struct {
	ContextName string
	clientset   *kubernetes.Clientset
	metricsCS   *metricsv.Clientset
}

// NewClient creates a K8s client for the given kubeconfig context.
func NewClient(contextName string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building config for context %s: %w", contextName, err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset for context %s: %w", contextName, err)
	}

	mcs, err := metricsv.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating metrics clientset for context %s: %w", contextName, err)
	}

	return &Client{
		ContextName: contextName,
		clientset:   cs,
		metricsCS:   mcs,
	}, nil
}

// ListAllPods returns all pods across all namespaces.
func (c *Client) ListAllPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods in context %s: %w", c.ContextName, err)
	}
	return list.Items, nil
}

// PodLogs retrieves recent logs for a specific pod/container.
func (c *Client) PodLogs(ctx context.Context, namespace, podName, container string, since time.Duration) (string, error) {
	sinceSeconds := int64(since.Seconds())
	opts := &corev1.PodLogOptions{
		Container:    container,
		SinceSeconds: &sinceSeconds,
	}

	req := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("streaming logs for %s/%s: %w", namespace, podName, err)
	}
	defer stream.Close()

	data, err := io.ReadAll(io.LimitReader(stream, 1<<20)) // 1MB limit per fetch
	if err != nil {
		return "", fmt.Errorf("reading logs for %s/%s: %w", namespace, podName, err)
	}
	return string(data), nil
}

// PodMetrics holds CPU and memory usage for a pod.
type PodMetrics struct {
	Context   string
	Namespace string
	Pod       string
	// Workload is the owning controller (e.g. Deployment/StatefulSet) name, used
	// as a stable key across pod restarts. Empty when the pod has no controller.
	Workload  string
	Container string
	CPUMilli  int64
	MemoryMB  int64
}

// GetPodMetrics fetches current resource usage for all pods.
func (c *Client) GetPodMetrics(ctx context.Context) ([]PodMetrics, error) {
	list, err := c.metricsCS.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetching metrics for context %s: %w", c.ContextName, err)
	}

	// The metrics API only exposes pod names, so build a pod -> workload map from
	// the live pod list to attach the stable owning-workload key. Failures here
	// are non-fatal: we simply fall back to per-pod keying.
	workloads := c.podWorkloadMap(ctx)

	var results []PodMetrics
	for _, pm := range list.Items {
		for _, cm := range pm.Containers {
			results = append(results, PodMetrics{
				Context:   c.ContextName,
				Namespace: pm.Namespace,
				Pod:       pm.Name,
				Workload:  workloads[pm.Namespace+"/"+pm.Name],
				Container: cm.Name,
				CPUMilli:  cm.Usage.Cpu().MilliValue(),
				MemoryMB:  cm.Usage.Memory().Value() / (1024 * 1024),
			})
		}
	}
	return results, nil
}

// podWorkloadMap returns a map of "namespace/pod" -> owning workload name for
// every live pod. If the pod list cannot be fetched it returns an empty map so
// callers transparently fall back to per-pod keying.
func (c *Client) podWorkloadMap(ctx context.Context) map[string]string {
	pods, err := c.ListAllPods(ctx)
	if err != nil {
		return map[string]string{}
	}
	m := make(map[string]string, len(pods))
	for i := range pods {
		if name := workloadName(&pods[i]); name != "" {
			m[pods[i].Namespace+"/"+pods[i].Name] = name
		}
	}
	return m
}

// ListLiveWorkloads returns the set of "namespace/workload" keys for every live
// pod, using the same workload derivation as ingestion (owning controller name,
// falling back to the pod name when there is no controller owner). This mirrors
// the rightsizing report's COALESCE(NULLIF(workload,''), pod) key so callers can
// tell which stored workloads still exist in the cluster.
func (c *Client) ListLiveWorkloads(ctx context.Context) (map[string]bool, error) {
	pods, err := c.ListAllPods(ctx)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(pods))
	for i := range pods {
		wl := workloadName(&pods[i])
		if wl == "" {
			wl = pods[i].Name
		}
		live[pods[i].Namespace+"/"+wl] = true
	}
	return live, nil
}

// workloadName derives the owning workload (Deployment/StatefulSet/DaemonSet/
// Job, etc.) name from a pod's controller owner reference. For ReplicaSet
// owners (created by Deployments) the trailing pod-template-hash segment is
// stripped to recover the Deployment name. Returns "" when the pod has no
// controller owner, signalling callers to fall back to the pod name.
func workloadName(p *corev1.Pod) string {
	for _, owner := range p.OwnerReferences {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		if owner.Kind == "ReplicaSet" {
			if idx := strings.LastIndex(owner.Name, "-"); idx > 0 {
				return owner.Name[:idx]
			}
		}
		return owner.Name
	}
	return ""
}

// PodResourceSpec holds the configured CPU/memory requests and limits for a container.
type PodResourceSpec struct {
	Context         string
	Namespace       string
	Pod             string
	// Workload is the owning controller name (stable across pod restarts). Empty
	// when the pod has no controller.
	Workload        string
	Container       string
	CPURequestMilli int64
	CPULimitMilli   int64
	MemRequestMB    int64
	MemLimitMB      int64
}

// GetPodResourceSpecs lists all pods and extracts the configured resource
// requests and limits for each container. This is the "desired" allocation that
// is compared against actual usage to find over-provisioned (costly) workloads.
func (c *Client) GetPodResourceSpecs(ctx context.Context) ([]PodResourceSpec, error) {
	pods, err := c.ListAllPods(ctx)
	if err != nil {
		return nil, err
	}

	var specs []PodResourceSpec
	for i := range pods {
		p := pods[i]
		workload := workloadName(&pods[i])
		for _, container := range p.Spec.Containers {
			spec := PodResourceSpec{
				Context:   c.ContextName,
				Namespace: p.Namespace,
				Pod:       p.Name,
				Workload:  workload,
				Container: container.Name,
			}
			if req := container.Resources.Requests; req != nil {
				spec.CPURequestMilli = req.Cpu().MilliValue()
				spec.MemRequestMB = req.Memory().Value() / (1024 * 1024)
			}
			if lim := container.Resources.Limits; lim != nil {
				spec.CPULimitMilli = lim.Cpu().MilliValue()
				spec.MemLimitMB = lim.Memory().Value() / (1024 * 1024)
			}
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

// NodeMetrics holds CPU and memory usage for a node.
type NodeMetrics struct {
	Context  string
	Node     string
	CPUMilli int64
	MemoryMB int64
}

// GetNodeMetrics fetches current resource usage for all nodes.
func (c *Client) GetNodeMetrics(ctx context.Context) ([]NodeMetrics, error) {
	list, err := c.metricsCS.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetching node metrics for context %s: %w", c.ContextName, err)
	}

	var results []NodeMetrics
	for _, nm := range list.Items {
		results = append(results, NodeMetrics{
			Context:  c.ContextName,
			Node:     nm.Name,
			CPUMilli: nm.Usage.Cpu().MilliValue(),
			MemoryMB: nm.Usage.Memory().Value() / (1024 * 1024),
		})
	}
	return results, nil
}

// GetEvents retrieves recent warning events from the cluster.
func (c *Client) GetEvents(ctx context.Context, since time.Duration) ([]corev1.Event, error) {
	cutoff := time.Now().Add(-since)
	list, err := c.clientset.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		return nil, fmt.Errorf("listing events in context %s: %w", c.ContextName, err)
	}

	var filtered []corev1.Event
	for _, e := range list.Items {
		if e.LastTimestamp.Time.After(cutoff) {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
}

// GetAvailableContexts reads the kubeconfig and returns all context names.
func GetAvailableContexts() ([]string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	var names []string
	for name := range config.Contexts {
		names = append(names, name)
	}
	return names, nil
}
