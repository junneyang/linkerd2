package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/linkerd/linkerd2/controller/api/public"
	healthcheckPb "github.com/linkerd/linkerd2/controller/gen/common/healthcheck"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/version"
	authorizationapi "k8s.io/api/authorization/v1beta1"
	"k8s.io/api/core/v1"
	k8sVersion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
)

type Checks int

const (
	// KubernetesAPIChecks adds a series of checks to validate that the caller is
	// configured to interact with a working Kubernetes cluster and that the
	// cluster meets the minimum version requirements, unless the
	// ShouldCheckKubeVersion option is false.
	KubernetesAPIChecks Checks = iota

	// LinkerdPreInstallChecks adds a check to validate that the control plane
	// namespace does not already exist. This check only runs as part of the set
	// of pre-install checks.
	// This check is dependent on the output of KubernetesAPIChecks, so those
	// checks must be added first.
	LinkerdPreInstallChecks

	// LinkerdDataPlaneChecks adds a data plane check to validate that the proxy
	// containers are in the ready state.
	// This check is dependent on the output of KubernetesAPIChecks, so those
	// checks must be added first.
	LinkerdDataPlaneChecks

	// LinkerdAPIChecks adds a series of checks to validate that the control plane
	// namespace exists and that it's successfully serving the public API.
	// These checks are dependent on the output of KubernetesAPIChecks, so those
	// checks must be added first.
	LinkerdAPIChecks

	// LinkerdVersionChecks adds a series of checks to validate that the CLI,
	// control plane, and data plane are running the latest available version.
	// These checks are dependent on the output of AddLinkerdAPIChecks, so those
	// checks must be added first, unless the the ShouldCheckControlPlaneVersion
	// and ShouldCheckDataPlaneVersion options are false.
	LinkerdVersionChecks

	KubernetesAPICategory     = "kubernetes-api"
	LinkerdPreInstallCategory = "kubernetes-setup"
	LinkerdDataPlaneCategory  = "linkerd-data-plane"
	LinkerdAPICategory        = "linkerd-api"
	LinkerdVersionCategory    = "linkerd-version"
)

var (
	maxRetries  = 60
	retryWindow = 5 * time.Second
)

type checker struct {
	category      string
	description   string
	fatal         bool
	retryDeadline time.Time
	check         func() error
	checkRPC      func() (*healthcheckPb.SelfCheckResponse, error)
}

type CheckResult struct {
	Category    string
	Description string
	Retry       bool
	Err         error
}

type checkObserver func(*CheckResult)

type HealthCheckOptions struct {
	ControlPlaneNamespace          string
	DataPlaneNamespace             string
	KubeConfig                     string
	APIAddr                        string
	VersionOverride                string
	RetryDeadline                  time.Time
	ShouldCheckKubeVersion         bool
	ShouldCheckControlPlaneVersion bool
	ShouldCheckDataPlaneVersion    bool
	SingleNamespace                bool
}

type HealthChecker struct {
	checkers []*checker
	*HealthCheckOptions

	// these fields are set in the process of running checks
	kubeAPI          *k8s.KubernetesAPI
	httpClient       *http.Client
	clientset        *kubernetes.Clientset
	kubeVersion      *k8sVersion.Info
	controlPlanePods []v1.Pod
	apiClient        pb.ApiClient
	dataPlanePods    []v1.Pod
	latestVersion    string
}

func NewHealthChecker(checks []Checks, options *HealthCheckOptions) *HealthChecker {
	hc := &HealthChecker{
		checkers:           make([]*checker, 0),
		HealthCheckOptions: options,
	}

	for _, check := range checks {
		switch check {
		case KubernetesAPIChecks:
			hc.addKubernetesAPIChecks()
		case LinkerdPreInstallChecks:
			hc.addLinkerdPreInstallChecks()
		case LinkerdDataPlaneChecks:
			hc.addLinkerdDataPlaneChecks()
		case LinkerdAPIChecks:
			hc.addLinkerdAPIChecks()
		case LinkerdVersionChecks:
			hc.addLinkerdVersionChecks()
		}
	}

	return hc
}

