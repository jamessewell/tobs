package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/spf13/cobra"
	root "github.com/timescale/tobs/cli/cmd"
	"github.com/timescale/tobs/cli/cmd/common"
	"github.com/timescale/tobs/cli/cmd/install"
	"github.com/timescale/tobs/cli/pkg/helm"
	"github.com/timescale/tobs/cli/pkg/k8s"
	"github.com/timescale/tobs/cli/pkg/otel"
	"github.com/timescale/tobs/cli/pkg/utils"
	"gopkg.in/yaml.v2"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// upgradeCmd represents the upgrade command
var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrades The Observability Stack",
	Args:  cobra.ExactArgs(0),
	RunE:  upgrade,
}

func init() {
	root.RootCmd.AddCommand(upgradeCmd)
	root.AddRootFlags(upgradeCmd)
	upgradeCmd.Flags().BoolP("reset-values", "", false, "Reset helm chart to default values of the helm chart. This is same flag that exists in helm upgrade")
	upgradeCmd.Flags().BoolP("reuse-values", "", false, "Reuse the last release's values and merge in any overrides from the command line via --set and -f. If '--reset-values' is specified, this is ignored.\nThis is same flag that exists in helm upgrade ")
	upgradeCmd.Flags().BoolP("same-chart", "", false, "Use the same helm chart do not upgrade helm chart but upgrade the existing chart with new values")
	upgradeCmd.Flags().BoolP("confirm", "y", false, "Confirmation flag for upgrading")
	upgradeCmd.Flags().BoolP("skip-crds", "", false, "Option to skip creating CRDs on upgrade")
}

func upgrade(cmd *cobra.Command, args []string) error {
	return upgradeTobs(cmd, args)
}

type upgradeSpec struct {
	deployedChartVersion string
	newChartVersion      string
	skipCrds             bool
	k8sClient            k8s.Client
	upgradeValues        string
	chartRef             string
	valuesFile           string
	upgradeCertManager   bool
}

