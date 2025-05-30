package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/operator-framework/api/pkg/lib/version"
	ofapiv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/opendatahub-operator/v2/api/common"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/cluster/gvk"
)

type ClusterInfo struct {
	Type        string                  `json:"type,omitempty"` // openshift , TODO: can be other value if we later support other type
	Version     version.OperatorVersion `json:"version,omitempty"`
	FipsEnabled bool                    `json:"fips_enabled,omitempty"`
}

var clusterConfig struct {
	Namespace   string
	Release     common.Release
	ClusterInfo ClusterInfo
}

type InstallConfig struct {
	FIPS bool `json:"fips"`
}

// Init initializes cluster configuration variables on startup
// init() won't work since it is needed to check the error.
func Init(ctx context.Context, cli client.Client) error {
	var err error
	log := logf.FromContext(ctx)

	clusterConfig.Namespace, err = getOperatorNamespace()
	if err != nil {
		log.Error(err, "unable to find operator namespace")
		// not fatal, fallback to ""
	}

	clusterConfig.Release, err = getRelease(ctx, cli)
	if err != nil {
		return err
	}

	clusterConfig.ClusterInfo, err = getClusterInfo(ctx, cli)
	if err != nil {
		return err
	}

	printClusterConfig(log)

	return nil
}

func printClusterConfig(log logr.Logger) {
	log.Info("Cluster config",
		"Operator Namespace", clusterConfig.Namespace,
		"Release", clusterConfig.Release,
		"Cluster", clusterConfig.ClusterInfo)
}

func GetOperatorNamespace() (string, error) {
	if clusterConfig.Namespace == "" {
		return "", errors.New("unable to find operator namespace")
	}
	return clusterConfig.Namespace, nil
}

func GetRelease() common.Release {
	return clusterConfig.Release
}

func GetClusterInfo() ClusterInfo {
	return clusterConfig.ClusterInfo
}

func GetDomain(ctx context.Context, c client.Client) (string, error) {
	ingress := &unstructured.Unstructured{}
	ingress.SetGroupVersionKind(gvk.OpenshiftIngress)

	if err := c.Get(ctx, client.ObjectKey{
		Namespace: "",
		Name:      "cluster",
	}, ingress); err != nil {
		return "", fmt.Errorf("failed fetching cluster's ingress details: %w", err)
	}

	domain, found, err := unstructured.NestedString(ingress.Object, "spec", "domain")
	if !found {
		return "", errors.New("spec.domain not found")
	}

	return domain, err
}

// This is an openshift speicifc implementation.
func getOCPVersion(ctx context.Context, c client.Client) (version.OperatorVersion, error) {
	clusterVersion := &configv1.ClusterVersion{}
	if err := c.Get(ctx, client.ObjectKey{
		Name: OpenShiftVersionObj,
	}, clusterVersion); err != nil {
		return version.OperatorVersion{}, errors.New("unable to get OCP version")
	}
	v, err := semver.ParseTolerant(clusterVersion.Status.History[0].Version)
	if err != nil {
		return version.OperatorVersion{}, errors.New("unable to parse OCP version")
	}
	return version.OperatorVersion{Version: v}, nil
}

