/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"context"
	"sort"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	logf "sigs.k8s.io/cluster-api/cmd/clusterctl/log"
)

// ProviderUpgrader defines methods for supporting provider upgrade.
type ProviderUpgrader interface {
	// Plan returns a set of suggested Upgrade plans for the management cluster, and more specifically:
	//   - Upgrade to the latest version in the the v1alpha3 series: ....
	//   - Upgrade to the latest version in the the v1alpha4 series: ....
	Plan() ([]UpgradePlan, error)

	// ApplyPlan executes an upgrade following an UpgradePlan generated by clusterctl.
	ApplyPlan(clusterAPIVersion string) error

	// ApplyCustomPlan plan executes an upgrade using the UpgradeItems provided by the user.
	ApplyCustomPlan(providersToUpgrade ...UpgradeItem) error
}

// UpgradePlan defines a list of possible upgrade targets for a management cluster.
type UpgradePlan struct {
	Contract  string
	Providers []UpgradeItem
}

// isPartialUpgrade returns true if at least one upgradeItem in the plan does not have a target version.
func (u *UpgradePlan) isPartialUpgrade() bool {
	for _, i := range u.Providers {
		if i.NextVersion == "" {
			return true
		}
	}
	return false
}

// UpgradeItem defines a possible upgrade target for a provider in the management cluster.
type UpgradeItem struct {
	clusterctlv1.Provider
	NextVersion string
}

// UpgradeRef returns a string identifying the upgrade item; this string is derived by the provider.
func (u *UpgradeItem) UpgradeRef() string {
	return u.InstanceName()
}

type providerUpgrader struct {
	configClient            config.Client
	proxy                   Proxy
	repositoryClientFactory RepositoryClientFactory
	providerInventory       InventoryClient
	providerComponents      ComponentsClient
}

var _ ProviderUpgrader = &providerUpgrader{}

func (u *providerUpgrader) Plan() ([]UpgradePlan, error) {
	log := logf.Log
	log.Info("Checking new release availability...")

	providerList, err := u.providerInventory.List()
	if err != nil {
		return nil, err
	}

	// The core provider is driving all the plan logic for entire management cluster, because all the providers
	// are expected to support the same API Version of Cluster API (contract).
	// e.g if the core provider supports v1alpha4, all the providers in the same management cluster should support v1alpha4 as well;
	// all the providers in the management cluster can upgrade to the latest release supporting v1alpha4, or if available,
	// all the providers can upgrade to the latest release supporting v1alpha5 (not supported in current clusterctl release,
	// but upgrade plan should report these options)
	// Please note that upgrade plan also works on management cluster still in v1alpha3. In this case upgrade plan is shown, but
	// upgrade to latest version in the v1alpha3 series are not supported using clusterctl v1alpha4 (use older releases).

	// Gets the upgrade info for the core provider.
	coreProviders := providerList.FilterCore()
	if len(coreProviders) != 1 {
		return nil, errors.Errorf("invalid management cluster: there should a core provider, found %d", len(coreProviders))
	}
	coreProvider := coreProviders[0]

	coreUpgradeInfo, err := u.getUpgradeInfo(coreProvider)
	if err != nil {
		return nil, err
	}

	// Identifies the API Version of Cluster API (contract) that we should consider for the management cluster update (Nb. the core provider is driving the entire management cluster).
	// This includes the current contract and the new ones available, if any.
	contractsForUpgrade := coreUpgradeInfo.getContractsForUpgrade()
	if len(contractsForUpgrade) == 0 {
		return nil, errors.Wrapf(err, "invalid metadata: unable to find the API Version of Cluster API (contract) supported by the %s provider", coreProvider.InstanceName())
	}

	// Creates an UpgradePlan for each contract considered for upgrades; each upgrade plans contains
	// an UpgradeItem for each provider defining the next available version with the target contract, if available.
	// e.g. v1alpha3, cluster-api --> v0.3.2, kubeadm bootstrap --> v0.3.2, aws --> v0.5.4 (not supported in current clusterctl release, but upgrade plan should report these options).
	// e.g. v1alpha4, cluster-api --> v0.4.1, kubeadm bootstrap --> v0.4.1, aws --> v0.X.2
	// e.g. v1alpha4, cluster-api --> v0.5.1, kubeadm bootstrap --> v0.5.1, aws --> v0.Y.4 (not supported in current clusterctl release, but upgrade plan should report these options).
	ret := make([]UpgradePlan, 0)
	for _, contract := range contractsForUpgrade {
		upgradePlan, err := u.getUpgradePlan(providerList.Items, contract)
		if err != nil {
			return nil, err
		}

		// If the upgrade plan is partial (at least one upgradeItem in the plan does not have a target version) and
		// the upgrade plan requires a change of the contract for this management cluster, then drop it
		// (all the provider in a management cluster are required to change contract at the same time).
		if upgradePlan.isPartialUpgrade() && coreUpgradeInfo.currentContract != contract {
			continue
		}

		ret = append(ret, *upgradePlan)
	}

	return ret, nil
}