func upgradeTobs(cmd *cobra.Command, args []string) error {
	file, err := cmd.Flags().GetString("filename")
	if err != nil {
		return fmt.Errorf("couldn't get the filename flag value: %w", err)
	}

	ref, err := cmd.Flags().GetString("chart-reference")
	if err != nil {
		return fmt.Errorf("couldn't get the chart-reference flag value: %w", err)
	}

	reset, err := cmd.Flags().GetBool("reset-values")
	if err != nil {
		return fmt.Errorf("couldn't get the reset-values flag value: %w", err)
	}

	reuse, err := cmd.Flags().GetBool("reuse-values")
	if err != nil {
		return fmt.Errorf("couldn't get the reuse-values flag value: %w", err)
	}

	confirm, err := cmd.Flags().GetBool("confirm")
	if err != nil {
		return fmt.Errorf("couldn't get the confirm flag value: %w", err)
	}

	sameChart, err := cmd.Flags().GetBool("same-chart")
	if err != nil {
		return fmt.Errorf("couldn't get the same-chart flag value: %w", err)
	}

	skipCrds, err := cmd.Flags().GetBool("skip-crds")
	if err != nil {
		return fmt.Errorf("could not install The Observability Stack: %w", err)
	}

	upgradeHelmSpec := &helm.ChartSpec{
		ReleaseName: root.HelmReleaseName,
		ChartName:   ref,
		Namespace:   root.Namespace,
		ResetValues: reset,
		ReuseValues: reuse,
	}

	if file != "" {
		upgradeHelmSpec.ValuesFiles = []string{file}
	}

	helmClient := helm.NewClient(root.Namespace)
	defer helmClient.Close()
	latestChart, err := helmClient.GetChartMetadata(ref)
	if err != nil {
		return err
	}

	deployedChart, err := helmClient.GetDeployedChartMetadata(root.HelmReleaseName, root.Namespace)
	if err != nil {
		if err.Error() != utils.ErrorTobsDeploymentNotFound(root.HelmReleaseName).Error() {
			return err
		} else {
			fmt.Println("couldn't find the existing tobs deployment. Deploying tobs...")
			if !confirm {
				utils.ConfirmAction()
			}
			s := install.InstallSpec{
				ConfigFile: file,
				Ref:        ref,
			}
			err = s.InstallStack()
			if err != nil {
				return err
			}
			return nil
		}
	}

	// add & update helm chart only if it's default chart
	// if same-chart upgrade is disabled
	if ref == utils.DEFAULT_CHART && !sameChart {
		err = helmClient.AddOrUpdateChartRepo(utils.DEFAULT_REGISTRY_NAME, utils.REPO_LOCATION)
		if err != nil {
			return fmt.Errorf("failed to add & update tobs helm chart %v", err)
		}
	}

	lVersion, err := utils.ParseVersion(latestChart.Version, 3)
	if err != nil {
		return fmt.Errorf("failed to parse latest helm chart version %w", err)
	}

	dVersion, err := utils.ParseVersion(deployedChart.Version, 3)
	if err != nil {
		return fmt.Errorf("failed to parse deployed helm chart version %w", err)
	}

	var foundNewChart bool
	if lVersion <= dVersion {
		dValues, err := helmClient.GetReleaseValues(root.HelmReleaseName)
		if err != nil {
			return err
		}

		nValues, err := helmClient.GetValuesYamlFromChart(ref, file)
		if err != nil {
			return err
		}

		deployedValuesBytes, err := json.Marshal(dValues)
		if err != nil {
			return err
		}

		newValuesBytes, err := json.Marshal(nValues)
		if err != nil {
			return err
		}

		if ok := reflect.DeepEqual(deployedValuesBytes, newValuesBytes); ok {
			err = errors.New("failed to upgrade there is no latest helm chart available and existing helm deployment values are same as the provided values")
			return err
		}
	} else {
		foundNewChart = true
		if sameChart {
			err = errors.New("provided helm chart is newer compared to existing deployed helm chart cannot upgrade as --same-chart flag is provided")
			return err
		}
	}

	if foundNewChart {
		fmt.Printf("Upgrading to latest helm chart version: %s\n", latestChart.Version)
	} else {
		fmt.Println("Upgrading the existing helm chart with values.yaml file")
	}

	if !confirm {
		utils.ConfirmAction()
	}

	upgradeDetails := &upgradeSpec{
		deployedChartVersion: deployedChart.Version,
		newChartVersion:      latestChart.Version,
		skipCrds:             skipCrds,
		k8sClient:            k8s.NewClient(),
		chartRef:             ref,
		valuesFile:           file,
	}

	err = upgradeDetails.UpgradePathBasedOnVersion()
	if err != nil {
		return err
	}
	upgradeHelmSpec.ValuesYaml = upgradeDetails.upgradeValues

	helmClient = helm.NewClient(root.Namespace)
	_, err = helmClient.InstallOrUpgradeChart(context.Background(), upgradeHelmSpec)
	if err != nil {
		return fmt.Errorf("failed to upgrade %w", err)
	}

	// upgrade cert-manager post upgrade process as
	// helm diff tries to evaluate resources with required APIVersions
	// upgrading cert-manager prior to helm upgrade prompts the below error
	//
	// Error: failed to upgrade current release manifest contains removed kubernetes api(s)
	// for this kubernetes version and it is therefore unable to build the kubernetes objects for performing the diff.
	// error from kubernetes: [unable to recognize "": no matches for kind "Certificate" in version "cert-manager.io/v1alpha2",
	// unable to recognize "": no matches for kind "Issuer" in version "cert-manager.io/v1alpha2"]
	//
	// This is expected as upgrade CM before helm upgrade doesn't support
	// he deprecated API's tha helm expects to have.
	if upgradeDetails.upgradeCertManager {
		err = otel.UpgradeCertManager()
		if err != nil {
			return err
		}
	}

	fmt.Printf("Successfully upgraded %s to version: %s\n", root.HelmReleaseName, latestChart.Version)
	return nil
}