func (hc *HealthChecker) addKubernetesAPIChecks() {
	hc.checkers = append(hc.checkers, &checker{
		category:    KubernetesAPICategory,
		description: "can initialize the client",
		fatal:       true,
		check: func() (err error) {
			hc.kubeAPI, err = k8s.NewAPI(hc.KubeConfig)
			return
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    KubernetesAPICategory,
		description: "can query the Kubernetes API",
		fatal:       true,
		check: func() (err error) {
			hc.httpClient, err = hc.kubeAPI.NewClient()
			if err != nil {
				return
			}
			hc.kubeVersion, err = hc.kubeAPI.GetVersionInfo(hc.httpClient)
			return
		},
	})

	if hc.ShouldCheckKubeVersion {
		hc.checkers = append(hc.checkers, &checker{
			category:    KubernetesAPICategory,
			description: "is running the minimum Kubernetes API version",
			fatal:       false,
			check: func() error {
				return hc.kubeAPI.CheckVersion(hc.kubeVersion)
			},
		})
	}
}

func (hc *HealthChecker) addLinkerdPreInstallChecks() {
	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "control plane namespace does not already exist",
		fatal:       false,
		check: func() error {
			exists, err := hc.kubeAPI.NamespaceExists(hc.httpClient, hc.ControlPlaneNamespace)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("The \"%s\" namespace already exists", hc.ControlPlaneNamespace)
			}
			return nil
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "can create Namespaces",
		fatal:       true,
		check: func() error {
			return hc.checkCanCreate("", "", "v1", "Namespace")
		},
	})

	if hc.SingleNamespace {
		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdPreInstallCategory,
			description: "can create Roles",
			fatal:       true,
			check: func() error {
				return hc.checkCanCreate("", "rbac.authorization.k8s.io", "v1beta1", "Role")
			},
		})

		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdPreInstallCategory,
			description: "can create RoleBindings",
			fatal:       true,
			check: func() error {
				return hc.checkCanCreate("", "rbac.authorization.k8s.io", "v1beta1", "RoleBinding")
			},
		})
	} else {
		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdPreInstallCategory,
			description: "can create ClusterRoles",
			fatal:       true,
			check: func() error {
				return hc.checkCanCreate("", "rbac.authorization.k8s.io", "v1beta1", "ClusterRole")
			},
		})

		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdPreInstallCategory,
			description: "can create ClusterRoleBindings",
			fatal:       true,
			check: func() error {
				return hc.checkCanCreate("", "rbac.authorization.k8s.io", "v1beta1", "ClusterRoleBinding")
			},
		})
	}

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "can create ServiceAccounts",
		fatal:       true,
		check: func() error {
			return hc.checkCanCreate(hc.ControlPlaneNamespace, "", "v1", "ServiceAccount")
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "can create Services",
		fatal:       true,
		check: func() error {
			return hc.checkCanCreate(hc.ControlPlaneNamespace, "", "v1", "Service")
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "can create Deployments",
		fatal:       true,
		check: func() error {
			return hc.checkCanCreate(hc.ControlPlaneNamespace, "extensions", "v1beta1", "Deployments")
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdPreInstallCategory,
		description: "can create ConfigMaps",
		fatal:       true,
		check: func() error {
			return hc.checkCanCreate(hc.ControlPlaneNamespace, "", "v1", "ConfigMap")
		},
	})
}

func (hc *HealthChecker) addLinkerdAPIChecks() {
	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdAPICategory,
		description: "control plane namespace exists",
		fatal:       true,
		check: func() error {
			return hc.checkNamespace(hc.ControlPlaneNamespace)
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:      LinkerdAPICategory,
		description:   "control plane pods are ready",
		retryDeadline: hc.RetryDeadline,
		fatal:         true,
		check: func() error {
			var err error
			hc.controlPlanePods, err = hc.kubeAPI.GetPodsByNamespace(hc.httpClient, hc.ControlPlaneNamespace)
			if err != nil {
				return err
			}
			return validateControlPlanePods(hc.controlPlanePods)
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdAPICategory,
		description: "can initialize the client",
		fatal:       true,
		check: func() (err error) {
			if hc.APIAddr != "" {
				hc.apiClient, err = public.NewInternalClient(hc.ControlPlaneNamespace, hc.APIAddr)
			} else {
				hc.apiClient, err = public.NewExternalClient(hc.ControlPlaneNamespace, hc.kubeAPI)
			}
			return
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdAPICategory,
		description: "can query the control plane API",
		fatal:       true,
		checkRPC: func() (*healthcheckPb.SelfCheckResponse, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return hc.apiClient.SelfCheck(ctx, &healthcheckPb.SelfCheckRequest{})
		},
	})
}

func (hc *HealthChecker) addLinkerdDataPlaneChecks() {
	if hc.DataPlaneNamespace != "" {
		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdDataPlaneCategory,
			description: "data plane namespace exists",
			fatal:       true,
			check: func() error {
				return hc.checkNamespace(hc.DataPlaneNamespace)
			},
		})
	}

	hc.checkers = append(hc.checkers, &checker{
		category:      LinkerdDataPlaneCategory,
		description:   "data plane proxies are ready",
		retryDeadline: hc.RetryDeadline,
		fatal:         true,
		check: func() error {
			var err error
			hc.dataPlanePods, err = hc.kubeAPI.GetPodsByControllerNamespace(
				hc.httpClient,
				hc.ControlPlaneNamespace,
				hc.DataPlaneNamespace,
			)
			if err != nil {
				return err
			}

			return validateDataPlanePods(hc.dataPlanePods, hc.DataPlaneNamespace)
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:      LinkerdDataPlaneCategory,
		description:   "data plane proxy metrics are present in Prometheus",
		retryDeadline: hc.RetryDeadline,
		fatal:         false,
		check: func() error {
			req := &pb.ListPodsRequest{}
			if hc.DataPlaneNamespace != "" {
				req.Namespace = hc.DataPlaneNamespace
			}
			// ListPods returns all pods, but we can use the `Added` field to verify
			// which are found in Prometheus
			resp, err := hc.apiClient.ListPods(context.Background(), req)
			if err != nil {
				return err
			}

			return validateDataPlanePodReporting(hc.dataPlanePods, resp.GetPods())
		},
	})
}

