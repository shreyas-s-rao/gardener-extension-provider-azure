// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package controlplane

import (
	"context"
	"encoding/json"

	apisazure "github.com/gardener/gardener-extension-provider-azure/pkg/apis/azure"
	"github.com/gardener/gardener-extension-provider-azure/pkg/azure"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/controlplane/genericactuator"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	mockclient "github.com/gardener/gardener/pkg/mock/controller-runtime/client"
	"github.com/gardener/gardener/pkg/utils"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"
	fakesecretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager/fake"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
)

const (
	namespace                              = "test"
	maxNodes                         int32 = 0
	genericTokenKubeconfigSecretName       = "generic-token-kubeconfig-92e9ae14"
)

var _ = Describe("ValuesProvider", func() {
	var (
		ctrl *gomock.Controller
		ctx  = context.TODO()

		fakeClient         client.Client
		fakeSecretsManager secretsmanager.Interface

		c  *mockclient.MockClient
		vp genericactuator.ValuesProvider

		scheme = runtime.NewScheme()
		_      = apisazure.AddToScheme(scheme)

		infrastructureStatus *apisazure.InfrastructureStatus
		controlPlaneConfig   *apisazure.ControlPlaneConfig
		cluster              *extensionscontroller.Cluster

		defaultInfrastructureStatus = &apisazure.InfrastructureStatus{
			ResourceGroup: apisazure.ResourceGroup{
				Name: "rg-abcd1234",
			},
			Networks: apisazure.NetworkStatus{
				VNet: apisazure.VNetStatus{
					Name: "vnet-abcd1234",
				},
				Subnets: []apisazure.Subnet{
					{
						Name:    "subnet-abcd1234-nodes",
						Purpose: "nodes",
					},
				},
			},
			SecurityGroups: []apisazure.SecurityGroup{
				{
					Purpose: "nodes",
					Name:    "security-group-name-workers",
				},
			},
			RouteTables: []apisazure.RouteTable{
				{
					Purpose: "nodes",
					Name:    "route-table-name",
				},
			},
			Zoned: true,
		}

		defaultControlPlaneConfig = &apisazure.ControlPlaneConfig{
			CloudControllerManager: &apisazure.CloudControllerManagerConfig{
				FeatureGates: map[string]bool{
					"CustomResourceValidation": true,
				},
			},
		}

		cidr                    = "10.250.0.0/19"
		cloudProviderConfigData = "foo"

		k8sVersionLessThan121    = "1.15.4"
		k8sVersionHigherEqual121 = "1.21.4"

		enabledTrue  = map[string]interface{}{"enabled": true}
		enabledFalse = map[string]interface{}{"enabled": false}

		// Azure Container Registry
		azureContainerRegistryConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: azure.CloudProviderAcrConfigName, Namespace: namespace},
		}
		errorAzureContainerRegistryConfigMapNotFound = errors.NewNotFound(schema.GroupResource{}, azure.CloudProviderAcrConfigName)

		// Primary AvailabilitySet
		primaryAvailabilitySetName = "primary-availability-set"
		primaryAvailabilitySet     = apisazure.AvailabilitySet{
			Name:    primaryAvailabilitySetName,
			Purpose: "nodes",
			ID:      "/my/azure/id",
		}

		checksums = map[string]string{
			v1beta1constants.SecretNameCloudProvider: "8bafb35ff1ac60275d62e1cbd495aceb511fb354f74a20f7d06ecb48b3a68432",
			azure.CloudProviderDiskConfigName:        "77627eb2343b9f2dc2fca3cce35f2f9eec55783aa5f7dac21c473019e5825de2",
		}
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())

		fakeClient = fakeclient.NewClientBuilder().Build()
		fakeSecretsManager = fakesecretsmanager.New(fakeClient, namespace)

		c = mockclient.NewMockClient(ctrl)
		vp = NewValuesProvider()

		err := vp.(inject.Scheme).InjectScheme(scheme)
		Expect(err).NotTo(HaveOccurred())
		err = vp.(inject.Client).InjectClient(c)
		Expect(err).NotTo(HaveOccurred())

		infrastructureStatus = defaultInfrastructureStatus.DeepCopy()
		controlPlaneConfig = defaultControlPlaneConfig.DeepCopy()
		cluster = generateCluster(cidr, k8sVersionLessThan121, false, nil)
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	Describe("#GetConfigChartValues", func() {
		var (
			controlPlaneSecretKey = client.ObjectKey{Namespace: namespace, Name: v1beta1constants.SecretNameCloudProvider}
			controlPlaneSecret    = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      v1beta1constants.SecretNameCloudProvider,
					Namespace: namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"clientID":       []byte(`ClientID`),
					"clientSecret":   []byte(`ClientSecret`),
					"subscriptionID": []byte(`SubscriptionID`),
					"tenantID":       []byte(`TenantID`),
				},
			}
		)

		BeforeEach(func() {
			c.EXPECT().Get(ctx, controlPlaneSecretKey, &corev1.Secret{}).DoAndReturn(clientGet(controlPlaneSecret))
		})

		Context("Error due to missing resources in the infrastructure status", func() {
			BeforeEach(func() {
				c.EXPECT().Delete(ctx, azureContainerRegistryConfigMap).Return(errorAzureContainerRegistryConfigMapNotFound)
			})

			It("should return error, missing subnet", func() {

				infrastructureStatus.Networks.Subnets[0].Purpose = "internal"
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				_, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("could not determine subnet for purpose 'nodes'"))
			})

			It("should return error, missing route tables", func() {
				infrastructureStatus.RouteTables[0].Purpose = "internal"
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				_, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("could not determine route table for purpose 'nodes'"))
			})

			It("should return error, missing security groups", func() {
				infrastructureStatus.SecurityGroups[0].Purpose = "internal"
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				_, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("could not determine security group for purpose 'nodes'"))
			})
		})

		Context("Generate config chart values", func() {
			It("should return correct config chart values for a cluster with primary availabilityset (non zoned)", func() {
				c.EXPECT().Delete(ctx, azureContainerRegistryConfigMap).Return(errorAzureContainerRegistryConfigMapNotFound)

				infrastructureStatus.Zoned = false
				infrastructureStatus.AvailabilitySets = []apisazure.AvailabilitySet{primaryAvailabilitySet}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				values, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"tenantId":            "TenantID",
					"subscriptionId":      "SubscriptionID",
					"aadClientId":         "ClientID",
					"aadClientSecret":     "ClientSecret",
					"resourceGroup":       "rg-abcd1234",
					"vnetName":            "vnet-abcd1234",
					"subnetName":          "subnet-abcd1234-nodes",
					"region":              "eu-west-1a",
					"availabilitySetName": primaryAvailabilitySetName,
					"routeTableName":      "route-table-name",
					"securityGroupName":   "security-group-name-workers",
					"kubernetesVersion":   k8sVersionLessThan121,
					"maxNodes":            maxNodes,
				}))
			})

			It("should return correct config chart valued for cluser with vmo (non-zoned)", func() {
				c.EXPECT().Delete(ctx, azureContainerRegistryConfigMap).Return(errorAzureContainerRegistryConfigMapNotFound)

				infrastructureStatus.Zoned = false
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				values, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"tenantId":          "TenantID",
					"subscriptionId":    "SubscriptionID",
					"aadClientId":       "ClientID",
					"aadClientSecret":   "ClientSecret",
					"resourceGroup":     "rg-abcd1234",
					"vnetName":          "vnet-abcd1234",
					"subnetName":        "subnet-abcd1234-nodes",
					"region":            "eu-west-1a",
					"routeTableName":    "route-table-name",
					"securityGroupName": "security-group-name-workers",
					"kubernetesVersion": k8sVersionLessThan121,
					"maxNodes":          maxNodes,
					"vmType":            "vmss",
				}))
			})

			It("should return correct config chart values for zoned cluster", func() {
				c.EXPECT().Delete(ctx, azureContainerRegistryConfigMap).Return(errorAzureContainerRegistryConfigMapNotFound)
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				values, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"tenantId":          "TenantID",
					"subscriptionId":    "SubscriptionID",
					"aadClientId":       "ClientID",
					"aadClientSecret":   "ClientSecret",
					"resourceGroup":     "rg-abcd1234",
					"vnetName":          "vnet-abcd1234",
					"subnetName":        "subnet-abcd1234-nodes",
					"region":            "eu-west-1a",
					"routeTableName":    "route-table-name",
					"securityGroupName": "security-group-name-workers",
					"kubernetesVersion": k8sVersionLessThan121,
					"maxNodes":          maxNodes,
				}))
			})

			It("should return correct control plane chart values with identity", func() {
				identityName := "identity-client-id"
				infrastructureStatus.Identity = &apisazure.IdentityStatus{
					ClientID:  identityName,
					ACRAccess: true,
				}

				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

				values, err := vp.GetConfigChartValues(ctx, cp, cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"tenantId":            "TenantID",
					"subscriptionId":      "SubscriptionID",
					"aadClientId":         "ClientID",
					"aadClientSecret":     "ClientSecret",
					"resourceGroup":       "rg-abcd1234",
					"vnetName":            "vnet-abcd1234",
					"subnetName":          "subnet-abcd1234-nodes",
					"region":              "eu-west-1a",
					"routeTableName":      "route-table-name",
					"securityGroupName":   "security-group-name-workers",
					"kubernetesVersion":   k8sVersionLessThan121,
					"acrIdentityClientId": identityName,
					"maxNodes":            maxNodes,
				}))
			})
		})
	})

	Describe("#GetControlPlaneChartValues", func() {
		var (
			controlPlaneConfigSecretKey = client.ObjectKey{Namespace: namespace, Name: azure.CloudProviderConfigName}
			controlPlaneConfigSecret    = &corev1.Secret{
				Data: map[string][]byte{azure.CloudProviderConfigMapKey: []byte(cloudProviderConfigData)},
			}

			ccmChartValues = utils.MergeMaps(enabledTrue, map[string]interface{}{
				"replicas":          1,
				"clusterName":       namespace,
				"kubernetesVersion": k8sVersionLessThan121,
				"podNetwork":        cidr,
				"podAnnotations": map[string]interface{}{
					"checksum/secret-cloudprovider":         "8bafb35ff1ac60275d62e1cbd495aceb511fb354f74a20f7d06ecb48b3a68432",
					"checksum/secret-cloud-provider-config": "77627eb2343b9f2dc2fca3cce35f2f9eec55783aa5f7dac21c473019e5825de2",
				},
				"podLabels": map[string]interface{}{
					"maintenance.gardener.cloud/restart": "true",
				},
				"featureGates": map[string]bool{
					"CustomResourceValidation": true,
				},
				"tlsCipherSuites": []string{
					"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
					"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
					"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305",
					"TLS_RSA_WITH_AES_128_CBC_SHA",
					"TLS_RSA_WITH_AES_256_CBC_SHA",
					"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
				},
				"secrets": map[string]interface{}{
					"server": "cloud-controller-manager-server",
				},
			})
		)

		BeforeEach(func() {
			c.EXPECT().Get(ctx, controlPlaneConfigSecretKey, &corev1.Secret{}).DoAndReturn(clientGet(controlPlaneConfigSecret))

			By("creating secrets managed outside of this package for whose secretsmanager.Get() will be called")
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca-provider-azure-controlplane", Namespace: namespace}})).To(Succeed())
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snapshot-validation-server", Namespace: namespace}})).To(Succeed())
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloud-controller-manager-server", Namespace: namespace}})).To(Succeed())
		})

		It("should return correct control plane chart values (k8s < 1.21)", func() {
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetControlPlaneChartValues(ctx, cp, cluster, fakeSecretsManager, checksums, false)

			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{
				"global": map[string]interface{}{
					"genericTokenKubeconfigSecretName": genericTokenKubeconfigSecretName,
				},
				azure.CloudControllerManagerName: utils.MergeMaps(ccmChartValues, map[string]interface{}{
					"kubernetesVersion": cluster.Shoot.Spec.Kubernetes.Version,
				}),
				azure.CSIControllerName: enabledFalse,
				azure.RemedyControllerName: utils.MergeMaps(enabledTrue, map[string]interface{}{
					"replicas": 1,
					"podAnnotations": map[string]interface{}{
						"checksum/secret-" + azure.CloudProviderConfigName: checksums[azure.CloudProviderConfigName],
					},
				}),
			}))
		})

		It("should return correct control plane chart values (k8s >= 1.21) without zoned infrastructure", func() {
			cluster = generateCluster(cidr, k8sVersionHigherEqual121, true, nil)
			infrastructureStatus.Zoned = false
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

			values, err := vp.GetControlPlaneChartValues(ctx, cp, cluster, fakeSecretsManager, checksums, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{
				"global": map[string]interface{}{
					"genericTokenKubeconfigSecretName": genericTokenKubeconfigSecretName,
				},
				azure.CloudControllerManagerName: utils.MergeMaps(ccmChartValues, map[string]interface{}{
					"kubernetesVersion": cluster.Shoot.Spec.Kubernetes.Version,
				}),
				azure.CSIControllerName: utils.MergeMaps(enabledTrue, map[string]interface{}{
					"replicas": 1,
					"podAnnotations": map[string]interface{}{
						"checksum/secret-" + azure.CloudProviderConfigName: checksums[azure.CloudProviderConfigName],
					},
					"csiSnapshotController": map[string]interface{}{
						"replicas": 1,
					},
					"csiSnapshotValidationWebhook": map[string]interface{}{
						"replicas": 1,
						"secrets": map[string]interface{}{
							"server": "csi-snapshot-validation-server",
						},
					},
					"vmType": "vmss",
				}),
				azure.RemedyControllerName: utils.MergeMaps(enabledTrue, map[string]interface{}{
					"replicas": 1,
					"podAnnotations": map[string]interface{}{
						"checksum/secret-" + azure.CloudProviderConfigName: checksums[azure.CloudProviderConfigName],
					},
				}),
			}))
		})

		It("should return correct control plane chart values (k8s >= 1.21) with zoned infrastructure", func() {
			cluster = generateCluster(cidr, k8sVersionHigherEqual121, true, nil)
			infrastructureStatus.Zoned = true
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)

			values, err := vp.GetControlPlaneChartValues(ctx, cp, cluster, fakeSecretsManager, checksums, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{
				"global": map[string]interface{}{
					"genericTokenKubeconfigSecretName": genericTokenKubeconfigSecretName,
				},
				azure.CloudControllerManagerName: utils.MergeMaps(ccmChartValues, map[string]interface{}{
					"kubernetesVersion": cluster.Shoot.Spec.Kubernetes.Version,
				}),
				azure.CSIControllerName: utils.MergeMaps(enabledTrue, map[string]interface{}{
					"replicas": 1,
					"podAnnotations": map[string]interface{}{
						"checksum/secret-" + azure.CloudProviderConfigName: checksums[azure.CloudProviderConfigName],
					},
					"csiSnapshotController": map[string]interface{}{
						"replicas": 1,
					},
					"csiSnapshotValidationWebhook": map[string]interface{}{
						"replicas": 1,
						"secrets": map[string]interface{}{
							"server": "csi-snapshot-validation-server",
						},
					},
				}),
				azure.RemedyControllerName: utils.MergeMaps(enabledTrue, map[string]interface{}{
					"replicas": 1,
					"podAnnotations": map[string]interface{}{
						"checksum/secret-" + azure.CloudProviderConfigName: checksums[azure.CloudProviderConfigName],
					},
				}),
			}))
		})

		It("should return correct control plane chart values when remedy controller is disabled", func() {
			cluster = generateCluster(cidr, k8sVersionLessThan121, false, map[string]string{
				azure.DisableRemedyControllerAnnotation: "true",
			})

			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetControlPlaneChartValues(ctx, cp, cluster, fakeSecretsManager, checksums, false)

			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{
				"global": map[string]interface{}{
					"genericTokenKubeconfigSecretName": genericTokenKubeconfigSecretName,
				},
				azure.CloudControllerManagerName: utils.MergeMaps(ccmChartValues, map[string]interface{}{
					"kubernetesVersion": cluster.Shoot.Spec.Kubernetes.Version,
				}),
				azure.CSIControllerName:    enabledFalse,
				azure.RemedyControllerName: enabledFalse,
			}))
		})
	})

	Describe("#GetControlPlaneShootChartValues", func() {
		var (
			csiNodeNotEnabled = utils.MergeMaps(enabledFalse, map[string]interface{}{
				"podAnnotations": map[string]interface{}{
					"checksum/configmap-" + azure.CloudProviderDiskConfigName: "",
				},
				"cloudProviderConfig": "",
				"kubernetesVersion":   "1.15.4",
			})
			globalVpaDisabled = map[string]interface{}{
				"vpaEnabled": false,
			}
			globalVpaEnabled = map[string]interface{}{
				"vpaEnabled": true,
			}
			csiNodeEnabled = utils.MergeMaps(enabledTrue, map[string]interface{}{
				"podAnnotations": map[string]interface{}{
					"checksum/configmap-" + azure.CloudProviderDiskConfigName: checksums[azure.CloudProviderDiskConfigName],
				},
				"cloudProviderConfig": cloudProviderConfigData,
				"kubernetesVersion":   "1.21.4",
			})
		)

		BeforeEach(func() {
			By("creating secrets managed outside of this package for whose secretsmanager.Get() will be called")
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca-provider-azure-controlplane", Namespace: namespace}})).To(Succeed())
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snapshot-validation-server", Namespace: namespace}})).To(Succeed())
			Expect(fakeClient.Create(context.TODO(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloud-controller-manager-server", Namespace: namespace}})).To(Succeed())
		})

		Context("k8s < 1.21", func() {
			It("should return correct control plane shoot chart values for zoned cluster", func() {
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})

			It("should return correct control plane shoot chart values for cluster with primary availabilityset (non zoned)", func() {
				infrastructureStatus.Zoned = false
				infrastructureStatus.AvailabilitySets = []apisazure.AvailabilitySet{primaryAvailabilitySet}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledFalse,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})
		})

		Context("k8s >= 1.21", func() {
			var (
				cpDiskConfigKey = client.ObjectKey{Namespace: namespace, Name: azure.CloudProviderDiskConfigName}
				cpDiskConfig    = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      azure.CloudProviderDiskConfigName,
						Namespace: namespace,
					},
					Data: map[string][]byte{
						azure.CloudProviderConfigMapKey: []byte(cloudProviderConfigData),
					},
				}
			)

			BeforeEach(func() {
				c.EXPECT().Get(ctx, cpDiskConfigKey, &corev1.Secret{}).DoAndReturn(clientGet(cpDiskConfig))
				cluster = generateCluster(cidr, k8sVersionHigherEqual121, true, nil)
			})

			It("should return correct control plane shoot chart values for zoned cluster", func() {
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaEnabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})

			It("should return correct control plane shoot chart values for cluster with primary availabilityset (non zoned)", func() {
				infrastructureStatus.Zoned = false
				infrastructureStatus.AvailabilitySets = []apisazure.AvailabilitySet{primaryAvailabilitySet}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaEnabled,
					azure.AllowEgressName:            enabledFalse,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})

			It("should return correct control plane shoot chart values for cluster with vmss flex (vmo, non zoned)", func() {
				infrastructureStatus.Zoned = false
				infrastructureStatus.AvailabilitySets = []apisazure.AvailabilitySet{}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaEnabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})
		})

		Context("remedy controller is disabled", func() {
			BeforeEach(func() {
				cluster = generateCluster(cidr, k8sVersionLessThan121, false, map[string]string{
					azure.DisableRemedyControllerAnnotation: "true",
				})
			})

			It("should return correct control plane shoot chart values for zoned cluster", func() {
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledFalse,
				}))
			})

			It("should return correct control plane shoot chart values for a cluster with primary availabilityset (non zoned)", func() {
				infrastructureStatus.Zoned = false
				infrastructureStatus.AvailabilitySets = []apisazure.AvailabilitySet{primaryAvailabilitySet}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledFalse,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledFalse,
				}))
			})
		})

		Context("podSecurityPolicy", func() {
			It("should return correct shoot control plane chart when PodSecurityPolicy admission plugin is not disabled in the shoot", func() {
				cluster.Shoot.Spec.Kubernetes.KubeAPIServer = &gardencorev1beta1.KubeAPIServerConfig{
					AdmissionPlugins: []gardencorev1beta1.AdmissionPlugin{
						{
							Name: "PodSecurityPolicy",
						},
					},
				}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": false,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})

			It("should return correct shoot control plane chart when PodSecurityPolicy admission plugin is disabled in the shoot", func() {
				cluster.Shoot.Spec.Kubernetes.KubeAPIServer = &gardencorev1beta1.KubeAPIServerConfig{
					AdmissionPlugins: []gardencorev1beta1.AdmissionPlugin{
						{
							Name:     "PodSecurityPolicy",
							Disabled: pointer.Bool(true),
						},
					},
				}
				cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
				csiNode := utils.MergeMaps(csiNodeNotEnabled, map[string]interface{}{
					"webhookConfig": map[string]interface{}{
						"url":      "https://" + azure.CSISnapshotValidation + "." + cp.Namespace + "/volumesnapshot",
						"caBundle": "",
					},
					"pspDisabled": true,
				})

				values, err := vp.GetControlPlaneShootChartValues(ctx, cp, cluster, fakeSecretsManager, checksums)
				Expect(err).NotTo(HaveOccurred())
				Expect(values).To(Equal(map[string]interface{}{
					"global":                         globalVpaDisabled,
					azure.AllowEgressName:            enabledTrue,
					azure.CloudControllerManagerName: enabledTrue,
					azure.CSINodeName:                csiNode,
					azure.RemedyControllerName:       enabledTrue,
				}))
			})
		})
	})

	Describe("#GetControlPlaneShootCRDsChartValues", func() {
		It("should return correct control plane shoot CRDs chart values (k8s < 1.21)", func() {
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetControlPlaneShootCRDsChartValues(ctx, cp, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{"volumesnapshots": map[string]interface{}{"enabled": false}}))
		})

		It("should return correct control plane shoot CRDs chart values (k8s >= 1.21)", func() {
			cluster = generateCluster(cidr, k8sVersionHigherEqual121, true, nil)
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetControlPlaneShootCRDsChartValues(ctx, cp, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{"volumesnapshots": map[string]interface{}{"enabled": true}}))
		})
	})

	Describe("#GetStorageClassesChartValues()", func() {
		It("should return correct storage class chart values (k8s < 1.21)", func() {
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetStorageClassesChartValues(ctx, cp, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{"useLegacyProvisioner": true}))
		})

		It("should return correct storage class chart values (k8s >= 1.21)", func() {
			cluster = generateCluster(cidr, k8sVersionHigherEqual121, true, nil)
			cp := generateControlPlane(controlPlaneConfig, infrastructureStatus)
			values, err := vp.GetStorageClassesChartValues(ctx, cp, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(Equal(map[string]interface{}{"useLegacyProvisioner": false}))
		})
	})
})

