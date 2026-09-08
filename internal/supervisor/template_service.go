package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	templateServiceCommandEnv  = "DEVSANDBOX_TEMPLATE_SERVICE_COMMAND"
	templateServiceReadyURLEnv = "DEVSANDBOX_TEMPLATE_SERVICE_READY_URL"
)

type TemplateServiceConfig struct {
	Executable     string
	Arguments      []string
	ReadyURL       string
	StartupTimeout time.Duration
	PollInterval   time.Duration
}

func TemplateServiceConfigFromEnvironment(getenv func(string) string) (TemplateServiceConfig, bool, error) {
	if getenv == nil {
		return TemplateServiceConfig{}, false, errors.New("environment reader is required")
	}
	executable := strings.TrimSpace(getenv(templateServiceCommandEnv))
	readyURL := strings.TrimSpace(getenv(templateServiceReadyURLEnv))
	if executable == "" && readyURL == "" {
		return TemplateServiceConfig{}, false, nil
	}
	if executable == "" || readyURL == "" {
		return TemplateServiceConfig{}, false, errors.New("template service command and readiness URL must be configured together")
	}
	if strings.ContainsAny(executable, " \t\r\n") {
		return TemplateServiceConfig{}, false, errors.New("template service command must be a single executable path")
	}
	parsed, err := url.Parse(readyURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return TemplateServiceConfig{}, false, errors.New("template service readiness URL must be an absolute internal HTTP URL")
	}
	host := parsed.Hostname()
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		return TemplateServiceConfig{}, false, errors.New("template service readiness URL must use a loopback host")
	}
	return TemplateServiceConfig{
		Executable: executable, ReadyURL: parsed.String(),
		StartupTimeout: time.Minute, PollInterval: 250 * time.Millisecond,
	}, true, nil
}

type TemplateService struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.RWMutex
	err     error
	client  *http.Client
	url     string
	process *os.Process
}

func StartTemplateService(ctx context.Context, config TemplateServiceConfig, output io.Writer) (*TemplateService, error) {
	if config.Executable == "" || config.ReadyURL == "" {
		return nil, errors.New("template service executable and readiness URL are required")
	}
	if output == nil {
		output = io.Discard
	}
	startupTimeout := config.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = time.Minute
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(serviceCtx, config.Executable, config.Arguments...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start template service: %w", err)
	}
	service := &TemplateService{
		cancel: cancel, done: make(chan struct{}), client: &http.Client{Timeout: 3 * time.Second}, url: config.ReadyURL,
		process: command.Process,
	}
	go func() {
		err := command.Wait()
		service.mu.Lock()
		service.err = err
		service.mu.Unlock()
		close(service.done)
	}()

	deadline := time.NewTimer(startupTimeout)
	ticker := time.NewTicker(pollInterval)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if service.Check(ctx) == nil {
			return service, nil
		}
		select {
		case <-ctx.Done():
			service.Close()
			return nil, ctx.Err()
		case <-service.done:
			return nil, fmt.Errorf("template service exited before becoming ready: %w", service.processError())
		case <-deadline.C:
			service.Close()
			return nil, errors.New("template service did not become ready before the startup timeout")
		case <-ticker.C:
		}
	}
}

func (s *TemplateService) Check(ctx context.Context) error {
	select {
	case <-s.done:
		return fmt.Errorf("template service exited: %w", s.processError())
	default:
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("template service readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *TemplateService) Close() {
	if s == nil {
		return
	}
	s.cancel()
	if s.process != nil {
		_ = s.process.Kill()
	}
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
	}
}

func (s *TemplateService) processError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err == nil {
		return errors.New("process stopped")
	}
	return s.err
}
