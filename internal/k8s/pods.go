package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Pod is what the portal needs to know about a pod to name the service behind
// a broker connection that did not name itself.
type Pod struct {
	IP        string
	Name      string
	Namespace string
	// Service is the pod's app.kubernetes.io/name, else its app label, else
	// its name without the ReplicaSet and pod suffixes.
	Service string
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	} `json:"items"`
}

// PodsByIP lists running pods, keyed by their address. It needs `list pods`,
// which the Ingress discovery does not: without it the error says so, and the
// caller carries on naming connections by connection_name only.
func (c *Client) PodsByIP(ctx context.Context) (map[string]Pod, error) {
	base := strings.TrimRight(c.cfg.APIServer, "/")
	endpoint := base + "/api/v1/pods"
	if len(c.cfg.Namespaces) == 1 {
		endpoint = fmt.Sprintf("%s/api/v1/namespaces/%s/pods", base, url.PathEscape(c.cfg.Namespaces[0]))
	}
	endpoint += "?fieldSelector=" + url.QueryEscape("status.phase=Running")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("нет прав на чтение подов (HTTP 403): соединения RabbitMQ без connection_name "+
			"не сопоставить с сервисами по адресу. Выдайте ServiceAccount право list на pods. Ответ API-сервера: %s",
			apiServerMessage(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("не удалось получить список подов: HTTP %d: %s", resp.StatusCode, apiServerMessage(body))
	}

	var list podList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode pod list: %w", err)
	}
	out := make(map[string]Pod, len(list.Items))
	for _, item := range list.Items {
		if item.Status.PodIP == "" {
			continue
		}
		out[item.Status.PodIP] = Pod{
			IP:        item.Status.PodIP,
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Service:   podService(item.Metadata.Name, item.Metadata.Labels),
		}
	}
	return out, nil
}

func podService(name string, labels map[string]string) string {
	if v := labels["app.kubernetes.io/name"]; v != "" {
		return v
	}
	if v := labels["app"]; v != "" {
		return v
	}
	// A Deployment's pod is <deployment>-<replicaset hash>-<pod suffix>.
	parts := strings.Split(name, "-")
	if len(parts) > 2 {
		return strings.Join(parts[:len(parts)-2], "-")
	}
	return name
}
