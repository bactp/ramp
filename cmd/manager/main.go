// Command ramp-manager runs the RAMP controllers.
//
// Placement rationale (see docs/ramp-scenario1/01-integration-design.md Sec.4):
// the manager runs against the MANAGEMENT cluster because readiness is
// inherently a multi-cluster question and because the API RAMP delegates
// actuation to -- the Transition Operator's Checkpoint CRD -- exists only
// there. Workload-side facts are read through per-cluster clients registered
// with --cluster, the same shape the Transition Operator already uses via CAPI
// kubeconfig secrets.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/artifacts"
	"github.com/dcn-ssu/ramp/internal/clusters"
	"github.com/dcn-ssu/ramp/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(rampv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		leaderElect     bool
		clusterFlags    clusters.Flag
		minioEndpoint   string
		minioBucket     string
		stageProbeImage string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Metrics endpoint address; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "Health probe endpoint address.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.Var(&clusterFlags, "cluster", "Register a workload cluster as name=/path/to/kubeconfig. Repeatable.")
	flag.StringVar(&minioEndpoint, "artifact-store-endpoint", "192.168.28.158:32000",
		"host:port of the MinIO checkpoint artifact store shared with the Transition Operator.")
	flag.StringVar(&minioBucket, "artifact-store-bucket", "checkpoints", "Artifact store bucket.")
	flag.StringVar(&stageProbeImage, "stage-probe-image", "busybox:1.36",
		"Image used by the one-shot pod that verifies an artifact is staged on a target node.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if len(clusterFlags.Entries) == 0 {
		setupLog.Error(fmt.Errorf("no --cluster registered"),
			"RAMP needs at least the source and target workload clusters")
		os.Exit(1)
	}

	registry, err := clusters.NewRegistry(clusterFlags.Entries, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build the workload cluster registry")
		os.Exit(1)
	}
	setupLog.Info("registered workload clusters", "clusters", strings.Join(registry.Names(), ","))

	store, err := artifacts.New(artifacts.Config{
		Endpoint:  minioEndpoint,
		AccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		SecretKey: os.Getenv("MINIO_SECRET_KEY"),
		Bucket:    minioBucket,
	})
	if err != nil {
		setupLog.Error(err, "unable to reach the checkpoint artifact store", "endpoint", minioEndpoint)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "ramp.dcn.ssu.ac.kr",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Three reconcilers, three responsibilities. They are deliberately not one
	// controller: membership, epoch coordination and target readiness fail for
	// different reasons and need to be debuggable apart.
	if err := (&controller.RecoveryGroupReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clusters: registry,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RecoveryGroup")
		os.Exit(1)
	}
	if err := (&controller.RecoveryPointReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clusters: registry, Store: store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RecoveryPoint")
		os.Exit(1)
	}
	if err := (&controller.RecoveryPathReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clusters: registry, Store: store,
		StageProbeImage: stageProbeImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RecoveryPath")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting RAMP manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
