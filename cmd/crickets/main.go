// Command crickets labels workloads whose Ingress/HTTPRoute/LoadBalancer got no traffic. It never deletes.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/Infra-Dots/crickets/internal/config"
	"github.com/Infra-Dots/crickets/internal/discovery"
	"github.com/Infra-Dots/crickets/internal/prom"
	"github.com/Infra-Dots/crickets/internal/providers"
	"github.com/Infra-Dots/crickets/internal/reconcile"
)

func main() { os.Exit(run()) }

func run() int {
	cfgPath := flag.String("config", "/etc/crickets/config.yaml", "config file")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (in-cluster when empty)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fail := func(msg string, err error) int {
		log.Error(msg, "error", err)
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail("config invalid", err)
	}
	set, err := providers.Load(cfg.Providers)
	if err != nil {
		return fail("providers invalid", err)
	}
	q, err := prom.New(cfg.Prometheus.URL, cfg.Prometheus.BearerTokenFile, cfg.Prometheus.Timeout)
	if err != nil {
		return fail("prometheus client", err)
	}
	var rc *rest.Config
	if *kubeconfig == "" {
		rc, err = rest.InClusterConfig()
	} else {
		rc, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		return fail("kube config", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fail("kube client", err)
	}
	gw, err := gwclient.NewForConfig(rc)
	if err != nil {
		return fail("gateway client", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	r := &reconcile.Reconciler{
		Disc: &discovery.Discoverer{Kube: kube, GW: gw}, Providers: set, Prom: q,
		Writer: reconcile.KubeWriter{Kube: kube}, Cfg: cfg, Now: time.Now, Log: log,
	}
	s, err := r.Run(ctx)
	log.Info("pass complete", "dryRun", cfg.DryRun, "workloads", s.Workloads, "idle", s.Idle, "active", s.Active,
		"unknown", s.Unknown, "unexposed", s.Unexposed, "ignored", s.Ignored, "young", s.Young,
		"labeled", s.Labeled, "unlabeled", s.Unlabeled, "writeErrors", s.WriteErrors)
	if err != nil {
		return fail("pass failed", err)
	}
	return 0
}