func (u *providerUpgrader) ApplyPlan(contract string) error {
	if contract != clusterv1.GroupVersion.Version {
		return errors.Errorf("current version of clusterctl could only upgrade to %s contract, requested %s", clusterv1.GroupVersion.Version, contract)
	}

	log := logf.Log
	log.Info("Performing upgrade...")

	// Gets the upgrade plan for the selected API Version of Cluster API (contract).
	providerList, err := u.providerInventory.List()
	if err != nil {
		return err
	}

	upgradePlan, err := u.getUpgradePlan(providerList.Items, contract)
	if err != nil {
		return err
	}

	// Do the upgrade
	return u.doUpgrade(upgradePlan)
}

func (u *providerUpgrader) ApplyCustomPlan(upgradeItems ...UpgradeItem) error {
	log := logf.Log
	log.Info("Performing upgrade...")

	// Create a custom upgrade plan from the upgrade items, taking care of ensuring all the providers in a management
	// cluster are consistent with the API Version of Cluster API (contract).
	upgradePlan, err := u.createCustomPlan(upgradeItems)
	if err != nil {
		return err
	}

	// Do the upgrade
	return u.doUpgrade(upgradePlan)
}

// getUpgradePlan returns the upgrade plan for a specific set of providers/contract
// NB. this function is used both for upgrade plan and upgrade apply.
func (u *providerUpgrader) getUpgradePlan(providers []clusterctlv1.Provider, contract string) (*UpgradePlan, error) {
	upgradeItems := []UpgradeItem{}
	for _, provider := range providers {
		// Gets the upgrade info for the provider.
		providerUpgradeInfo, err := u.getUpgradeInfo(provider)
		if err != nil {
			return nil, err
		}

		// Identifies the next available version with the target contract for the provider, if available.
		nextVersion := providerUpgradeInfo.getLatestNextVersion(contract)

		// Append the upgrade item for the provider/with the target contract.
		upgradeItems = append(upgradeItems, UpgradeItem{
			Provider:    provider,
			NextVersion: versionTag(nextVersion),
		})
	}

	return &UpgradePlan{
		Contract:  contract,
		Providers: upgradeItems,
	}, nil
}