func (c *upgradeSpec) UpgradePathBasedOnVersion() error {
	nVersion, err := utils.ParseVersion(c.newChartVersion, 3)
	if err != nil {
		return fmt.Errorf("failed to parse latest helm chart version %w", err)
	}

	dVersion, err := utils.ParseVersion(c.deployedChartVersion, 3)
	if err != nil {
		return fmt.Errorf("failed to parse deployed helm chart version %w", err)
	}

	version0_2_2, err := utils.ParseVersion("0.2.2", 3)
	if err != nil {
		return fmt.Errorf("failed to parse 0.2.2 version %w", err)
	}

	version0_4_0, err := utils.ParseVersion(utils.Version_040, 3)
	if err != nil {
		return fmt.Errorf("failed to parse 0.4.0 version %w", err)
	}

	version0_8_0, err := utils.ParseVersion("0.8.0", 3)
	if err != nil {
		return fmt.Errorf("failed to parse 0.8.0 version %w", err)
	}

	// kube-prometheus is introduced on tobs >= 0.4.0 release
	// so create CRDs if version >= 0.4.0 and only create CRDs
	// if version change is noticed in upgrades...
	if nVersion >= version0_4_0 && dVersion <= version0_4_0 && nVersion != dVersion {
		if !c.skipCrds {
			// Kube-Prometheus CRDs
			err = c.applyCRDS(kubePrometheusCRDs)
			if err != nil {
				return err
			}
		}

		prometheusNodeExporter := root.HelmReleaseName + "-prometheus-node-exporter"
		err = c.k8sClient.DeleteDaemonset(prometheusNodeExporter, root.Namespace)
		if err != nil {
			ok := errors2.IsNotFound(err)
			if !ok {
				return fmt.Errorf("failed to delete %s daemonset %v", prometheusNodeExporter, err)
			}
		}
		err = c.k8sClient.KubeDeleteService(root.Namespace, prometheusNodeExporter)
		if err != nil {
			ok := errors2.IsNotFound(err)
			if !ok {
				return fmt.Errorf("failed to delete %s service %v", prometheusNodeExporter, err)
			}
		}

		if dVersion < version0_4_0 {
			err = c.persistPrometheusDataDuringUpgrade()
			if err != nil {
				return err
			}
		}
	}

	switch {
	// The below case if for upgrade from any versions <= 0.2.2 to greater versions
	case dVersion <= version0_2_2 && nVersion > version0_2_2:
		return fmt.Errorf("upgrade from version below 0.2.2 is no longer supported in this tobs verison. Please use older tobs binary to do a step-by-step upgrade")
	case dVersion < version0_8_0:
		err = c.upgradeTo08X()
		if err != nil {
			return fmt.Errorf("failed to perform upgrade path to 0.8.0 %v", err)
		}

	default:
		// if the upgrade doesn't match the above condition
		// that means we do not have an upgrade path for the base version to new version
		// Note: This is helpful when someone wants to upgrade with just values.yaml (not between versions)
		return nil
	}

	return nil
}

