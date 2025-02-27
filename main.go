/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (&the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	zkConfig "github.com/pravega/zookeeper-operator/pkg/controller/config"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	"github.com/pravega/zookeeper-operator/pkg/version"
	zkClient "github.com/pravega/zookeeper-operator/pkg/zk"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelsdkresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/component-base/tracing"

	tracingV1 "k8s.io/component-base/tracing/api/v1"
	nodeutil "k8s.io/component-helpers/node/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/controllers"
	// +kubebuilder:scaffold:imports
)

var (
	log         = ctrl.Log.WithName("cmd")
	versionFlag bool
	scheme      = apimachineryruntime.NewScheme()
)

func init() {
	flag.BoolVar(&versionFlag, "version", false, "Show version and quit")
	flag.BoolVar(&zkConfig.DisableFinalizer, "disableFinalizer", false,
		"Disable finalizers for zookeeperclusters. Use this flag with awareness of the consequences")
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
}

func printVersion() {
	log.Info(fmt.Sprintf("zookeeper-operator Version: %v", version.Version))
	log.Info(fmt.Sprintf("Git SHA: %s", version.GitSHA))
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
}

func main() {
	var metricsAddr string
	var tracingEndpoint string
	var tracingSamplingRateInt int
	flag.StringVar(&metricsAddr, "metrics-bind-address", "127.0.0.1:6000", "The address the metric endpoint binds to.")
	flag.StringVar(&tracingEndpoint, "tracing-endpoint", "", "The endpoint of the collector this component will report traces to.")
	flag.IntVar(&tracingSamplingRateInt, "tracing-sampling-rate", 100000, "The number of samples to collect per million spans.")
	flag.Parse()
	tracingSamplingRate := int32(tracingSamplingRateInt)

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	log.Info(fmt.Sprintf("Tracing configuration: endpoint=%s, samplingRate=%d", tracingEndpoint, tracingSamplingRate))

	namespaces, err := getWatchNamespace()
	if err != nil {
		log.Error(err, "unable to get WatchNamespace, "+
			"the manager will watch and manage resources in all namespaces")
	}

	printVersion()

	if versionFlag {
		os.Exit(0)
	}

	if zkConfig.DisableFinalizer {
		logrus.Warn("----- Running with finalizer disabled. -----")
	}

	//When operator is started to watch resources in a specific set of namespaces, we use the MultiNamespacedCacheBuilder cache.
	//In this scenario, it is also suggested to restrict the provided authorization to this namespace by replacing the default
	//ClusterRole and ClusterRoleBinding to Role and RoleBinding respectively
	//For further information see the kubernetes documentation about
	//Using [RBAC Authorization](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).
	managerNamespaces := []string{}
	if namespaces != "" {
		ns := strings.Split(namespaces, ",")
		for i := range ns {
			ns[i] = strings.TrimSpace(ns[i])
		}
		managerNamespaces = ns
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		logrus.Fatal(err)
	}

	operatorNs, err := GetOperatorNamespace()
	if err != nil {
		log.Error(err, "failed to get operator namespace")
		os.Exit(1)
	}

	ctx := context.Background()

	// Become the leader before proceeding
	err = utils.BecomeLeader(ctx, cfg, "zookeeper-operator-lock", operatorNs)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}
	hostname, err := nodeutil.GetHostname("")
	if err != nil {
		log.Error(err, "failed to get hostname")
	}
	resourceOpts := []otelsdkresource.Option{
		otelsdkresource.WithAttributes(
			semconv.ServiceNameKey.String("zookeeper-operator"),
			semconv.HostNameKey.String(hostname),
		),
	}
	tracingConfig := tracingV1.TracingConfiguration{}
	if tracingEndpoint != "" {
		tracingConfig.Endpoint = &tracingEndpoint
		tracingConfig.SamplingRatePerMillion = &tracingSamplingRate
	}
	tp, err := tracing.NewProvider(ctx, &tracingConfig, []otlptracegrpc.Option{}, resourceOpts)
	tracer := tp.Tracer("zookeeper-operator")
	if err != nil {
		log.Error(err, "failed to create tracing provider")
	}
	defer tp.Shutdown(ctx)

	mgrConfig := ctrl.GetConfigOrDie()
	if err == nil {
		mgrConfig.Wrap(tracing.WrapperFor(tp))
	}
	mgr, err := ctrl.NewManager(mgrConfig, ctrl.Options{
		Scheme:             scheme,
		Cache:              cache.Options{Namespaces: managerNamespaces},
		MetricsBindAddress: metricsAddr,
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if err = (&controllers.ZookeeperClusterReconciler{
		Client:   mgr.GetClient(),
		Log:      ctrl.Log.WithName("controllers").WithName("ZookeeperCluster"),
		Scheme:   mgr.GetScheme(),
		ZkClient: new(zkClient.DefaultZookeeperClient),
		Tracer:   tracer,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "ZookeeperCluster")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// getWatchNamespace returns the Namespace the operator should be watching for changes
func getWatchNamespace() (string, error) {
	// WatchNamespaceEnvVar is the constant for env variable WATCH_NAMESPACE
	// which specifies the Namespace to watch.
	// An empty value means the operator is running with cluster scope.
	var watchNamespaceEnvVar = "WATCH_NAMESPACE"

	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", watchNamespaceEnvVar)
	}
	return ns, nil
}

func GetOperatorNamespace() (string, error) {
	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("file does not exist")
		}
		return "", err
	}
	ns := strings.TrimSpace(string(nsBytes))
	return ns, nil
}