func (hc *HealthChecker) addLinkerdVersionChecks() {
	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdVersionCategory,
		description: "can determine the latest version",
		fatal:       true,
		check: func() (err error) {
			if hc.VersionOverride != "" {
				hc.latestVersion = hc.VersionOverride
			} else {
				// The UUID is only known to the web process. At some point we may want
				// to consider providing it in the Public API.
				uuid := "unknown"
				for _, pod := range hc.controlPlanePods {
					if strings.Split(pod.Name, "-")[0] == "web" {
						for _, container := range pod.Spec.Containers {
							if container.Name == "web" {
								for _, arg := range container.Args {
									if strings.HasPrefix(arg, "-uuid=") {
										uuid = strings.TrimPrefix(arg, "-uuid=")
									}
								}
							}
						}
					}
				}
				hc.latestVersion, err = version.GetLatestVersion(uuid, "cli")
			}
			return
		},
	})

	hc.checkers = append(hc.checkers, &checker{
		category:    LinkerdVersionCategory,
		description: "cli is up-to-date",
		fatal:       false,
		check: func() error {
			return version.CheckClientVersion(hc.latestVersion)
		},
	})

	if hc.ShouldCheckControlPlaneVersion {
		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdVersionCategory,
			description: "control plane is up-to-date",
			fatal:       false,
			check: func() error {
				return version.CheckServerVersion(hc.apiClient, hc.latestVersion)
			},
		})
	}

	if hc.ShouldCheckDataPlaneVersion {
		hc.checkers = append(hc.checkers, &checker{
			category:    LinkerdVersionCategory,
			description: "data plane is up-to-date",
			fatal:       false,
			check: func() error {
				return hc.kubeAPI.CheckProxyVersion(hc.dataPlanePods, hc.latestVersion)
			},
		})
	}
}

// Add adds an arbitrary checker. This should only be used for testing. For
// production code, pass in the desired set of checks when calling
// NewHeathChecker.
func (hc *HealthChecker) Add(category, description string, check func() error) {
	hc.checkers = append(hc.checkers, &checker{
		category:    category,
		description: description,
		check:       check,
	})
}

// RunChecks runs all configured checkers, and passes the results of each
// check to the observer. If a check fails and is marked as fatal, then all
// remaining checks are skipped. If at least one check fails, RunChecks returns
// false; if all checks passed, RunChecks returns true.
func (hc *HealthChecker) RunChecks(observer checkObserver) bool {
	success := true

	for _, checker := range hc.checkers {
		if checker.check != nil {
			if !hc.runCheck(checker, observer) {
				success = false
				if checker.fatal {
					break
				}
			}
		}

		if checker.checkRPC != nil {
			if !hc.runCheckRPC(checker, observer) {
				success = false
				if checker.fatal {
					break
				}
			}
		}
	}

	return success
}

func (hc *HealthChecker) runCheck(c *checker, observer checkObserver) bool {
	for {
		err := c.check()
		checkResult := &CheckResult{
			Category:    c.category,
			Description: c.description,
			Err:         err,
		}

		if err != nil && time.Now().Before(c.retryDeadline) {
			checkResult.Retry = true
			observer(checkResult)
			time.Sleep(retryWindow)
			continue
		}

		observer(checkResult)
		return err == nil
	}
}

func (hc *HealthChecker) runCheckRPC(c *checker, observer checkObserver) bool {
	checkRsp, err := c.checkRPC()
	observer(&CheckResult{
		Category:    c.category,
		Description: c.description,
		Err:         err,
	})
	if err != nil {
		return false
	}

	for _, check := range checkRsp.Results {
		var err error
		if check.Status != healthcheckPb.CheckStatus_OK {
			err = fmt.Errorf(check.FriendlyMessageToUser)
		}
		observer(&CheckResult{
			Category:    fmt.Sprintf("%s[%s]", c.category, check.SubsystemName),
			Description: check.CheckDescription,
			Err:         err,
		})
		if err != nil {
			return false
		}
	}

	return true
}