func (c *upgradeSpec) upgradeTo08X() error {
	helmClient := helm.NewClient(root.Namespace)
	defer helmClient.Close()
	releaseValues, err := helmClient.GetReleaseValues(root.HelmReleaseName)
	if err != nil {
		return err
	}

	// capture existing TimescaleDB secret
	isTSDBEnabled, err := common.IsTimescaleDBEnabled(root.HelmReleaseName, root.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get is timescaledb enabled value %v", err)
	}

	var tsdbSecretValue string
	if isTSDBEnabled {
		tsdbSecret, err := c.k8sClient.KubeGetSecret(root.Namespace, root.HelmReleaseName+"-credentials")
		if err != nil {
			return fmt.Errorf("failed to get secret %v", err)
		}
		tsdbSecretValue = string(tsdbSecret.Data[common.DBSuperUserSecretKey])
	}

	// Delete kube-state-metrics as per kube-prometheus upgrade guide
	err = c.k8sClient.DeleteDeployment(map[string]string{"app.kubernetes.io/instance": root.HelmReleaseName,
		"app.kubernetes.io/name": "kube-state-metrics"}, root.Namespace)
	if err != nil {
		return fmt.Errorf("failed to delete kube-state-metrics deployment %v", err)
	}

	// Delete the grafana-db job to re-run the job on upgrade
	// and the db job includes changes to spec in 0.8.0 version
	grafanaJob := root.HelmReleaseName + "-grafana-db"
	err = c.k8sClient.DeleteJob(grafanaJob, root.Namespace)
	if err != nil && !errors2.IsNotFound(err) {
		return fmt.Errorf("failed to delete %s job %v", grafanaJob, err)
	}

	// update Kube-Prometheus CRDs
	err = c.applyCRDS(kubePrometheusCRDs)
	if err != nil {
		return err
	}

	// delete timescaledbExternal section in values.yaml
	var externalDBURI string
	_, tsdbExternalExists := releaseValues["timescaledbExternal"]
	if tsdbExternalExists {
		timescaleDBExternal, ok := releaseValues["timescaledbExternal"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("timescaledbExternal is not the expected type: %T", releaseValues["timescaledbExternal"])
		}
		if timescaleDBExternal != nil {
			isExternalTsdbenabled, ok := timescaleDBExternal["enabled"].(bool)
			if !ok {
				return fmt.Errorf("timescaledbExternal.enabled is not the expected type: %T", timescaleDBExternal["enabled"])
			}
			if isExternalTsdbenabled {
				externalDBURI, ok = timescaleDBExternal["db_uri"].(string)
				if !ok {
					return fmt.Errorf("timescaledbExternal.db_uri is not the expected type: %T", timescaleDBExternal["db_uri"])
				}
			}
			delete(releaseValues, "timescaledbExternal")
		}
	}

	// refactor promscale section in values.yaml
	var isTracingEnabled, ok bool
	var promscaleValues map[string]interface{}
	_, promscaleExists := releaseValues["promscale"]
	if promscaleExists {
		promscaleValues, ok = releaseValues["promscale"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("promscale is not the expected type: %T", releaseValues["promscale"])
		}
	}

	if !promscaleExists {
		// promscale values is nil, construct the spec to
		// assign db password/uri
		releaseValues["promscale"] = make(map[string]interface{})
		promscaleValues = releaseValues["promscale"].(map[string]interface{})
		promscaleValues["connection"] = make(map[string]interface{})
		connValues := promscaleValues["connection"].(map[string]interface{})
		connValues["uri"] = externalDBURI
		connValues["password"] = tsdbSecretValue
	} else {
		for k1, v1 := range promscaleValues {
			if k1 == "tracing" {
				traceMap, ok := v1.(map[string]interface{})
				if !ok {
					return fmt.Errorf("promscale.tracing is not the expected type: %T", v1)
				}
				isTracingEnabled, ok = traceMap["enabled"].(bool)
				if !ok {
					return fmt.Errorf("promscale.tracing.enabled is not the expected type: %T", traceMap["enabled"])
				}

				promscaleValues["openTelemetry"] = v1
				// drop older tracing field which is no longer used
				delete(promscaleValues, "tracing")
			}

			if k1 == "image" {
				imageName, ok := v1.(string)
				if !ok {
					return fmt.Errorf("promscale.image is not the expected type: %T", v1)
				}
				// In tobs 0.7.0 release we hardcoded
				// beta release if tracing is enabled, from 0.8.0 tobs
				// release all default images will include out of the tracing support in
				// all Promscale images.
				if imageName == "timescale/promscale:0.7.0-beta.latest" {
					delete(promscaleValues, "image")
				}
			}

			if k1 == "args" {
				aV, ok := v1.([]interface{})
				if !ok {
					return fmt.Errorf("promscale.args is not the expected type: %T", v1)
				}
				for in, value := range aV {
					if value == "-otlp-grpc-server-listen-address=:9202" {
						aV = append(aV[:in], aV[in+1:]...)
					}
					// if HA arg is found in Promscale
					// change it to new HA arg.
					if value == "--high-availability" {
						aV[in] = "--metrics.high-availability"
					}
				}
				promscaleValues["args"] = aV
			}

			if k1 == "connection" {
				connectionValues, ok := v1.(map[string]interface{})
				if !ok {
					return fmt.Errorf("promscale.connection is not the expected type: %T", v1)
				}

				connectionValues["uri"] = externalDBURI

				for k2, v2 := range connectionValues {
					if k2 == "password" {
						connectionValues["password"] = tsdbSecretValue
					}

					if k2 == "host" {
						hostValues, ok := v2.(map[string]interface{})
						if !ok {
							return fmt.Errorf("promscale.connection.host is not the expected type: %T", v2)
						}
						hostValue := hostValues["nameTemplate"]
						connectionValues["host"] = hostValue
					}
				}
			}

			if k1 == "service" {
				promService, ok := v1.(map[string]interface{})
				if !ok {
					return fmt.Errorf("promscale.service is not the expected type: %T", v1)
				}
				lbValues := promService["loadBalancer"]
				lb, ok := lbValues.(map[string]interface{})
				if !ok {
					return fmt.Errorf("promscale.service.loadBalancer is not the expected type: %T", lbValues)
				}
				lbEnabled := lb["enabled"]
				if lbEnabled == "true" {
					promService["type"] = "LoadBalancer"
				} else {
					promService["type"] = "ClusterIP"
				}
				delete(promService, "loadBalancer")
			}
		}
	}

	if isTracingEnabled {
		otelCol := otel.OtelCol{
			ReleaseName: root.HelmReleaseName,
			Namespace:   root.Namespace,
			K8sClient:   c.k8sClient,
			HelmClient:  helmClient,
		}

		// apply OpenTelemetry CRDs
		err = c.k8sClient.ApplyManifests(otel.OpenTelemetryCRDs)
		if err != nil {
			return err
		}
		fmt.Println("Successfully created CRDs: ", reflect.ValueOf(otel.OpenTelemetryCRDs).MapKeys())

		config, err := helmClient.ExportValuesFieldFromChart(c.chartRef, c.valuesFile, []string{"opentelemetryOperator", "collector", "config"})
		if err != nil {
			return err
		}
		otelColConfig, ok := config.(string)
		if !ok {
			return fmt.Errorf("opentelemetryOperator.collector.config is not the expected type: %T", config)
		}

		err = otelCol.ValidateCertManager()
		if err != nil {
			return err
		}
		c.upgradeCertManager = otelCol.UpgradeCM

		if err = otelCol.DeleteDefaultOtelCollector(); err != nil {
			return err
		}

		if err = otelCol.CreateDefaultCollector(otelColConfig); err != nil {
			return err
		}
		// re-structure jaeger values
		otelValues, ok := releaseValues["opentelemetryOperator"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("opentelemetryOperator is not the expected type: %T", releaseValues["opentelemetryOperator"])
		}
		// delete jaegerPromscaleQuery as it's no-longer used in values.yaml
		delete(otelValues, "jaegerPromscaleQuery")
	}

	d, err := yaml.Marshal(&releaseValues)
	if err != nil {
		return fmt.Errorf("failed to marshal release values %v", err)
	}

	c.upgradeValues = string(d)
	return nil
}