// createCustomPlan creates a custom upgrade plan from a set of upgrade items, taking care of ensuring all the providers
// in a management cluster are consistent with the API Version of Cluster API (contract).
func (u *providerUpgrader) createCustomPlan(upgradeItems []UpgradeItem) (*UpgradePlan, error) {
	// Gets the API Version of Cluster API (contract).
	// The this is required to ensure all the providers in a management cluster are consistent with the contract supported by the core provider.
	// e.g if the core provider is v1alpha3, all the provider should be v1alpha3 as well.

	// The target contract is derived from the current version of the core provider, or, if the core provider is included in the upgrade list,
	// from its target version.
	providerList, err := u.providerInventory.List()
	if err != nil {
		return nil, err
	}
	coreProviders := providerList.FilterCore()
	if len(coreProviders) != 1 {
		return nil, errors.Errorf("invalid management cluster: there should a core provider, found %d", len(coreProviders))
	}
	coreProvider := coreProviders[0]

	targetCoreProviderVersion := coreProvider.Version
	for _, providerToUpgrade := range upgradeItems {
		if providerToUpgrade.InstanceName() == coreProvider.InstanceName() {
			targetCoreProviderVersion = providerToUpgrade.NextVersion
			break
		}
	}

	targetContract, err := u.getProviderContractByVersion(coreProvider, targetCoreProviderVersion)
	if err != nil {
		return nil, err
	}

	if targetContract != clusterv1.GroupVersion.Version {
		return nil, errors.Errorf("current version of clusterctl could only upgrade to %s contract, requested %s", clusterv1.GroupVersion.Version, targetContract)
	}

	// Builds the custom upgrade plan, by adding all the upgrade items after checking consistency with the targetContract.
	upgradeInstanceNames := sets.NewString()
	upgradePlan := &UpgradePlan{
		Contract: targetContract,
	}

	for _, upgradeItem := range upgradeItems {
		// Match the upgrade item with the corresponding provider in the management cluster
		var provider *clusterctlv1.Provider
		for i := range providerList.Items {
			if providerList.Items[i].InstanceName() == upgradeItem.InstanceName() {
				provider = &providerList.Items[i]
				break
			}
		}
		if provider == nil {
			return nil, errors.Errorf("unable to complete that upgrade: the provider %s in not part of the management cluster", upgradeItem.InstanceName())
		}

		// Retrieves the contract that is supported by the target version of the provider.
		contract, err := u.getProviderContractByVersion(*provider, upgradeItem.NextVersion)
		if err != nil {
			return nil, err
		}

		if contract != targetContract {
			return nil, errors.Errorf("unable to complete that upgrade: the target version for the provider %s supports the %s API Version of Cluster API (contract), while the management cluster is using %s", upgradeItem.InstanceName(), contract, targetContract)
		}

		upgradePlan.Providers = append(upgradePlan.Providers, upgradeItem)
		upgradeInstanceNames.Insert(upgradeItem.InstanceName())
	}

	// Before doing upgrades, checks if other providers in the management cluster are lagging behind the target contract.
	for _, provider := range providerList.Items {
		// skip providers already included in the upgrade plan
		if upgradeInstanceNames.Has(provider.InstanceName()) {
			continue
		}

		// Retrieves the contract that is supported by the current version of the provider.
		contract, err := u.getProviderContractByVersion(provider, provider.Version)
		if err != nil {
			return nil, err
		}

		if contract != targetContract {
			return nil, errors.Errorf("unable to complete that upgrade: the provider %s supports the %s API Version of Cluster API (contract), while the management cluster is being updated to %s. Please include the %[1]s provider in the upgrade", provider.InstanceName(), contract, targetContract)
		}
	}
	return upgradePlan, nil
}

// getProviderContractByVersion returns the contract that a provider will support if updated to the given target version.
func (u *providerUpgrader) getProviderContractByVersion(provider clusterctlv1.Provider, targetVersion string) (string, error) {
	targetSemVersion, err := version.ParseSemantic(targetVersion)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse target version for the %s provider", provider.InstanceName())
	}

	// Gets the metadata for the core Provider
	upgradeInfo, err := u.getUpgradeInfo(provider)
	if err != nil {
		return "", err
	}

	releaseSeries := upgradeInfo.metadata.GetReleaseSeriesForVersion(targetSemVersion)
	if releaseSeries == nil {
		return "", errors.Errorf("invalid target version: version %s for the provider %s does not match any release series", targetVersion, provider.InstanceName())
	}
	return releaseSeries.Contract, nil
}

// getUpgradeComponents returns the provider components for the selected target version.
func (u *providerUpgrader) getUpgradeComponents(provider UpgradeItem) (repository.Components, error) {
	configRepository, err := u.configClient.Providers().Get(provider.ProviderName, provider.GetProviderType())
	if err != nil {
		return nil, err
	}

	providerRepository, err := u.repositoryClientFactory(configRepository, u.configClient)
	if err != nil {
		return nil, err
	}

	options := repository.ComponentsOptions{
		Version:         provider.NextVersion,
		TargetNamespace: provider.Namespace,
	}
	components, err := providerRepository.Components().Get(options)
	if err != nil {
		return nil, err
	}
	return components, nil
}