func encode(obj runtime.Object) []byte {
	data, _ := json.Marshal(obj)
	return data
}

func clientGet(result runtime.Object) interface{} {
	return func(ctx context.Context, key client.ObjectKey, obj runtime.Object) error {
		switch obj.(type) {
		case *corev1.Secret:
			*obj.(*corev1.Secret) = *result.(*corev1.Secret)
		case *corev1.ConfigMap:
			*obj.(*corev1.ConfigMap) = *result.(*corev1.ConfigMap)
		}
		return nil
	}
}

func generateControlPlane(controlPlaneConfig *apisazure.ControlPlaneConfig, infrastructureStatus *apisazure.InfrastructureStatus) *extensionsv1alpha1.ControlPlane {
	return &extensionsv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "control-plane",
			Namespace: namespace,
		},
		Spec: extensionsv1alpha1.ControlPlaneSpec{
			Region: "eu-west-1a",
			SecretRef: corev1.SecretReference{
				Name:      v1beta1constants.SecretNameCloudProvider,
				Namespace: namespace,
			},
			DefaultSpec: extensionsv1alpha1.DefaultSpec{
				ProviderConfig: &runtime.RawExtension{
					Raw: encode(controlPlaneConfig),
				},
			},
			InfrastructureProviderStatus: &runtime.RawExtension{
				Raw: encode(infrastructureStatus),
			},
		},
	}
}

func generateCluster(cidr, k8sVersion string, vpaEnabled bool, shootAnnotations map[string]string) *extensionscontroller.Cluster {
	shoot := gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Networking: gardencorev1beta1.Networking{
				Pods: &cidr,
			},
			Kubernetes: gardencorev1beta1.Kubernetes{
				Version: k8sVersion,
				VerticalPodAutoscaler: &gardencorev1beta1.VerticalPodAutoscaler{
					Enabled: vpaEnabled,
				},
			},
		},
	}
	if shootAnnotations != nil {
		shoot.ObjectMeta.Annotations = shootAnnotations
	}

	return &extensionscontroller.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"generic-token-kubeconfig.secret.gardener.cloud/name": genericTokenKubeconfigSecretName,
			},
		},
		Shoot: &shoot,
	}
}
