/*
Copyright 2025 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"golang.org/x/sys/unix"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

const (
	controllerName = "dravip-controller"
)

var (
	kubeconfig       string
	bindAddress      string
	leaderElection   bool
	leaderElectionID string
	leaderElectionNS string
	leaseDuration    time.Duration
	renewDeadline    time.Duration
	retryPeriod      time.Duration

	ready atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&bindAddress, "bind-address", ":9177", "The IP address and port for the metrics and healthz server to serve on")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election for controller manager. Ensuring only one active controller.")
	flag.StringVar(&leaderElectionID, "leader-elect-lock-name", "dravip-controller", "The name of the resource lock for leader election")
	flag.StringVar(&leaderElectionNS, "leader-elect-lock-namespace", "kube-system", "The namespace for the resource lock for leader election")
	flag.DurationVar(&leaseDuration, "leader-elect-lease-duration", 15*time.Second, "The duration that non-leader candidates will wait to force acquire leadership")
	flag.DurationVar(&renewDeadline, "leader-elect-renew-deadline", 10*time.Second, "The duration that the acting master will retry refreshing leadership before giving up")
	flag.DurationVar(&retryPeriod, "leader-elect-retry-period", 2*time.Second, "The duration the LeaderElector clients should wait between tries of actions")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", controllerName)
		flag.PrintDefaults()
	}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	printVersion()
	flag.VisitAll(func(f *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", f.Name, f.Value)
	})

	mux := http.NewServeMux()
	// Add healthz handler
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	})
	// Add metrics handler
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		_ = http.ListenAndServe(bindAddress, mux)
	}()

	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		// creates the in-cluster config
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatalf("can not create client-go configuration: %v", err)
	}

	// use protobuf for better performance at scale
	// https://kubernetes.io/docs/reference/using-api/api-concepts/#alternate-representations-of-resources
	config.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	config.ContentType = "application/vnd.kubernetes.protobuf"

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("can not create client-go client: %v", err)
	}

	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	// trap Ctrl+C and call cancel on the context
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// Enable signal handler
	signalCh := make(chan os.Signal, 2)
	defer func() {
		close(signalCh)
		cancel()
	}()
	signal.Notify(signalCh, os.Interrupt, unix.SIGINT)

	// controller logic here
	controller := NewController(controllerName, clientset, informerFactory.Resource().V1().ResourceClaims())

	if leaderElection {
		// Leader election setup
		lock, err := resourcelock.New(
			resourcelock.LeasesResourceLock,
			leaderElectionNS,
			leaderElectionID,
			clientset.CoreV1(),
			clientset.CoordinationV1(),
			resourcelock.ResourceLockConfig{
				Identity: getLeaderID(),
			},
		)
		if err != nil {
			klog.Fatalf("Failed to create resource lock: %v", err)
		}

		// Leader election configuration
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: leaseDuration,
			RenewDeadline: renewDeadline,
			RetryPeriod:   retryPeriod,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Info("Started leading, starting controller")
					go controller.Run(ctx)
					informerFactory.Start(ctx.Done())
					ready.Store(true)
					klog.Info("Controller started successfully")
				},
				OnStoppedLeading: func() {
					klog.Info("Lost leadership, stopping controller")
					ready.Store(false)
				},
				OnNewLeader: func(identity string) {
					if identity != getLeaderID() {
						klog.Infof("New leader elected: %s", identity)
					}
				},
			},
			ReleaseOnCancel: true,
			Name:            controllerName,
		})
	} else {
		// Run without leader election
		klog.Info("Leader election disabled, starting controller directly")
		go controller.Run(ctx)
		informerFactory.Start(ctx.Done())
		ready.Store(true)
		klog.Info("Controller started successfully")
	}

	select {
	case <-signalCh:
		klog.Infof("Exiting: received signal")
		cancel()
	case <-ctx.Done():
		klog.Infof("Exiting: context cancelled")
	}
}

// getLeaderID returns a unique identifier for this controller instance
func getLeaderID() string {
	hostname, err := os.Hostname()
	if err != nil {
		klog.Warningf("Failed to get hostname: %v", err)
		hostname = "unknown"
	}
	return fmt.Sprintf("%s_%d", hostname, os.Getpid())
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var vcsRevision, vcsTime string
	for _, f := range info.Settings {
		switch f.Key {
		case "vcs.revision":
			vcsRevision = f.Value
		case "vcs.time":
			vcsTime = f.Value
		}
	}
	klog.Infof("%s go %s build: %s time: %s", controllerName, info.GoVersion, vcsRevision, vcsTime)
}
