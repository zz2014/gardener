// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hybridbotanist

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var chartPathControlPlane = filepath.Join(common.ChartPath, "seed-controlplane", "charts")

// getResourcesForAPIServer returns the cpu and memory requirements for API server based on nodeCount
func getResourcesForAPIServer(nodeCount int) (string, string, string, string) {
	cpuRequest := "0m"
	memoryRequest := "0Mi"
	cpuLimit := "0m"
	memoryLimit := "0Mi"

	switch {
	case nodeCount <= 2:
		cpuRequest = "800m"
		memoryRequest = "600Mi"

		cpuLimit = "1000m"
		memoryLimit = "900Mi"
	case nodeCount <= 10:
		cpuRequest = "1000m"
		memoryRequest = "800Mi"

		cpuLimit = "1200m"
		memoryLimit = "1400Mi"
	case nodeCount <= 50:
		cpuRequest = "1200m"
		memoryRequest = "1200Mi"

		cpuLimit = "1500m"
		memoryLimit = "3000Mi"
	case nodeCount <= 100:
		cpuRequest = "2500m"
		memoryRequest = "4000Mi"

		cpuLimit = "3000m"
		memoryLimit = "4500Mi"
	default:
		cpuRequest = "3000m"
		memoryRequest = "4000Mi"

		cpuLimit = "4000m"
		memoryLimit = "6000Mi"
	}

	return cpuRequest, memoryRequest, cpuLimit, memoryLimit
}

