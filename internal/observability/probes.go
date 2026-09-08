package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"
)

type Check struct {
	Name string
	Run  func(context.Context) error
}

type Probes struct {
	Checks  []Check
	Timeout time.Duration
}

func (p Probes) Handler(next, metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			if request.Method != http.MethodGet {
				writer.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			writeProbe(writer, http.StatusOK, map[string]string{"process": "ok"})
		case "/readyz":
			p.ready(writer, request)
		case "/metrics":
			metrics.ServeHTTP(writer, request)
		default:
			next.ServeHTTP(writer, request)
		}
	})
}

func (p Probes) ready(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	results := make(map[string]string, len(p.Checks))
	status := http.StatusOK
	checks := append([]Check(nil), p.Checks...)
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	for _, check := range checks {
		if check.Name == "" || check.Run == nil {
			continue
		}
		if err := check.Run(ctx); err != nil {
			results[check.Name] = "unavailable"
			status = http.StatusServiceUnavailable
		} else {
			results[check.Name] = "ok"
		}
	}
	writeProbe(writer, status, results)
}

func HTTPCheck(name, endpoint string, client *http.Client) Check {
	return Check{Name: name, Run: func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		httpClient := client
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		// Authentication failures still prove that the dependency is reachable.
		if response.StatusCode >= http.StatusInternalServerError {
			return errors.New("dependency returned server error")
		}
		return nil
	}}
}

func writeProbe(writer http.ResponseWriter, status int, checks map[string]string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"status": map[bool]string{true: "ok", false: "unavailable"}[status == http.StatusOK], "checks": checks})
}