var (
	// FIXME(paulfantom): if CRDs would contain label with version, we could deduct this value
	// Needs https://github.com/prometheus-operator/prometheus-operator/issues/4344 to be completed
	KubePrometheusCRDVersion     = "v0.56.2"
	kubePrometheusCRDsPathPrefix = fmt.Sprintf("https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/%s/example/prometheus-operator-crd/monitoring.coreos.com", KubePrometheusCRDVersion)
	kubePrometheusCRDs           = map[string]string{
		"alertmanagerconfigs.monitoring.coreos.com": fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "alertmanagerconfigs"),
		"alertmanagers.monitoring.coreos.com":       fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "alertmanagers"),
		"podmonitors.monitoring.coreos.com":         fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "podmonitors"),
		"probes.monitoring.coreos.com":              fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "probes"),
		"prometheuses.monitoring.coreos.com":        fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "prometheuses"),
		"servicemonitors.monitoring.coreos.com":     fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "servicemonitors"),
		"thanosrulers.monitoring.coreos.com":        fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "thanosrulers"),
		"prometheusrules.monitoring.coreos.com":     fmt.Sprintf("%s_%s.yaml", kubePrometheusCRDsPathPrefix, "prometheusrules"),
	}
)

func (c *upgradeSpec) applyCRDS(crds map[string]string) error {
	err := c.k8sClient.ApplyManifests(crds)
	if err != nil {
		return fmt.Errorf("failed to apply manifest with error %v", err)
	}

	fmt.Println("Successfully created CRDs: ", reflect.ValueOf(crds).MapKeys())
	return nil
}

