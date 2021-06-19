// Package manager implements the controller manager for all controllers in Railgun.
package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bombsimon/logrusr"
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kong/kubernetes-ingress-controller/pkg/sendconfig"
	"github.com/kong/kubernetes-ingress-controller/pkg/util"

	konghqcomv1 "github.com/kong/kubernetes-ingress-controller/railgun/apis/configuration/v1"
	configurationv1alpha1 "github.com/kong/kubernetes-ingress-controller/railgun/apis/configuration/v1alpha1"
	configurationv1beta1 "github.com/kong/kubernetes-ingress-controller/railgun/apis/configuration/v1beta1"
	"github.com/kong/kubernetes-ingress-controller/railgun/controllers/configuration"
	kongctrl "github.com/kong/kubernetes-ingress-controller/railgun/controllers/configuration"
	"github.com/kong/kubernetes-ingress-controller/railgun/internal/ctrlutils"
	"github.com/kong/kubernetes-ingress-controller/railgun/internal/mgrutils"
	"github.com/kong/kubernetes-ingress-controller/railgun/internal/proxy"
	"github.com/kong/kubernetes-ingress-controller/railgun/pkg/config"
	knativev1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/signals"

	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	knativeversioned "knative.dev/networking/pkg/client/clientset/versioned"
	knativeinformerexternal "knative.dev/networking/pkg/client/informers/externalversions"
)

// -----------------------------------------------------------------------------
// Controller Manager - Setup & Run
// -----------------------------------------------------------------------------