func (u *providerUpgrader) doUpgrade(upgradePlan *UpgradePlan) error {
	// Check for multiple instances of the same provider if current contract is v1alpha3.
	if upgradePlan.Contract == clusterv1.GroupVersion.Version {
		if err := u.providerInventory.CheckSingleProviderInstance(); err != nil {
			return err
		}
	}

	// Ensure Providers are updated in the following order: Core, Bootstrap, ControlPlane, Infrastructure.
	providers := upgradePlan.Providers
	sort.Slice(providers, func(a, b int) bool {
		return providers[a].GetProviderType().Order() < providers[b].GetProviderType().Order()
	})

	// Scale down all providers.
	// This is done to ensure all Pods of all "old" provider Deployments have been deleted.
	// Otherwise it can happen that a provider Pod survives the upgrade because we create
	// a new Deployment with the same selector directly after `Delete`.
	// This can lead to a failed upgrade because:
	// * new provider Pods fail to startup because they try to list resources.
	// * list resources fails, because the API server hits the old provider Pod when trying to
	//   call the conversion webhook for those resources.
	for _, upgradeItem := range providers {
		// If there is not a specified next version, skip it (we are already up-to-date).
		if upgradeItem.NextVersion == "" {
			continue
		}

		// Scale down provider.
		if err := u.scaleDownProvider(upgradeItem.Provider); err != nil {
			return err
		}
	}

	// Delete old providers and deploy new ones if necessary, i.e. there is a NextVersion.
	for _, upgradeItem := range providers {
		// If there is not a specified next version, skip it (we are already up-to-date).
		if upgradeItem.NextVersion == "" {
			continue
		}

		// Gets the provider components for the target version.
		components, err := u.getUpgradeComponents(upgradeItem)
		if err != nil {
			return err
		}

		// Delete the provider, preserving CRD, namespace and the inventory.
		if err := u.providerComponents.Delete(DeleteOptions{
			Provider:         upgradeItem.Provider,
			IncludeNamespace: false,
			IncludeCRDs:      false,
			SkipInventory:    true,
		}); err != nil {
			return err
		}

		// Install the new version of the provider components.
		if err := installComponentsAndUpdateInventory(components, u.providerComponents, u.providerInventory); err != nil {
			return err
		}
	}

	// Delete webhook namespace since it's not needed from v1alpha4.
	if upgradePlan.Contract == clusterv1.GroupVersion.Version {
		if err := u.providerComponents.DeleteWebhookNamespace(); err != nil {
			return err
		}
	}

	return nil
}

func (u *providerUpgrader) scaleDownProvider(provider clusterctlv1.Provider) error {
	log := logf.Log
	log.Info("Scaling down", "Provider", provider.Name, "Version", provider.Version, "Namespace", provider.Namespace)

	cs, err := u.proxy.NewClient()
	if err != nil {
		return err
	}

	// Fetch all Deployments belonging to a provider.
	deploymentList := &appsv1.DeploymentList{}
	if err := cs.List(ctx,
		deploymentList,
		client.InNamespace(provider.Namespace),
		client.MatchingLabels{
			clusterctlv1.ClusterctlLabelName: "",
			clusterv1.ProviderLabelName:      provider.ManifestLabel(),
		}); err != nil {
		return errors.Wrapf(err, "failed to list Deployments for provider %s", provider.Name)
	}

	// Scale down provider Deployments.
	for _, deployment := range deploymentList.Items {
		log.V(5).Info("Scaling down", "Deployment", deployment.Name, "Namespace", deployment.Namespace)
		if err := scaleDownDeployment(ctx, cs, deployment); err != nil {
			return err
		}
	}

	return nil
}

// scaleDownDeployment scales down a Deployment to 0 and waits until all replicas have been deleted.
func scaleDownDeployment(ctx context.Context, c client.Client, deploy appsv1.Deployment) error {
	if err := retryWithExponentialBackoff(newWriteBackoff(), func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(&deploy), deployment); err != nil {
			return errors.Wrapf(err, "failed to get Deployment/%s", deploy.GetName())
		}

		// Deployment already scaled down, return early.
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
			return nil
		}

		// Scale down.
		deployment.Spec.Replicas = pointer.Int32Ptr(0)
		if err := c.Update(ctx, deployment); err != nil {
			return errors.Wrapf(err, "failed to update Deployment/%s", deploy.GetName())
		}
		return nil
	}); err != nil {
		return errors.Wrapf(err, "failed to scale down Deployment")
	}

	deploymentScaleToZeroBackOff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1,
		Steps:    60,
		Jitter:   0.4,
	}
	if err := retryWithExponentialBackoff(deploymentScaleToZeroBackOff, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(&deploy), deployment); err != nil {
			return errors.Wrapf(err, "failed to get Deployment/%s", deploy.GetName())
		}

		// Deployment is scaled down.
		if deployment.Status.Replicas == 0 {
			return nil
		}

		return errors.Errorf("Deployment still has %d replicas", deployment.Status.Replicas)
	}); err != nil {
		return errors.Wrapf(err, "failed to wait until Deployment is scaled down")
	}

	return nil
}

func newProviderUpgrader(configClient config.Client, proxy Proxy, repositoryClientFactory RepositoryClientFactory, providerInventory InventoryClient, providerComponents ComponentsClient) *providerUpgrader {
	return &providerUpgrader{
		configClient:            configClient,
		proxy:                   proxy,
		repositoryClientFactory: repositoryClientFactory,
		providerInventory:       providerInventory,
		providerComponents:      providerComponents,
	}
}