func (c *upgradeSpec) persistPrometheusDataDuringUpgrade() error {
	// scale down prometheus replicas to 0
	fmt.Println("Migrating the underlying prometheus persistent volume to new prometheus instance...")
	prometheus := root.HelmReleaseName + "-prometheus-server"
	prometheusDeployment, err := c.k8sClient.GetDeployment(prometheus, root.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get %s %v", prometheus, err)
	}

	fmt.Println("Scaling down prometheus instances to 0 replicas...")
	var r int32 = 0
	prometheusDeployment.Spec.Replicas = &r
	err = c.k8sClient.UpdateDeployment(prometheusDeployment)
	if err != nil {
		return fmt.Errorf("failed to update %s %v", prometheus, err)
	}

	count := 0
	for {
		pods, err := c.k8sClient.KubeGetPods(root.Namespace, map[string]string{"app": "prometheus", "component": "server", "release": root.HelmReleaseName})
		if err != nil {
			return fmt.Errorf("unable to get pods from prometheus deployment %v", err)
		}
		if len(pods) == 0 {
			break
		}

		if count == 10 {
			return fmt.Errorf("prometheus pod shutdown saves all in memory data to persistent volume, prometheus pod is taking too long to shut down... ")
		}
		count++
		time.Sleep(time.Duration(count*10) * time.Second)
	}

	// update existing prometheus PV to persist data and create a new PVC so the
	// new prometheus mounts to the created PVC which binds to older prometheus PV.
	err = c.k8sClient.UpdatePVToNewPVC(prometheus, utils.PrometheusPVCName, root.Namespace, map[string]string{"prometheus": "tobs-kube-prometheus", "release": root.HelmReleaseName})
	if err != nil {
		return fmt.Errorf("failed to update prometheus persistent volume %v", err)
	}

	// create job to update prometheus data directory permissions as the
	// new prometheus expects the data dir to be owned by userid 1000.
	fmt.Println("Create job to update prometheus data directory permissions...")
	err = c.k8sClient.CreateJob(getJobForPrometheusDataPermissionChange(utils.PrometheusPVCName))
	if err != nil {
		return fmt.Errorf("failed to create job for prometheus data migration %v", err)
	}

	return nil
}

func getJobForPrometheusDataPermissionChange(pvcName string) *batchv1.Job {
	var backoff int32 = 3
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.UpgradeJob_040,
			Namespace: root.Namespace,
			Labels:    map[string]string{"app": "tobs-upgrade", "release": root.HelmReleaseName},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: v1.PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []v1.Container{
						{
							Name:            "upgrade-tobs",
							Image:           "alpine",
							ImagePullPolicy: v1.PullIfNotPresent,
							Stdin:           true,
							TTY:             true,
							Command: []string{
								"chown",
								"1000:1000",
								"-R",
								"/data/",
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "prometheus",
									MountPath: "/data",
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "prometheus",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}
}
