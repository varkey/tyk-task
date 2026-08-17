package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/varkey/tyk-task/golang/internal/api"
	"github.com/varkey/tyk-task/golang/internal/k8shealth"
	"github.com/varkey/tyk-task/golang/internal/logging"
)

// buildVersion is stamped at build time via -ldflags "-X main.buildVersion=...".
var buildVersion = "dev"

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")
	authEnabled := flag.Bool("auth-enabled", true,
		"require Kubernetes TokenReview/SubjectAccessReview authentication and authorization on the /api/v1 routes; "+
			"disable only for local testing (also settable via the chart's auth.enabled value)")
	k8sTimeout := flag.Duration("k8s-timeout", 5*time.Second, "timeout for calls made to the Kubernetes API server")
	namespacesFlag := flag.String("namespaces", "",
		"comma-separated namespaces to operate within for deployment health and isolation list/delete; "+
			"leave empty for cluster-wide (requires cluster-scoped RBAC - see the chart's rbac.clusterScoped/"+
			"rbac.namespaces values, which set this flag automatically)")
	logLevelFlag := flag.String("log-level", "info",
		"minimum severity to log: debug, info, warn, or error. At the default (info) every request logs a "+
			"one-line access record except /healthz and /readyz, which kubelet probes every few seconds and "+
			"only log at debug; warn/error - which cover authentication and authorization failures - are "+
			"always logged regardless of this setting.")

	flag.Parse()

	logLevel, err := logging.ParseLevel(*logLevelFlag)
	if err != nil {
		log.Fatalf("tyk-sre-assignment: %v", err)
	}
	logging.SetLevel(logLevel)

	namespaces := parseNamespaces(*namespacesFlag)

	logging.Infof("tyk-sre-assignment %s starting", buildVersion)

	if !*authEnabled {
		logging.Warnf("caller authentication/authorization is DISABLED (--auth-enabled=false); do not use this outside local testing")
	}

	kConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	kConfig.Timeout = *k8sTimeout

	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		panic(err)
	}

	// Unlike the two panics above, a failed connectivity check here
	// shouldn't be fatal - /readyz (story 3) already reports this live on
	// an ongoing basis, so let the server start and stay not-ready instead
	// of crash-looping the pod.
	kubeVersion, err := k8shealth.GetKubernetesVersion(clientset)
	if err != nil {
		logging.Warnf("could not reach the Kubernetes API server at startup: %v", err)
	} else {
		logging.Infof("Connected to Kubernetes %s", kubeVersion)
	}

	if len(namespaces) > 0 {
		logging.Infof("Operating in namespace-scoped mode: %s", strings.Join(namespaces, ", "))
	}

	server := &api.Server{Clientset: clientset, AuthEnabled: *authEnabled, Namespaces: namespaces}

	logging.Infof("Server listening on %s", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, server.Handler()); err != nil {
		panic(err)
	}
}

// parseNamespaces splits a comma-separated --namespaces flag, trimming
// whitespace and dropping empty entries so "" (the default) and "," both
// mean "no namespaces configured" rather than [""].
func parseNamespaces(flagValue string) []string {
	var namespaces []string
	for _, ns := range strings.Split(flagValue, ",") {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}