// Run starts the controller manager and blocks until it exits.
func Run(ctx context.Context, c *config.Config) error {
	var deprecatedLogger logrus.FieldLogger
	var err error

	if v := os.Getenv("KONG_TEST_ENVIRONMENT"); v != "" {
		deprecatedLogger = util.MakeDebugLoggerWithReducedRedudancy(os.Stdout, &logrus.TextFormatter{}, 3, time.Second*30)
		deprecatedLogger.Info("detected that the controller is running in an automated testing environment: " +
			"log stifling has been enabled")
	} else {
		deprecatedLogger, err = util.MakeLogger(c.LogLevel, c.LogFormat)
		if err != nil {
			return fmt.Errorf("failed to make logger: %w", err)
		}
	}
	var logger logr.Logger = logrusr.NewLogger(deprecatedLogger)

	ctrl.SetLogger(logger)
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting controller manager", "release", Release, "repo", Repo, "commit", Commit)

	kubeconfig, err := c.GetKubeconfig()
	if err != nil {
		return fmt.Errorf("get kubeconfig from file %q: %w", c.KubeconfigPath, err)
	}

	// set "kubernetes.io/ingress.class" to be used by controllers (defaults to "kong")
	setupLog.Info(`the ingress class name has been set`, "value", c.IngressClassName)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(konghqcomv1.AddToScheme(scheme))
	utilruntime.Must(configurationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(configurationv1beta1.AddToScheme(scheme))
	utilruntime.Must(knativev1alpha1.AddToScheme(scheme))

	controllerOpts := ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     c.MetricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: c.ProbeAddr,
		LeaderElection:         c.EnableLeaderElection,
		LeaderElectionID:       c.LeaderElectionID,
	}

	// determine how to configure namespace watchers
	if strings.Contains(c.WatchNamespace, ",") {
		setupLog.Info("manager set up with multiple namespaces", "namespaces", c.WatchNamespace)
		// this mode does not set the Namespace option, so the manager will default to watching all namespaces
		// MultiNamespacedCacheBuilder imposes a filter on top of that watch to retrieve scoped resources
		// from the watched namespaces only.
		controllerOpts.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(c.WatchNamespace, ","))
	} else {
		controllerOpts.Namespace = c.WatchNamespace
	}

	// build the controller manager
	mgr, err := ctrl.NewManager(kubeconfig, controllerOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}

	kongClient, err := c.GetKongClient(ctx)
	if err != nil {
		setupLog.Error(err, "cannot create a Kong Admin API client")
		return err
	}

	// configure the kong client
	kongConfig := sendconfig.Kong{
		URL:               c.KongAdminURL,
		FilterTags:        c.FilterTags,
		Concurrency:       c.Concurrency,
		Client:            kongClient,
		PluginSchemaStore: util.NewPluginSchemaStore(kongClient),
	}

	// determine the proxy synchronization strategy
	syncTickDuration, err := time.ParseDuration(fmt.Sprintf("%gs", c.ProxySyncSeconds))
	if err != nil {
		setupLog.Error(err, "%s is not a valid number of seconds to stagger the proxy server synchronization")
		return err
	}

	// determine the proxy timeout
	timeoutDuration, err := time.ParseDuration(fmt.Sprintf("%gs", c.ProxyTimeoutSeconds))
	if err != nil {
		setupLog.Error(err, "%s is not a valid number of seconds to the timeout config for the kong client")
		return err
	}

	// start the proxy cache server
	prx, err := proxy.NewCacheBasedProxyWithStagger(ctx,
		// NOTE: logr-based loggers use the "logger" field instead of "subsystem". When replacing logrus with logr, replace
		// WithField("subsystem", ...) with WithName(...).
		deprecatedLogger.WithField("subsystem", "proxy-cache-resolver"),
		mgr.GetClient(),
		kongConfig,
		c.IngressClassName,
		c.EnableReverseSync,
		syncTickDuration,
		timeoutDuration,
		sendconfig.UpdateKongAdminSimple)
	if err != nil {
		setupLog.Error(err, "unable to start proxy cache server")
		return err
	}

	enableKnative := util.EnablementStatusDisabled
	if ctrlutils.KnativeCRDExist(mgr.GetClient()) == true {
		setupLog.Info("ingresses.networking.internal.knative.dev crd exists. enable knative contorller.")
		enableKnative = util.EnablementStatusEnabled
	}
	controllers := []ControllerDef{
		// ---------------------------------------------------------------------------
		// Core API Controllers
		// ---------------------------------------------------------------------------

		{
			IsEnabled: &c.ServiceEnabled,
			Controller: &configuration.CoreV1ServiceReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("Service"),
				Scheme: mgr.GetScheme(),
				Proxy:  prx,
			},
		},
		{
			IsEnabled: &c.ServiceEnabled,
			Controller: &configuration.CoreV1EndpointsReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("Endpoints"),
				Scheme: mgr.GetScheme(),
				Proxy:  prx,
			},
		},
		{
			IsEnabled: &c.IngressNetV1Enabled,
			Controller: &configuration.NetV1IngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("Ingress").WithName("netv1"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &c.IngressNetV1beta1Enabled,
			Controller: &configuration.NetV1Beta1IngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("Ingress").WithName("netv1beta1"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &c.IngressExtV1beta1Enabled,
			Controller: &configuration.ExtV1Beta1IngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("Ingress").WithName("extv1beta1"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},

		// ---------------------------------------------------------------------------
		// Kong API Controllers
		// ---------------------------------------------------------------------------
		{
			IsEnabled: &c.UDPIngressEnabled,
			Controller: &kongctrl.KongV1Alpha1UDPIngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("UDPIngress"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &c.TCPIngressEnabled,
			Controller: &kongctrl.KongV1Beta1TCPIngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("TCPIngress"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &enableKnative,
			Controller: &kongctrl.Knativev1alpha1IngressReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("Ingress").WithName("KnativeV1Alpha1"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &c.KongIngressEnabled,
			Controller: &kongctrl.KongV1KongIngressReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("KongIngress"),
				Scheme: mgr.GetScheme(),
				Proxy:  prx,
			},
		},
		{
			IsEnabled: &c.KongClusterPluginEnabled,
			Controller: &kongctrl.KongV1KongClusterPluginReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("KongClusterPlugin"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
		{
			IsEnabled: &c.KongPluginEnabled,
			Controller: &kongctrl.KongV1KongPluginReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("KongPlugin"),
				Scheme: mgr.GetScheme(),
				Proxy:  prx,
			},
		},
		{
			IsEnabled: &c.KongConsumerEnabled,
			Controller: &kongctrl.KongV1KongConsumerReconciler{
				Client:           mgr.GetClient(),
				Log:              ctrl.Log.WithName("controllers").WithName("KongConsumer"),
				Scheme:           mgr.GetScheme(),
				Proxy:            prx,
				IngressClassName: c.IngressClassName,
			},
		},
	}

	for _, c := range controllers {
		if err := c.MaybeSetupWithManager(mgr); err != nil {
			return fmt.Errorf("unable to create controller %q: %w", c.Name(), err)
		}
	}

	// BUG: kubebuilder (at the time of writing - 3.0.0-rc.1) does not allow this tag anywhere else than main.go
	// See https://github.com/kubernetes-sigs/kubebuilder/issues/932
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		return fmt.Errorf("unable to setup healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		return fmt.Errorf("unable to setup readyz: %w", err)
	}

	if c.AnonymousReports {
		setupLog.Info("running anonymous reports")
		if err := mgrutils.RunReport(ctx, kubeconfig, kongConfig, Release); err != nil {
			setupLog.Error(err, "anonymous reporting failed")
		}
	} else {
		setupLog.Info("anonymous reports disabled, skipping")
	}
	go flipKnativeController(mgr, prx, &enableKnative, c)
	setupLog.Info("starting manager")
	return mgr.Start(ctx)
}

// wait for knative cr before register and starting knative controller
func flipKnativeController(mgr manager.Manager, prx proxy.Proxy, enablestatus *util.EnablementStatus, cfg *config.Config) error {
	if *enablestatus == util.EnablementStatusEnabled {
		fmt.Printf("knative controller already enabled. skip flip process.\n")
		return nil
	}
	//kubeCfg, err := ctrlutils.InClusterConfig()
	kubeCfg, err := clientcmd.BuildConfigFromFlags("", "/home/ec2-user/.kube/config")
	if err != nil {
		return fmt.Errorf("failed to generate incluster configuration. err %v", err)
	}
	knativeCli, err := knativeversioned.NewForConfig(kubeCfg)
	if err != nil {
		return fmt.Errorf("failed to generate knative client. err %v", err)
	}
	knativeFactory := knativeinformerexternal.NewSharedInformerFactory(knativeCli, 0)
	knativeInformer := knativeFactory.Networking().V1alpha1().Ingresses().Informer()
	_, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	knativeInformer.AddEventHandler(&k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fmt.Printf("knative networking customer resource added.\n")
			if *enablestatus == util.EnablementStatusDisabled {
				fmt.Printf("knative controller does not exist. register one.")
				knative := configuration.Knativev1alpha1IngressReconciler{
					Client:           mgr.GetClient(),
					Log:              ctrl.Log.WithName("controllers").WithName("Ingress").WithName("KnativeV1Alpha1"),
					Scheme:           mgr.GetScheme(),
					IngressClassName: cfg.IngressClassName,
					Proxy:            prx,
				}
				knative.SetupWithManager(mgr)
				*enablestatus = util.EnablementStatusEnabled
			} else {
				fmt.Printf("knative controller already exist. Skip registration.\n")
			}
			cancel()
		},
	})
	stopCh := signals.SetupSignalHandler()
	fmt.Println("starting informer.")
	knativeFactory.Start(stopCh)
	fmt.Println("end of starting informer.")
	return nil
}
