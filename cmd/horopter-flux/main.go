// Command horopter-flux records what Flux does as Horopter changes, keeping no state of its own.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"horopter.dev/horopter/internal/buildinfo"
	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/logging"
)

const (
	exitError = 1
	exitUsage = 2
)

func main() {
	code, handled := buildinfo.Handle("horopter-flux", os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		os.Exit(code)
	}
	if err := logging.Setup(); err != nil {
		fmt.Fprintln(os.Stderr, "horopter-flux:", err)
		os.Exit(exitError)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := start(ctx)
	if err == nil {
		return
	}
	slog.Error("horopter-flux failed", "error", err)
	var usage usageError
	if errors.As(err, &usage) {
		os.Exit(exitUsage)
	}
	os.Exit(exitError)
}

func start(ctx context.Context) error {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("load the Kubernetes client configuration (in-cluster, or "+
			"$KUBECONFIG outside a cluster): %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build the Kubernetes client: %w", err)
	}
	return run(ctx, os.Getenv, dyn)
}

// usageError is a configuration the client cannot start with.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var namespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// run checks its configuration and the cluster's identity, then records until ctx ends.
func run(ctx context.Context, env func(string) string, dyn dynamic.Interface) error {
	url, key := env("HOROPTER_INGRESS_URL"), env("HOROPTER_INGRESS_KEY")
	if url == "" || key == "" {
		return usageError{"HOROPTER_INGRESS_URL and HOROPTER_INGRESS_KEY are required: " +
			"ingress's base URL, and a producer key issued with horoctl admin keys issue"}
	}
	c, err := client.New(url, key)
	if err != nil {
		return usageError{"HOROPTER_INGRESS_URL: " + err.Error()}
	}
	ns, err := dyn.Resource(namespaces).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the kube-system Namespace, whose uid prefixes every "+
			"idempotency key; the chart's ClusterRole grants get on it: %w", err)
	}
	cluster := string(ns.GetUID())
	slog.Info("horopter-flux starting", "cluster", cluster, "ingress", url)
	return watch(ctx, dyn, cluster, newPoster(c))
}