func getOperatorNamespace() (string, error) {
	operatorNS, exist := os.LookupEnv("OPERATOR_NAMESPACE")
	if exist && operatorNS != "" {
		return operatorNS, nil
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	return string(data), err
}

func IsNotReservedNamespace(ns *corev1.Namespace) bool {
	return !IsReservedNamespace(ns)
}

func IsReservedNamespace(ns *corev1.Namespace) bool {
	switch {
	case strings.HasPrefix(ns.GetName(), "openshift-"):
		return true
	case strings.HasPrefix(ns.GetName(), "kube-"):
		return true
	case ns.GetName() == "default":
		return true
	case ns.GetName() == "openshift":
		return true
	default:
		return false
	}
}

func IsActiveNamespace(ns *corev1.Namespace) bool {
	return ns.Status.Phase == corev1.NamespaceActive
}

// GetClusterServiceVersion retries CSV only from the defined namespace.
func GetClusterServiceVersion(ctx context.Context, c client.Client, namespace string) (*ofapiv1alpha1.ClusterServiceVersion, error) {
	clusterServiceVersionList := &ofapiv1alpha1.ClusterServiceVersionList{}
	paginateListOption := &client.ListOptions{
		Limit:     100,
		Namespace: namespace,
	}
	for { // for the case we have very big size of CSV even just in one namespace
		if err := c.List(ctx, clusterServiceVersionList, paginateListOption); err != nil {
			return nil, fmt.Errorf("failed listing cluster service versions for %s: %w", namespace, err)
		}
		for _, csv := range clusterServiceVersionList.Items {
			for _, operatorCR := range csv.Spec.CustomResourceDefinitions.Owned {
				if operatorCR.Kind == "DataScienceCluster" {
					return &csv, nil
				}
			}
		}
		if paginateListOption.Continue = clusterServiceVersionList.GetContinue(); paginateListOption.Continue == "" {
			break
		}
	}

	return nil, k8serr.NewNotFound(
		schema.GroupResource{Group: gvk.ClusterServiceVersion.Group},
		gvk.ClusterServiceVersion.Kind)
}

// detectSelfManaged detects if it is Self Managed Rhoai or OpenDataHub.
func detectSelfManaged(ctx context.Context, cli client.Client) (common.Platform, error) {
	exists, err := OperatorExists(ctx, cli, "rhods-operator")
	if exists {
		return SelfManagedRhoai, nil
	}

	return OpenDataHub, err
}

// detectManagedRhoai checks if catsrc CR add-on exists ManagedRhoai.
func detectManagedRhoai(ctx context.Context, cli client.Client) (common.Platform, error) {
	catalogSource := &ofapiv1alpha1.CatalogSource{}
	operatorNs, err := GetOperatorNamespace()
	if err != nil {
		operatorNs = "redhat-ods-operator"
	}
	err = cli.Get(ctx, client.ObjectKey{Name: "addon-managed-odh-catalog", Namespace: operatorNs}, catalogSource)
	if err != nil {
		return OpenDataHub, client.IgnoreNotFound(err)
	}
	return ManagedRhoai, nil
}

func getPlatform(ctx context.Context, cli client.Client) (common.Platform, error) {
	switch os.Getenv("ODH_PLATFORM_TYPE") {
	case "OpenDataHub":
		return OpenDataHub, nil
	case "ManagedRHOAI":
		return ManagedRhoai, nil
	case "SelfManagedRHOAI":
		return SelfManagedRhoai, nil
	default:
		// fall back to detect platform if ODH_PLATFORM_TYPE env is not provided in CSV or set to ""
		platform, err := detectManagedRhoai(ctx, cli)
		if err != nil {
			return OpenDataHub, err
		}
		if platform == ManagedRhoai {
			return ManagedRhoai, nil
		}
		return detectSelfManaged(ctx, cli)
	}
}

func getRelease(ctx context.Context, cli client.Client) (common.Release, error) {
	initRelease := common.Release{
		// dummy version set to name "", version 0.0.0
		Version: version.OperatorVersion{
			Version: semver.Version{},
		},
	}

	// Set platform
	platform, err := getPlatform(ctx, cli)
	if err != nil {
		return initRelease, err
	}
	initRelease.Name = platform

	// For unit-tests
	if os.Getenv("CI") == "true" {
		return initRelease, nil
	}

	// Set Version
	// Get watchNamespace
	operatorNamespace, err := GetOperatorNamespace()
	if err != nil {
		// unit test does not have k8s file or env var set, return default
		return initRelease, err
	}
	csv, err := GetClusterServiceVersion(ctx, cli, operatorNamespace)
	if k8serr.IsNotFound(err) {
		// hide not found, return default
		return initRelease, nil
	}
	if err != nil {
		return initRelease, err
	}
	initRelease.Version = csv.Spec.Version
	return initRelease, nil
}

func getClusterInfo(ctx context.Context, cli client.Client) (ClusterInfo, error) {
	c := ClusterInfo{
		Version: version.OperatorVersion{
			Version: semver.Version{},
		},
		Type:        "OpenShift",
		FipsEnabled: false,
	}
	// Set OCP
	ocpVersion, err := getOCPVersion(ctx, cli)
	if err != nil {
		return c, err
	}
	c.Version = ocpVersion

	// Check for FIPs
	if fipsEnabled, err := IsFipsEnabled(ctx, cli); err == nil {
		c.FipsEnabled = fipsEnabled
	} else {
		logf.FromContext(ctx).Info("could not determine FIPS status, defaulting to false", "error", err)
	}

	return c, nil
}

func IsFipsEnabled(ctx context.Context, cli client.Client) (bool, error) {
	// Check the install-config for the fips flag and it's value
	// https://access.redhat.com/solutions/6525331
	cm := &corev1.ConfigMap{}
	namespacedName := types.NamespacedName{
		Name:      "cluster-config-v1",
		Namespace: "kube-system",
	}

	if err := cli.Get(ctx, namespacedName, cm); err != nil {
		return false, err
	}

	installConfigStr := cm.Data["install-config"]
	if installConfigStr == "" {
		// No install-config found or empty
		return false, nil
	}

	var installConfig InstallConfig
	if err := yaml.Unmarshal([]byte(installConfigStr), &installConfig); err != nil {
		// fallback: ignore unmarshal error and fall back to string search
		return strings.Contains(strings.ToLower(installConfigStr), "fips: true"), nil //nolint:nilerr
	}

	return installConfig.FIPS, nil
}
