package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/operator"
	"github.com/rmoreirao/AKSAgentSandbox/internal/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func main() {
	var (
		metricsAddress string
		probeAddress   string
		workloadNS     string
		leaderElection bool
		showVersion    bool
	)
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "address for the metrics endpoint")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "address for health probes")
	flag.StringVar(&workloadNS, "workload-namespace", "devsandbox-workloads", "namespace containing DevSandbox workloads")
	flag.BoolVar(&leaderElection, "leader-elect", true, "enable leader election")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	if showVersion {
		fmt.Printf("devsandbox-operator %s\n", version.String())
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	scheme := runtime.NewScheme()
	utilruntime.Must(operator.AddSchemes(scheme))
	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddress,
		HealthProbeBindAddress: probeAddress,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "devsandbox-operator.devsandbox.io",
		Namespace:              workloadNS,
		ClientDisableCacheFor:  []client.Object{&corev1.PersistentVolume{}},
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to create manager")
		os.Exit(1)
	}
	audit := observability.NewAuditor(os.Stdout, "operator")
	metrics := observability.NewRegisteredMetrics("operator", ctrlmetrics.Registry)
	if err := operator.SetupControllersWithObservability(manager, audit, metrics); err != nil {
		ctrl.Log.Error(err, "unable to set up controllers")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("kubernetes-api", func(request *http.Request) error {
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		var sandboxes devsandboxv1alpha1.DevSandboxList
		return manager.GetAPIReader().List(ctx, &sandboxes, client.InNamespace(workloadNS), client.Limit(1))
	}); err != nil {
		ctrl.Log.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}