// DeployETCD deploys two etcd clusters via StatefulSets. The first etcd cluster (called 'main') is used for all the
/// data the Shoot Kubernetes cluster needs to store, whereas the second etcd luster (called 'events') is only used to
// store the events data. The objectstore is also set up to store the backups.
func (b *HybridBotanist) DeployETCD() error {
	secretData, backupConfigData, err := b.SeedCloudBotanist.GenerateEtcdBackupConfig()
	if err != nil {
		return err
	}

	// Some cloud botanists do not yet support backup and won't return secret data.
	if secretData != nil {
		_, err = b.K8sSeedClient.CreateSecret(b.Shoot.SeedNamespace, common.BackupSecretName, corev1.SecretTypeOpaque, secretData, true)
		if err != nil {
			return err
		}
	}

	etcdConfig := map[string]interface{}{
		"podAnnotations": map[string]interface{}{
			"checksum/secret-etcd-ca":         b.CheckSums["ca-etcd"],
			"checksum/secret-etcd-server-tls": b.CheckSums["etcd-server-tls"],
			"checksum/secret-etcd-client-tls": b.CheckSums["etcd-client-tls"],
		},
	}

	// Some cloud botanists do not yet support backup and won't return backup config data.
	if backupConfigData != nil {
		etcdConfig["backup"] = backupConfigData
	}

	etcd, err := b.Botanist.InjectImages(etcdConfig, b.K8sSeedClient.Version(), map[string]string{"etcd": "etcd", "etcd-backup-restore": "etcd-backup-restore"})
	if err != nil {
		return err
	}

	for _, role := range []string{common.EtcdRoleMain, common.EtcdRoleEvents} {
		etcd["role"] = role
		if role == common.EtcdRoleEvents {
			etcd["backup"] = map[string]interface{}{
				"storageProvider": "", //No storage provider means no backup
			}
		}
		if err := b.ApplyChartSeed(filepath.Join(chartPathControlPlane, "etcd"), fmt.Sprintf("etcd-%s", role), b.Shoot.SeedNamespace, nil, etcd); err != nil {
			return err
		}
		if err := b.K8sSeedClient.DeleteService(b.Shoot.SeedNamespace, fmt.Sprintf("etcd-%s", role)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// DeployCloudProviderConfig asks the Cloud Botanist to provide the cloud specific values for the cloud
// provider configuration. It will create a ConfigMap for it and store it in the Seed cluster.
func (b *HybridBotanist) DeployCloudProviderConfig() error {
	cloudProviderConfig, err := b.ShootCloudBotanist.GenerateCloudProviderConfig()
	if err != nil {
		return err
	}
	b.CheckSums[common.CloudProviderConfigName] = computeCloudProviderConfigChecksum(cloudProviderConfig)

	defaultValues := map[string]interface{}{
		"cloudProviderConfig": cloudProviderConfig,
	}

	return b.ApplyChartSeed(filepath.Join(chartPathControlPlane, common.CloudProviderConfigName), common.CloudProviderConfigName, b.Shoot.SeedNamespace, nil, defaultValues)
}

// RefreshCloudProviderConfig asks the Cloud Botanist to refresh the cloud provider config in case it stores
// the cloud provider credentials. The Cloud Botanist is expected to return the complete updated cloud config.
func (b *HybridBotanist) RefreshCloudProviderConfig() error {
	currentConfig, err := b.K8sSeedClient.GetConfigMap(b.Shoot.SeedNamespace, common.CloudProviderConfigName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	newConfigData := b.ShootCloudBotanist.RefreshCloudProviderConfig(currentConfig.Data)
	b.CheckSums[common.CloudProviderConfigName] = computeCloudProviderConfigChecksum(newConfigData[common.CloudProviderConfigMapKey])

	_, err = b.K8sSeedClient.UpdateConfigMap(b.Shoot.SeedNamespace, common.CloudProviderConfigName, newConfigData)
	return err
}

func computeCloudProviderConfigChecksum(cloudProviderConfig string) string {
	return utils.ComputeSHA256Hex([]byte(strings.TrimSpace(cloudProviderConfig)))
}

// DeployKubeAPIServer asks the Cloud Botanist to provide the cloud specific configuration values for the
// kube-apiserver deployment.
func (b *HybridBotanist) DeployKubeAPIServer() error {
	apiServerAddressIP, err := utils.WaitUntilDNSNameResolvable(b.Operation.APIServerAddress)
	if err != nil {
		return err
	}
	b.Botanist.APIServerAddressIP = apiServerAddressIP

	defaultValues := map[string]interface{}{
		"etcdServicePort":       2379,
		"etcdMainServiceFqdn":   fmt.Sprintf("etcd-%s-client.%s.svc", common.EtcdRoleMain, b.Shoot.SeedNamespace),
		"etcdEventsServiceFqdn": fmt.Sprintf("etcd-%s-client.%s.svc", common.EtcdRoleEvents, b.Shoot.SeedNamespace),
		"kubernetesVersion":     b.Shoot.Info.Spec.Kubernetes.Version,
		"shootNetworks": map[string]interface{}{
			"service": b.Shoot.GetServiceNetwork(),
		},
		"seedNetworks": map[string]interface{}{
			"service": b.Seed.Info.Spec.Networks.Services,
			"pod":     b.Seed.Info.Spec.Networks.Pods,
			"node":    b.Seed.Info.Spec.Networks.Nodes,
		},
		"maxReplicas":      3,
		"securePort":       443,
		"probeCredentials": utils.EncodeBase64([]byte(fmt.Sprintf("%s:%s", b.Secrets["kubecfg"].Data["username"], b.Secrets["kubecfg"].Data["password"]))),
		"podAnnotations": map[string]interface{}{
			"checksum/secret-ca":                        b.CheckSums["ca"],
			"checksum/secret-ca-front-proxy":            b.CheckSums["ca-front-proxy"],
			"checksum/secret-kube-apiserver":            b.CheckSums[common.KubeAPIServerDeploymentName],
			"checksum/secret-kube-aggregator":           b.CheckSums["kube-aggregator"],
			"checksum/secret-kube-apiserver-kubelet":    b.CheckSums["kube-apiserver-kubelet"],
			"checksum/secret-kube-apiserver-basic-auth": b.CheckSums["kube-apiserver-basic-auth"],
			"checksum/secret-vpn-seed":                  b.CheckSums["vpn-seed"],
			"checksum/secret-vpn-seed-tlsauth":          b.CheckSums["vpn-seed-tlsauth"],
			"checksum/secret-service-account-key":       b.CheckSums["service-account-key"],
			"checksum/secret-etcd-ca":                   b.CheckSums["ca-etcd"],
			"checksum/secret-etcd-client-tls":           b.CheckSums["etcd-client-tls"],
		},
	}
	cloudSpecificValues, err := b.ShootCloudBotanist.GenerateKubeAPIServerConfig()
	if err != nil {
		return err
	}

	if b.ShootedSeed != nil {
		var (
			apiServer  = b.ShootedSeed.APIServer
			autoscaler = apiServer.Autoscaler
		)
		defaultValues["replicas"] = *apiServer.Replicas
		defaultValues["minReplicas"] = *autoscaler.MinReplicas
		defaultValues["maxReplicas"] = autoscaler.MaxReplicas
		defaultValues["apiServerResources"] = map[string]interface{}{
			"limits": map[string]interface{}{
				"cpu":    "1500m",
				"memory": "4000Mi",
			},
		}
	} else {
		// As kube-apiserver HPA manages the number of replicas, we have to maintain current number of replicas
		// otherwise keep the value to default
		existingAPIServerDeployment, err := b.K8sSeedClient.GetDeployment(b.Shoot.SeedNamespace, common.KubeAPIServerDeploymentName)
		if err == nil && existingAPIServerDeployment.Spec.Replicas != nil && *existingAPIServerDeployment.Spec.Replicas > 0 {
			defaultValues["replicas"] = *existingAPIServerDeployment.Spec.Replicas
		}

		cpuRequest, memoryRequest, cpuLimit, memoryLimit := getResourcesForAPIServer(b.Shoot.GetNodeCount())
		defaultValues["apiServerResources"] = map[string]interface{}{
			"limits": map[string]interface{}{
				"cpu":    cpuLimit,
				"memory": memoryLimit,
			},
			"requests": map[string]interface{}{
				"cpu":    cpuRequest,
				"memory": memoryRequest,
			},
		}
	}

	var (
		apiServerConfig  = b.Shoot.Info.Spec.Kubernetes.KubeAPIServer
		admissionPlugins = kubernetes.GetAdmissionPluginsForVersion(b.Shoot.Info.Spec.Kubernetes.Version)
	)

	if apiServerConfig != nil {
		defaultValues["featureGates"] = apiServerConfig.FeatureGates
		defaultValues["runtimeConfig"] = apiServerConfig.RuntimeConfig

		if apiServerConfig.OIDCConfig != nil {
			defaultValues["oidcConfig"] = apiServerConfig.OIDCConfig
		}

		for _, plugin := range apiServerConfig.AdmissionPlugins {
			pluginOverwritesDefault := false

			for i, defaultPlugin := range admissionPlugins {
				if defaultPlugin.Name == plugin.Name {
					pluginOverwritesDefault = true
					admissionPlugins[i] = plugin
					break
				}
			}

			if !pluginOverwritesDefault {
				admissionPlugins = append(admissionPlugins, plugin)
			}
		}
	}
	defaultValues["admissionPlugins"] = admissionPlugins

	values, err := b.Botanist.InjectImages(defaultValues, b.K8sSeedClient.Version(), map[string]string{
		"hyperkube":         "hyperkube",
		"vpn-seed":          "vpn-seed",
		"blackbox-exporter": "blackbox-exporter",
	})
	if err != nil {
		return err
	}

	return b.ApplyChartSeed(filepath.Join(chartPathControlPlane, common.KubeAPIServerDeploymentName), common.KubeAPIServerDeploymentName, b.Shoot.SeedNamespace, values, cloudSpecificValues)
}

// DeployKubeControllerManager asks the Cloud Botanist to provide the cloud specific configuration values for the
// kube-controller-manager deployment.
func (b *HybridBotanist) DeployKubeControllerManager() error {
	defaultValues := map[string]interface{}{
		"cloudProvider":     b.ShootCloudBotanist.GetCloudProviderName(),
		"clusterName":       b.Shoot.SeedNamespace,
		"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
		"podNetwork":        b.Shoot.GetPodNetwork(),
		"serviceNetwork":    b.Shoot.GetServiceNetwork(),
		"podAnnotations": map[string]interface{}{
			"checksum/secret-ca":                       b.CheckSums["ca"],
			"checksum/secret-kube-controller-manager":  b.CheckSums[common.KubeControllerManagerDeploymentName],
			"checksum/secret-service-account-key":      b.CheckSums["service-account-key"],
			"checksum/secret-cloudprovider":            b.CheckSums[common.CloudProviderSecretName],
			"checksum/configmap-cloud-provider-config": b.CheckSums[common.CloudProviderConfigName],
		},
	}
	cloudSpecificValues, err := b.ShootCloudBotanist.GenerateKubeControllerManagerConfig()
	if err != nil {
		return err
	}

	if b.ShootedSeed != nil {
		defaultValues["resources"] = map[string]interface{}{
			"limits": map[string]interface{}{
				"cpu":    "750m",
				"memory": "1Gi",
			},
		}
		defaultValues["horizontalPodAutoscaler"] = map[string]interface{}{
			"downscaleDelay": "24h",
			"upscaleDelay":   "1m",
			"tolerance":      0.2,
		}
	}

	controllerManagerConfig := b.Shoot.Info.Spec.Kubernetes.KubeControllerManager
	if controllerManagerConfig != nil {
		defaultValues["featureGates"] = controllerManagerConfig.FeatureGates
	}

	values, err := b.Botanist.InjectImages(defaultValues, b.K8sSeedClient.Version(), map[string]string{"hyperkube": "hyperkube"})
	if err != nil {
		return err
	}

	return b.ApplyChartSeed(filepath.Join(chartPathControlPlane, common.KubeControllerManagerDeploymentName), common.KubeControllerManagerDeploymentName, b.Shoot.SeedNamespace, values, cloudSpecificValues)
}

// DeployCloudControllerManager asks the Cloud Botanist to provide the cloud specific configuration values for the
// cloud-controller-manager deployment.
func (b *HybridBotanist) DeployCloudControllerManager() error {
	defaultValues := map[string]interface{}{
		"cloudProvider":     b.ShootCloudBotanist.GetCloudProviderName(),
		"clusterName":       b.Shoot.SeedNamespace,
		"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
		"podNetwork":        b.Shoot.GetPodNetwork(),
		"podAnnotations": map[string]interface{}{
			"checksum/secret-cloud-controller-manager": b.CheckSums[common.CloudControllerManagerDeploymentName],
			"checksum/secret-cloudprovider":            b.CheckSums[common.CloudProviderSecretName],
			"checksum/configmap-cloud-provider-config": b.CheckSums[common.CloudProviderConfigName],
		},
	}
	cloudSpecificValues, err := b.ShootCloudBotanist.GenerateCloudControllerManagerConfig()
	if err != nil {
		return err
	}

	if b.ShootedSeed != nil {
		defaultValues["resources"] = map[string]interface{}{
			"limits": map[string]interface{}{
				"cpu":    "500m",
				"memory": "512Mi",
			},
		}
	}

	cloudControllerManagerConfig := b.Shoot.Info.Spec.Kubernetes.CloudControllerManager
	if cloudControllerManagerConfig != nil {
		defaultValues["featureGates"] = cloudControllerManagerConfig.FeatureGates
	}

	values, err := b.Botanist.InjectImages(defaultValues, b.K8sSeedClient.Version(), map[string]string{"hyperkube": "hyperkube"})
	if err != nil {
		return err
	}

	return b.ApplyChartSeed(filepath.Join(chartPathControlPlane, common.CloudControllerManagerDeploymentName), common.CloudControllerManagerDeploymentName, b.Shoot.SeedNamespace, values, cloudSpecificValues)
}

// DeployKubeScheduler asks the Cloud Botanist to provide the cloud specific configuration values for the
// kube-scheduler deployment.
func (b *HybridBotanist) DeployKubeScheduler() error {
	defaultValues := map[string]interface{}{
		"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
		"podAnnotations": map[string]interface{}{
			"checksum/secret-kube-scheduler": b.CheckSums[common.KubeSchedulerDeploymentName],
		},
	}
	cloudValues, err := b.ShootCloudBotanist.GenerateKubeSchedulerConfig()
	if err != nil {
		return err
	}

	if b.ShootedSeed != nil {
		defaultValues["resources"] = map[string]interface{}{
			"limits": map[string]interface{}{
				"cpu":    "300m",
				"memory": "350Mi",
			},
		}
	}

	schedulerConfig := b.Shoot.Info.Spec.Kubernetes.KubeScheduler
	if schedulerConfig != nil {
		defaultValues["featureGates"] = schedulerConfig.FeatureGates
	}

	values, err := b.Botanist.InjectImages(defaultValues, b.K8sSeedClient.Version(), map[string]string{"hyperkube": "hyperkube"})
	if err != nil {
		return err
	}

	return b.ApplyChartSeed(filepath.Join(chartPathControlPlane, common.KubeSchedulerDeploymentName), common.KubeSchedulerDeploymentName, b.Shoot.SeedNamespace, values, cloudValues)
}