// PublicAPIClient returns a fully configured public API client. This client is
// only configured if the KubernetesAPIChecks and LinkerdAPIChecks are
// configured and run first.
func (hc *HealthChecker) PublicAPIClient() pb.ApiClient {
	return hc.apiClient
}

func (hc *HealthChecker) checkNamespace(namespace string) error {
	exists, err := hc.kubeAPI.NamespaceExists(hc.httpClient, namespace)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("The \"%s\" namespace does not exist", namespace)
	}
	return nil
}

func (hc *HealthChecker) checkCanCreate(namespace, group, version, resource string) error {
	if hc.clientset == nil {
		var err error
		hc.clientset, err = kubernetes.NewForConfig(hc.kubeAPI.Config)
		if err != nil {
			return err
		}
	}

	auth := hc.clientset.AuthorizationV1beta1()

	sar := &authorizationapi.SelfSubjectAccessReview{
		Spec: authorizationapi.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationapi.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Group:     group,
				Version:   version,
				Resource:  resource,
			},
		},
	}

	response, err := auth.SelfSubjectAccessReviews().Create(sar)
	if err != nil {
		return err
	}

	if !response.Status.Allowed {
		if len(response.Status.Reason) > 0 {
			return fmt.Errorf("Missing permissions to create %s: %v", resource, response.Status.Reason)
		}
		return fmt.Errorf("Missing permissions to create %s", resource)
	}
	return nil
}

func validateControlPlanePods(pods []v1.Pod) error {
	statuses := make(map[string][]v1.ContainerStatus)

	for _, pod := range pods {
		if pod.Status.Phase == v1.PodRunning {
			// strip the single-namespace "linkerd-" prefix if it exists
			name := strings.TrimPrefix(pod.Name, "linkerd-")
			name = strings.Split(name, "-")[0]
			if _, found := statuses[name]; !found {
				statuses[name] = make([]v1.ContainerStatus, 0)
			}
			statuses[name] = append(statuses[name], pod.Status.ContainerStatuses...)
		}
	}

	names := []string{"controller", "grafana", "prometheus", "web"}
	if _, found := statuses["ca"]; found {
		names = append(names, "ca")
	}

	for _, name := range names {
		containers, found := statuses[name]
		if !found {
			return fmt.Errorf("No running pods for \"%s\"", name)
		}
		for _, container := range containers {
			if !container.Ready {
				return fmt.Errorf("The \"%s\" pod's \"%s\" container is not ready", name,
					container.Name)
			}
		}
	}

	return nil
}

func validateDataPlanePods(pods []v1.Pod, targetNamespace string) error {
	if len(pods) == 0 {
		msg := fmt.Sprintf("No \"%s\" containers found", k8s.ProxyContainerName)
		if targetNamespace != "" {
			msg += fmt.Sprintf(" in the \"%s\" namespace", targetNamespace)
		}
		return fmt.Errorf(msg)
	}

	for _, pod := range pods {
		if pod.Status.Phase != v1.PodRunning {
			return fmt.Errorf("The \"%s\" pod in the \"%s\" namespace is not running",
				pod.Name, pod.Namespace)
		}

		var proxyReady bool
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name == k8s.ProxyContainerName {
				proxyReady = container.Ready
			}
		}

		if !proxyReady {
			return fmt.Errorf("The \"%s\" container in the \"%s\" pod in the \"%s\" namespace is not ready",
				k8s.ProxyContainerName, pod.Name, pod.Namespace)
		}
	}

	return nil
}

func validateDataPlanePodReporting(k8sPods []v1.Pod, promPods []*pb.Pod) error {
	k8sMap := map[string]struct{}{}
	promMap := map[string]struct{}{}

	for _, p := range k8sPods {
		k8sMap[p.Namespace+"/"+p.Name] = struct{}{}
	}
	for _, p := range promPods {
		// the `Added` field indicates the pod was found in Prometheus
		if p.Added {
			promMap[p.Name] = struct{}{}
		}
	}

	onlyInK8s := []string{}
	for k := range k8sMap {
		if _, ok := promMap[k]; !ok {
			onlyInK8s = append(onlyInK8s, k)
		}
	}

	onlyInProm := []string{}
	for k := range promMap {
		if _, ok := k8sMap[k]; !ok {
			onlyInProm = append(onlyInProm, k)
		}
	}

	errMsg := ""
	if len(onlyInK8s) > 0 {
		errMsg = fmt.Sprintf("Data plane metrics not found for %s. ", strings.Join(onlyInK8s, ", "))
	}
	if len(onlyInProm) > 0 {
		errMsg += fmt.Sprintf("Found data plane metrics for %s, but not found in Kubernetes.", strings.Join(onlyInProm, ", "))
	}

	if errMsg != "" {
		return fmt.Errorf(errMsg)
	}

	return nil
}
